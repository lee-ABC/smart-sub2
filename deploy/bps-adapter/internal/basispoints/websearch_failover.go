package basispoints

import (
	"strings"
	"time"
)

// web_search_failover (h27, 2026-10-09).
//
// The upstream's built-in web search fails after the search itself completed:
// the response ends in response.failed / error server_error (see
// websearch_guard.go). Other upstreams behind the main site search fine. The
// main site switches a request to another account when the stream fails before
// anything but response.created / response.in_progress reached the client
// (relay_errors.go, E5) -- and since CPA is the only CPA account there, that
// other account is an upstream whose search works.
//
// So, with the switch on, a turn that opens with reasoning, a web search or a
// commentary message is held back from the client:
//
//   - reasoning / commentary only, then answer text or a tool call: everything
//     held is released and the turn streams normally from there on (such a
//     prelude is released after webSearchFailoverPreludeCap at the latest);
//   - a web search, then answer text or a tool call: the search went through,
//     released the same way;
//   - a web search, then response.failed / error with code server_error:
//     nothing held is sent; the upstream's failure goes on in-band with an
//     empty output and the E5 retry wording, and the main site replays the
//     turn elsewhere (the stream ends normally, so CPA cools nothing);
//   - any other failure (a policy refusal, context length, ...) is passed on
//     exactly as before -- this switch never turns a refusal into a retry.
//
// Once released, a turn is never held again. While holding, the batch
// heartbeat keeps the main site's stream-interval timer fed with SSE comments,
// which the main site neither forwards nor counts as output. Off (the
// default), none of this runs.
const (
	wsFailoverIdle = iota
	wsFailoverPrelude
	wsFailoverSearch
	wsFailoverReleased
)

var webSearchFailoverPreludeCap = 60 * time.Second

// webSearchFailoverHoldAllCap bounds a whole-turn hold (after_failure): a turn
// still going after this long is released and streams on from there.
var webSearchFailoverHoldAllCap = 300 * time.Second

const (
	webSearchFailoverScopeAfterFailure = "after_failure"
	webSearchFailoverScopeAll          = "all"
)

// webSearchFailoverFor decides at stream start whether this stream may fail
// over and whether it holds its whole turn.
//
// after_failure (the default): only a conversation that had a turn fail after
// a web search within web_search_guard_ttl_seconds -- typically the client's
// own automatic retry of that turn -- is held, and then entirely. Measured
// 2026-10-09 with "all": holding the reasoning of every turn (74% of turns open
// with it) moved CPA's first-token p50 3.3 -> 5.5 s and p90 6.5 -> 15.2 s while
// rescuing about 40% of the failed searches; text a model wrote before
// searching was already released. after_failure leaves the other ~97% of
// conversations untouched and catches the whole of the flagged ones.
func webSearchFailoverFor(cfg Config, scope string) (failover, holdAll bool) {
	if !cfg.WebSearchFailover {
		return false, false
	}
	if cfg.WebSearchFailoverScope == webSearchFailoverScopeAll {
		return true, false
	}
	if webSearchFailedRecently(scope) {
		countRelayEvent("web_search_failover_hold_all")
		return true, true
	}
	return false, false
}

func normalizeWebSearchFailover(c *Config) error {
	switch c.WebSearchFailoverScope {
	case "":
		c.WebSearchFailoverScope = webSearchFailoverScopeAfterFailure
	case webSearchFailoverScopeAfterFailure, webSearchFailoverScopeAll:
	default:
		return fail(400, "invalid_config", "web_search_failover_scope must be after_failure or all")
	}
	return nil
}

// noOutputYet: nothing but response.created / response.in_progress has been
// written to the client, flushed or not.
func (r *streamRelay) noOutputYet() bool {
	if r.pendingOutput {
		return false
	}
	return r.progress == nil || !r.progress.outputSent.Load()
}

// progressScope is the conversation key executeStream froze for this stream.
func progressScope(progress *streamProgress) string {
	if progress == nil {
		return ""
	}
	return progress.scope
}

// webSearchFailoverObserve runs for every upstream event before it is
// handled. handled=true: the event was written here (the failover terminal).
//
// Held: reasoning, web search, and "commentary" messages (the preamble a model
// writes before acting). Released by: the first text of any other message (a
// final answer -- measured 2026-10-09: the upstream opens the final_answer
// message and then fails, so its output_item.added alone proves nothing), a
// client tool call or any other output item, or a successful terminal. Events
// that carry no decision (content_part.added, *.done, ...) leave the state as
// it is: holding loses nothing, release flushes all of it.
func (r *streamRelay) webSearchFailoverObserve(kind string, payload map[string]any) (bool, error) {
	// h27d: whatever the hold state, a server error before anything but
	// response.created / response.in_progress reached the client is switched
	// away at once -- the upstream's own wording would put a pool-mode account
	// into same-account retries instead (see webSearchFailoverMessage).
	if r.wsSwitchOn && (kind == "response.failed" || kind == "error") && !r.wsHolding.Load() &&
		r.noOutputYet() && upstreamServerError(kind, payload) {
		countRelayEvent("web_search_failover_zero_output")
		return r.emitWebSearchFailover(kind, payload)
	}
	if !r.wsFailover || r.wsState == wsFailoverReleased {
		return false, nil
	}
	if r.wsHoldAll {
		return r.webSearchHoldAllObserve(kind, payload)
	}
	hold := func(state int) {
		if r.wsState == wsFailoverIdle {
			r.wsHoldStart = time.Now()
		}
		if state > r.wsState {
			r.wsState = state
		}
		r.wsHolding.Store(true)
	}
	switch {
	case kind == "response.created" || kind == "response.in_progress":
		return false, nil
	case kind == "response.output_item.added":
		item := objectValue(payload["item"])
		switch stringValue(item["type"]) {
		case "reasoning":
			hold(wsFailoverPrelude)
		case "web_search_call":
			hold(wsFailoverSearch)
		case "message":
			if r.wsPhases == nil {
				r.wsPhases = map[int]string{}
			}
			phase := stringValue(item["phase"])
			r.wsPhases[eventOutputIndex(payload)] = phase
			if phase == "commentary" {
				hold(wsFailoverPrelude)
			} else if r.wsState == wsFailoverIdle {
				// A turn that opens with its answer is never held.
				r.webSearchRelease()
			}
		default:
			r.webSearchRelease()
		}
	case kind == "response.output_item.done":
		if stringValue(objectValue(payload["item"])["type"]) == "web_search_call" {
			hold(wsFailoverSearch)
		}
	case strings.HasPrefix(kind, "response.reasoning"):
		hold(wsFailoverPrelude)
	case strings.HasPrefix(kind, "response.web_search_call."):
		hold(wsFailoverSearch)
	case kind == "response.output_text.delta" || kind == "response.refusal.delta":
		if r.wsPhases[eventOutputIndex(payload)] == "commentary" {
			hold(wsFailoverPrelude)
		} else if stringValue(payload["delta"]) != "" {
			r.webSearchRelease()
		}
	case kind == "response.failed" || kind == "error":
		if r.wsState == wsFailoverSearch && r.wsHolding.Load() && upstreamServerError(kind, payload) {
			return r.emitWebSearchFailover(kind, payload)
		}
		r.webSearchRelease()
	case kind == "response.completed" || kind == "response.incomplete":
		r.webSearchRelease()
	}
	if r.wsState == wsFailoverPrelude && r.wsHolding.Load() && time.Since(r.wsHoldStart) > webSearchFailoverPreludeCap {
		countRelayEvent("web_search_failover_prelude_released")
		r.webSearchRelease()
	}
	return false, nil
}

// webSearchHoldAllObserve is after_failure's whole-turn hold for a flagged
// conversation: everything -- answer text included -- is held until the turn
// completes or hands the client a tool call. A server error anywhere before
// that, after a search or not, leaves the client with zero output and the main
// site replays the turn on another account. A refusal or any other failure is
// passed on unchanged.
func (r *streamRelay) webSearchHoldAllObserve(kind string, payload map[string]any) (bool, error) {
	hold := func() {
		if r.wsState == wsFailoverIdle {
			r.wsHoldStart = time.Now()
			r.wsState = wsFailoverPrelude
		}
		r.wsHolding.Store(true)
	}
	switch {
	case kind == "response.created" || kind == "response.in_progress":
		return false, nil
	case kind == "response.failed" || kind == "error":
		if r.wsHolding.Load() && upstreamServerError(kind, payload) {
			return r.emitWebSearchFailover(kind, payload)
		}
		r.webSearchRelease()
		return false, nil
	case kind == "response.completed" || kind == "response.incomplete":
		r.webSearchRelease()
		return false, nil
	case kind == "response.output_item.added":
		switch stringValue(objectValue(payload["item"])["type"]) {
		case "function_call", "custom_tool_call":
			// Handed over at once: the client acts on it, and a turn that
			// calls a tool is not waiting on a search.
			r.webSearchRelease()
			return false, nil
		case "web_search_call":
			hold()
			r.wsState = wsFailoverSearch
			return false, nil
		}
	}
	hold()
	if time.Since(r.wsHoldStart) > webSearchFailoverHoldAllCap {
		countRelayEvent("web_search_failover_hold_all_released")
		r.webSearchRelease()
	}
	return false, nil
}

// emitWebSearchFailover drops everything held and passes the upstream's
// failure on in-band, as h25 does, with an empty output snapshot and our own
// retry wording (webSearchFailoverMessage). The stream itself ends normally,
// so CPA does not cool the credential (a stream error would: about a minute),
// and the main site sees a zero-output, retryable failure and replays the turn
// on another account.
func (r *streamRelay) emitWebSearchFailover(kind string, payload map[string]any) (bool, error) {
	r.out.Reset()
	r.pendingOutput = false
	r.cacheAfterFlush = nil
	r.nativeAfterFlush = nil
	if response := objectValue(payload["response"]); response != nil {
		restoreCallerEcho(response, r.source)
		dropNonStandardUsageDetails(response)
		response["output"] = []any{}
	}
	replaceFailoverMessage(kind, payload)
	r.discardPendingTransport()
	r.webSearchRelease()
	r.writeEvent(kind, payload)
	r.terminal = kind
	if r.progress != nil {
		r.progress.failedOver.Store(true)
	}
	countRelayEvent("web_search_failover")
	if r.wsHoldAll {
		countRelayEvent("web_search_failover_from_hold_all")
	}
	return true, nil
}

// webSearchRelease lets the held output go out with the next flush.
func (r *streamRelay) webSearchRelease() {
	if r.wsState == wsFailoverPrelude || r.wsState == wsFailoverSearch {
		countRelayEvent("web_search_failover_released")
	}
	r.wsState = wsFailoverReleased
	r.wsHolding.Store(false)
}

// webSearchFailoverMessage replaces the upstream's failure text on a failover.
// It must keep the main site's switch-account path and stay off its
// same-account path (main site v0.2.14, measured 2026-10-09): the upstream's
// own "An error occurred while processing your request ... help.openai.com ...
// request ID" wording is a "transient processing error" there, which a
// pool-mode account retries on the same account up to ten times -- every retry
// searching and failing again. This text carries "temporary" / "please retry"
// (an error event needs one of them to fail over) and none of the no-retry
// markers ("policy", "invalid_request", "not allowed", ...).
const webSearchFailoverMessage = "Basis Points web search failed after the search completed" + retryableErrorHint

// replaceFailoverMessage puts webSearchFailoverMessage wherever the failure
// event carries its message, so no copy of the upstream wording is left.
func replaceFailoverMessage(kind string, payload map[string]any) {
	set := func(holder map[string]any) bool {
		if holder == nil {
			return false
		}
		if _, ok := holder["message"]; !ok {
			return false
		}
		holder["message"] = webSearchFailoverMessage
		return true
	}
	if kind == "response.failed" {
		response := objectValue(payload["response"])
		if !set(objectValue(response["error"])) && response != nil {
			response["error"] = map[string]any{"code": "server_error", "message": webSearchFailoverMessage}
		}
		return
	}
	replaced := set(objectValue(payload["error"]))
	if _, ok := payload["message"]; ok || !replaced {
		payload["message"] = webSearchFailoverMessage
	}
}

// upstreamServerError: the failure event is the upstream's generic server
// error, not a refusal or a request problem.
func upstreamServerError(kind string, payload map[string]any) bool {
	errObject := objectValue(payload["error"])
	if kind == "response.failed" {
		errObject = objectValue(objectValue(payload["response"])["error"])
	}
	code, message := "", ""
	if errObject != nil {
		code = stringValue(errObject["code"])
		message = stringValue(errObject["message"])
	}
	if code == "" {
		code = stringValue(payload["code"])
	}
	if message == "" {
		message = stringValue(payload["message"])
	}
	return code == "server_error" || strings.HasPrefix(message, "An error occurred while processing your request")
}
