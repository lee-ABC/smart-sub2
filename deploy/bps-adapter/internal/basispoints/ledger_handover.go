package basispoints

import (
	"encoding/json"
	"os"
	"sort"
	"strconv"
	"sync/atomic"
	"time"
)

// Ledger handover (a1wide2, 2026-10-15).
//
// CPA 8.0.23-fc9 swaps a plugin version without a restart: the new instance
// loads beside the old one and takes the traffic, and the old one is retired
// with its in-flight streams still running -- but it stays loaded until CPA
// stops (pluginhost host.go, retired). Both keep the ledger of the same
// credentials in the same file. Up to a1wide every save wrote the instance's
// whole memory over the file, and an instance read the file only once, when
// it loaded. So after a swap:
//   - what the old instance recorded after the new one loaded -- its unsaved
//     last minute and every stream that finished in it -- was gone as soon as
//     the new one saved: 881 requests ($122 list) on 2026-10-15 09:32;
//   - when CPA stops it shuts the active instance down first and the retired
//     ones after it (host.go detachPlugin), and each flushes its whole view: a
//     retired instance with anything unsaved writes last and takes the ledger
//     back to the moment it was retired.
//
// A save now reads the file and merges into it instead of overwriting it:
//   - the file names the generation of the instance that wrote it (when that
//     instance loaded); a file without one comes from a build without
//     handover;
//   - a file from an older generation, or from a build without handover, was
//     written by an instance that never read ours, so it lacks everything
//     this instance recorded since it loaded: all of that is added;
//   - a file from this generation, or from a newer one (which loaded ours),
//     already holds everything this instance saved before: only what it
//     recorded since its last save is added, and a newer generation's mark is
//     kept.
// Quota samples are replayed onto the file's windows in the second case; in
// the first, each credential keeps the windows of whichever instance sampled
// it last, since two views' spend totals cannot share one window. Every
// instance writes through its own temp file and saves under an exclusive
// lock on <ledger>.lock, so two merging instances never interleave. After a
// save, memory is the merged ledger, so the active instance also shows what a
// retired one finished.
//
// Builds without handover (workspace1, a1wide) stay loaded as retired
// instances after this swap and would still flush their stale view when CPA
// stops. They write through <ledger>.tmp, so once their streams are done a
// directory of that name makes every such save fail (deploy_a1wide2.py
// guard); handover builds never use that name.
//
// Counters: ledger_handover:older_writer (a save found the file rewritten by
// an older instance or a build without handover since this one last saved),
// ledger_handover:newer_writer (a save merged into a newer instance's file).

// ledgerWriter is the mark a handover build leaves on the file it writes.
type ledgerWriter struct {
	Generation int64     `json:"generation"`
	Version    string    `json:"plugin_version,omitempty"`
	SavedAt    time.Time `json:"saved_at"`
}

// accountDelta is what one instance added to one credential's ledger.
type accountDelta struct {
	File, Label    string
	Since, Touched time.Time
	CostUSD        float64
	Requests       int64
	Unpriced       int64
	Downgraded     int64
	Hours          map[int64]*hourCost
}

type ledgerDelta map[string]*accountDelta

// pendingSample is a quota reading taken since the last save.
type pendingSample struct {
	key, file, label string
	sample           quotaSample
}

var lastLedgerGeneration atomic.Int64

// nextLedgerGeneration is the moment an instance loaded its ledger, made
// strictly increasing within one runtime (tests run several ledgers in one).
func nextLedgerGeneration() int64 {
	for {
		now := time.Now().UnixNano()
		last := lastLedgerGeneration.Load()
		if now <= last {
			now = last + 1
		}
		if lastLedgerGeneration.CompareAndSwap(last, now) {
			return now
		}
	}
}

// add counts one finished request, with exactly the increments record makes.
func (d ledgerDelta) add(target usageTarget, now time.Time, cost float64, priced, downgraded bool, usage tokenUsage, longTier int64) {
	delta := d[target.Key]
	if delta == nil {
		delta = &accountDelta{Since: now, Hours: map[int64]*hourCost{}}
		d[target.Key] = delta
	}
	if target.File != "" {
		delta.File = target.File
	}
	if target.Label != "" {
		delta.Label = target.Label
	}
	delta.Touched = now
	delta.Requests++
	if downgraded {
		delta.Downgraded++
	}
	if !priced {
		delta.Unpriced++
	} else if cost > 0 {
		delta.CostUSD += cost
	}
	hour := now.Unix() / 3600
	bucket := delta.Hours[hour]
	if bucket == nil {
		bucket = &hourCost{Hour: hour}
		delta.Hours[hour] = bucket
	}
	addHour(bucket, hourCost{Cost: cost, Requests: 1, InputTokens: usage.Input, CachedTokens: usage.Cached,
		CacheWriteTokens: usage.CacheWrite, OutputTokens: usage.Output, LongTierRequests: longTier})
}

// touch notes a credential the instance only sampled.
func (d ledgerDelta) touch(target usageTarget, now time.Time) {
	delta := d[target.Key]
	if delta == nil {
		delta = &accountDelta{Since: now, Hours: map[int64]*hourCost{}}
		d[target.Key] = delta
	}
	if target.File != "" {
		delta.File = target.File
	}
	if target.Label != "" {
		delta.Label = target.Label
	}
	delta.Touched = now
}

func addHour(into *hourCost, from hourCost) {
	into.Cost += from.Cost
	into.Requests += from.Requests
	into.InputTokens += from.InputTokens
	into.CachedTokens += from.CachedTokens
	into.CacheWriteTokens += from.CacheWriteTokens
	into.OutputTokens += from.OutputTokens
	into.LongTierRequests += from.LongTierRequests
}

// applyTo adds d to accounts, creating the credentials it does not hold yet.
func (d ledgerDelta) applyTo(accounts map[string]*accountLedger) {
	for key, delta := range d {
		account := accounts[key]
		if account == nil {
			account = &accountLedger{Since: delta.Since}
			accounts[key] = account
		}
		if delta.File != "" {
			account.File = delta.File
		}
		if delta.Label != "" {
			account.Label = delta.Label
		}
		if account.Since.IsZero() || (!delta.Since.IsZero() && delta.Since.Before(account.Since)) {
			account.Since = delta.Since
		}
		if delta.Touched.After(account.Touched) {
			account.Touched = delta.Touched
		}
		account.CostUSD += delta.CostUSD
		account.Requests += delta.Requests
		account.Unpriced += delta.Unpriced
		account.Downgraded += delta.Downgraded
		if len(delta.Hours) > 0 {
			account.Hours = mergeHours(account.Hours, delta.Hours)
		}
	}
}

// mergeHours adds hourly buckets, in hour order, keeping the newest
// costHourRetention as record does.
func mergeHours(hours []hourCost, add map[int64]*hourCost) []hourCost {
	merged := append([]hourCost(nil), hours...)
	index := make(map[int64]int, len(merged))
	for i, bucket := range merged {
		index[bucket.Hour] = i
	}
	for hour, bucket := range add {
		if i, ok := index[hour]; ok {
			addHour(&merged[i], *bucket)
			continue
		}
		copied := *bucket
		copied.Hour = hour
		merged = append(merged, copied)
		index[hour] = len(merged) - 1
	}
	sort.SliceStable(merged, func(i, j int) bool { return merged[i].Hour < merged[j].Hour })
	if extra := len(merged) - costHourRetention; extra > 0 {
		merged = append([]hourCost(nil), merged[extra:]...)
	}
	return merged
}

// replaySample applies one reading taken since the last save to an account
// read back from the file. A reading older than the file's latest one for the
// credential is not replayed: the window already moved past it.
func replaySample(account *accountLedger, sample quotaSample) {
	if n := len(account.Segments); n > 0 {
		current := &account.Segments[n-1]
		if !sample.At.After(current.Last.At) {
			return
		}
		if continuesSegment(current.Last, sample) {
			current.Last = sample
			return
		}
	}
	account.Segments = append(account.Segments, quotaSegment{First: sample, Last: sample})
	if extra := len(account.Segments) - segmentRetention; extra > 0 {
		account.Segments = append([]quotaSegment(nil), account.Segments[extra:]...)
	}
}

// keepLatestWindows gives every credential the quota windows of whichever
// view sampled it last: the file's, or this instance's memory.
func keepLatestWindows(accounts map[string]*accountLedger, memory map[string]*accountLedger) {
	for key, mine := range memory {
		if mine == nil || len(mine.Segments) == 0 {
			continue
		}
		account := accounts[key]
		if account == nil {
			continue
		}
		mineLast := mine.Segments[len(mine.Segments)-1].Last.At
		if n := len(account.Segments); n == 0 || mineLast.After(account.Segments[n-1].Last.At) {
			account.Segments = append([]quotaSegment(nil), mine.Segments...)
		}
	}
}

// readLedgerFile reads the file a save merges into; false when there is none
// that can be read, in which case memory is the whole ledger.
func readLedgerFile(path string) (ledgerFile, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return ledgerFile{}, false
	}
	var file ledgerFile
	if json.Unmarshal(data, &file) != nil || file.Version != ledgerVersion || file.Accounts == nil {
		return ledgerFile{}, false
	}
	for key, account := range file.Accounts {
		if account == nil {
			delete(file.Accounts, key)
		}
	}
	return file, true
}

// mergedLocked is the ledger this save writes, and the mark it carries.
func (l *costLedger) mergedLocked() (map[string]*accountLedger, *ledgerWriter) {
	mine := &ledgerWriter{Generation: l.generation, Version: Version, SavedAt: l.now()}
	file, ok := readLedgerFile(l.path)
	if !ok {
		accounts := make(map[string]*accountLedger, len(l.accounts))
		for key, account := range l.accounts {
			copied := *account
			copied.Hours = append([]hourCost(nil), account.Hours...)
			copied.Segments = append([]quotaSegment(nil), account.Segments...)
			accounts[key] = &copied
		}
		return accounts, mine
	}
	accounts := file.Accounts
	if file.Writer == nil || file.Writer.Generation < l.generation {
		// Written by an instance that never read this one's file.
		if l.saves > 0 {
			countRelayEvent("ledger_handover:older_writer")
		}
		l.lifetime.applyTo(accounts)
		keepLatestWindows(accounts, l.accounts)
		return accounts, mine
	}
	// This instance's own file, or a newer instance's that loaded it.
	l.unsaved.applyTo(accounts)
	for _, pending := range l.samples {
		account := accounts[pending.key]
		if account == nil {
			account = &accountLedger{Since: pending.sample.At, Touched: pending.sample.At, File: pending.file, Label: pending.label}
			accounts[pending.key] = account
		}
		replaySample(account, pending.sample)
	}
	if file.Writer.Generation > l.generation {
		countRelayEvent("ledger_handover:newer_writer")
		return accounts, file.Writer
	}
	return accounts, mine
}

// tempPath is this instance's own temp file, never <ledger>.tmp: builds
// without handover use that one, and the guard turns it into a directory.
func (l *costLedger) tempPath() string {
	return l.path + ".tmp-" + strconv.FormatInt(l.generation, 36)
}
