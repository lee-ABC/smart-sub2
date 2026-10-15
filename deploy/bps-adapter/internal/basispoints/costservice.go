package basispoints

import (
	"net/http"
	"strings"
	"time"
)

// Cost estimate: wiring.
//
// Two inputs feed the ledger. Every finished request contributes what it was
// worth, priced from its own reported token usage. Separately, a credential's
// quota percentage is read from the ChatGPT usage endpoint every so often --
// the only upstream traffic this feature adds, and the reason it is paced per
// credential and jittered rather than swept.
//
// The sampling runs after a request finishes, on the goroutine pattern the
// stream relays already use, so it never adds latency to a request and
// shutdown still drains anything mid-call. Only credentials that are actually
// serving traffic get sampled, which is exactly the set worth measuring.

const (
	costDataPath  = "cost-data"
	costDebugPath = "cost-debug"
)

// usageTarget names the credential a request ran on, in the same terms the
// concurrency page uses, so both features agree on what a credential is.
type usageTarget struct {
	Key   string
	File  string
	Label string
}

func targetFor(request ExecutorRequest, c credential) usageTarget {
	file := strings.TrimSpace(stringValue(request.AuthMetadata["file_name"]))
	label := strings.TrimSpace(c.Email)
	if label == "" {
		label = strings.TrimSpace(stringValue(request.AuthMetadata["label"]))
	}
	return usageTarget{Key: concurrencyKey(request, c), File: file, Label: label}
}

// recordResponse prices one finished response and files it under its
// credential. A response without usage still counts as a request, so the
// unpriced tally shows when pricing is missing something.
func (s *Service) recordResponse(target usageTarget, model string, response map[string]any) {
	if s == nil || s.ledger == nil || target.Key == "" {
		return
	}
	usage, ok := usageFromResponse(response)
	if !ok {
		s.ledger.record(target, 0, false, false, tokenUsage{})
		return
	}
	cfg := s.config()
	served := servedModel(response, model)
	downgraded := normalizePriceModel(served) != normalizePriceModel(model)
	// The served model decides the price. Falling back to the requested one
	// keeps a substitution this table has never heard of from being recorded
	// as free.
	price, known := cfg.priceFor(served)
	if !known {
		price, known = cfg.priceFor(model)
	}
	if !known {
		s.ledger.record(target, 0, false, downgraded, usage)
		return
	}
	s.ledger.record(target, price.cost(usage), true, downgraded, usage)
}

// maybeSampleQuota reads this credential's quota when it is due, in the
// background. The reading is what later turns percentages into dollars.
func (s *Service) maybeSampleQuota(target usageTarget, c credential) {
	cfg := s.config()
	interval := cfg.quotaSampleInterval()
	if s == nil || target.Key == "" || interval <= 0 || !s.ledger.keepingBooks() {
		return
	}
	if !s.ledger.claimSample(target.Key, interval) {
		return
	}
	// Registered before the goroutine starts, so plugin.shutdown cannot tear
	// the host binding down while this is mid-call. See drainStreams.
	if !s.beginStream() {
		return
	}
	go func() {
		defer s.streams.Done()
		defer func() { _ = recover() }()
		snapshot, err := s.fetchUsage(c)
		if err != nil {
			return
		}
		storeQuota(target.Key, snapshot)
		s.ledger.addSample(target, snapshot)
	}()
}

// costSnapshot prices every Basis Points credential the host knows about, not
// only those the ledger has seen, so a credential with no traffic yet still
// appears.
func (s *Service) costSnapshot() costSummary {
	view := s.ledger.view()
	summary := estimateCosts(view, time.Now())

	var list struct {
		Files []struct {
			Name      string `json:"name"`
			Label     string `json:"label"`
			Provider  string `json:"provider"`
			Type      string `json:"type"`
			AuthIndex string `json:"auth_index"`
		} `json:"files"`
	}
	if err := s.call("host.auth.list", map[string]any{}, &list); err != nil {
		summary.Error = safeError(err)
		return summary
	}

	seen := map[string]bool{}
	for _, account := range summary.Accounts {
		seen[account.Key] = true
	}
	for _, file := range list.Files {
		if !strings.EqualFold(file.Provider, Provider) && !strings.EqualFold(file.Type, Provider) {
			continue
		}
		key := credentialID(file.Name)
		if seen[key] {
			for i := range summary.Accounts {
				if summary.Accounts[i].Key == key {
					summary.Accounts[i].Label = displayName(file.Name, file.Label)
					summary.Accounts[i].File = file.Name
				}
			}
			continue
		}
		summary.Accounts = append(summary.Accounts, costEstimate{
			Key:    key,
			File:   file.Name,
			Label:  displayName(file.Name, file.Label),
			Reason: "还没有这个号的用量记录",
		})
		summary.TotalCount++
	}
	return summary
}

func (s *Service) serveCostData() map[string]any {
	return map[string]any{
		"StatusCode": http.StatusOK,
		"Headers": map[string][]string{
			"Content-Type":  {"application/json; charset=utf-8"},
			"Cache-Control": {"no-store"},
		},
		"Body": jsonBytes(s.costSnapshot()),
	}
}

// ledgerPath is where the ledger is kept, inside the plugin's data directory.
func (c Config) ledgerPath() string {
	if strings.TrimSpace(c.DataDir) == "" {
		return ""
	}
	return c.DataDir + "/" + ledgerFileName
}

func (c Config) quotaSampleInterval() time.Duration {
	if c.QuotaSampleSeconds <= 0 {
		return 0
	}
	return time.Duration(c.QuotaSampleSeconds) * time.Second
}

// costDebugSnapshot exposes the raw ledger for verification. It carries no
// token material: only file names, labels, percentages and dollar figures.
func (s *Service) costDebugSnapshot() map[string]any {
	view := s.ledger.view()
	accounts := make(map[string]any, len(view))
	for key, account := range view {
		segments := make([]map[string]any, 0, len(account.Segments))
		for _, segment := range account.Segments {
			percent := segment.Last.UsedPercent - segment.First.UsedPercent
			cost := segment.Last.CostUSD - segment.First.CostUSD
			entry := map[string]any{
				"first_at": segment.First.At, "first_percent": segment.First.UsedPercent, "first_cost": segment.First.CostUSD,
				"last_at": segment.Last.At, "last_percent": segment.Last.UsedPercent, "last_cost": segment.Last.CostUSD,
				"delta_percent": percent, "delta_cost": cost,
				"window_secs": segment.Last.WindowSecs, "reset_at": segment.Last.ResetAt,
			}
			if percent > 0 {
				entry["usd_per_percent"] = cost / percent
			}
			segments = append(segments, entry)
		}
		// Token counts, per hour, so a cache hit rate can be measured instead of
		// solved for. Reading it out of dollars alone leaves the output token
		// count free, which is a 17-point band on the number every rollout
		// threshold is set against.
		hours := make([]map[string]any, 0, len(account.Hours))
		var totalInput, totalCached, totalWrite, totalOutput, totalLong int64
		for _, hour := range account.Hours {
			totalInput += hour.InputTokens
			totalCached += hour.CachedTokens
			totalWrite += hour.CacheWriteTokens
			totalOutput += hour.OutputTokens
			totalLong += hour.LongTierRequests
			entry := map[string]any{
				"hour": hour.Hour, "cost": hour.Cost, "requests": hour.Requests,
				"input_tokens": hour.InputTokens, "cached_tokens": hour.CachedTokens,
				"cache_write_tokens": hour.CacheWriteTokens, "output_tokens": hour.OutputTokens,
				"long_tier_requests": hour.LongTierRequests,
			}
			if hour.InputTokens > 0 {
				entry["cache_hit_rate"] = float64(hour.CachedTokens) / float64(hour.InputTokens)
				entry["cache_write_rate"] = float64(hour.CacheWriteTokens) / float64(hour.InputTokens)
			}
			hours = append(hours, entry)
		}
		summary := map[string]any{
			"file": account.File, "label": account.Label,
			"cost_usd": account.CostUSD, "requests": account.Requests, "unpriced": account.Unpriced,
			"downgraded": account.Downgraded, "since": account.Since,
			"hours": len(account.Hours), "segments": segments,
			"input_tokens": totalInput, "cached_tokens": totalCached,
			"cache_write_tokens": totalWrite, "output_tokens": totalOutput,
			"long_tier_requests": totalLong,
			"hourly": hours,
		}
		if totalInput > 0 {
			summary["cache_hit_rate"] = float64(totalCached) / float64(totalInput)
			summary["cache_write_rate"] = float64(totalWrite) / float64(totalInput)
		}
		if account.Requests > 0 {
			summary["long_tier_share"] = float64(totalLong) / float64(account.Requests)
		}
		accounts[key] = summary
	}
	return map[string]any{"accounts": accounts, "at": time.Now().Format(time.RFC3339)}
}

func (s *Service) serveCostDebug() map[string]any {
	return map[string]any{
		"StatusCode": http.StatusOK,
		"Headers": map[string][]string{
			"Content-Type":  {"application/json; charset=utf-8"},
			"Cache-Control": {"no-store"},
		},
		"Body": jsonBytes(s.costDebugSnapshot()),
	}
}
