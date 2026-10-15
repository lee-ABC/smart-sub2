package basispoints

import (
	"strings"
	"testing"
	"time"
)

func h27Relay(on bool) *streamRelay {
	relay := newStreamRelay(NewService(), "client", weatherRequest(), usageTarget{}, "gpt-6-astra")
	relay.progress = &streamProgress{}
	relay.wsFailover = on
	relay.wsSwitchOn = on
	return relay
}

func h27Feed(t *testing.T, relay *streamRelay, events ...string) error {
	t.Helper()
	var last error
	for _, e := range events {
		if err := relay.feed([]byte(e)); err != nil {
			return err
		}
		last = nil
	}
	return last
}

var (
	h27Created   = h26Event("response.created", map[string]any{"response": map[string]any{"id": "resp_1", "status": "in_progress", "output": []any{}}})
	h27Reasoning = h26Event("response.output_item.added", map[string]any{"output_index": 0, "item": map[string]any{"type": "reasoning", "id": "rs_1", "summary": []any{}}})
	h27RsDelta   = h26Event("response.reasoning_summary_text.delta", map[string]any{"output_index": 0, "item_id": "rs_1", "summary_index": 0, "delta": "thinking"})
	h27WSAdded   = h26Event("response.output_item.added", map[string]any{"output_index": 1, "item": map[string]any{"type": "web_search_call", "id": "ws_1", "status": "in_progress"}})
	h27WSDone    = h26Event("response.output_item.done", map[string]any{"output_index": 1, "item": map[string]any{"type": "web_search_call", "id": "ws_1", "status": "completed", "action": map[string]any{"type": "search", "query": "q"}}})
	h27MsgAdded  = h26Event("response.output_item.added", map[string]any{"output_index": 2, "item": map[string]any{"type": "message", "id": "msg_1", "role": "assistant", "phase": "final_answer", "status": "in_progress", "content": []any{}}})
	h27MsgDelta  = h26Event("response.output_text.delta", map[string]any{"output_index": 2, "item_id": "msg_1", "content_index": 0, "delta": "Rust"})
	h27Comment   = h26Event("response.output_item.added", map[string]any{"output_index": 1, "item": map[string]any{"type": "message", "id": "msg_c", "role": "assistant", "phase": "commentary", "status": "in_progress", "content": []any{}}})
	h27CommentD  = h26Event("response.output_text.delta", map[string]any{"output_index": 1, "item_id": "msg_c", "content_index": 0, "delta": "I will check."})
	h27ToolAdded = h26Event("response.output_item.added", map[string]any{"output_index": 3, "item": map[string]any{"type": "function_call", "id": "fc_1", "call_id": "call_1", "name": "exec_command", "arguments": ""}})
)

func h27Failed(code string) string {
	return h26Event("response.failed", map[string]any{"response": map[string]any{"id": "resp_1", "status": "failed", "output": []any{},
		"error": map[string]any{"code": code, "message": "An error occurred while processing your request. You can retry your request, or contact us through our help center at help.openai.com if the error persists. Please include the request ID 1234 in your message."}}})
}

func h27Completed() string {
	return h26Event("response.completed", map[string]any{"response": map[string]any{"id": "resp_1", "status": "completed", "output": []any{}}})
}

func TestH27OffKeepsInBandFailure(t *testing.T) {
	relay := h27Relay(false)
	if err := h27Feed(t, relay, h27Created, h27Reasoning, h27WSAdded, h27WSDone, h27Failed("server_error")); err != nil {
		t.Fatalf("off: failure must stay in-band, got %v", err)
	}
	if relay.wsHolding.Load() || relay.terminal != "response.failed" {
		t.Fatalf("off: holding=%v terminal=%q", relay.wsHolding.Load(), relay.terminal)
	}
}

func h27AssertFailover(t *testing.T, relay *streamRelay, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("failover must end the stream normally (CPA must not cool the credential), got %v", err)
	}
	out := relay.out.String()
	if strings.Contains(out, "web_search_call") || strings.Contains(out, "\"reasoning\"") || strings.Contains(out, "output_item") {
		t.Fatalf("held output leaked: %s", out)
	}
	if !strings.Contains(out, "please retry") || relay.terminal == "" {
		t.Fatalf("failure must go on in-band with the retry wording: terminal=%q out=%s", relay.terminal, out)
	}
	// Main site v0.2.14 isOpenAITransientProcessingError: either phrase set sends
	// a pool-mode account into same-account retries instead of a switch.
	lower := strings.ToLower(out)
	if strings.Contains(lower, "an error occurred while processing your request") ||
		(strings.Contains(lower, "you can retry your request") && strings.Contains(lower, "help.openai.com")) ||
		strings.Contains(lower, "server is overloaded") || strings.Contains(lower, "servers are") {
		t.Fatalf("upstream wording leaked into the failover failure: %s", out)
	}
	if !strings.Contains(out, webSearchFailoverMessage) {
		t.Fatalf("failover message missing: %s", out)
	}
	for _, marker := range []string{"invalid_request", "content_policy", "policy", "safety", "high-risk cyber", "not allowed", "violat"} {
		if strings.Contains(strings.ToLower(out), marker) {
			t.Fatalf("failure carries main-site no-retry marker %q", marker)
		}
	}
	if relay.wsHolding.Load() {
		t.Fatal("must not stay holding")
	}
}

func TestH27SearchThenServerErrorFailsOverWithZeroOutput(t *testing.T) {
	relay := h27Relay(true)
	err := h27Feed(t, relay, h27Created, h27Reasoning, h27RsDelta, h27WSAdded, h27WSDone, h27Failed("server_error"))
	h27AssertFailover(t, relay, err)
	if relay.terminal != "response.failed" {
		t.Fatalf("terminal %q", relay.terminal)
	}
}

func TestH27BareErrorEventFailsOver(t *testing.T) {
	relay := h27Relay(true)
	err := h27Feed(t, relay, h27Created, h27WSAdded, h27WSDone,
		h26Event("error", map[string]any{"error": map[string]any{"type": "server_error", "code": "server_error", "message": "An error occurred while processing your request."}}))
	h27AssertFailover(t, relay, err)
	if relay.terminal != "error" {
		t.Fatalf("terminal %q", relay.terminal)
	}
}

func TestH27ReasoningThenMessageReleases(t *testing.T) {
	relay := h27Relay(true)
	if err := h27Feed(t, relay, h27Created, h27Reasoning, h27RsDelta); err != nil {
		t.Fatal(err)
	}
	if !relay.wsHolding.Load() {
		t.Fatal("reasoning prelude must be held")
	}
	if err := h27Feed(t, relay, h27MsgAdded); err != nil {
		t.Fatal(err)
	}
	if !relay.wsHolding.Load() {
		t.Fatal("an answer message with no text yet must not release")
	}
	if err := h27Feed(t, relay, h27MsgDelta); err != nil {
		t.Fatal(err)
	}
	if relay.wsHolding.Load() || relay.wsState != wsFailoverReleased {
		t.Fatal("the first answer text must release the held output")
	}
	if err := h27Feed(t, relay, h27Completed()); err != nil {
		t.Fatal(err)
	}
}

func TestH27SearchThenMessageThenFailureStaysInBand(t *testing.T) {
	relay := h27Relay(true)
	if err := h27Feed(t, relay, h27Created, h27WSAdded, h27WSDone, h27MsgAdded, h27MsgDelta, h27Failed("server_error")); err != nil {
		t.Fatalf("released turn: failure must stay in-band, got %v", err)
	}
	if relay.terminal != "response.failed" {
		t.Fatalf("terminal %q", relay.terminal)
	}
}

func TestH27NonServerFailureIsNeverFailedOver(t *testing.T) {
	relay := h27Relay(true)
	if err := h27Feed(t, relay, h27Created, h27WSAdded, h27WSDone,
		h26Event("response.failed", map[string]any{"response": map[string]any{"id": "resp_1", "status": "failed", "output": []any{},
			"error": map[string]any{"code": "cyber_policy", "message": "This content was flagged"}}})); err != nil {
		t.Fatalf("a refusal must pass through unchanged, got %v", err)
	}
	if relay.wsHolding.Load() {
		t.Fatal("refusal must release")
	}
}

func TestH27ReasoningOnlyFailureIsNotFailedOver(t *testing.T) {
	relay := h27Relay(true)
	if err := h27Feed(t, relay, h27Created, h27Reasoning, h27Failed("server_error")); err != nil {
		t.Fatalf("no search: failure stays in-band, got %v", err)
	}
}

func TestH27PreludeCapReleases(t *testing.T) {
	saved := webSearchFailoverPreludeCap
	webSearchFailoverPreludeCap = 0
	defer func() { webSearchFailoverPreludeCap = saved }()
	relay := h27Relay(true)
	relay.wsHoldStart = time.Now().Add(-time.Second)
	if err := h27Feed(t, relay, h27Created, h27Reasoning, h27RsDelta); err != nil {
		t.Fatal(err)
	}
	if relay.wsHolding.Load() {
		t.Fatal("prelude cap must release")
	}
}

func TestH27FinishReleasesHeldOutput(t *testing.T) {
	relay := h27Relay(true)
	if err := h27Feed(t, relay, h27Created, h27WSAdded, h27WSDone, h27Completed()); err != nil {
		t.Fatal(err)
	}
	if relay.wsHolding.Load() {
		t.Fatal("a completed terminal must release")
	}
}

func TestH27SearchThenEmptyAnswerThenFailureFailsOver(t *testing.T) {
	// Measured shape (gpt-6-astra, 2026-10-09): searches, then the final_answer
	// message opens, then response.failed before any text.
	relay := h27Relay(true)
	err := h27Feed(t, relay, h27Created, h27WSAdded, h27WSDone, h27MsgAdded, h27Failed("server_error"))
	h27AssertFailover(t, relay, err)
}

func TestH27CommentaryBeforeSearchStaysHeldAndFailsOver(t *testing.T) {
	// Measured shape (gpt-5.6-sol): reasoning, commentary with text, searches,
	// final_answer opens, response.failed.
	relay := h27Relay(true)
	ws := map[string]any{"type": "web_search_call", "id": "ws_2", "status": "completed", "action": map[string]any{"type": "search", "query": "q"}}
	err := h27Feed(t, relay, h27Created, h27Reasoning, h27Comment, h27CommentD,
		h26Event("response.output_item.added", map[string]any{"output_index": 2, "item": map[string]any{"type": "web_search_call", "id": "ws_2", "status": "in_progress"}}),
		h26Event("response.output_item.done", map[string]any{"output_index": 2, "item": ws}),
		h26Event("response.output_item.added", map[string]any{"output_index": 3, "item": map[string]any{"type": "message", "id": "msg_3", "role": "assistant", "phase": "final_answer", "status": "in_progress", "content": []any{}}}),
		h27Failed("server_error"))
	h27AssertFailover(t, relay, err)
}

func TestH27AnswerFirstTurnIsNeverHeld(t *testing.T) {
	relay := h27Relay(true)
	if err := h27Feed(t, relay, h27Created, h27MsgAdded); err != nil {
		t.Fatal(err)
	}
	if relay.wsHolding.Load() || relay.wsState != wsFailoverReleased {
		t.Fatal("a turn opening with its answer must stream untouched")
	}
}

func TestH27ToolCallReleases(t *testing.T) {
	relay := h27Relay(true)
	if err := h27Feed(t, relay, h27Created, h27Reasoning, h27Comment, h27CommentD); err != nil {
		t.Fatal(err)
	}
	if !relay.wsHolding.Load() {
		t.Fatal("commentary must be held")
	}
	if err := h27Feed(t, relay, h27ToolAdded); err != nil {
		t.Fatal(err)
	}
	if relay.wsHolding.Load() {
		t.Fatal("a tool call must release")
	}
}

func h27HoldAllRelay() *streamRelay {
	relay := h27Relay(true)
	relay.wsHoldAll = true
	return relay
}

func TestH27cDecision(t *testing.T) {
	defer h26ResetWebSearch(t)()
	cfg := defaultConfig()
	cfg.WebSearchFailover = true
	if err := cfg.normalize(); err != nil {
		t.Fatal(err)
	}
	if cfg.WebSearchFailoverScope != webSearchFailoverScopeAfterFailure {
		t.Fatalf("default scope %q", cfg.WebSearchFailoverScope)
	}
	if f, h := webSearchFailoverFor(cfg, "conv-a"); f || h {
		t.Fatal("an unflagged conversation must stream untouched")
	}
	noteWebSearchFailure("conv-a", time.Minute)
	if f, h := webSearchFailoverFor(cfg, "conv-a"); !f || !h {
		t.Fatal("a flagged conversation must hold its whole turn")
	}
	if f, _ := webSearchFailoverFor(cfg, "conv-b"); f {
		t.Fatal("another conversation must stay untouched")
	}
	if f, _ := webSearchFailoverFor(cfg, ""); f {
		t.Fatal("an empty scope is never flagged")
	}
	cfg.WebSearchFailoverScope = webSearchFailoverScopeAll
	if f, h := webSearchFailoverFor(cfg, "conv-b"); !f || h {
		t.Fatal("all: the h27b behaviour")
	}
	off := defaultConfig()
	if err := off.normalize(); err != nil {
		t.Fatal(err)
	}
	if f, _ := webSearchFailoverFor(off, "conv-a"); f {
		t.Fatal("switch off: never")
	}
	bad := defaultConfig()
	bad.WebSearchFailoverScope = "sometimes"
	if bad.normalize() == nil {
		t.Fatal("an invalid scope must be refused")
	}
}

func TestH27cHoldAllHoldsAnswerTextUntilCompleted(t *testing.T) {
	relay := h27HoldAllRelay()
	if err := h27Feed(t, relay, h27Created, h27MsgAdded, h27MsgDelta); err != nil {
		t.Fatal(err)
	}
	if !relay.wsHolding.Load() {
		t.Fatal("answer text must stay held in a flagged conversation")
	}
	if err := h27Feed(t, relay, h27Completed()); err != nil {
		t.Fatal(err)
	}
	if relay.wsHolding.Load() {
		t.Fatal("completed must release")
	}
}

func TestH27cHoldAllTextThenSearchThenFailureFailsOver(t *testing.T) {
	// The case h27b let through: text shown before the search.
	relay := h27HoldAllRelay()
	err := h27Feed(t, relay, h27Created,
		h26Event("response.output_item.added", map[string]any{"output_index": 0, "item": map[string]any{"type": "message", "id": "msg_0", "role": "assistant", "status": "in_progress", "content": []any{}}}),
		h26Event("response.output_text.delta", map[string]any{"output_index": 0, "item_id": "msg_0", "content_index": 0, "delta": "Let me check."}),
		h27WSAdded, h27WSDone, h27Failed("server_error"))
	h27AssertFailover(t, relay, err)
}

func TestH27cHoldAllServerErrorWithoutSearchFailsOver(t *testing.T) {
	relay := h27HoldAllRelay()
	err := h27Feed(t, relay, h27Created, h27Reasoning, h27RsDelta, h27Failed("server_error"))
	h27AssertFailover(t, relay, err)
}

func TestH27cHoldAllToolCallReleases(t *testing.T) {
	relay := h27HoldAllRelay()
	if err := h27Feed(t, relay, h27Created, h27Reasoning, h27Comment, h27CommentD, h27ToolAdded); err != nil {
		t.Fatal(err)
	}
	if relay.wsHolding.Load() {
		t.Fatal("a tool call must release")
	}
}

func TestH27cHoldAllRefusalPassesThrough(t *testing.T) {
	relay := h27HoldAllRelay()
	if err := h27Feed(t, relay, h27Created, h27WSAdded, h27WSDone,
		h26Event("response.failed", map[string]any{"response": map[string]any{"id": "resp_1", "status": "failed", "output": []any{},
			"error": map[string]any{"code": "cyber_policy", "message": "This content was flagged"}}})); err != nil {
		t.Fatalf("a refusal must pass through unchanged, got %v", err)
	}
	if relay.wsHolding.Load() || !strings.Contains(relay.out.String(), "cyber_policy") {
		t.Fatalf("refusal must be released as it came: holding=%v out=%s", relay.wsHolding.Load(), relay.out.String())
	}
}

func TestH27cHoldAllCapReleases(t *testing.T) {
	saved := webSearchFailoverHoldAllCap
	webSearchFailoverHoldAllCap = 0
	defer func() { webSearchFailoverHoldAllCap = saved }()
	relay := h27HoldAllRelay()
	if err := h27Feed(t, relay, h27Created, h27MsgAdded, h27MsgDelta); err != nil {
		t.Fatal(err)
	}
	if relay.wsHolding.Load() {
		t.Fatal("the whole-turn cap must release")
	}
}

func TestH27dZeroOutputServerErrorFailsOverEvenUnflagged(t *testing.T) {
	// Measured shape: a Codex retry carrying the failed turn's web_search_call
	// items fails before any output item.
	relay := h27Relay(false)
	relay.wsSwitchOn = true
	err := h27Feed(t, relay, h27Created, h27Failed("server_error"))
	h27AssertFailover(t, relay, err)
	if !relay.progress.failedOver.Load() {
		t.Fatal("a failover must flag its conversation")
	}
}

func TestH27dOutputAlreadySentStaysInBand(t *testing.T) {
	relay := h27Relay(false)
	relay.wsSwitchOn = true
	if err := h27Feed(t, relay, h27Created, h27Reasoning, h27RsDelta, h27Failed("server_error")); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(relay.out.String(), webSearchFailoverMessage) {
		t.Fatal("once the client has output, the failure must pass through unchanged")
	}
}

func TestH27dSwitchOffNeverRewrites(t *testing.T) {
	relay := h27Relay(false)
	if err := h27Feed(t, relay, h27Created, h27Failed("server_error")); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(relay.out.String(), webSearchFailoverMessage) {
		t.Fatal("switch off: the failure must pass through unchanged")
	}
}

func TestH27dZeroOutputRefusalPassesThrough(t *testing.T) {
	relay := h27Relay(false)
	relay.wsSwitchOn = true
	if err := h27Feed(t, relay, h27Created,
		h26Event("response.failed", map[string]any{"response": map[string]any{"id": "resp_1", "status": "failed", "output": []any{},
			"error": map[string]any{"code": "cyber_policy", "message": "This content was flagged"}}})); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(relay.out.String(), webSearchFailoverMessage) || !strings.Contains(relay.out.String(), "cyber_policy") {
		t.Fatalf("a refusal must pass through unchanged: %s", relay.out.String())
	}
}

func TestH27dFailoverFlagsConversation(t *testing.T) {
	defer h26ResetWebSearch(t)()
	cfg := defaultConfig()
	cfg.WebSearchFailover = true
	if err := cfg.normalize(); err != nil {
		t.Fatal(err)
	}
	p := &streamProgress{scope: "conv-z"}
	p.failedOver.Store(true)
	p.terminal.Store("response.failed")
	noteWebSearchStreamOutcome(cfg, p.scope, nil, p)
	if f, h := webSearchFailoverFor(cfg, "conv-z"); !f || !h {
		t.Fatal("a conversation whose turn failed over must be held from then on")
	}
}
