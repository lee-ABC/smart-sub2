package basispoints

import (
	"math"
	"sort"
	"time"
)

// Cost estimate: turning quota percent into dollars.
//
// The upstream publishes no dollar figure for a plan, only how much of a quota
// window is used. What a window is worth is therefore measured rather than
// looked up: between two samples of one window, the credential spent a known
// number of dollars and its usage advanced a known number of percentage points,
// so their ratio is what one point costs. Multiply by the points left and the
// remainder is in dollars.
//
// Two things make a single ratio unreliable on its own: the percentage arrives
// rounded to whole points, so a short stretch can be off by a lot, and the
// upstream's own accounting need not be proportional to list prices for every
// model. Both are handled by pooling: ratios from every credential on the same
// plan are combined by median, which one odd credential cannot move, and a
// stretch that has not advanced far enough is not used at all.

const (
	// minSegmentPercent is how far a window must have advanced before its ratio
	// is trusted. The percentage is whole-numbered, so a two point advance is
	// worth at most a quarter off; anything less is mostly rounding.
	minSegmentPercent = 2.0
	// minSegmentCost guards the other side: a ratio built on a few cents says
	// nothing about a window worth hundreds of dollars.
	minSegmentCost = 1.0
	// segmentMaxAge ignores stretches too old to describe current pricing.
	segmentMaxAge = 21 * 24 * time.Hour
	// unusedWindowPercent is how much of a window may already be spent for the
	// first reading to still count as having caught it from the start. The
	// percentage arrives as a whole number, so anything under a point is
	// indistinguishable from zero.
	unusedWindowPercent = 1.0
	// minDirectPercent is how far a window must have advanced before a
	// credential's own complete record of it is preferred over everyone
	// else's measurements. Below this the whole-number percentage is too
	// coarse to divide by.
	minDirectPercent = 2.0
)

// ratioSample is one credential's measured dollars per percentage point.
type ratioSample struct {
	Key           string
	Plan          string
	DollarsPerPct float64
	Percent       float64 // how far the window advanced while measuring
	Cost          float64
}

// costEstimate is what one credential's quota window is worth.
type costEstimate struct {
	Key   string `json:"key"`
	File  string `json:"file,omitempty"`
	Label string `json:"label,omitempty"`
	Plan  string `json:"plan,omitempty"`

	// Known reports whether an estimate could be made at all.
	Known  bool   `json:"known"`
	Reason string `json:"reason,omitempty"`

	UsedPercent float64   `json:"used_percent"`
	WindowSecs  int64     `json:"window_secs,omitempty"`
	ResetAt     time.Time `json:"reset_at,omitempty"`
	SampledAt   time.Time `json:"sampled_at,omitempty"`

	// UsedUSD and RemainingUSD describe the whole window, including whatever
	// the credential spent before this plugin started keeping books.
	UsedUSD      float64 `json:"used_usd"`
	RemainingUSD float64 `json:"remaining_usd"`
	WindowUSD    float64 `json:"window_usd"`
	// LowUSD and HighUSD bound RemainingUSD across the pooled measurements.
	LowUSD  float64 `json:"low_usd"`
	HighUSD float64 `json:"high_usd"`
	// Confidence is one of "high", "medium", "low".
	Confidence string `json:"confidence,omitempty"`

	// MeteredUSD is what this plugin actually priced for the window: ground
	// truth, unlike the figures above.
	MeteredUSD  float64 `json:"metered_usd"`
	Requests    int64   `json:"requests"`
	Unpriced    int64   `json:"unpriced"`
	Downgraded  int64   `json:"downgraded"`
	SampleCount int     `json:"sample_count"`
	// Direct reports that this credential's own complete record of the window
	// produced the figures, rather than other credentials' measurements.
	Direct bool `json:"direct"`
}

// costSummary is every credential's estimate plus the totals.
type costSummary struct {
	Accounts     []costEstimate `json:"accounts"`
	UsedUSD      float64        `json:"used_usd"`
	RemainingUSD float64        `json:"remaining_usd"`
	MeteredUSD   float64        `json:"metered_usd"`
	KnownCount   int            `json:"known_count"`
	TotalCount   int            `json:"total_count"`
	Plans        map[string]int `json:"plans,omitempty"`
	At           string         `json:"at"`
	Error        string         `json:"error,omitempty"`
}

// ratios extracts every usable measurement from one credential's history.
func (a accountLedger) ratios(key string, now time.Time) []ratioSample {
	var samples []ratioSample
	for _, segment := range a.Segments {
		if now.Sub(segment.Last.At) > segmentMaxAge {
			continue
		}
		percent := segment.Last.UsedPercent - segment.First.UsedPercent
		cost := segment.Last.CostUSD - segment.First.CostUSD
		if percent < minSegmentPercent || cost < minSegmentCost {
			continue
		}
		samples = append(samples, ratioSample{
			Key:           key,
			Plan:          segment.Last.Plan,
			DollarsPerPct: cost / percent,
			Percent:       percent,
			Cost:          cost,
		})
	}
	return samples
}

// current is the credential's latest quota reading, if it still applies.
func (a accountLedger) current(now time.Time) (quotaSample, bool) {
	if len(a.Segments) == 0 {
		return quotaSample{}, false
	}
	last := a.Segments[len(a.Segments)-1].Last
	if !last.ResetAt.IsZero() && now.After(last.ResetAt) {
		// The window has rolled over since; its percentage is stale.
		return quotaSample{}, false
	}
	return last, true
}

func median(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	sorted := append([]float64(nil), values...)
	sort.Float64s(sorted)
	middle := len(sorted) / 2
	if len(sorted)%2 == 1 {
		return sorted[middle]
	}
	return (sorted[middle-1] + sorted[middle]) / 2
}

// estimateCosts prices every credential's remaining quota.
func estimateCosts(view ledgerView, now time.Time) costSummary {
	summary := costSummary{
		Plans: map[string]int{},
		At:    now.Format("2006-01-02 15:04:05"),
	}

	// Pool the measurements by plan, and keep one pool across all plans for
	// credentials whose own plan has nothing to go on yet.
	byPlan := map[string][]ratioSample{}
	own := map[string][]ratioSample{}
	var all []ratioSample
	for key, account := range view {
		for _, sample := range account.ratios(key, now) {
			byPlan[sample.Plan] = append(byPlan[sample.Plan], sample)
			own[key] = append(own[key], sample)
			all = append(all, sample)
		}
	}

	keys := make([]string, 0, len(view))
	for key := range view {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	for _, key := range keys {
		account := view[key]
		estimate := costEstimate{
			Key:      key,
			File:     account.File,
			Label:    account.Label,
			Requests: account.Requests,
			Unpriced: account.Unpriced,
		}
		latest, ok := account.current(now)
		if !ok {
			estimate.Reason = "还没有读到这个号的额度"
			summary.Accounts = append(summary.Accounts, estimate)
			summary.TotalCount++
			continue
		}
		estimate.Plan = latest.Plan
		estimate.UsedPercent = latest.UsedPercent
		estimate.WindowSecs = latest.WindowSecs
		estimate.ResetAt = latest.ResetAt
		estimate.SampledAt = latest.At
		started := windowStart(latest, now)
		estimate.MeteredUSD = account.costSince(started)
		estimate.Downgraded = account.Downgraded
		summary.MeteredUSD += estimate.MeteredUSD
		summary.Plans[latest.Plan]++

		// When nothing was spent on this credential before the books opened,
		// its own spend for the window is not an estimate at all, and dividing
		// it by the percentage used measures this very credential rather than
		// borrowing from others. That is strictly better than any pooled
		// figure, so it is preferred whenever it is available.
		if coversWholeWindow(account, started) &&
			latest.UsedPercent >= minDirectPercent && estimate.MeteredUSD > 0 {
			remainingPercent := math.Max(0, 100-latest.UsedPercent)
			perPercent := estimate.MeteredUSD / latest.UsedPercent
			estimate.Known = true
			estimate.Direct = true
			estimate.UsedUSD = estimate.MeteredUSD
			estimate.WindowUSD = perPercent * 100
			estimate.RemainingUSD = perPercent * remainingPercent
			// The percentage arrives rounded to whole points, so the true
			// figure lies within half a point either way.
			estimate.LowUSD = estimate.MeteredUSD / (latest.UsedPercent + 0.5) * remainingPercent
			estimate.HighUSD = estimate.MeteredUSD / math.Max(0.5, latest.UsedPercent-0.5) * remainingPercent
			estimate.Confidence = "high"
			estimate.SampleCount = 1
			summary.Accounts = append(summary.Accounts, estimate)
			summary.TotalCount++
			summary.KnownCount++
			summary.UsedUSD += estimate.UsedUSD
			summary.RemainingUSD += estimate.RemainingUSD
			continue
		}

		// A credential's own measurements describe it better than its peers',
		// so the pool is only there to cover one that has none of its own.
		pool := own[key]
		if len(pool) == 0 {
			pool = byPlan[latest.Plan]
		}
		if len(pool) == 0 {
			pool = all
		}
		if len(pool) == 0 {
			estimate.Reason = "样本不足：还没有观察到额度涨了足够多"
			summary.Accounts = append(summary.Accounts, estimate)
			summary.TotalCount++
			continue
		}

		values := make([]float64, 0, len(pool))
		observed := 0.0
		for _, sample := range pool {
			values = append(values, sample.DollarsPerPct)
			observed += sample.Percent
		}
		perPercent := median(values)
		low, high := values[0], values[0]
		for _, value := range values {
			low = math.Min(low, value)
			high = math.Max(high, value)
		}
		remainingPercent := math.Max(0, 100-latest.UsedPercent)

		estimate.Known = perPercent > 0
		estimate.WindowUSD = perPercent * 100
		// What this plugin priced is the only figure on this path that was
		// measured rather than inferred, so it is a floor. Without it a
		// reading of 0% erases spend that was actually observed, and the card
		// reports "spent $0.00" directly above "$1.10 through CPA" -- two
		// numbers from the same ledger contradicting each other.
		estimate.UsedUSD = math.Max(perPercent*latest.UsedPercent, estimate.MeteredUSD)
		estimate.RemainingUSD = perPercent * remainingPercent
		estimate.LowUSD = low * remainingPercent
		estimate.HighUSD = high * remainingPercent
		estimate.SampleCount = len(pool)
		estimate.Confidence = confidenceOf(len(pool), observed, low, high, perPercent)
		if !estimate.Known {
			estimate.Reason = "样本不足：还没有观察到额度涨了足够多"
		}

		summary.Accounts = append(summary.Accounts, estimate)
		summary.TotalCount++
		if estimate.Known {
			summary.KnownCount++
			summary.UsedUSD += estimate.UsedUSD
			summary.RemainingUSD += estimate.RemainingUSD
		}
	}
	return summary
}

// coversWholeWindow reports whether this credential's books account for the
// whole quota window, which is what lets its spend stand as measured rather
// than estimated.
//
// Comparing when the books opened against when the window opened is not enough
// on its own. A newly imported credential starts its quota window with its
// first request, so the ledger entry is created a fraction of a second after
// the window begins and the comparison fails for exactly the credentials whose
// records are most complete. The first quota reading of the window settles it
// directly: if it found the window unused, nothing was spent before the books
// opened, whatever the clocks say.
func coversWholeWindow(account accountLedger, started time.Time) bool {
	if started.IsZero() {
		return false
	}
	if !account.Since.After(started) {
		return true
	}
	if len(account.Segments) == 0 {
		return false
	}
	first := account.Segments[len(account.Segments)-1].First
	return first.UsedPercent <= unusedWindowPercent && !first.At.Before(started)
}

// windowStart is when the current quota window began, so metered spend can be
// summed over the same stretch the percentage describes.
func windowStart(latest quotaSample, now time.Time) time.Time {
	if latest.WindowSecs > 0 && !latest.ResetAt.IsZero() {
		return latest.ResetAt.Add(-time.Duration(latest.WindowSecs) * time.Second)
	}
	if latest.WindowSecs > 0 {
		return now.Add(-time.Duration(latest.WindowSecs) * time.Second)
	}
	return time.Time{}
}

// confidenceOf grades an estimate by how much was measured and how much the
// measurements disagree.
func confidenceOf(count int, observedPercent, low, high, median float64) string {
	if median <= 0 {
		return ""
	}
	spread := (high - low) / median
	switch {
	case count >= 3 && observedPercent >= 30 && spread <= 0.5:
		return "high"
	case count >= 2 && observedPercent >= 10 && spread <= 1.5:
		return "medium"
	default:
		return "low"
	}
}
