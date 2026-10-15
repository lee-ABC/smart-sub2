package basispoints

// Ledger handover (ledger_handover.go): two instances of the plugin keeping
// one ledger file across a hot swap, and a retired one flushing last when CPA
// stops, lose nothing and count nothing twice.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// legacyLedger is a build without handover, saving exactly as a1wide does:
// its whole view through <ledger>.tmp, no writer mark.
type legacyLedger struct {
	path     string
	accounts map[string]*accountLedger
}

func legacyLoad(t *testing.T, path string) *legacyLedger {
	t.Helper()
	legacy := &legacyLedger{path: path, accounts: map[string]*accountLedger{}}
	if data, err := os.ReadFile(path); err == nil {
		// The struct a1wide decodes into: version and accounts only.
		var file struct {
			Version  int                       `json:"version"`
			Accounts map[string]*accountLedger `json:"accounts"`
		}
		if err := json.Unmarshal(data, &file); err != nil {
			t.Fatalf("a build without handover cannot read the file: %v", err)
		}
		legacy.accounts = file.Accounts
	}
	return legacy
}

func (l *legacyLedger) record(key string, now time.Time, cost float64) {
	account := l.accounts[key]
	if account == nil {
		account = &accountLedger{Since: now}
		l.accounts[key] = account
	}
	account.Touched = now
	account.Requests++
	account.CostUSD += cost
	hour := now.Unix() / 3600
	if n := len(account.Hours); n > 0 && account.Hours[n-1].Hour == hour {
		account.Hours[n-1].Cost += cost
		account.Hours[n-1].Requests++
	} else {
		account.Hours = append(account.Hours, hourCost{Hour: hour, Cost: cost, Requests: 1})
	}
}

// save is a1wide's saveLocked: the whole view, through <ledger>.tmp.
func (l *legacyLedger) save() error {
	data, err := json.Marshal(struct {
		Version  int                       `json:"version"`
		Accounts map[string]*accountLedger `json:"accounts"`
	}{ledgerVersion, l.accounts})
	if err != nil {
		return err
	}
	temp := l.path + ".tmp"
	if err := os.WriteFile(temp, data, 0600); err != nil {
		return err
	}
	return os.Rename(temp, l.path)
}

func handoverFile(t *testing.T, path string) ledgerFile {
	t.Helper()
	file, ok := readLedgerFile(path)
	if !ok {
		t.Fatal("ledger file unreadable")
	}
	return file
}

func handoverRequests(file ledgerFile, key string) int64 {
	if account := file.Accounts[key]; account != nil {
		return account.Requests
	}
	return 0
}

func handoverHourRequests(file ledgerFile, key string) int64 {
	total := int64(0)
	if account := file.Accounts[key]; account != nil {
		for _, bucket := range account.Hours {
			total += bucket.Requests
		}
	}
	return total
}

func recordN(ledger *costLedger, now *time.Time, key string, n int) {
	for i := 0; i < n; i++ {
		*now = now.Add(time.Second)
		ledger.record(usageTarget{Key: key, File: key + ".json"}, 1, true, false, tokenUsage{Input: 10, Output: 1})
	}
}

// The swap this build goes out with: a1wide (no handover) keeps finishing its
// streams and rewriting the file after a1wide2 loaded it.
func TestLedgerHandoverFromABuildWithoutIt(t *testing.T) {
	now := time.Date(2026, 10, 15, 3, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "cost-ledger.json")
	old := legacyLoad(t, path)
	for i := 0; i < 10; i++ {
		old.record("bp-a", now, 1)
	}
	if err := old.save(); err != nil {
		t.Fatal(err)
	}
	fresh := ledgerAt(t, &now)
	fresh.setPath(path)
	// The old instance's in-flight streams finish and it saves its whole view.
	for i := 0; i < 3; i++ {
		old.record("bp-a", now, 1)
	}
	if err := old.save(); err != nil {
		t.Fatal(err)
	}
	recordN(fresh, &now, "bp-a", 5)
	recordN(fresh, &now, "bp-b", 2)
	fresh.flush()
	file := handoverFile(t, path)
	if got := handoverRequests(file, "bp-a"); got != 18 {
		t.Fatalf("after the first merge bp-a = %d, want 10+3+5", got)
	}
	if file.Writer == nil || file.Writer.Generation != fresh.generation {
		t.Fatalf("writer mark %+v", file.Writer)
	}
	// It rewrites the file again, without the new instance's requests.
	old.record("bp-a", now, 1)
	old.record("bp-a", now, 1)
	if err := old.save(); err != nil {
		t.Fatal(err)
	}
	before := relayCounterSnapshot()
	now = now.Add(2 * time.Minute)
	recordN(fresh, &now, "bp-a", 1)
	fresh.flush()
	file = handoverFile(t, path)
	if got, hours := handoverRequests(file, "bp-a"), handoverHourRequests(file, "bp-a"); got != 21 || hours != 21 {
		t.Fatalf("bp-a = %d (hours %d), want 10+3+5+2+1", got, hours)
	}
	if got := handoverRequests(file, "bp-b"); got != 2 {
		t.Fatalf("bp-b = %d", got)
	}
	about(t, file.Accounts["bp-a"].CostUSD, 21, "bp-a cost")
	if a1CounterDelta(before)["ledger_handover:older_writer"] != 1 {
		t.Fatalf("counters %v", a1CounterDelta(before))
	}
	// Memory is the merged ledger: the old instance's late requests show.
	if got := fresh.view()["bp-a"].Requests; got != 21 {
		t.Fatalf("memory bp-a = %d", got)
	}
	// The guard: <ledger>.tmp becomes a directory, and the old build's
	// shutdown flush of its stale view fails instead of rolling the ledger back.
	if err := os.Mkdir(path+".tmp", 0700); err != nil {
		t.Fatal(err)
	}
	old.record("bp-a", now, 1)
	if err := old.save(); err == nil {
		t.Fatal("the guard did not stop a build without handover")
	}
	recordN(fresh, &now, "bp-a", 1)
	fresh.flush()
	if got := handoverRequests(handoverFile(t, path), "bp-a"); got != 22 {
		t.Fatalf("the guard stopped the handover build too: bp-a = %d", got)
	}
}

// Two handover builds: the next swap, and CPA stopping with the retired one
// shut down last. Every interleaving adds up, nothing twice.
func TestLedgerHandoverBetweenTwoHandoverBuilds(t *testing.T) {
	now := time.Date(2026, 10, 15, 3, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "cost-ledger.json")
	old := ledgerAt(t, &now)
	old.setPath(path)
	recordN(old, &now, "bp-a", 10)
	old.flush()

	fresh := ledgerAt(t, &now)
	fresh.setPath(path)
	if fresh.generation <= old.generation {
		t.Fatal("generations do not increase")
	}
	recordN(old, &now, "bp-a", 3) // streams finishing in the old instance
	recordN(fresh, &now, "bp-a", 5)
	fresh.flush() // reads the old one's file: adds everything it recorded
	if got := handoverRequests(handoverFile(t, path), "bp-a"); got != 15 {
		t.Fatalf("bp-a = %d, want 10+5 (the old 3 unsaved yet)", got)
	}
	before := relayCounterSnapshot()
	old.flush() // reads the new one's file: adds only what it had not saved
	file := handoverFile(t, path)
	if got := handoverRequests(file, "bp-a"); got != 18 {
		t.Fatalf("bp-a = %d, want 18", got)
	}
	if file.Writer.Generation != fresh.generation || a1CounterDelta(before)["ledger_handover:newer_writer"] != 1 {
		t.Fatalf("the newer instance's mark was not kept: %+v", file.Writer)
	}
	recordN(fresh, &now, "bp-a", 1)
	fresh.flush()
	// CPA stops: the active instance flushes first, the retired one last.
	recordN(fresh, &now, "bp-a", 1)
	recordN(old, &now, "bp-a", 2)
	fresh.flush()
	old.flush()
	file = handoverFile(t, path)
	if got, hours := handoverRequests(file, "bp-a"), handoverHourRequests(file, "bp-a"); got != 22 || hours != 22 {
		t.Fatalf("bp-a = %d (hours %d), want 10+3+5+1+1+2", got, hours)
	}
	// After the restart one instance loads it all.
	restarted := ledgerAt(t, &now)
	restarted.setPath(path)
	if got := restarted.view()["bp-a"].Requests; got != 22 {
		t.Fatalf("after restart bp-a = %d", got)
	}
}

// The old instance saving first after the swap.
func TestLedgerHandoverOldSavesFirst(t *testing.T) {
	now := time.Date(2026, 10, 15, 3, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "cost-ledger.json")
	old := ledgerAt(t, &now)
	old.setPath(path)
	recordN(old, &now, "bp-a", 10)
	old.flush()
	fresh := ledgerAt(t, &now)
	fresh.setPath(path)
	recordN(old, &now, "bp-a", 3)
	old.flush() // still its own file: plain addition
	recordN(fresh, &now, "bp-a", 5)
	fresh.flush()
	if got := handoverRequests(handoverFile(t, path), "bp-a"); got != 18 {
		t.Fatalf("bp-a = %d, want 18", got)
	}
}

// Quota windows: a reading taken since the last save is replayed onto the
// file's window; after a rewrite by an older instance the latest sampler's
// windows are kept.
func TestLedgerHandoverQuotaSamples(t *testing.T) {
	now := time.Date(2026, 10, 15, 3, 0, 0, 0, time.UTC)
	reset := now.Add(72 * time.Hour)
	path := filepath.Join(t.TempDir(), "cost-ledger.json")
	target := usageTarget{Key: "bp-a", File: "bp-a.json"}
	old := ledgerAt(t, &now)
	old.setPath(path)
	old.addSample(target, sampleAt(10, reset))
	recordN(old, &now, "bp-a", 10)
	old.flush()
	fresh := ledgerAt(t, &now)
	fresh.setPath(path)
	recordN(fresh, &now, "bp-a", 5)
	now = now.Add(time.Minute)
	fresh.addSample(target, sampleAt(20, reset))
	fresh.flush()
	recordN(old, &now, "bp-a", 1)
	now = now.Add(time.Minute)
	old.addSample(target, sampleAt(25, reset))
	old.flush() // replays its reading onto the newer instance's window
	file := handoverFile(t, path)
	segments := file.Accounts["bp-a"].Segments
	if len(segments) != 1 || segments[0].First.UsedPercent != 10 || segments[0].Last.UsedPercent != 25 {
		t.Fatalf("segments %+v", segments)
	}
	// A reading older than the file's last is not replayed.
	stale := ledgerAt(t, &now)
	stale.path, stale.generation = path, fresh.generation
	stale.samples = []pendingSample{{key: "bp-a", sample: quotaSample{At: now.Add(-time.Hour), UsedPercent: 1, WindowSecs: 7 * 24 * 3600, ResetAt: reset}}}
	accounts, _ := stale.mergedLocked()
	if got := accounts["bp-a"].Segments; len(got) != 1 || got[0].Last.UsedPercent != 25 {
		t.Fatalf("an old reading was replayed: %+v", got)
	}
}

// After a rewrite by a build without handover, each credential keeps the
// windows of whichever side sampled it last: two views' spend totals cannot
// share one window.
func TestLedgerHandoverWindowsAfterARewriteByAnOlderBuild(t *testing.T) {
	now := time.Date(2026, 10, 15, 3, 0, 0, 0, time.UTC)
	reset := now.Add(72 * time.Hour)
	path := filepath.Join(t.TempDir(), "cost-ledger.json")
	old := legacyLoad(t, path)
	old.record("bp-a", now, 1)
	old.record("bp-b", now, 1)
	window := func(at time.Time, used float64) []quotaSegment {
		sample := quotaSample{At: at, UsedPercent: used, WindowSecs: 7 * 24 * 3600, ResetAt: reset}
		return []quotaSegment{{First: sample, Last: sample}}
	}
	old.accounts["bp-a"].Segments = window(now, 10)
	old.accounts["bp-b"].Segments = window(now, 10)
	if err := old.save(); err != nil {
		t.Fatal(err)
	}
	fresh := ledgerAt(t, &now)
	fresh.setPath(path)
	recordN(fresh, &now, "bp-a", 1) // saves once (first save)
	now = now.Add(time.Minute)
	fresh.addSample(usageTarget{Key: "bp-a"}, sampleAt(30, reset)) // fresh samples bp-a later
	// The old build samples bp-b later still, and rewrites the file.
	old.accounts["bp-b"].Segments = window(now.Add(time.Minute), 50)
	old.record("bp-a", now, 1)
	if err := old.save(); err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Minute)
	recordN(fresh, &now, "bp-a", 1)
	fresh.flush()
	file := handoverFile(t, path)
	a, b := file.Accounts["bp-a"].Segments, file.Accounts["bp-b"].Segments
	if len(a) == 0 || a[len(a)-1].Last.UsedPercent != 30 {
		t.Fatalf("bp-a lost the newer instance's later reading: %+v", a)
	}
	if len(b) != 1 || b[0].Last.UsedPercent != 50 {
		t.Fatalf("bp-b lost the old build's later reading: %+v", b)
	}
	if got := handoverRequests(file, "bp-a"); got != 4 {
		t.Fatalf("bp-a = %d, want 1+1 old and 2 new", got)
	}
}

// A file written by a handover build stays readable by one without it, and
// what the latter writes back is still merged correctly.
func TestLedgerHandoverFileStaysReadableWithoutIt(t *testing.T) {
	now := time.Date(2026, 10, 15, 3, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "cost-ledger.json")
	fresh := ledgerAt(t, &now)
	fresh.setPath(path)
	recordN(fresh, &now, "bp-a", 4)
	fresh.flush()
	legacy := legacyLoad(t, path) // e.g. a restart into a build without handover
	if legacy.accounts["bp-a"] == nil || legacy.accounts["bp-a"].Requests != 4 {
		t.Fatalf("legacy read %+v", legacy.accounts["bp-a"])
	}
	if err := legacy.save(); err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	data, _ := os.ReadFile(path)
	if json.Unmarshal(data, &raw) != nil || raw["writer"] != nil {
		t.Fatal("legacy rewrite kept a writer mark")
	}
}

// Hourly buckets merge per hour and keep the newest costHourRetention.
func TestLedgerHandoverMergeHours(t *testing.T) {
	var hours []hourCost
	for h := int64(0); h < costHourRetention; h++ {
		hours = append(hours, hourCost{Hour: h, Requests: 1, Cost: 1})
	}
	merged := mergeHours(hours, map[int64]*hourCost{
		costHourRetention - 1: {Hour: costHourRetention - 1, Requests: 2, Cost: 2, InputTokens: 5},
		costHourRetention:     {Hour: costHourRetention, Requests: 3, Cost: 3},
	})
	if len(merged) != costHourRetention || merged[0].Hour != 1 {
		t.Fatalf("retention: %d buckets from hour %d", len(merged), merged[0].Hour)
	}
	last, previous := merged[len(merged)-1], merged[len(merged)-2]
	if last.Hour != costHourRetention || last.Requests != 3 || previous.Requests != 3 || previous.InputTokens != 5 {
		t.Fatalf("merge: %+v %+v", previous, last)
	}
}

// Two handover instances saving at the same time never lose each other's
// requests: the lock serialises read, merge and write.
func TestLedgerHandoverConcurrentSaves(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cost-ledger.json")
	var wg sync.WaitGroup
	start := time.Date(2026, 10, 15, 3, 0, 0, 0, time.UTC)
	ledgers := make([]*costLedger, 2)
	for i := range ledgers {
		clock := start
		ledger := newCostLedger()
		var mu sync.Mutex
		ledger.now = func() time.Time {
			mu.Lock()
			defer mu.Unlock()
			clock = clock.Add(time.Second)
			return clock
		}
		ledger.setPath(path)
		ledgers[i] = ledger
	}
	const perInstance = 200
	for _, ledger := range ledgers {
		wg.Add(1)
		go func(ledger *costLedger) {
			defer wg.Done()
			for i := 0; i < perInstance; i++ {
				ledger.record(usageTarget{Key: "bp-a"}, 1, true, false, tokenUsage{})
				if i%7 == 0 {
					ledger.flush()
				}
			}
			ledger.flush()
		}(ledger)
	}
	wg.Wait()
	// One more round each, now that both have seen the other's file.
	ledgers[0].flush()
	ledgers[1].flush()
	if got := handoverRequests(handoverFile(t, path), "bp-a"); got != 2*perInstance {
		t.Fatalf("bp-a = %d, want %d", got, 2*perInstance)
	}
}

// A ledger that has never saved and finds no file writes its memory, as
// before; a damaged file is replaced rather than merged into.
func TestLedgerHandoverNoFileOrDamagedFile(t *testing.T) {
	now := time.Date(2026, 10, 15, 3, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	path := filepath.Join(dir, "cost-ledger.json")
	ledger := ledgerAt(t, &now)
	ledger.setPath(path)
	recordN(ledger, &now, "bp-a", 2)
	ledger.flush()
	if got := handoverRequests(handoverFile(t, path), "bp-a"); got != 2 {
		t.Fatalf("bp-a = %d", got)
	}
	if err := os.WriteFile(path, []byte("{broken"), 0600); err != nil {
		t.Fatal(err)
	}
	recordN(ledger, &now, "bp-a", 1)
	ledger.flush()
	if got := handoverRequests(handoverFile(t, path), "bp-a"); got != 3 {
		t.Fatalf("bp-a = %d after a damaged file", got)
	}
	// No temp files are left behind.
	entries, _ := os.ReadDir(dir)
	for _, entry := range entries {
		if name := entry.Name(); name != "cost-ledger.json" && name != "cost-ledger.json.lock" {
			t.Fatalf("left behind: %s", name)
		}
	}
}
