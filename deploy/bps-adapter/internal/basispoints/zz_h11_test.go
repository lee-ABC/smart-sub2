package basispoints

import (
	"encoding/json"
	"testing"
	"time"
)

// 在面板上停用一个号之后，它必须一直是停用的。
//
// 宿主把这个标记写进凭据文件，又从插件交回的记录里读它；记录里缺这个字段就解成
// false。令牌续期会自己重建记录，所以这个字段一旦漏掉，被停用的号过几分钟就自己
// 回到启用——正是 2026-10-01 观察到的现象。
func TestADisabledCredentialStaysDisabled(t *testing.T) {
	raw := []byte(`{"disabled":true,"access_token":"x","account_id":"acct","email":"a@example.com"}`)
	c, err := parseCredential(raw)
	if err != nil {
		t.Fatal(err)
	}
	data := authData(raw, "a.json", c, Provider)
	disabled, ok := data["Disabled"].(bool)
	if !ok {
		t.Fatalf("记录里没有 Disabled 字段（有的是 %v），宿主会把它当成启用", data["Disabled"])
	}
	if !disabled {
		t.Error("被停用的号交回宿主时变成了启用")
	}
}

// 没被停用的号当然要照常启用；文件损坏也不能把能用的号停掉。
func TestAnEnabledCredentialIsNotDisabled(t *testing.T) {
	cases := map[string][]byte{
		"明确写了 false": []byte(`{"disabled":false,"access_token":"x","account_id":"a"}`),
		"根本没这个字段":    []byte(`{"access_token":"x","account_id":"a"}`),
		"字段类型不对":     []byte(`{"disabled":"yes","access_token":"x","account_id":"a"}`),
		"不是 JSON":    []byte(`{{{`),
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			if credentialDisabled(raw) {
				t.Error("把一个能用的号判成了停用")
			}
		})
	}
	// 续期之后这个标记也要还在：续期是整份 JSON 原样带走、只改令牌字段。
	var root map[string]any
	if err := json.Unmarshal([]byte(`{"disabled":true,"refresh_token":"r"}`), &root); err != nil {
		t.Fatal(err)
	}
	root["access_token"] = "new"
	updated, _ := json.Marshal(root)
	if !credentialDisabled(updated) {
		t.Error("续期改写之后停用标记丢了")
	}
}

// 新导入的号，额度窗口是从它第一个请求开始算的，所以账本条目必然比窗口起点晚一瞬。
// 按时间戳硬比会把"覆盖最完整"的号判成不完整，这正是 albertaascensio 那个号显示
// "约花 $44" 而实测是 $64.62 的原因。
func TestANewCredentialCountsAsCoveringItsFirstWindow(t *testing.T) {
	started := time.Date(2026, 10, 1, 14, 38, 22, 0, time.UTC)
	// 账本比窗口晚了 807 毫秒——线上实际发生的差值。
	account := accountLedger{
		Since: started.Add(807 * time.Millisecond),
		Segments: []quotaSegment{{
			First: quotaSample{At: started.Add(time.Second), UsedPercent: 0},
			Last:  quotaSample{At: started.Add(time.Hour), UsedPercent: 4},
		}},
	}
	if !coversWholeWindow(account, started) {
		t.Error("晚了不到一秒就被判成没覆盖整个窗口")
	}

	// 中途接手的号：第一次采样时窗口已经用掉一大截，就不能当实测。
	late := accountLedger{
		Since: started.Add(3 * 24 * time.Hour),
		Segments: []quotaSegment{{
			First: quotaSample{At: started.Add(3 * 24 * time.Hour), UsedPercent: 40},
			Last:  quotaSample{At: started.Add(4 * 24 * time.Hour), UsedPercent: 45},
		}},
	}
	if coversWholeWindow(late, started) {
		t.Error("接手时窗口已用掉 40%，却被当成完整覆盖")
	}

	// 一次额度都没采到，就无从判断。
	if coversWholeWindow(accountLedger{Since: started.Add(time.Second)}, started) {
		t.Error("没有任何额度采样却判成了完整覆盖")
	}
	if coversWholeWindow(account, time.Time{}) {
		t.Error("窗口起点未知却判成了完整覆盖")
	}
}

// 端到端：复刻线上那个号的情形，它必须走实测口径。
func TestTheNewCredentialIsMeasuredEndToEnd(t *testing.T) {
	now := time.Date(2026, 10, 1, 14, 38, 22, 0, time.UTC)
	reset := now.Add(7 * 24 * time.Hour)
	ledger := newCostLedger()
	ledger.now = func() time.Time { return now }
	target := usageTarget{Key: "bp-fresh", File: "fresh.json"}

	// 第一个请求先到，账本条目就此建立——比窗口起点晚一瞬。
	now = now.Add(807 * time.Millisecond)
	ledger.record(target, 0.01, true, false, tokenUsage{})
	now = now.Add(time.Second)
	ledger.addSample(target, sampleAt(0, reset))
	for i := 0; i < 64; i++ {
		now = now.Add(time.Minute)
		ledger.record(target, 1, true, false, tokenUsage{})
	}
	ledger.addSample(target, sampleAt(4, reset))

	account := estimateCosts(ledger.view(), now).Accounts[0]
	if !account.Direct {
		t.Fatalf("没走实测口径: direct=%v reason=%q", account.Direct, account.Reason)
	}
	about(t, account.UsedUSD, account.MeteredUSD, "已花应当等于实测")
	about(t, account.UsedUSD, 64.01, "已花")
	if account.Confidence != "high" {
		t.Errorf("置信度 = %q, want high", account.Confidence)
	}
}
