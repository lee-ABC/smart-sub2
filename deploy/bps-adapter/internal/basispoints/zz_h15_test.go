package basispoints

import (
	"testing"
	"time"
)

// 按需额度读数写回账本时，键只能查、不能造。
//
// addSample 在键不存在时会新建一条账户记录。所以写回如果用的是这里现算的键而不
// 是执行器实际用的那个，不会报错——会悄悄多出一条同号的影子记录：花费 0、额度
// 满格。它随后进入同套餐的中位数，把别的号的估算一起带偏。
func TestWriteBackNeverInventsAnAccount(t *testing.T) {
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	reset := now.Add(7 * 24 * time.Hour)
	ledger := newCostLedger()
	ledger.now = func() time.Time { return now }
	ledger.record(usageTarget{Key: "bp-real", File: "a.json", Label: "a@example.com"},
		12, true, false, tokenUsage{})

	key, label, ok := ledger.keyForFile("a.json")
	if !ok || key != "bp-real" || label != "a@example.com" {
		t.Fatalf("按文件名查键 = (%q,%q,%v)，想要 (bp-real,a@example.com,true)", key, label, ok)
	}
	if _, _, ok := ledger.keyForFile("nobody.json"); ok {
		t.Error("账本里没有的文件名也查出了键")
	}

	service := &Service{ledger: ledger}
	if service.noteQuotaForFile("nobody.json", sampleAt(5, reset)) {
		t.Error("给一个账本里没有的号写回，竟然说写成功了")
	}
	if n := len(ledger.view()); n != 1 {
		t.Fatalf("写回一个不认识的号之后账本有 %d 条，想要 1 条（不许凭空多出影子号）", n)
	}

	if !service.noteQuotaForFile("a.json", sampleAt(5, reset)) {
		t.Fatal("给账本里已有的号写回失败")
	}
	account, found := ledger.view()["bp-real"]
	if !found || len(account.Segments) != 1 {
		t.Fatalf("写回没有落到已有的那条记录上: found=%v %#v", found, account)
	}
	if got := account.Segments[0].Last.UsedPercent; got != 5 {
		t.Errorf("写回的百分比 = %v, want 5", got)
	}
}

// 大小写不同的文件名指的是同一个号。查不到就等于不写回，那条卡片会一直停在旧数
// 上，而且没有任何提示说为什么。
func TestWriteBackMatchesFileNameCaseInsensitively(t *testing.T) {
	ledger := newCostLedger()
	ledger.record(usageTarget{Key: "bp-x", File: "Mixed.JSON"}, 1, true, false, tokenUsage{})
	if _, _, ok := ledger.keyForFile("mixed.json"); !ok {
		t.Error("只是大小写不同就查不到了")
	}
}

// 实测花费是这条路径上唯一被量出来的数，所以它是下限。
//
// 没有这一条，一份读数为 0% 的额度会把真实发生过的花费抹成 0，卡片就会在
// "本周约花 $0.00" 的正下方写着"本周经 CPA $1.10"——两个数来自同一本账，却互相
// 打脸。线上 ak.hqryshyjananhj 就是这样：used_percent 0、metered_usd 1.0977。
func TestMeasuredSpendIsAFloorForTheEstimate(t *testing.T) {
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	reset := now.Add(7 * 24 * time.Hour)
	ledger := newCostLedger()
	ledger.now = func() time.Time { return now }

	// 一个号提供比例样本，好让池子里有东西可借。
	donor := usageTarget{Key: "bp-donor", File: "donor.json"}
	ledger.addSample(donor, sampleAt(10, reset))
	ledger.record(donor, 100, true, false, tokenUsage{})
	ledger.addSample(donor, sampleAt(20, reset))

	// 受测的号：读数停在 0%，但本网关实打实记了 1.10 美元。
	quiet := usageTarget{Key: "bp-quiet", File: "quiet.json"}
	ledger.record(quiet, 1.10, true, false, tokenUsage{})
	ledger.addSample(quiet, sampleAt(0, reset))

	summary := estimateCosts(ledger.view(), now)
	var got *costEstimate
	for i := range summary.Accounts {
		if summary.Accounts[i].File == "quiet.json" {
			got = &summary.Accounts[i]
		}
	}
	if got == nil {
		t.Fatal("估算结果里没有这个号")
	}
	if got.MeteredUSD <= 0 {
		t.Fatalf("前提不成立：实测花费是 %v", got.MeteredUSD)
	}
	if got.UsedUSD < got.MeteredUSD {
		t.Errorf("已花 %.4f 小于实测 %.4f —— 卡片会报一个比真实花费还小的数",
			got.UsedUSD, got.MeteredUSD)
	}
}
