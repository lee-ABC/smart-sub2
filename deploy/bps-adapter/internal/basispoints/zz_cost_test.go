package basispoints

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func about(t *testing.T, got, want float64, what string) {
	t.Helper()
	if math.Abs(got-want) > 1e-9 {
		t.Errorf("%s = %.10f, want %.10f", what, got, want)
	}
}

// The prices have to reproduce what the main site bills, or the plugin's
// figures cannot be reconciled with it. These cases are real rows.
func TestPricingMatchesTheMainSite(t *testing.T) {
	cfg := defaultConfig()
	cases := []struct {
		model   string
		usage   tokenUsage
		wantUSD float64
	}{
		// Ordinary request: uncached input, cached input and output each at
		// their own rate.
		{"gpt-6-sol", tokenUsage{Input: 100000, Cached: 90000, Output: 2000}, (10000*2 + 90000*0.2 + 2000*10) / 1e6},
		{"gpt-6-astra", tokenUsage{Input: 50000, Cached: 40000, Output: 3000}, (10000*10 + 40000*1 + 3000*50) / 1e6},
		// A prompt over the long-context threshold prices entirely at the
		// long-context rates -- the cached part included.
		{"gpt-5.6-sol", tokenUsage{Input: 300000, Cached: 280000, Output: 5000}, (20000*10 + 280000*1 + 5000*45) / 1e6},
		// Exactly at the threshold is still the standard rate.
		{"gpt-6-sol", tokenUsage{Input: 272000, Cached: 0, Output: 0}, 272000 * 2 / 1e6},
		{"gpt-6-sol", tokenUsage{Input: 272001, Cached: 0, Output: 0}, 272001 * 4 / 1e6},
	}
	for _, c := range cases {
		price, ok := cfg.priceFor(c.model)
		if !ok {
			t.Fatalf("no price for %s", c.model)
		}
		about(t, price.cost(c.usage), c.wantUSD, c.model)
	}
}

// CPA may carry a thinking level in the model name; pricing must still match.
func TestPricingIgnoresThinkingSuffix(t *testing.T) {
	cfg := defaultConfig()
	plain, ok := cfg.priceFor("gpt-6-sol")
	if !ok {
		t.Fatal("no price")
	}
	suffixed, ok := cfg.priceFor("gpt-6-sol(high)")
	if !ok {
		t.Fatal("a thinking level made the model unpriceable")
	}
	if plain != suffixed {
		t.Error("the thinking level changed the price")
	}
	if _, ok := cfg.priceFor("some-unknown-model"); ok {
		t.Error("an unknown model was priced anyway")
	}
}

// Cached tokens are part of input_tokens in the Responses API. Pricing them
// twice would overstate every cached request, which is nearly all of them.
func TestCachedTokensAreNotChargedTwice(t *testing.T) {
	price := defaultModelPrices["gpt-6-sol"]
	allCached := price.cost(tokenUsage{Input: 100000, Cached: 100000})
	about(t, allCached, 100000*0.2/1e6, "fully cached request")
	if allCached >= price.cost(tokenUsage{Input: 100000}) {
		t.Error("a fully cached prompt costs as much as an uncached one")
	}
}

func TestUsageIsReadFromTheResponse(t *testing.T) {
	var response map[string]any
	if err := json.Unmarshal([]byte(`{"usage":{"input_tokens":1200,"output_tokens":340,
		"input_tokens_details":{"cached_tokens":1000},"output_tokens_details":{"reasoning_tokens":200}}}`), &response); err != nil {
		t.Fatal(err)
	}
	usage, ok := usageFromResponse(response)
	if !ok {
		t.Fatal("usage not found")
	}
	if usage.Input != 1200 || usage.Cached != 1000 || usage.Output != 340 {
		t.Errorf("usage = %+v", usage)
	}
	// Reasoning tokens are already inside output_tokens; counting them again
	// would inflate every reasoning request.
	if _, ok := usageFromResponse(map[string]any{}); ok {
		t.Error("a response without usage reported usage anyway")
	}
}

func ledgerAt(t *testing.T, now *time.Time) *costLedger {
	t.Helper()
	ledger := newCostLedger()
	ledger.now = func() time.Time { return *now }
	return ledger
}

func sampleAt(percent float64, reset time.Time) quotaSnapshot {
	return quotaSnapshot{
		Plan:    "team",
		Primary: &quotaWindow{UsedPercent: percent, WindowSecs: 7 * 24 * 3600, ResetAt: reset},
	}
}

// The whole estimate rests on this: spend and quota move together, so their
// ratio says what a percentage point is worth.
func TestRemainingIsEstimatedFromSpendPerPercent(t *testing.T) {
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	reset := now.Add(72 * time.Hour)
	ledger := ledgerAt(t, &now)
	target := usageTarget{Key: "bp-a", File: "a.json", Label: "a@example.com"}

	ledger.addSample(target, sampleAt(20, reset))
	for i := 0; i < 100; i++ {
		now = now.Add(time.Minute)
		ledger.record(target, 2, true, false, tokenUsage{}) // $200 in total
	}
	ledger.addSample(target, sampleAt(40, reset))

	summary := estimateCosts(ledger.view(), now)
	if len(summary.Accounts) != 1 {
		t.Fatalf("accounts = %d", len(summary.Accounts))
	}
	account := summary.Accounts[0]
	if !account.Known {
		t.Fatalf("no estimate: %s", account.Reason)
	}
	// $200 bought 20 points, so a point is $10: 60 points left is $600, and
	// the 40 points used are worth $400.
	about(t, account.RemainingUSD, 600, "remaining")
	about(t, account.UsedUSD, 400, "used")
	about(t, account.WindowUSD, 1000, "window")
	about(t, account.MeteredUSD, 200, "metered")
}

// A short stretch is mostly rounding noise -- the percentage arrives in whole
// points -- so it must not produce a confident dollar figure.
func TestTooLittleMovementGivesNoEstimate(t *testing.T) {
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	reset := now.Add(72 * time.Hour)
	ledger := ledgerAt(t, &now)
	target := usageTarget{Key: "bp-a"}

	ledger.addSample(target, sampleAt(20, reset))
	now = now.Add(time.Minute)
	ledger.record(target, 5, true, false, tokenUsage{})
	ledger.addSample(target, sampleAt(21, reset)) // one point only

	account := estimateCosts(ledger.view(), now).Accounts[0]
	if account.Known {
		t.Errorf("estimated from a single percentage point: remaining %.2f", account.RemainingUSD)
	}
	if account.Reason == "" {
		t.Error("no reason given for the missing estimate")
	}
	// What was actually metered is still reported: that part is not a guess.
	about(t, account.MeteredUSD, 5, "metered")
}

// Credentials on one plan share a measurement pool, so a credential that has
// not moved much yet is still priced from what its peers measured.
func TestAPlansMeasurementsCoverACredentialWithNoneOfItsOwn(t *testing.T) {
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	reset := now.Add(72 * time.Hour)
	ledger := ledgerAt(t, &now)
	measured := usageTarget{Key: "bp-measured"}
	fresh := usageTarget{Key: "bp-fresh"}

	ledger.addSample(measured, sampleAt(10, reset))
	for i := 0; i < 50; i++ {
		now = now.Add(time.Minute)
		ledger.record(measured, 4, true, false, tokenUsage{}) // $200 over 20 points -> $10/point
	}
	ledger.addSample(measured, sampleAt(30, reset))
	ledger.addSample(fresh, sampleAt(25, reset))

	summary := estimateCosts(ledger.view(), now)
	byKey := map[string]costEstimate{}
	for _, account := range summary.Accounts {
		byKey[account.Key] = account
	}
	if !byKey["bp-fresh"].Known {
		t.Fatalf("the fresh credential got no estimate: %s", byKey["bp-fresh"].Reason)
	}
	about(t, byKey["bp-fresh"].RemainingUSD, 750, "fresh remaining")
	// It has spent nothing through this gateway, and that is reported honestly
	// even though its quota says otherwise.
	about(t, byKey["bp-fresh"].MeteredUSD, 0, "fresh metered")
}

// A quota reset ends the stretch: usage drops back and the spend that follows
// belongs to the new window, not the old one.
func TestAResetStartsANewWindow(t *testing.T) {
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	reset := now.Add(2 * time.Hour)
	ledger := ledgerAt(t, &now)
	target := usageTarget{Key: "bp-a"}

	ledger.addSample(target, sampleAt(80, reset))
	now = now.Add(time.Hour)
	ledger.record(target, 50, true, false, tokenUsage{})
	ledger.addSample(target, sampleAt(90, reset))

	// The window rolls over: usage falls and the reset time moves on.
	now = now.Add(2 * time.Hour)
	newReset := reset.Add(7 * 24 * time.Hour)
	ledger.addSample(target, sampleAt(3, newReset))

	view := ledger.view()
	if got := len(view["bp-a"].Segments); got != 2 {
		t.Fatalf("segments = %d, want 2: a reset must not extend the old window", got)
	}
	// The old stretch still measures the ratio; the new one has barely begun.
	account := estimateCosts(view, now).Accounts[0]
	if !account.Known {
		t.Fatalf("no estimate: %s", account.Reason)
	}
	about(t, account.UsedPercent, 3, "used percent after reset")
	about(t, account.RemainingUSD, 5*97, "remaining after reset")
}

// A credential whose window has already expired must not be priced from a
// stale percentage.
func TestAnExpiredWindowIsNotPriced(t *testing.T) {
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	ledger := ledgerAt(t, &now)
	target := usageTarget{Key: "bp-a"}
	ledger.addSample(target, sampleAt(90, now.Add(time.Hour)))

	later := now.Add(3 * time.Hour)
	account := estimateCosts(ledger.view(), later).Accounts[0]
	if account.Known {
		t.Error("priced a credential from a window that has already reset")
	}
}

// Sampling is paced per credential: that pacing is the only thing keeping this
// feature from polling the upstream.
func TestSamplingIsPacedPerCredential(t *testing.T) {
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	ledger := ledgerAt(t, &now)
	if !ledger.claimSample("bp-a", 15*time.Minute) {
		t.Fatal("the first sample was refused")
	}
	if ledger.claimSample("bp-a", 15*time.Minute) {
		t.Error("a second sample was allowed immediately")
	}
	if !ledger.claimSample("bp-b", 15*time.Minute) {
		t.Error("another credential was blocked by the first one's pacing")
	}
	now = now.Add(20 * time.Minute) // past the interval even with jitter
	if !ledger.claimSample("bp-a", 15*time.Minute) {
		t.Error("sampling never resumed")
	}
	if ledger.claimSample("bp-a", 0) {
		t.Error("sampling ran while disabled")
	}
}

// The ledger has to survive a restart, or every reload would throw away the
// measurements the estimate depends on.
func TestTheLedgerSurvivesARestart(t *testing.T) {
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "cost-ledger.json")
	target := usageTarget{Key: "bp-a", File: "a.json"}
	reset := now.Add(72 * time.Hour)

	first := ledgerAt(t, &now)
	first.setPath(path)
	first.addSample(target, sampleAt(10, reset))
	for i := 0; i < 30; i++ {
		now = now.Add(time.Minute)
		first.record(target, 10, true, false, tokenUsage{})
	}
	first.addSample(target, sampleAt(40, reset))
	first.flush()

	second := ledgerAt(t, &now)
	second.setPath(path)
	account := estimateCosts(second.view(), now).Accounts[0]
	if !account.Known {
		t.Fatalf("the reloaded ledger lost its measurements: %s", account.Reason)
	}
	about(t, account.RemainingUSD, 600, "remaining after reload")
	about(t, account.MeteredUSD, 300, "metered after reload")
}

// A damaged ledger must cost history, not the plugin.
func TestADamagedLedgerIsDiscarded(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cost-ledger.json")
	if err := os.WriteFile(path, []byte("{not json"), 0600); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	ledger := ledgerAt(t, &now)
	ledger.setPath(path)
	if got := len(ledger.view()); got != 0 {
		t.Errorf("a damaged ledger loaded %d accounts", got)
	}
	ledger.record(usageTarget{Key: "bp-a"}, 1, true, false, tokenUsage{})
	if got := ledger.view()["bp-a"].CostUSD; got != 1 {
		t.Errorf("the ledger did not recover: cost = %v", got)
	}
}

// A response that carries no usage, or a model with no price, must not be
// silently counted as free: the unpriced tally is what makes that visible.
func TestUnpricedRequestsAreCountedSeparately(t *testing.T) {
	now := time.Now()
	ledger := ledgerAt(t, &now)
	target := usageTarget{Key: "bp-a"}
	ledger.record(target, 0, false, false, tokenUsage{})
	ledger.record(target, 3, true, false, tokenUsage{})

	account := ledger.view()["bp-a"]
	if account.Requests != 2 || account.Unpriced != 1 {
		t.Errorf("requests = %d, unpriced = %d, want 2 and 1", account.Requests, account.Unpriced)
	}
	about(t, account.CostUSD, 3, "cost")
}

// The long-context threshold is the one place a small change in tokens changes
// the rate, so it is worth pinning what the window covers.
func TestOnlyTheGoverningWindowIsPriced(t *testing.T) {
	snapshot := quotaSnapshot{
		Primary:   &quotaWindow{UsedPercent: 50, WindowSecs: 5 * 3600},
		Secondary: &quotaWindow{UsedPercent: 12, WindowSecs: 7 * 24 * 3600},
	}
	window := governingWindow(snapshot)
	if window == nil || window.UsedPercent != 12 {
		t.Fatalf("governing window = %+v, want the weekly one", window)
	}
	if governingWindow(quotaSnapshot{}) != nil {
		t.Error("a snapshot with no windows produced one")
	}
}

// 上游有时换一个更便宜的模型来回答。按请求的模型计价会把这种请求高估几十倍，
// 所以计价要认上游实际服务的那个。
func TestPricingFollowsTheModelTheUpstreamActuallyServed(t *testing.T) {
	cfg := defaultConfig()
	response := map[string]any{
		"model": "gpt-5.6-luna",
		"usage": map[string]any{"input_tokens": int64(100000), "output_tokens": int64(2000)},
	}
	served := servedModel(response, "gpt-6-astra")
	if served != "gpt-5.6-luna" {
		t.Fatalf("served = %q, want the model in the response", served)
	}
	usage, _ := usageFromResponse(response)
	luna, _ := cfg.priceFor(served)
	astra, _ := cfg.priceFor("gpt-6-astra")
	if luna.cost(usage) >= astra.cost(usage) {
		t.Fatal("the substituted model is not cheaper, so this test proves nothing")
	}
	about(t, luna.cost(usage), (100000*0.2+2000*1.2)/1e6, "priced at what was served")

	// 回包没写模型时，退回到请求的那个。
	if got := servedModel(map[string]any{}, "gpt-6-sol"); got != "gpt-6-sol" {
		t.Errorf("servedModel = %q, want the requested model", got)
	}
}

// 被换掉的次数要单独记，否则卡片上看不出上游在降级。
func TestDowngradesAreCounted(t *testing.T) {
	now := time.Now()
	ledger := ledgerAt(t, &now)
	target := usageTarget{Key: "bp-a"}
	ledger.record(target, 1, true, true, tokenUsage{})
	ledger.record(target, 1, true, false, tokenUsage{})
	ledger.record(target, 0, false, true, tokenUsage{})

	account := ledger.view()["bp-a"]
	if account.Downgraded != 2 {
		t.Errorf("降级次数 = %d, want 2", account.Downgraded)
	}
	if account.Requests != 3 {
		t.Errorf("请求数 = %d, want 3", account.Requests)
	}
}

// 如果这个号从额度窗口开始之前就在记账，那它这一窗的花费就是实测值，
// 不是估计；拿它自己的实测除以已用百分比，比借用别人的测量准得多。
func TestACompleteWindowIsMeasuredNotEstimated(t *testing.T) {
	now := time.Date(2026, 10, 1, 14, 38, 0, 0, time.UTC)
	reset := now.Add(7 * 24 * time.Hour)
	ledger := ledgerAt(t, &now)
	target := usageTarget{Key: "bp-subject"}

	// 这个号从窗口一开始就在记账。
	ledger.addSample(target, sampleAt(0, reset))
	for i := 0; i < 70; i++ {
		now = now.Add(time.Minute)
		ledger.record(target, 1, true, false, tokenUsage{}) // 合计 $70
	}
	ledger.addSample(target, sampleAt(7, reset))

	// 另一个号给出一个差得很远的测量，用来证明池子没有把结论带偏。
	other := usageTarget{Key: "bp-other"}
	ledger.addSample(other, sampleAt(10, reset))
	for i := 0; i < 20; i++ {
		now = now.Add(time.Minute)
		ledger.record(other, 1, true, false, tokenUsage{})
	}
	ledger.addSample(other, sampleAt(30, reset))

	var subject costEstimate
	for _, a := range estimateCosts(ledger.view(), now).Accounts {
		if a.Key == "bp-subject" {
			subject = a
		}
	}
	if !subject.Known {
		t.Fatalf("没有给出估算: %s", subject.Reason)
	}
	if !subject.Direct {
		t.Error("用了别人的测量，而不是这个号自己的完整记录")
	}
	// 已用就是实测的 $70，不是推算出来的。
	about(t, subject.UsedUSD, 70, "已用")
	about(t, subject.MeteredUSD, 70, "实测")
	// $70 买了 7 个百分点，所以剩下的 93 点约值 $930。
	about(t, subject.RemainingUSD, 70.0/7*93, "剩余")
	if subject.Confidence != "high" {
		t.Errorf("置信度 = %q, want high", subject.Confidence)
	}
	// 区间要体现整数百分比带来的 ±0.5 点不确定性。
	if !(subject.LowUSD < subject.RemainingUSD && subject.RemainingUSD < subject.HighUSD) {
		t.Errorf("区间不合理: %.1f < %.1f < %.1f", subject.LowUSD, subject.RemainingUSD, subject.HighUSD)
	}
}

// 中途才接手的号没有完整记录，只能估，而且要优先用它自己的测量。
func TestAPartialWindowFallsBackToItsOwnMeasurement(t *testing.T) {
	now := time.Date(2026, 10, 1, 14, 0, 0, 0, time.UTC)
	reset := now.Add(3 * 24 * time.Hour) // 窗口早就开始了
	ledger := ledgerAt(t, &now)
	target := usageTarget{Key: "bp-late"}

	ledger.addSample(target, sampleAt(40, reset))
	for i := 0; i < 50; i++ {
		now = now.Add(time.Minute)
		ledger.record(target, 1, true, false, tokenUsage{}) // $50 买了 5 个点 -> 每点 $10
	}
	ledger.addSample(target, sampleAt(45, reset))

	// 同套餐另一个号给出完全不同的单价，若被采纳就说明优先级错了。
	other := usageTarget{Key: "bp-other"}
	ledger.addSample(other, sampleAt(10, reset))
	for i := 0; i < 60; i++ {
		now = now.Add(time.Minute)
		ledger.record(other, 2, true, false, tokenUsage{}) // $120 买了 60 点 -> 每点 $2
	}
	ledger.addSample(other, sampleAt(70, reset))

	var late costEstimate
	for _, a := range estimateCosts(ledger.view(), now).Accounts {
		if a.Key == "bp-late" {
			late = a
		}
	}
	if !late.Known {
		t.Fatalf("没有给出估算: %s", late.Reason)
	}
	if late.Direct {
		t.Error("记录并不覆盖整个窗口，却当成了实测")
	}
	// 用它自己的每点 $10，而不是另一个号的 $2。
	about(t, late.RemainingUSD, 10*55, "剩余")
	// 已用是推算的，不能当成实测。
	if late.MeteredUSD == late.UsedUSD {
		t.Error("把不完整的实测当成了整窗已用")
	}
}
