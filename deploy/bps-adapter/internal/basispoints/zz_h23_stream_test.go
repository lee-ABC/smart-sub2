package basispoints

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func h23NumericSource() map[string]any {
	return map[string]any{"input": []any{messageItem("user", "h23-stream-numbers")}, "tools": []any{
		map[string]any{"type": "function", "name": "lookup", "parameters": map[string]any{"type": "object"}},
	}}
}

func h23TerminalResponse(kind string, response map[string]any) string {
	return sseEvent(kind, map[string]any{"type": kind, "response": response})
}

func h23Events(t *testing.T, text string) []map[string]any {
	t.Helper()
	var result []map[string]any
	decoder := newSSEDecoder()
	if err := decoder.feed([]byte(text), func(_ string, data string) error {
		if strings.TrimSpace(data) == "[DONE]" {
			return nil
		}
		var value map[string]any
		if err := json.Unmarshal([]byte(data), &value); err != nil {
			return err
		}
		result = append(result, value)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return result
}

func TestH23StreamEquivalentFunctionArguments(t *testing.T) {
	for _, values := range [][2]string{
		{`{"n":1e3,"nested":[true,{"v":-0}]}`, `{"nested":[true,{"v":0.0}],"n":1000}`},
		{`{"n":9007199254740993}`, `{"n":9007199254740993.0}`},
	} {
		first := h21TransportItem("numeric", "lookup", parseArguments(values[0]))
		last := h21TransportItem("numeric", "lookup", parseArguments(values[1]))
		result := runStreamTest(t, h23NumericSource(), []string{
			h21ItemEvent("response.output_item.done", 0, first), h21Terminal("response.completed", last),
		})
		if result.closeErr != "" || strings.Count(result.joined(), "event: response.output_item.done") != 1 {
			t.Fatalf("equivalent function arguments rejected or re-executed: %q", result.closeErr)
		}
		var deliveredArguments string
		for _, event := range h23Events(t, result.joined()) {
			if event["type"] == "response.output_item.done" {
				deliveredArguments = stringValue(objectValue(event["item"])["arguments"])
			}
			if event["type"] == "response.completed" {
				terminal := objectValue(objectValue(event["response"])["output"].([]any)[0])
				if terminal["arguments"] != deliveredArguments {
					t.Fatal("equivalent terminal arguments changed the already delivered spelling")
				}
			}
		}
	}
}

func TestH23StreamUnverifiedArgumentsAreNotCalledChanged(t *testing.T) {
	large := `{"n":1,"text":"` + strings.Repeat("a", toolJSONMaxBytes) + `"}`
	first := map[string]any{"type": "function_call", "id": "fc_bounded", "call_id": "call_bounded", "name": "lookup", "arguments": large}
	last := cloneObject(first)
	last["arguments"] = strings.Replace(large, `"n":1`, `"n":1.0`, 1)
	if reason := streamToolDifference(first, last); reason != "terminal_args_unverified" {
		t.Fatalf("unknown semantic equality misclassified: %q", reason)
	}
}

func TestH23StreamRejectsActualArgumentChanges(t *testing.T) {
	for _, changed := range []string{`{"n":1001}`, `{"n":"1000"}`, `{"n":null}`, `{"n":9007199254740992}`} {
		original := `{"n":1000}`
		if strings.Contains(changed, "9007199254740992") {
			original = `{"n":9007199254740993}`
		}
		first := h21TransportItem("change", "lookup", parseArguments(original))
		last := h21TransportItem("change", "lookup", parseArguments(changed))
		for _, mode := range s2Modes() {
			result := runStreamTestWith(t, mode.configure, h23NumericSource(), []string{
				h21ItemEvent("response.output_item.done", 0, first), h21Terminal("response.completed", last),
			})
			h21AssertToolFailure(t, result)
			// Default (= h22): the call went out on its own done; the changed
			// terminal still fails the stream. batch (rc2): nothing went out.
			wantCalls := 1
			if mode.rc2 {
				wantCalls = 0
			}
			if strings.Count(result.joined(), "event: response.output_item.done") != wantCalls {
				t.Fatal("changed terminal released a pending relay tool, or a delivered call was replayed")
			}
		}
	}
}

func TestH23StreamUnsuccessfulTerminalPreservesCause(t *testing.T) {
	for _, kind := range []string{"response.failed", "response.incomplete"} {
		for _, outputPresent := range []bool{false, true} {
			first := h21TransportItem("before_failure", "get_weather", map[string]any{"city": "Paris"})
			response := map[string]any{"status": strings.TrimPrefix(kind, "response."),
				"error":              map[string]any{"code": "server_error", "message": "synthetic upstream failure"},
				"incomplete_details": map[string]any{"reason": "max_output_tokens"},
			}
			if outputPresent {
				response["output"] = []any{}
			}
			// Default (= h22): the call went out on its own done, and a
			// terminal that no longer lists it is invalid_tool_call.
			h22 := runStreamTest(t, weatherRequest(), []string{
				h21ItemEvent("response.output_item.done", 0, first), h23TerminalResponse(kind, response),
			})
			h21AssertToolFailure(t, h22)
			if strings.Count(h22.joined(), "event: response.output_item.done") != 1 {
				t.Fatal("delivered call lost or replayed")
			}
			// relay_incomplete: passthrough + batch delivery (rc2).
			result := runStreamTestWith(t, s2RC2, weatherRequest(), []string{
				h21ItemEvent("response.output_item.done", 0, first), h23TerminalResponse(kind, response),
			})
			if result.closeErr != "" || !strings.Contains(result.joined(), "event: "+kind) ||
				strings.Contains(result.joined(), "invalid_tool_call") || strings.Contains(result.joined(), "event: response.completed") {
				t.Fatalf("upstream failure was replaced: %q", result.closeErr)
			}
			if strings.Count(result.joined(), "event: response.output_item.done") != 0 || !strings.Contains(result.joined(), "synthetic upstream failure") || !strings.Contains(result.joined(), "max_output_tokens") {
				t.Fatal("failure cause lost or a pending relay tool was released")
			}
		}
	}
}

func TestH23StreamFailureDoesNotDeliverOrCachePendingTransport(t *testing.T) {
	for _, kind := range []string{"response.failed", "response.incomplete"} {
		source := weatherRequest()
		source["input"] = []any{messageItem("user", "h23-unsuccessful-"+kind)}
		first := h21TransportItem("committed", "get_weather", map[string]any{"city": "Paris"})
		changed := h21TransportItem("committed", "get_weather", map[string]any{"city": "Oslo"})
		pending := h21TransportItem("pending", "NOT_FOR_THE_CALLER", "NOT_FOR_THE_CALLER")
		pending["status"] = "incomplete"
		chunks := []string{
			h21ItemEvent("response.output_item.done", 0, first),
			h21ItemEvent("response.output_item.added", 1, pending),
			h23TerminalResponse(kind, map[string]any{"status": strings.TrimPrefix(kind, "response."),
				"output": []any{changed, pending}, "error": map[string]any{"code": "server_error"}}),
		}
		// Default (= h22): the committed call went out on its own done and is
		// replay history; the truncated pending one makes the terminal
		// invalid_tool_call and is neither delivered nor cached.
		t.Run("default/"+kind, func(t *testing.T) {
			h22Source := weatherRequest()
			h22Source["input"] = []any{messageItem("user", "h23-unsuccessful-default-"+kind)}
			h22 := runStreamTest(t, h22Source, chunks)
			h21AssertToolFailure(t, h22)
			if strings.Count(h22.joined(), "event: response.output_item.done") != 1 || strings.Contains(h22.joined(), "Oslo") {
				t.Fatal("delivered call lost/replayed or the changed terminal leaked")
			}
			if rememberedNativeCall(streamTestReplayScope(t, h22Source), "call_committed") == nil || rememberedNativeCall(streamTestReplayScope(t, h22Source), "call_pending") != nil {
				t.Fatal("delivered call not cached, or the truncated pending call was cached")
			}
		})
		// relay_incomplete: passthrough + batch delivery (rc2).
		result := runStreamTestWith(t, s2RC2, source, chunks)
		if result.closeErr != "" || strings.Contains(result.joined(), "run_officejs") || strings.Contains(result.joined(), "NOT_FOR_THE_CALLER") || strings.Contains(result.joined(), "Oslo") {
			t.Fatalf("failed response leaked or reclassified pending transport: %q", result.closeErr)
		}
		if strings.Count(result.joined(), "event: response.output_item.done") != 0 {
			t.Fatal("failure delivered a pending relay tool")
		}
		if rememberedNativeCall(streamTestReplayScope(t, source), "call_committed") != nil || rememberedNativeCall(streamTestReplayScope(t, source), "call_pending") != nil {
			t.Fatal("failure cached an undelivered pending batch")
		}
	}
}

func TestH23StreamTruncatedItemWaitsForActualFailureTerminal(t *testing.T) {
	for _, kind := range []string{"response.failed", "response.incomplete"} {
		item := h21TransportItem("truncated", "get_weather", map[string]any{"city": "Paris"})
		item["status"] = "incomplete"
		// Default (= h22): a transport call done with an unfinished status is
		// invalid_tool_call on the spot (reason incomplete_item).
		h22 := runStreamTest(t, weatherRequest(), []string{
			h21ItemEvent("response.output_item.done", 0, item),
			h23TerminalResponse(kind, map[string]any{"status": strings.TrimPrefix(kind, "response."),
				"output": []any{item}, "incomplete_details": map[string]any{"reason": "max_output_tokens"}}),
		})
		h21AssertToolFailure(t, h22)
		if !strings.Contains(h22.closeErr, "reason=incomplete_item") || strings.Contains(h22.joined(), "event: response.output_item.done") {
			t.Fatalf("truncated item lost its cause or became executable: %q", h22.closeErr)
		}
		// relay_incomplete: passthrough (rc2).
		result := runStreamTestWith(t, s2RC2, weatherRequest(), []string{
			h21ItemEvent("response.output_item.done", 0, item),
			h23TerminalResponse(kind, map[string]any{"status": strings.TrimPrefix(kind, "response."),
				"output": []any{item}, "incomplete_details": map[string]any{"reason": "max_output_tokens"}}),
		})
		if result.closeErr != "" || !strings.Contains(result.joined(), "event: "+kind) || !strings.Contains(result.joined(), "max_output_tokens") || strings.Contains(result.joined(), "run_officejs") || strings.Contains(result.joined(), "event: response.output_item.done") {
			t.Fatalf("truncated item obscured its actual terminal cause or became executable: %q", result.closeErr)
		}
		// A contradictory later success must not turn that unfinished item
		// into an executable call, even if its snapshot now says completed.
		finished := cloneObject(item)
		finished["status"] = "completed"
		for _, mode := range s2Modes() {
			contradiction := runStreamTestWith(t, mode.configure, weatherRequest(), []string{
				h21ItemEvent("response.output_item.done", 0, item), h21Terminal("response.completed", finished),
			})
			h21AssertToolFailure(t, contradiction)
			repeatedDone := runStreamTestWith(t, mode.configure, weatherRequest(), []string{
				h21ItemEvent("response.output_item.done", 0, item), h21ItemEvent("response.output_item.done", 0, finished),
			})
			h21AssertToolFailure(t, repeatedDone)
		}
	}
}

func TestH23StreamCompletedRestoresOnlyMissingItemID(t *testing.T) {
	for _, custom := range []bool{false, true} {
		source := weatherRequest()
		tool, args := "get_weather", any(map[string]any{"city": "Paris"})
		if custom {
			tool, args = "functions.exec", "  raw input  "
			source["tools"] = []any{map[string]any{"type": "namespace", "name": "functions", "tools": []any{map[string]any{"type": "custom", "name": "exec"}}}}
		}
		first := h21TransportItem("missing_id", tool, args)
		last := cloneObject(first)
		delete(last, "id")
		result := runStreamTest(t, source, []string{
			h21ItemEvent("response.output_item.done", 0, first), h21Terminal("response.completed", last),
		})
		if result.closeErr != "" || strings.Count(result.joined(), "event: response.output_item.done") != 1 {
			t.Fatalf("missing optional id was rejected or replayed: %q", result.closeErr)
		}
		var delivered, terminal map[string]any
		for _, event := range h23Events(t, result.joined()) {
			if event["type"] == "response.output_item.done" {
				delivered = objectValue(event["item"])
			}
			if event["type"] == "response.completed" {
				terminal = objectValue(objectValue(event["response"])["output"].([]any)[0])
			}
		}
		if delivered == nil || terminal == nil || delivered["id"] != terminal["id"] {
			t.Fatal("terminal did not retain the already delivered item id")
		}
	}
}

func TestH23StreamCompletedRejectsIdentityChanges(t *testing.T) {
	for _, field := range []string{"id", "call_id", "name"} {
		first := h21TransportItem("identity", "get_weather", map[string]any{"city": "Paris"})
		last := cloneObject(first)
		last[field] = "different"
		result := runStreamTest(t, weatherRequest(), []string{
			h21ItemEvent("response.output_item.done", 0, first), h21Terminal("response.completed", last),
		})
		h21AssertToolFailure(t, result)
	}
}

func TestH23StreamMissingIDWithAnnouncedTransportKeepsOriginalCache(t *testing.T) {
	source := weatherRequest()
	source["input"] = []any{messageItem("user", "h23-announced-optional-id")}
	first := h21TransportItem("announced_id", "get_weather", map[string]any{"city": "Paris"})
	added := cloneObject(first)
	added["status"], added["arguments"] = "in_progress", ""
	last := cloneObject(first)
	delete(last, "id")
	result := runStreamTest(t, source, []string{
		h21ItemEvent("response.output_item.added", 0, added),
		h21ItemEvent("response.output_item.done", 0, first), h21Terminal("response.completed", last),
	})
	if result.closeErr != "" || strings.Count(result.joined(), "event: response.output_item.done") != 1 {
		t.Fatalf("announced optional-id call rejected or replayed: %q", result.closeErr)
	}
	if string(jsonBytes(rememberedNativeCall(streamTestReplayScope(t, source), "call_announced_id"))) != string(jsonBytes(first)) {
		t.Fatal("restoring terminal id changed the original replay cache")
	}
}

func TestH23StreamMissingIDDoesNotHideChangedCallOrCollision(t *testing.T) {
	first := h21TransportItem("stable", "get_weather", map[string]any{"city": "Paris"})
	for _, mutation := range []string{"call_id_missing", "call_id_changed", "arguments_changed", "duplicate_call", "different_id"} {
		last := cloneObject(first)
		delete(last, "id")
		items := []any{last}
		switch mutation {
		case "call_id_missing":
			delete(last, "call_id")
		case "call_id_changed":
			last["call_id"] = "different_call"
		case "arguments_changed":
			changed := h21TransportItem("stable", "get_weather", map[string]any{"city": "Oslo"})
			last["arguments"] = changed["arguments"]
		case "duplicate_call":
			items = append(items, cloneObject(last))
		case "different_id":
			last["id"] = "fc_different_valid_id"
		}
		result := runStreamTest(t, weatherRequest(), []string{
			h21ItemEvent("response.output_item.done", 0, first), h21Terminal("response.completed", items...),
		})
		h21AssertToolFailure(t, result)
	}
}

func TestH23StreamCustomIdentityCannotAliasItsOriginalTransportID(t *testing.T) {
	source := map[string]any{"input": []any{messageItem("user", "custom-id-alias")}, "tools": []any{
		map[string]any{"type": "custom", "name": "patch"},
	}}
	first := h21TransportItem("alias", "patch", "raw")
	last := cloneObject(first)
	// Both ids would translate to ctc_alias. The original upstream identity
	// nevertheless changed, so custom id conversion must not conceal it.
	last["id"] = "ctc_alias"
	chunks := []string{h21ItemEvent("response.output_item.done", 0, first), h21Terminal("response.completed", last)}
	// tool_policy_checks: enforce (rc2) rejects it -- here before anything was
	// delivered, since batch delivery holds the call.
	result := runStreamTestWith(t, s2RC2, source, chunks)
	h21AssertToolFailure(t, result)
	// Default (= h22): h22 compared only the translated ids, which agree, and
	// delivered the call; the changed transport id is logged, not fatal.
	h22 := runStreamTest(t, source, chunks)
	if h22.closeErr != "" || strings.Count(h22.joined(), "event: response.output_item.done") != 1 {
		t.Fatalf("h22 delivered this call: %q", h22.closeErr)
	}
}

func TestH23StreamNativeToolsKeepTheirCompletedIdentity(t *testing.T) {
	first := map[string]any{"type": "function_call", "id": "fc_native", "call_id": "native_call",
		"name": "get_weather", "namespace": "native", "arguments": `{"city":"Paris"}`}
	last := cloneObject(first)
	delete(last, "id")
	result := runStreamTest(t, weatherRequest(), []string{
		h21ItemEvent("response.output_item.done", 0, first), h21Terminal("response.completed", last),
	})
	if result.closeErr != "" || strings.Count(result.joined(), "event: response.output_item.done") != 1 {
		t.Fatalf("native call was rejected or replayed: %q", result.closeErr)
	}
	for _, event := range h23Events(t, result.joined()) {
		if event["type"] == "response.completed" {
			terminal := objectValue(objectValue(event["response"])["output"].([]any)[0])
			if terminal["id"] != first["id"] || terminal["name"] != first["name"] || terminal["namespace"] != first["namespace"] {
				t.Fatal("native terminal identity was changed")
			}
		}
	}
}

func TestH23StreamSuccessfulTerminalRejectsMissingOutput(t *testing.T) {
	first := h21TransportItem("missing_output", "get_weather", map[string]any{"city": "Paris"})
	result := runStreamTest(t, weatherRequest(), []string{
		h21ItemEvent("response.output_item.done", 0, first),
		h23TerminalResponse("response.completed", map[string]any{"status": "completed"}),
	})
	h21AssertToolFailure(t, result)
}

func TestH23StreamMissingTerminalObjectIsProtocolFailure(t *testing.T) {
	first := h21TransportItem("missing_response", "get_weather", map[string]any{"city": "Paris"})
	for _, kind := range []string{"response.completed", "response.failed", "response.incomplete"} {
		chunks := []string{h21ItemEvent("response.output_item.done", 0, first), sseEvent(kind, map[string]any{"type": kind})}
		// relay_incomplete: passthrough (rc2): a protocol failure.
		result := runStreamTestWith(t, s2RC2, weatherRequest(), chunks)
		if result.closeErr == "" || strings.Contains(result.closeErr, "invalid_tool_call") || strings.Contains(result.joined(), "data: [DONE]") {
			t.Fatalf("missing terminal object not classified as protocol failure: %q", result.closeErr)
		}
		// Default (= h22): a delivered relay call cannot be checked against
		// it, which h22 reported as invalid_tool_call.
		h21AssertToolFailure(t, runStreamTest(t, weatherRequest(), chunks))
		// Default (= h22): with no relay call, h22 ended the stream with it.
		plain := runStreamTest(t, weatherRequest(), []string{sseEvent(kind, map[string]any{"type": kind})})
		if plain.closeErr != "" || !strings.Contains(plain.joined(), "event: "+kind) || !strings.Contains(plain.joined(), "data: [DONE]") {
			t.Fatalf("h22 passed a bare terminal through: %q", plain.closeErr)
		}
	}
}

func TestH23StreamRejectsMalformedEventTail(t *testing.T) {
	valid := h21ItemEvent("response.output_item.done", 0, h21TransportItem("tail", "get_weather", map[string]any{"city": "Paris"}))
	malformed := strings.TrimSuffix(valid, "\n\n") + " {}\n\n"
	// relay_incomplete: passthrough (rc2) reads the stream strictly.
	result := runStreamTestWith(t, s2RC2, weatherRequest(), []string{malformed})
	if result.closeErr == "" || strings.Contains(result.joined(), "event: response.output_item.done") {
		t.Fatal("stream parser accepted trailing data after the JSON event")
	}
	// Default (= h22): the first JSON value is the event; this stream then
	// ends without a terminal, which is still a failure.
	h22 := runStreamTest(t, weatherRequest(), []string{malformed})
	if h22.closeErr == "" || strings.Count(h22.joined(), "event: response.output_item.done") != 1 {
		t.Fatalf("h22 read the event's first JSON value: %q", h22.closeErr)
	}
}

func TestH23StreamTruncatedTransportIsAnUpstreamError(t *testing.T) {
	for _, sentinel := range []bool{false, true} {
		// Default (= h22): invalid_tool_call, with the actual cause.
		h22 := newStreamRelay(NewService(), "client", weatherRequest(), usageTarget{}, "gpt-6-astra")
		if err := h22.feed([]byte(h21ItemEvent("response.output_item.added", 0, h21TransportItem("truncated", "get_weather", map[string]any{"city": "Paris"})))); err != nil {
			t.Fatal(err)
		}
		var h22Err error
		if sentinel {
			h22Err = h22.feed([]byte("data: [DONE]\n\n"))
		} else {
			h22Err = h22.finish()
		}
		var h22API *APIError
		if !errors.As(h22Err, &h22API) || h22API.StatusCode() != 422 || h22API.Code() != "invalid_tool_call" || !strings.Contains(h22Err.Error(), "reason=missing_terminal_event") || h22.out.Len() != 0 {
			t.Fatalf("h22 classification of a truncated relay call changed: %v", h22Err)
		}
		// relay_incomplete: passthrough (rc2).
		relay := newStreamRelay(NewService(), "client", s2Source(weatherRequest(), s2RC2), usageTarget{}, "gpt-6-astra")
		item := h21TransportItem("truncated", "get_weather", map[string]any{"city": "Paris"})
		if err := relay.feed([]byte(h21ItemEvent("response.output_item.added", 0, item))); err != nil {
			t.Fatal(err)
		}
		var err error
		if sentinel {
			err = relay.feed([]byte("data: [DONE]\n\n"))
		} else {
			err = relay.finish()
		}
		var apiErr *APIError
		if !errors.As(err, &apiErr) || apiErr.StatusCode() != 502 || apiErr.Code() != "invalid_upstream_response" || strings.Contains(err.Error(), "invalid_tool_call") {
			t.Fatalf("truncated stream has wrong classification: %v", err)
		}
		if relay.out.Len() != 0 {
			t.Fatal("truncated transport reached the client")
		}
	}
}
