package basispoints

import (
	"sync"
	"time"
)

// web_search_guard (h26, 2026-10-09).
//
// Since 2026-10-09 09:37 (Beijing) every Basis Points turn in which the model
// uses the upstream's own built-in web search ends in response.failed
// server_error after the search itself completed: 283/283 such turns in a
// 20-minute production capture, and 12/12 direct probes across models, tools
// versions and runtime contexts. The request body cannot switch the search off
// (the upstream answers 422 to tool_choice and max_tool_calls), and a generic
// "search is unavailable" instruction is ignored by the gpt-6 models. What the
// models did follow in testing is being told that the search they just tried
// failed: with that sentence appended, a retried turn answers without
// searching (gpt-6-astra 2/2 retries; preventively gpt-6 4/6, gpt-5.6 4/4) and
// still calls the client's own tools normally (3/3).
//
// So the guard remembers, per caller-scoped conversation, that a turn died
// after a web search, and appends that sentence to the end of the input of the
// conversation's next requests for a while. Appending at the end keeps the
// cached prefix intact. Off (the default) changes nothing.
//
//	off     nothing is appended (h25 behaviour).
//	retry   appended only for a conversation whose turn failed after a web
//	        search within the last web_search_guard_ttl_seconds.
//	always  appended to every request (emergency switch).
const (
	webSearchGuardOff    = "off"
	webSearchGuardRetry  = "retry"
	webSearchGuardAlways = "always"

	defaultWebSearchGuardTTLSeconds = 900
	webSearchGuardMaxScopes         = 8192
)

const webSearchGuardNotice = "The web search you just attempted failed: live web search is unavailable right now and every further " +
	"search or page-open call will fail. Do not search again. Continue the task without web search, using your " +
	"own knowledge and the other tools you already have, and mention that live verification was not possible."

var webSearchNow = time.Now

var webSearchFailures = struct {
	sync.Mutex
	until map[string]time.Time
}{until: map[string]time.Time{}}

// noteWebSearchFailure records that a turn of this conversation died after a
// web search. An empty scope (no session key, no fingerprint) is never shared.
func noteWebSearchFailure(scope string, ttl time.Duration) {
	if scope == "" || ttl <= 0 {
		return
	}
	now := webSearchNow()
	webSearchFailures.Lock()
	defer webSearchFailures.Unlock()
	webSearchFailures.until[scope] = now.Add(ttl)
	if len(webSearchFailures.until) > webSearchGuardMaxScopes {
		for key, until := range webSearchFailures.until {
			if !now.Before(until) {
				delete(webSearchFailures.until, key)
			}
		}
		for key := range webSearchFailures.until {
			if len(webSearchFailures.until) <= webSearchGuardMaxScopes {
				break
			}
			if key != scope {
				delete(webSearchFailures.until, key)
			}
		}
	}
	countRelayEvent("web_search_failure_noted")
}

func webSearchFailedRecently(scope string) bool {
	if scope == "" {
		return false
	}
	webSearchFailures.Lock()
	defer webSearchFailures.Unlock()
	until, ok := webSearchFailures.until[scope]
	if !ok {
		return false
	}
	if !webSearchNow().Before(until) {
		delete(webSearchFailures.until, scope)
		return false
	}
	return true
}

// applyWebSearchGuard appends the notice to the end of the upstream input when
// the switch says so. It never touches anything else in the body.
func applyWebSearchGuard(output map[string]any, cfg Config, scope string) {
	switch cfg.WebSearchGuard {
	case webSearchGuardAlways:
	case webSearchGuardRetry:
		if !webSearchFailedRecently(scope) {
			return
		}
	default:
		return
	}
	items, ok := output["input"].([]any)
	if !ok {
		return
	}
	output["input"] = append(items, messageItem("developer", webSearchGuardNotice))
	countRelayEvent("web_search_notice_added:" + cfg.WebSearchGuard)
}

// noteWebSearchStreamOutcome is called when a stream closes: a turn that used
// the web search and then failed marks its conversation.
func noteWebSearchStreamOutcome(cfg Config, scope string, err error, progress *streamProgress) {
	if progress == nil || (!progress.webSearch.Load() && !progress.failedOver.Load()) {
		return
	}
	terminal := progress.upstreamTerminal()
	if err == nil && terminal != "error" && terminal != "response.failed" {
		return
	}
	noteWebSearchFailure(scope, time.Duration(cfg.WebSearchGuardTTLSeconds)*time.Second)
}

func normalizeWebSearchGuard(c *Config) error {
	switch c.WebSearchGuard {
	case "":
		c.WebSearchGuard = webSearchGuardOff
	case webSearchGuardOff, webSearchGuardRetry, webSearchGuardAlways:
	default:
		return fail(400, "invalid_config", "web_search_guard must be off, retry or always")
	}
	if c.WebSearchGuardTTLSeconds == 0 {
		c.WebSearchGuardTTLSeconds = defaultWebSearchGuardTTLSeconds
	}
	if c.WebSearchGuardTTLSeconds < 60 || c.WebSearchGuardTTLSeconds > 86400 {
		return fail(400, "invalid_config", "web_search_guard_ttl_seconds must be between 60 and 86400")
	}
	return nil
}
