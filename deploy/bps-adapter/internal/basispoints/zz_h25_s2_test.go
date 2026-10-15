package basispoints

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// A scriptable host for the S2 stream tests.

type s2HostScript struct {
	configure  func(*Config)
	beforeRead func(service *Service, index int) // before chunk index is returned
	readDelay  time.Duration                     // before every chunk but the first
	failEmit   func(payload string) bool         // host.stream.emit refuses this payload
}

type s2StreamRun struct {
	emits    []string
	closeErr string
	logs     []map[string]any
	trace    []string
}

func (r s2StreamRun) joined() string { return strings.Join(r.emits, "") }

func s2RunStream(t *testing.T, script s2HostScript, body map[string]any, chunks []string) s2StreamRun {
	t.Helper()
	service := NewService()
	if script.configure != nil {
		cfg := service.config()
		script.configure(&cfg)
		service.mu.Lock()
		service.cfg = cfg
		service.mu.Unlock()
	}
	var (
		mu   sync.Mutex
		run  s2StreamRun
		next int
	)
	done := make(chan struct{})
	service.SetHost(func(method string, payload any, out any) error {
		request, _ := payload.(map[string]any)
		switch method {
		case "host.http.do_stream":
			stream := out.(*upstreamStream)
			stream.StatusCode, stream.StreamID = 200, "upstream-s2"
		case "host.http.stream_read":
			mu.Lock()
			index := next
			next++
			mu.Unlock()
			if index > 0 && script.readDelay > 0 {
				time.Sleep(script.readDelay)
			}
			if script.beforeRead != nil {
				script.beforeRead(service, index)
			}
			chunk := out.(*streamChunk)
			mu.Lock()
			run.trace = append(run.trace, "read")
			mu.Unlock()
			if index < len(chunks) {
				chunk.Payload = []byte(chunks[index])
			} else {
				chunk.Done = true
			}
		case "host.stream.emit":
			text := string(request["payload"].([]byte))
			if script.failEmit != nil && script.failEmit(text) {
				return errors.New("synthetic client hang-up")
			}
			mu.Lock()
			run.trace = append(run.trace, "emit")
			run.emits = append(run.emits, text)
			mu.Unlock()
		case "host.stream.close":
			mu.Lock()
			run.trace = append(run.trace, "close")
			run.closeErr, _ = request["error"].(string)
			mu.Unlock()
			close(done)
		case "host.log":
			mu.Lock()
			run.logs = append(run.logs, request)
			mu.Unlock()
		}
		return nil
	})
	identity := streamTestReplayRequest(t)
	raw, _ := json.Marshal(map[string]any{
		"OriginalRequest": jsonBytes(body), "Payload": jsonBytes(body), "StorageJSON": streamTestCredential(),
		"Model": "gpt-6-astra", "Stream": true, "stream_id": "client-s2", "host_callback_id": "s2-callback",
		"AuthID": identity.AuthID, "AuthProvider": identity.AuthProvider, "Metadata": identity.Metadata,
	})
	if _, err := service.Handle("executor.execute_stream", raw); err != nil {
		t.Fatalf("execute_stream: %v", err)
	}
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("relay never closed the stream")
	}
	mu.Lock()
	defer mu.Unlock()
	return s2StreamRun{emits: append([]string(nil), run.emits...), closeErr: run.closeErr,
		logs: append([]map[string]any(nil), run.logs...), trace: append([]string(nil), run.trace...)}
}

func s2PolicyLogs(logs []map[string]any) []map[string]any {
	var found []map[string]any
	for _, entry := range logs {
		if fields := objectValue(entry["fields"]); fields["event"] == "relay_tool_policy" {
			found = append(found, fields)
		}
	}
	return found
}

func s2DoneCount(text string) int { return strings.Count(text, "event: response.output_item.done") }

// ---------------------------------------------------------------------------
// S2.2 tool_policy_checks

func TestH25S2PolicyFindingsAreLoggedNotFatalByDefault(t *testing.T) {
	native := s2NativeCall("policy", `{"city":"Paris"}`)
	nativeAdded := cloneObject(native)
	nativeAdded["status"], nativeAdded["arguments"] = "in_progress", ""
	second := s2NativeCall("policy_two", `{"city":"Oslo"}`)
	wrongDelta := sseEvent("response.function_call_arguments.delta", map[string]any{"type": "response.function_call_arguments.delta", "output_index": 0, "item_id": "fc_WRONG", "delta": "x"})
	relayA := h21TransportItem("policy_a", "get_weather", map[string]any{"city": "Paris"})
	relayB := h21TransportItem("policy_b", "get_weather", map[string]any{"city": "Oslo"})
	changed := s2NativeCall("policy", `{"city":"Rome"}`)
	invalidJSON := s2NativeCall("policy_json", `{"city":`)
	cases := []struct {
		name, reason string
		request      func() map[string]any
		chunks       []string
		calls        int
	}{
		{"choice_none", "native_tool_not_allowed", func() map[string]any {
			r := weatherRequest()
			r["tool_choice"] = "none"
			return r
		}, []string{h21ItemEvent("response.output_item.done", 0, native), h21Terminal("response.completed", native)}, 1},
		{"forced_other", "native_tool_not_allowed", func() map[string]any {
			r := s2LiteWeatherRequest()
			r["tool_choice"] = map[string]any{"type": "function", "name": "other"}
			return r
		}, []string{h21ItemEvent("response.output_item.done", 0, native), h21Terminal("response.completed", native)}, 1},
		{"native_parallel_false", "parallel_tool_count", func() map[string]any {
			r := s2LiteWeatherRequest()
			r["parallel_tool_calls"] = false
			return r
		}, []string{h21ItemEvent("response.output_item.done", 0, native), h21ItemEvent("response.output_item.done", 1, second), h21Terminal("response.completed", native, second)}, 2},
		{"relay_parallel_false", "parallel_tool_count", func() map[string]any {
			r := weatherRequest()
			r["parallel_tool_calls"] = false
			return r
		}, []string{h21ItemEvent("response.output_item.done", 0, relayA), h21ItemEvent("response.output_item.done", 1, relayB), h21Terminal("response.completed", relayA, relayB)}, 2},
		{"required_text_only", "required_tool_missing", func() map[string]any {
			r := weatherRequest()
			r["tool_choice"] = "required"
			return r
		}, []string{h21Terminal("response.completed", s2Message("msg_p", "text"))}, 0},
		{"delta_identity", "native_delta_identity_mismatch", s2LiteWeatherRequest,
			[]string{h21ItemEvent("response.output_item.added", 0, nativeAdded), wrongDelta, h21ItemEvent("response.output_item.done", 0, native), h21Terminal("response.completed", native)}, 1},
		{"terminal_args_changed", "terminal_args_changed", s2LiteWeatherRequest,
			[]string{h21ItemEvent("response.output_item.done", 0, native), h21Terminal("response.completed", changed)}, 1},
		{"terminal_missing", "terminal_item_missing", s2LiteWeatherRequest,
			[]string{h21ItemEvent("response.output_item.done", 0, native), h21Terminal("response.completed")}, 1},
		{"native_invalid_json", "terminal_args_changed", s2LiteWeatherRequest,
			[]string{h21ItemEvent("response.output_item.done", 0, invalidJSON), h21Terminal("response.completed", invalidJSON)}, 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			before := relayCounterSnapshot()["tool_policy_logged:"+c.reason]
			run := s2RunStream(t, s2HostScript{}, c.request(), c.chunks)
			if run.closeErr != "" || s2DoneCount(run.joined()) != c.calls || !strings.Contains(run.joined(), "event: response.completed") {
				t.Fatalf("default delivered differently from h22: %q", run.closeErr)
			}
			logs := s2PolicyLogs(run.logs)
			found := false
			for _, fields := range logs {
				if fields["reason"] == c.reason && fields["tool_policy_checks"] == toolPolicyChecksLog && fields["enforced"] == false {
					found = true
				}
			}
			if !found {
				t.Fatalf("finding %s not logged: %v", c.reason, logs)
			}
			for _, entry := range run.logs {
				text := string(jsonBytes(entry))
				for _, private := range []string{"get_weather", "Paris", "Oslo", "Rome", "fc_WRONG", "call_policy"} {
					if strings.Contains(text, private) {
						t.Fatalf("log carried %q: %s", private, text)
					}
				}
				if entry["host_callback_id"] != "s2-callback" {
					t.Fatal("log lost its request context")
				}
			}
			if relayCounterSnapshot()["tool_policy_logged:"+c.reason] <= before {
				t.Fatal("finding not counted")
			}
			// tool_policy_checks: enforce rejects the same finding when no
			// complete call was delivered before it.
			enforced := s2RunStream(t, s2HostScript{configure: s2Enforce}, c.request(), c.chunks)
			delivered := c.reason == "terminal_args_changed" || c.reason == "terminal_item_missing" || c.name == "native_parallel_false" || c.name == "relay_parallel_false"
			if delivered {
				// The finding came after a delivered call: logged, never fatal.
				if enforced.closeErr != "" || s2DoneCount(enforced.joined()) != c.calls {
					t.Fatalf("enforce failed a stream after delivering a call: %q", enforced.closeErr)
				}
				after := false
				for _, fields := range s2PolicyLogs(enforced.logs) {
					after = after || (fields["reason"] == c.reason && fields["tool_policy_checks"] == "enforce_after_delivery")
				}
				if !after {
					t.Fatal("finding after delivery not logged")
				}
				return
			}
			if !strings.Contains(enforced.closeErr, "reason="+c.reason) || s2DoneCount(enforced.joined()) != 0 {
				t.Fatalf("enforce did not reject before delivery: %q", enforced.closeErr)
			}
		})
	}
}

// Checks h22 already made are not policy checks: they still reject in log mode.
func TestH25S2H22ChecksStillReject(t *testing.T) {
	native := s2NativeCall("dup", `{"city":"Paris"}`)
	nativeAdded := cloneObject(native)
	nativeAdded["status"], nativeAdded["arguments"] = "in_progress", ""
	otherID := cloneObject(native)
	otherID["id"] = "fc_other"
	for name, chunks := range map[string][]string{
		"native_done_twice":        {h21ItemEvent("response.output_item.done", 0, native), h21ItemEvent("response.output_item.done", 0, native)},
		"native_done_vs_announced": {h21ItemEvent("response.output_item.added", 0, nativeAdded), h21ItemEvent("response.output_item.done", 0, otherID)},
		"undeclared_relay_tool":    {h21ItemEvent("response.output_item.done", 0, h21TransportItem("x", "NOT_DECLARED", map[string]any{}))},
	} {
		t.Run(name, func(t *testing.T) {
			run := s2RunStream(t, s2HostScript{}, s2LiteWeatherRequest(), chunks)
			if !strings.Contains(run.closeErr, "invalid_tool_call") {
				t.Fatalf("h22 rejected this too: %q", run.closeErr)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// S2.3 relay_tool_delivery

func TestH25S2ImmediateDeliveryOnDone(t *testing.T) {
	paris := h21TransportItem("immediate", "get_weather", map[string]any{"city": "Paris"})
	chunks := []string{s2Created(), h21ItemEvent("response.output_item.done", 0, paris), sseEvent("response.output_text.delta", map[string]any{"type": "response.output_text.delta", "output_index": 1, "delta": "after"}), h21Terminal("response.completed", paris)}
	run := s2RunStream(t, s2HostScript{}, weatherRequest(), chunks)
	if run.closeErr != "" || s2DoneCount(run.joined()) != 1 {
		t.Fatal(run.closeErr)
	}
	// h22: the call reached the client while the upstream was still being
	// read, before the text that followed it and before the terminal.
	emitted, reads, emits := -1, 0, 0
	for _, entry := range run.trace {
		switch entry {
		case "read":
			reads++
		case "emit":
			if emitted < 0 && strings.Contains(run.emits[emits], `"name":"get_weather"`) {
				emitted = reads
			}
			emits++
		}
	}
	if emitted != 2 {
		t.Fatalf("call not delivered right after its done was read (after read %d)", emitted)
	}
	batch := s2RunStream(t, s2HostScript{configure: s2Batch}, weatherRequest(), chunks)
	if batch.closeErr != "" || s2DoneCount(batch.joined()) != 1 || strings.Index(batch.joined(), `"name":"get_weather"`) < strings.Index(batch.joined(), "after") {
		t.Fatalf("batch did not hold the call until the terminal: %q", batch.closeErr)
	}
}

func TestH25S2BatchHeartbeat(t *testing.T) {
	interval, poll := relayHeartbeatInterval, relayHeartbeatPoll
	relayHeartbeatInterval, relayHeartbeatPoll = 40*time.Millisecond, 5*time.Millisecond
	defer func() { relayHeartbeatInterval, relayHeartbeatPoll = interval, poll }()
	paris := h21TransportItem("heartbeat", "get_weather", map[string]any{"city": "Paris"})
	added := cloneObject(paris)
	added["status"], added["arguments"] = "in_progress", ""
	chunks := []string{s2Created() + h21ItemEvent("response.output_item.added", 0, added), h21ItemEvent("response.output_item.done", 0, paris), h21Terminal("response.completed", paris)}
	before := relayCounterSnapshot()["batch_heartbeat"]
	batch := s2RunStream(t, s2HostScript{configure: s2Batch, readDelay: 200 * time.Millisecond}, weatherRequest(), chunks)
	if batch.closeErr != "" || s2DoneCount(batch.joined()) != 1 {
		t.Fatal(batch.closeErr)
	}
	comments := 0
	for index, emit := range batch.emits {
		if emit == relayHeartbeatFrame {
			comments++
			if index == len(batch.emits)-1 || strings.Contains(strings.Join(batch.emits[:index], ""), "data: [DONE]") {
				t.Fatal("heartbeat after the stream ended")
			}
		} else if strings.Contains(emit, ": keepalive") {
			t.Fatal("heartbeat inside an event frame")
		}
	}
	if comments < 2 || relayCounterSnapshot()["batch_heartbeat"]-before != uint64(comments) {
		t.Fatalf("held relay call produced %d heartbeats", comments)
	}
	if batch.trace[len(batch.trace)-1] != "close" {
		t.Fatal("a heartbeat followed host.stream.close")
	}
	// The comment is not an event: every client frame still parses, with
	// contiguous sequence numbers.
	for sequence, event := range h23Events(t, batch.joined()) {
		if value, ok := event["sequence_number"].(float64); !ok || int(value) != sequence {
			t.Fatal("heartbeat disturbed the event sequence")
		}
	}
	// immediate (default) never sends one.
	immediate := s2RunStream(t, s2HostScript{readDelay: 200 * time.Millisecond}, weatherRequest(), chunks)
	if immediate.closeErr != "" || strings.Contains(immediate.joined(), "keepalive") {
		t.Fatal("immediate delivery sent a heartbeat")
	}
	// Heartbeats are not output: a held call that is then truncated still
	// failed before any output, and is still worth a retry (E5).
	truncated := s2RunStream(t, s2HostScript{configure: s2Batch, readDelay: 200 * time.Millisecond}, weatherRequest(), chunks[:1])
	if !strings.Contains(truncated.joined(), relayHeartbeatFrame) || !strings.HasSuffix(truncated.closeErr, retryableErrorHint) ||
		!strings.Contains(truncated.closeErr, "reason=missing_terminal_event") {
		t.Fatalf("truncation after heartbeats: %q", truncated.closeErr)
	}
}

// One stream keeps the delivery mode it started with.
func TestH25S2DeliveryModeFrozenAtRequestEntry(t *testing.T) {
	paris := h21TransportItem("frozen", "get_weather", map[string]any{"city": "Paris"})
	chunks := []string{s2Created(), h21ItemEvent("response.output_item.done", 0, paris), sseEvent("response.output_text.delta", map[string]any{"type": "response.output_text.delta", "output_index": 1, "delta": "after"}), h21Terminal("response.completed", paris)}
	flip := func(service *Service, index int) {
		if index == 1 {
			service.mu.Lock()
			service.cfg.RelayToolDelivery = relayToolDeliveryBatch
			service.cfg.RelayIncomplete = relayIncompletePassthrough
			service.cfg.ToolPolicyChecks = toolPolicyChecksEnforce
			service.mu.Unlock()
		}
	}
	run := s2RunStream(t, s2HostScript{beforeRead: flip}, weatherRequest(), chunks)
	if run.closeErr != "" || strings.Index(run.joined(), `"name":"get_weather"`) > strings.Index(run.joined(), "after") {
		t.Fatalf("a reconfigure changed an in-flight stream: %q", run.closeErr)
	}
}

// ---------------------------------------------------------------------------
// S2.4 relay_incomplete

func TestH25S2RelayIncompleteReasons(t *testing.T) {
	paris := h21TransportItem("cut", "get_weather", map[string]any{"city": "Paris"})
	added := cloneObject(paris)
	added["status"], added["arguments"] = "in_progress", ""
	unfinished := cloneObject(paris)
	unfinished["status"] = "incomplete"
	for name, c := range map[string]struct {
		chunks []string
		reason string
	}{
		"eof_while_held":      {[]string{s2Created(), h21ItemEvent("response.output_item.added", 0, added)}, "missing_terminal_event"},
		"sentinel_while_held": {[]string{s2Created(), h21ItemEvent("response.output_item.added", 0, added), "data: [DONE]\n\n"}, "missing_terminal_event"},
		"unfinished_done":     {[]string{s2Created(), h21ItemEvent("response.output_item.done", 0, unfinished)}, "incomplete_item"},
		"unfinished_terminal": {[]string{s2Created(), s2Response("response.incomplete", map[string]any{"status": "incomplete", "output": []any{unfinished}})}, "incomplete_item"},
	} {
		t.Run(name, func(t *testing.T) {
			run := s2RunStream(t, s2HostScript{}, weatherRequest(), c.chunks)
			if !strings.HasPrefix(run.closeErr, "[relay_tool_error] invalid_tool_call [reason="+c.reason+" ref=bpdiag_") || !strings.HasSuffix(run.closeErr, retryableErrorHint) {
				t.Fatalf("truncated relay call: %q", run.closeErr)
			}
			// passthrough (rc2): an upstream truncation, or the upstream's
			// own incomplete terminal.
			rc2 := s2RunStream(t, s2HostScript{configure: s2Passthrough}, weatherRequest(), c.chunks)
			if strings.Contains(rc2.closeErr, "invalid_tool_call") {
				t.Fatalf("passthrough still reported invalid_tool_call: %q", rc2.closeErr)
			}
		})
	}
	// Non-streaming: the same reason for a response that ended unfinished.
	_, _, _, err := transformResponseBody(jsonBytes(map[string]any{"status": "incomplete", "output": []any{h21Native("cut", "get_weather", map[string]any{"city": "P"})}}), weatherRequest())
	if err != nil {
		t.Fatalf("finished call of an incomplete response: %v", err)
	}
	cut := h21Native("cut2", "get_weather", map[string]any{"city": "P"})
	cut["status"] = "incomplete"
	_, _, _, err = transformResponseBody(jsonBytes(map[string]any{"status": "incomplete", "output": []any{cut}}), weatherRequest())
	if err == nil || !strings.Contains(err.Error(), "reason=incomplete_item") {
		t.Fatalf("unfinished call of an incomplete response: %v", err)
	}
}

// ---------------------------------------------------------------------------
// S2.5 / E5 error wording

func TestH25S2RetryHintOnlyBeforeOutput(t *testing.T) {
	good := h21TransportItem("hint_good", "get_weather", map[string]any{"city": "Paris"})
	bad := h21TransportItem("hint_bad", "NOT_DECLARED", map[string]any{})
	text := sseEvent("response.output_text.delta", map[string]any{"type": "response.output_text.delta", "output_index": 0, "delta": "visible"})
	reasoning := sseEvent("response.reasoning_summary_text.delta", map[string]any{"type": "response.reasoning_summary_text.delta", "output_index": 0, "delta": "thinking"})
	native := s2NativeCall("hint_native", `{"city":"Paris"}`)
	for name, c := range map[string]struct {
		chunks []string
		hint   bool
		calls  int
	}{
		"created_then_bad":           {[]string{s2Created(), h21ItemEvent("response.output_item.done", 1, bad)}, true, 0},
		"created_and_bad_one_chunk":  {[]string{s2Created() + h21ItemEvent("response.output_item.done", 1, bad)}, true, 0},
		"text_then_bad":              {[]string{text, h21ItemEvent("response.output_item.done", 1, bad)}, false, 0},
		"reasoning_then_bad":         {[]string{reasoning, h21ItemEvent("response.output_item.done", 1, bad)}, false, 0},
		"relay_call_then_bad":        {[]string{h21ItemEvent("response.output_item.done", 0, good), h21ItemEvent("response.output_item.done", 1, bad)}, false, 1},
		"native_call_then_bad":       {[]string{h21ItemEvent("response.output_item.done", 0, native), h21ItemEvent("response.output_item.done", 1, bad)}, false, 1},
		"relay_and_bad_one_chunk":    {[]string{h21ItemEvent("response.output_item.done", 0, good) + h21ItemEvent("response.output_item.done", 1, bad)}, true, 0},
		"created_then_eof":           {[]string{s2Created()}, true, 0},
		"nothing_then_eof":           {nil, true, 0},
		"text_then_eof":              {[]string{text}, false, 0},
		"created_then_upstream_fail": {[]string{s2Created(), s2Response("response.failed", map[string]any{"status": "failed", "error": map[string]any{"code": "server_error"}, "output": []any{}})}, false, 0},
	} {
		t.Run(name, func(t *testing.T) {
			run := s2RunStream(t, s2HostScript{}, weatherRequest(), c.chunks)
			if strings.HasSuffix(run.closeErr, retryableErrorHint) != c.hint || s2DoneCount(run.joined()) != c.calls {
				t.Fatalf("close %q, %d calls", run.closeErr, s2DoneCount(run.joined()))
			}
			if c.hint {
				if !strings.HasPrefix(run.closeErr, "[relay_tool_error] ") && !strings.HasPrefix(run.closeErr, "[upstream_error] ") {
					t.Fatalf("unexpected prefix: %q", run.closeErr)
				}
				off := s2RunStream(t, s2HostScript{configure: func(cfg *Config) { cfg.RetryableErrorHint = false }}, weatherRequest(), c.chunks)
				if off.closeErr == "" || strings.Contains(off.closeErr, retryableErrorHint) || strings.TrimSuffix(run.closeErr, retryableErrorHint)[:20] != off.closeErr[:20] {
					t.Fatalf("retryable_error_hint: false still hinted: %q", off.closeErr)
				}
			}
		})
	}
	// A client that hung up is never asked to retry.
	gone := s2RunStream(t, s2HostScript{failEmit: func(string) bool { return true }}, weatherRequest(), []string{s2Created()})
	if !strings.HasPrefix(gone.closeErr, "[client_disconnected] ") || strings.Contains(gone.closeErr, retryableErrorHint) {
		t.Fatalf("client hang-up: %q", gone.closeErr)
	}
	// A plugin panic before output is retried elsewhere.
	if message := streamCloseMessage(fail(500, "stream_panic", "basispoints stream aborted: boom"), defaultRelayOptions(), true); message != "[plugin_bug] basispoints stream aborted: boom"+retryableErrorHint {
		t.Fatalf("panic message %q", message)
	}
}

func TestH25S2ToolFailurePrefix(t *testing.T) {
	err := withToolDiagnostic(toolReject("schema_mismatch"), "response.output_item.done", 0, false)
	message := streamCloseMessage(err, defaultRelayOptions(), false)
	if !strings.HasPrefix(message, "[relay_tool_error] invalid_tool_call [reason=schema_mismatch ref=bpdiag_") || strings.Contains(message, "request_error") {
		t.Fatalf("tool failure wording: %q", message)
	}
	if streamCloseReason(fail(413, "x", "y")) != "[request_error] " || streamCloseReason(fail(400, "x", "y")) != "[request_error] " {
		t.Fatal("genuine request errors lost their label")
	}
}

// --- sub2api v0.2.14 (tag v0.2.14, commit 0363b8c), backend/internal/service.
// The predicates that read a plugin failure once CPA fc7 has wrapped it: for
// a non-Codex client {"type":"error","error":{"code":"internal_server_error",
// "message":MSG,"param":null,"type":"server_error"}}, for Codex a
// response.failed whose response.error is the same object. Ported in what they
// read from such a payload (code and type are fixed, so only MSG varies).

// openai_gateway_upstream_errors.go:196-243 (match)
func s2SubContextWindow(text string) bool {
	lower := strings.ToLower(strings.TrimSpace(text))
	if lower == "" {
		return false
	}
	if strings.Contains(lower, "context_too_large") || strings.Contains(lower, "context_length_exceeded") ||
		strings.Contains(lower, "maximum context length") || strings.Contains(lower, "max context length") {
		return true
	}
	exceeded := strings.Contains(lower, "exceed") || strings.Contains(lower, "too large") || strings.Contains(lower, "too long")
	return (strings.Contains(lower, "context window") && exceeded) || (strings.Contains(lower, "context length") && exceeded) ||
		(strings.Contains(lower, "token limit") && strings.Contains(lower, "context") && exceeded)
}

// openai_gateway_upstream_errors.go:182-187
func s2SubCapacityShed(text string) bool {
	lower := strings.ToLower(strings.TrimSpace(text))
	return strings.Contains(lower, "server is overloaded") || strings.Contains(lower, "servers are overloaded") || strings.Contains(lower, "servers are currently overloaded")
}

// openai_gateway_upstream_errors.go:119-180 for a 400 (capacity, then match)
func s2SubTransient400(text string) bool {
	if s2SubCapacityShed(text) {
		return true
	}
	lower := strings.ToLower(strings.TrimSpace(text))
	return strings.Contains(lower, "an error occurred while processing your request") || strings.Contains(lower, "selected model is at capacity") ||
		(strings.Contains(lower, "you can retry your request") && strings.Contains(lower, "help.openai.com") && strings.Contains(lower, "request id"))
}

const (
	s2SubCode = "internal_server_error"
	s2SubType = "server_error"
)

// openai_gateway_passthrough.go:1390-1423
func s2SubSemanticStatus(message string) int {
	if s2SubContextWindow(message) {
		return 400
	}
	combined := strings.TrimSpace(s2SubType + " " + s2SubCode + " " + strings.ToLower(strings.TrimSpace(message)))
	switch {
	case strings.Contains(combined, "rate_limit"):
		return 429
	case strings.Contains(s2SubType, "invalid_request"):
		return 400
	case strings.Contains(combined, "authentication") || strings.Contains(combined, "unauthorized") || strings.Contains(combined, "invalid_api_key"):
		return 401
	case strings.Contains(combined, "permission") || strings.Contains(combined, "forbidden") || strings.Contains(combined, "access denied"):
		return 403
	case s2SubCapacityShed(message):
		return 503
	}
	return 502
}

// openai_gateway_passthrough.go:1547-1597 (response.failed, Codex clients)
func s2SubFailedEventFailover(message string) bool {
	if s2SubContextWindow(message) {
		return false
	}
	switch s2SubSemanticStatus(message) {
	case 403:
		return false // only for an account failure, which needs a structured code
	case 429:
		return true
	}
	if s2SubTransient400(message) {
		return true
	}
	combined := strings.ToLower(strings.TrimSpace(message + " " + s2SubCode + " " + s2SubType))
	for _, marker := range []string{"invalid_request", "content_policy", "policy", "safety", "high-risk cyber", "not allowed", "violat"} {
		if strings.Contains(combined, marker) {
			return false
		}
	}
	return true
}

// openai_gateway_passthrough.go:1599-1624 (bare error event, other clients)
func s2SubErrorEventFailover(message string) bool {
	if s2SubContextWindow(message) {
		return false
	}
	switch s2SubSemanticStatus(message) {
	case 403:
		return false
	case 401, 429, 529:
		return true
	}
	if s2SubTransient400(message) {
		return true
	}
	combined := strings.ToLower(strings.TrimSpace(message + " " + message))
	return strings.Contains(combined, "temporary") || strings.Contains(combined, "try again") || strings.Contains(combined, "please retry")
}

// s2PluginFailureLiterals: every fail(status, kind, "message") literal in the
// plugin's own source, as (status, message) -- the message up to its first
// concatenation, which is the fixed part.
func s2PluginFailureLiterals(t *testing.T) [][2]string {
	t.Helper()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	pattern := regexp.MustCompile(`fail\((\d{3}),\s*"[a-z_]+",\s*"((?:[^"\\]|\\.)*)"`)
	var literals [][2]string
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		for _, match := range pattern.FindAllStringSubmatch(string(raw), -1) {
			message, err := strconv.Unquote(`"` + match[2] + `"`)
			if err != nil {
				t.Fatal(file, err)
			}
			literals = append(literals, [2]string{match[1], message})
		}
	}
	if len(literals) < 50 {
		t.Fatalf("found only %d fail() literals; the scan is broken", len(literals))
	}
	return literals
}

// Every message the plugin can close a stream with, through the main site's
// own keyword rules: never a 401/403/429/529/503 or context-window verdict
// (no account side effect, account.go:1181 / passthrough.go:1626-1660), the
// Codex retry of h22 kept, and the error-event retry exactly where the E5 hint
// is. CPA's routing markers never appear either.
func TestH25S2CloseMessagesAgainstMainSiteKeywords(t *testing.T) {
	var errs []error
	for reason := range toolRejectReasons {
		errs = append(errs, newToolRejection(reason, false), newToolRejection(reason, true))
		errs = append(errs, withToolDiagnostic(newToolRejection(reason, false), "response.output_item.done", 3, true))
	}
	for _, literal := range s2PluginFailureLiterals(t) {
		status, _ := strconv.Atoi(literal[0])
		errs = append(errs, fail(status, "literal", literal[1]))
	}
	errs = append(errs, timeoutError(defaultConfig()), fail(500, "stream_panic", "basispoints stream aborted: runtime error: index out of range [3] with length 3"),
		errors.New("plain host error"))
	checked := 0
	for _, err := range errs {
		for _, zeroOutput := range []bool{false, true} {
			for _, hint := range []bool{false, true} {
				options := defaultRelayOptions()
				options.RetryableErrorHint = hint
				message := streamCloseMessage(err, options, zeroOutput)
				hinted := strings.HasSuffix(message, retryableErrorHint)
				if hinted != (hint && zeroOutput && retryableFailure(err)) {
					t.Fatalf("hint %v on %q", hinted, message)
				}
				if status := s2SubSemanticStatus(message); status != 502 {
					t.Fatalf("main site reads %q as %d", message, status)
				}
				if s2SubContextWindow(message) || s2SubCapacityShed(message) || s2SubTransient400(message) {
					t.Fatalf("main site misreads %q", message)
				}
				if !s2SubFailedEventFailover(message) {
					t.Fatalf("a Codex client would no longer be retried for %q", message)
				}
				if s2SubErrorEventFailover(message) != hinted {
					t.Fatalf("error-event retry %v for %q", !hinted, message)
				}
				for _, marker := range []string{"basispoints_concurrency_limit", "basispoints_model_rerouted"} {
					if strings.Contains(message, marker) {
						t.Fatalf("CPA routing marker in %q", message)
					}
				}
				if json.Valid([]byte(strings.TrimSpace(message))) {
					t.Fatalf("CPA would parse %q as a JSON error body", message)
				}
				checked++
			}
		}
	}
	// The hint is exactly the error-event retry words and nothing else the
	// main site reads.
	if !strings.Contains(retryableErrorHint, "temporary") || !strings.Contains(retryableErrorHint, "please retry") || s2SubSemanticStatus(retryableErrorHint) != 502 {
		t.Fatal("hint wording")
	}
	t.Logf("checked %d close messages", checked)
}

// ---------------------------------------------------------------------------

func TestH25S2DiagnosticsReportDeliveryMode(t *testing.T) {
	err := withToolDiagnostic(toolReject("schema_mismatch"), "response.output_item.done", 0, false)
	for _, mode := range []string{relayToolDeliveryImmediate, relayToolDeliveryBatch} {
		fields, _ := toolDiagnosticFields(err, s2Source(weatherRequest(), func(cfg *Config) { cfg.RelayToolDelivery = mode }))
		if fields["relay_tool_delivery"] != mode {
			t.Fatalf("diagnostics say %v, want %s", fields["relay_tool_delivery"], mode)
		}
	}
	if fields, _ := toolDiagnosticFields(err, weatherRequest()); fields["relay_tool_delivery"] != relayToolDeliveryImmediate {
		t.Fatal("unprepared source should report the default")
	}
}
