package basispoints

import (
	"encoding/json"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Cost estimate: ledger.
//
// For every credential the ledger keeps what its requests were worth and how
// its quota moved. The estimate needs nothing more than two numbers taken at
// the same moment -- the credential's cumulative spend and its quota
// percentage -- because dollars per percentage point is the ratio of their
// changes between two such moments, and those changes simply add up across a
// window. So a quota window is stored as its first and last sample only; the
// samples in between would add nothing, and the file stays small.

const (
	ledgerFileName        = "cost-ledger.json"
	ledgerVersion         = 1
	ledgerSaveInterval    = time.Minute
	costHourRetention     = 35 * 24 // hourly buckets kept per credential
	segmentRetention      = 12      // quota windows kept per credential
	accountRetention      = 35 * 24 * time.Hour
	resetTolerance        = 10 * time.Minute
	percentNoiseTolerance = 0.5
)

type costLedger struct {
	mu       sync.Mutex
	path     string
	dirty    bool
	lastSave time.Time
	accounts map[string]*accountLedger
	// nextSample is when a credential's quota may next be read. It is kept in
	// memory only: after a restart every credential may be sampled at once.
	nextSample map[string]time.Time
	now        func() time.Time

	// Handover (ledger_handover.go): when this instance loaded the file, what
	// it recorded since then and since its last save, the quota readings it
	// took since its last save, and how many saves it made.
	generation int64
	lifetime   ledgerDelta
	unsaved    ledgerDelta
	samples    []pendingSample
	saves      int
}

type accountLedger struct {
	File     string    `json:"file,omitempty"`
	Label    string    `json:"label,omitempty"`
	Since    time.Time `json:"since"`
	Touched  time.Time `json:"touched"`
	CostUSD  float64   `json:"cost_usd"`
	Requests int64     `json:"requests"`
	Unpriced int64     `json:"unpriced"`
	// Downgraded counts answers the upstream served with a model other than
	// the one asked for.
	Downgraded int64          `json:"downgraded"`
	Hours      []hourCost     `json:"hours,omitempty"`
	Segments   []quotaSegment `json:"segments,omitempty"`
}

// hourCost is what one credential's requests were worth in one clock hour.
type hourCost struct {
	Hour     int64   `json:"h"` // unix seconds / 3600
	Cost     float64 `json:"c"`
	Requests int64   `json:"n"`
	// Token counts, kept so a cache hit rate can be measured rather than
	// inferred from dollars. Reading it back out of cost alone needs the output
	// token count as a free variable, which left a 17-point band on the one
	// number every rollout threshold is calibrated against.
	InputTokens      int64 `json:"it,omitempty"`
	CachedTokens     int64 `json:"ct,omitempty"`
	CacheWriteTokens int64 `json:"wt,omitempty"`
	OutputTokens     int64 `json:"ot,omitempty"`
	// LongTierRequests counts requests priced at the long-context rate, which
	// turns on above longContextThresholdTokens of prompt.
	LongTierRequests int64 `json:"lt,omitempty"`
}

// quotaSample pairs a credential's quota reading with its cumulative spend at
// the same moment.
type quotaSample struct {
	At          time.Time `json:"at"`
	Plan        string    `json:"plan,omitempty"`
	UsedPercent float64   `json:"used"`
	WindowSecs  int64     `json:"window"`
	ResetAt     time.Time `json:"reset"`
	CostUSD     float64   `json:"cost"`
}

// quotaSegment is one stretch of a single quota window: the same reset time and
// a usage that never goes down. A reset, scheduled or early, starts a new one.
type quotaSegment struct {
	First quotaSample `json:"first"`
	Last  quotaSample `json:"last"`
}

type ledgerFile struct {
	Version  int                       `json:"version"`
	Accounts map[string]*accountLedger `json:"accounts"`
	// Writer is the handover mark (ledger_handover.go); builds without
	// handover neither read nor write it.
	Writer *ledgerWriter `json:"writer,omitempty"`
}

func newCostLedger() *costLedger {
	return &costLedger{
		accounts:   map[string]*accountLedger{},
		nextSample: map[string]time.Time{},
		now:        time.Now,
		lifetime:   ledgerDelta{},
		unsaved:    ledgerDelta{},
	}
}

// setPath points the ledger at its file, loading it when the path changes.
func (l *costLedger) setPath(path string) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if path == l.path {
		return
	}
	if l.path != "" && l.dirty {
		l.saveLocked()
	}
	l.path = path
	l.accounts = map[string]*accountLedger{}
	l.dirty = false
	// A new generation starts with every load: what it adds is counted from
	// here (ledger_handover.go).
	l.generation = nextLedgerGeneration()
	l.lifetime, l.unsaved, l.samples, l.saves = ledgerDelta{}, ledgerDelta{}, nil, 0
	if path == "" {
		return
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var file ledgerFile
	if json.Unmarshal(data, &file) != nil || file.Version != ledgerVersion || file.Accounts == nil {
		// A damaged ledger costs only history; starting afresh is safe.
		return
	}
	l.accounts = file.Accounts
}

// keepingBooks reports whether the ledger has been pointed at a file, which
// only happens once the host has configured the plugin. Before that the plugin
// has no configuration of its own and must not be making upstream calls.
func (l *costLedger) keepingBooks() bool {
	if l == nil {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.path != ""
}

func (l *costLedger) accountLocked(target usageTarget, now time.Time) *accountLedger {
	account := l.accounts[target.Key]
	if account == nil {
		account = &accountLedger{Since: now}
		l.accounts[target.Key] = account
	}
	if target.File != "" {
		account.File = target.File
	}
	if target.Label != "" {
		account.Label = target.Label
	}
	account.Touched = now
	return account
}

// record adds one finished request to its credential's spend.
func (l *costLedger) record(target usageTarget, cost float64, priced, downgraded bool, usage tokenUsage) {
	if l == nil || target.Key == "" {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	account := l.accountLocked(target, now)
	account.Requests++
	if downgraded {
		account.Downgraded++
	}
	if !priced {
		account.Unpriced++
	} else if cost > 0 {
		account.CostUSD += cost
	}
	longTier := int64(0)
	if usage.Input > longContextThresholdTokens {
		longTier = 1
	}
	hour := now.Unix() / 3600
	if n := len(account.Hours); n > 0 && account.Hours[n-1].Hour == hour {
		account.Hours[n-1].Cost += cost
		account.Hours[n-1].Requests++
		account.Hours[n-1].InputTokens += usage.Input
		account.Hours[n-1].CachedTokens += usage.Cached
		account.Hours[n-1].CacheWriteTokens += usage.CacheWrite
		account.Hours[n-1].OutputTokens += usage.Output
		account.Hours[n-1].LongTierRequests += longTier
	} else {
		account.Hours = append(account.Hours, hourCost{
			Hour: hour, Cost: cost, Requests: 1,
			InputTokens: usage.Input, CachedTokens: usage.Cached,
			CacheWriteTokens: usage.CacheWrite, OutputTokens: usage.Output,
			LongTierRequests: longTier,
		})
		if extra := len(account.Hours) - costHourRetention; extra > 0 {
			account.Hours = append([]hourCost(nil), account.Hours[extra:]...)
		}
	}
	// The same increments, kept apart for the next save's merge.
	l.lifetime.add(target, now, cost, priced, downgraded, usage, longTier)
	l.unsaved.add(target, now, cost, priced, downgraded, usage, longTier)
	l.touchLocked(now)
}

// claimSample reports whether a credential's quota is due to be read, and if
// so pushes its next reading out by about one interval. The jitter keeps the
// credentials from being read in lockstep.
func (l *costLedger) claimSample(key string, interval time.Duration) bool {
	if l == nil || key == "" || interval <= 0 {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	if next, ok := l.nextSample[key]; ok && now.Before(next) {
		return false
	}
	jitter := time.Duration((rand.Float64()*0.4 - 0.2) * float64(interval))
	l.nextSample[key] = now.Add(interval + jitter)
	return true
}

// addSample records a quota reading together with the spend at that moment.
func (l *costLedger) addSample(target usageTarget, snapshot quotaSnapshot) {
	window := governingWindow(snapshot)
	if l == nil || target.Key == "" || window == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	account := l.accountLocked(target, now)
	sample := quotaSample{
		At:          now,
		Plan:        snapshot.Plan,
		UsedPercent: window.UsedPercent,
		WindowSecs:  window.WindowSecs,
		ResetAt:     window.ResetAt,
		CostUSD:     account.CostUSD,
	}
	// Kept for the next save's merge (ledger_handover.go).
	l.samples = append(l.samples, pendingSample{key: target.Key, file: target.File, label: target.Label, sample: sample})
	l.lifetime.touch(target, now)
	l.unsaved.touch(target, now)
	if n := len(account.Segments); n > 0 {
		current := &account.Segments[n-1]
		if continuesSegment(current.Last, sample) {
			current.Last = sample
			l.touchLocked(now)
			return
		}
	}
	account.Segments = append(account.Segments, quotaSegment{First: sample, Last: sample})
	if extra := len(account.Segments) - segmentRetention; extra > 0 {
		account.Segments = append([]quotaSegment(nil), account.Segments[extra:]...)
	}
	l.touchLocked(now)
}

// continuesSegment reports whether next belongs to the same window as last.
func continuesSegment(last, next quotaSample) bool {
	if last.WindowSecs != next.WindowSecs {
		return false
	}
	gap := last.ResetAt.Sub(next.ResetAt)
	if gap < 0 {
		gap = -gap
	}
	if gap > resetTolerance {
		return false
	}
	return next.UsedPercent >= last.UsedPercent-percentNoiseTolerance && next.CostUSD >= last.CostUSD
}

// governingWindow is the longest quota window the snapshot reports: the one
// that ultimately limits a credential and the one worth pricing.
// keyForFile reports the key an existing account is filed under. It never
// creates one, because the caller's whole purpose is to correct an account the
// executor already keeps: see noteQuotaForFile.
func (l *costLedger) keyForFile(file string) (string, string, bool) {
	if l == nil || strings.TrimSpace(file) == "" {
		return "", "", false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	for key, account := range l.accounts {
		if account != nil && strings.EqualFold(account.File, file) {
			return key, account.Label, true
		}
	}
	return "", "", false
}

func governingWindow(snapshot quotaSnapshot) *quotaWindow {
	var best *quotaWindow
	for _, window := range []*quotaWindow{snapshot.Primary, snapshot.Secondary} {
		if window != nil && (best == nil || window.WindowSecs > best.WindowSecs) {
			best = window
		}
	}
	return best
}

func (l *costLedger) touchLocked(now time.Time) {
	l.dirty = true
	if now.Sub(l.lastSave) >= ledgerSaveInterval {
		l.saveLocked()
	}
}

// flush writes the ledger out if anything changed since the last save.
func (l *costLedger) flush() {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.dirty {
		l.saveLocked()
	}
}

// saveLocked merges into the file rather than overwriting it, so another
// instance of the plugin keeping the same ledger loses nothing to this one
// (ledger_handover.go). What it wrote becomes the ledger in memory.
func (l *costLedger) saveLocked() {
	now := l.now()
	l.lastSave = now
	if l.path == "" {
		return
	}
	if err := os.MkdirAll(filepath.Dir(l.path), 0700); err != nil {
		return
	}
	release := lockLedgerFile(l.path + ".lock")
	defer release()
	accounts, writer := l.mergedLocked()
	for key, account := range accounts {
		if now.Sub(account.Touched) > accountRetention {
			delete(accounts, key)
		}
	}
	data, err := json.Marshal(ledgerFile{Version: ledgerVersion, Accounts: accounts, Writer: writer})
	if err != nil {
		return
	}
	temp := l.tempPath()
	if err := os.WriteFile(temp, data, 0600); err != nil {
		_ = os.Remove(temp)
		return
	}
	if err := os.Rename(temp, l.path); err != nil {
		_ = os.Remove(temp)
		return
	}
	l.accounts = accounts
	l.unsaved, l.samples = ledgerDelta{}, nil
	l.saves++
	l.dirty = false
}

// ledgerView is a consistent copy of the ledger for the estimator to read
// without holding the lock.
type ledgerView map[string]accountLedger

func (l *costLedger) view() ledgerView {
	view := ledgerView{}
	if l == nil {
		return view
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	for key, account := range l.accounts {
		copied := *account
		copied.Hours = append([]hourCost(nil), account.Hours...)
		copied.Segments = append([]quotaSegment(nil), account.Segments...)
		view[key] = copied
	}
	return view
}

// costSince sums what a credential spent from the given moment on, at the
// resolution of whole clock hours.
func (a accountLedger) costSince(from time.Time) float64 {
	firstHour := from.Unix() / 3600
	total := 0.0
	index := sort.Search(len(a.Hours), func(i int) bool { return a.Hours[i].Hour >= firstHour })
	for _, bucket := range a.Hours[index:] {
		total += bucket.Cost
	}
	return total
}
