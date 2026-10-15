package basispoints

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func h21TransportItem(id, tool string, args any) map[string]any {
	return map[string]any{
		"type": "function_call", "id": "fc_" + id, "call_id": "call_" + id,
		"name": "run_officejs", "status": "completed",
		"arguments": string(jsonBytes(map[string]any{
			"code": string(jsonBytes(map[string]any{"tool": tool, "args": args})),
		})),
	}
}

func h21ItemEvent(kind string, index any, item map[string]any) string {
	payload := map[string]any{"type": kind, "item": item}
	if index != nil {
		payload["output_index"] = index
	}
	return sseEvent(kind, payload)
}

func h21Terminal(kind string, items ...any) string {
	return sseEvent(kind, map[string]any{
		"type": kind,
		"response": map[string]any{
			"id": "resp_h21", "status": strings.TrimPrefix(kind, "response."), "output": items,
		},
	})
}

func h21AssertToolFailure(t *testing.T, result streamTestResult) {
	t.Helper()
	if !strings.Contains(result.closeErr, "invalid_tool_call") {
		t.Fatalf("close error = %q, want invalid_tool_call", result.closeErr)
	}
	for _, forbidden := range []string{"run_officejs", "NOT_FOR_THE_CALLER", "event: response.completed", "data: [DONE]"} {
		if strings.Contains(result.joined(), forbidden) {
			t.Fatalf("failed conversion emitted %q: %s", forbidden, result.joined())
		}
	}
	if strings.Contains(result.closeErr, "NOT_FOR_THE_CALLER") {
		t.Fatalf("conversion error disclosed arguments: %s", result.closeErr)
	}
}

func TestH21StreamRejectsUnsafeTransportVariants(t *testing.T) {
	valid := h21TransportItem("valid", "get_weather", map[string]any{"city": "Paris"})
	bad := cloneObject(valid)
	bad["arguments"] = `{"code":"NOT_FOR_THE_CALLER"}`
	unknown := h21TransportItem("unknown", "NOT_FOR_THE_CALLER", map[string]any{"city": "Paris"})
	for name, chunks := range map[string][]string{
		"bad envelope":               transportChunks(stringValue(bad["arguments"])),
		"unknown tool":               transportChunks(stringValue(unknown["arguments"])),
		"done only bad":              {h21ItemEvent("response.output_item.done", 0, bad)},
		"added missing index":        {h21ItemEvent("response.output_item.added", nil, valid)},
		"done missing index":         {h21ItemEvent("response.output_item.done", nil, valid)},
		"added negative index":       {h21ItemEvent("response.output_item.added", -1, valid)},
		"added fraction index":       {h21ItemEvent("response.output_item.added", 0.5, valid)},
		"added string index":         {h21ItemEvent("response.output_item.added", "0", valid)},
		"truncated held call":        {h21ItemEvent("response.output_item.added", 0, valid)},
		"truncated with sentinel":    {h21ItemEvent("response.output_item.added", 0, valid), "data: [DONE]\n\n"},
		"terminal erases held call":  {h21ItemEvent("response.output_item.added", 0, valid), h21Terminal("response.completed")},
		"terminal changes held tool": {h21ItemEvent("response.output_item.added", 0, valid), h21Terminal("response.completed", map[string]any{"type": "function_call", "name": "get_weather", "call_id": "call_valid", "arguments": `{}`})},
	} {
		for _, mode := range s2Modes() {
			t.Run(mode.name+"/"+name, func(t *testing.T) {
				result := runStreamTestWith(t, mode.configure, weatherRequest(), chunks)
				if strings.HasPrefix(name, "truncated") && mode.rc2 {
					// relay_incomplete: passthrough (rc2) reports a truncation
					// as an upstream failure.
					if result.closeErr == "" || strings.Contains(result.closeErr, "invalid_tool_call") || strings.Contains(result.joined(), "run_officejs") || strings.Contains(result.joined(), "data: [DONE]") {
						t.Fatalf("truncation was reclassified or exposed a pending tool: %q", result.closeErr)
					}
					return
				}
				// Default (= h22): a truncated relay call is invalid_tool_call too.
				h21AssertToolFailure(t, result)
			})
		}
	}
	t.Run("undeclared caller tool", func(t *testing.T) {
		h21AssertToolFailure(t, runStreamTest(t, map[string]any{"model": "gpt-6-astra", "input": "hello"},
			transportChunks(stringValue(valid["arguments"]))))
	})
}

func TestH21StreamValidatesTerminalOnlyTools(t *testing.T) {
	valid := h21TransportItem("one", "get_weather", map[string]any{"city": "Paris"})
	second := h21TransportItem("two", "get_weather", map[string]any{"city": "Oslo"})
	bad := h21TransportItem("bad", "NOT_FOR_THE_CALLER", map[string]any{"city": "Paris"})
	for _, terminal := range []string{"response.completed", "response.failed", "response.incomplete"} {
		for _, mode := range s2Modes() {
			t.Run(mode.name+"/"+terminal+" valid parallel", func(t *testing.T) {
				result := runStreamTestWith(t, mode.configure, weatherRequest(), []string{h21Terminal(terminal, valid, second)})
				if result.closeErr != "" {
					t.Fatal(result.closeErr)
				}
				wantCalls := 2
				if terminal != "response.completed" && mode.rc2 {
					// relay_incomplete: passthrough (rc2): failure snapshots do
					// not newly deliver tools, even if an item in their partial
					// output had finished before the failure. h22 converted and
					// delivered them like any terminal-only call.
					wantCalls = 0
				}
				if strings.Contains(result.joined(), "run_officejs") || strings.Count(result.joined(), "event: response.output_item.done") != wantCalls {
					t.Fatalf("terminal-only tools not converted: %s", result.joined())
				}
				if !strings.Contains(result.joined(), `"status":"`+strings.TrimPrefix(terminal, "response.")+`"`) {
					t.Fatalf("terminal status changed: %s", result.joined())
				}
			})
			t.Run(mode.name+"/"+terminal+" invalid", func(t *testing.T) {
				result := runStreamTestWith(t, mode.configure, weatherRequest(), []string{h21Terminal(terminal, valid, bad)})
				if terminal == "response.completed" || !mode.rc2 {
					h21AssertToolFailure(t, result)
				} else if result.closeErr != "" || strings.Contains(result.joined(), "run_officejs") || strings.Contains(result.joined(), "NOT_FOR_THE_CALLER") || !strings.Contains(result.joined(), "event: "+terminal) {
					t.Fatalf("failed terminal lost its status or exposed an internal tool: %q", result.closeErr)
				}
			})
		}
	}
}

func TestH21StreamConvertsDoneOnlyAndOrphanTransport(t *testing.T) {
	item := h21TransportItem("one", "get_weather", map[string]any{"city": "Paris"})
	orphan := sseEvent("response.function_call_arguments.delta", map[string]any{
		"type": "response.function_call_arguments.delta", "output_index": 0,
		"item_id": "fc_one", "delta": "NOT_FOR_THE_CALLER",
	})
	for name, prefix := range map[string][]string{"done only": nil, "orphan arguments": {orphan}} {
		t.Run(name, func(t *testing.T) {
			chunks := append(append([]string{}, prefix...), h21ItemEvent("response.output_item.done", 0, item), h21Terminal("response.completed", item))
			result := runStreamTest(t, weatherRequest(), chunks)
			if result.closeErr != "" {
				t.Fatal(result.closeErr)
			}
			for _, forbidden := range []string{"run_officejs", "NOT_FOR_THE_CALLER"} {
				if strings.Contains(result.joined(), forbidden) {
					t.Fatalf("leaked %q: %s", forbidden, result.joined())
				}
			}
			if strings.Count(result.joined(), "event: response.output_item.done") != 1 || !strings.Contains(result.joined(), `"name":"get_weather"`) {
				t.Fatalf("decoded tool missing or duplicated: %s", result.joined())
			}
		})
	}
}

func TestH21StreamHeldCallCanCompleteInTerminalSnapshot(t *testing.T) {
	item := h21TransportItem("one", "get_weather", map[string]any{"city": "Paris"})
	result := runStreamTest(t, weatherRequest(), []string{
		h21ItemEvent("response.output_item.added", 0, item), h21Terminal("response.completed", item),
	})
	if result.closeErr != "" || strings.Contains(result.joined(), "run_officejs") || !strings.Contains(result.joined(), `"name":"get_weather"`) {
		t.Fatalf("held call was not safely completed: error=%q stream=%s", result.closeErr, result.joined())
	}
	if strings.Count(result.joined(), "event: response.output_item.done") != 1 || strings.Index(result.joined(), "event: response.output_item.done") > strings.Index(result.joined(), "event: response.completed") {
		t.Fatalf("held call did not emit its lifecycle before the terminal: %s", result.joined())
	}
}

func TestH21StreamDoesNotReleasePendingToolAfterFailure(t *testing.T) {
	good := h21TransportItem("good", "get_weather", map[string]any{"city": "Paris"})
	bad := h21TransportItem("bad", "NOT_FOR_THE_CALLER", map[string]any{"city": "Oslo"})
	for name, tail := range map[string]string{
		"later invalid done":         h21ItemEvent("response.output_item.done", 1, bad),
		"later invalid terminal":     h21Terminal("response.completed", good, bad),
		"terminal changed arguments": h21Terminal("response.completed", h21TransportItem("good", "get_weather", map[string]any{"city": "Oslo"})),
		"duplicate done":             h21ItemEvent("response.output_item.done", 0, good),
	} {
		for _, mode := range s2Modes() {
			t.Run(mode.name+"/"+name, func(t *testing.T) {
				result := runStreamTestWith(t, mode.configure, weatherRequest(), []string{h21ItemEvent("response.output_item.done", 0, good), tail})
				h21AssertToolFailure(t, result)
				// Default (= h22, immediate delivery): the good call went out on
				// its own done and is neither replayed nor lost. batch (rc2):
				// a failed relay batch never becomes executable.
				wantCalls := 1
				if mode.rc2 {
					wantCalls = 0
				}
				if strings.Count(result.joined(), "event: response.output_item.done") != wantCalls {
					t.Fatalf("already emitted tool replayed or lost, or failed batch became executable: %s", result.joined())
				}
			})
		}
	}
}

func TestH21StreamSuppressesDuplicateTerminalEvents(t *testing.T) {
	result := runStreamTest(t, weatherRequest(), []string{
		h21Terminal("response.completed") + "data: [DONE]\n\n" + h21Terminal("response.failed") + "data: [DONE]\n\n",
	})
	if result.closeErr != "" || strings.Count(result.joined(), "event: response.completed") != 1 ||
		strings.Contains(result.joined(), "event: response.failed") || strings.Count(result.joined(), "data: [DONE]") != 1 {
		t.Fatalf("contradictory or duplicated terminal: error=%q stream=%s", result.closeErr, result.joined())
	}
}

func TestH21StreamOrphanArgumentsWithoutIndexFailClosed(t *testing.T) {
	result := runStreamTest(t, weatherRequest(), []string{sseEvent("response.function_call_arguments.delta", map[string]any{
		"type": "response.function_call_arguments.delta", "delta": "NOT_FOR_THE_CALLER",
	})})
	h21AssertToolFailure(t, result)
}

func TestH21StreamReasoningRemainsIncremental(t *testing.T) {
	reasoning := sseEvent("response.reasoning_summary_text.delta", map[string]any{
		"type": "response.reasoning_summary_text.delta", "output_index": 0, "delta": "reasoning-before-terminal",
	})
	result := runStreamTest(t, weatherRequest(), []string{reasoning, h21Terminal("response.completed")})
	if result.closeErr != "" || result.emitIndexContaining("reasoning-before-terminal") >= result.lastReadIndex() {
		t.Fatalf("reasoning stopped streaming: error=%q trace=%v", result.closeErr, result.trace)
	}
}

func TestH21StreamFailureKeepsTypedErrorAndDoesNotEmitDone(t *testing.T) {
	relay := newStreamRelay(NewService(), "client", weatherRequest(), usageTarget{}, "gpt-6-astra")
	item := h21TransportItem("bad", "NOT_FOR_THE_CALLER", map[string]any{})
	err := relay.feed([]byte(h21ItemEvent("response.output_item.done", 0, item)))
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Code() != "invalid_tool_call" || apiErr.StatusCode() != 422 {
		t.Fatalf("conversion lost typed error: %#v", err)
	}
	if relay.out.Len() != 0 {
		t.Fatalf("failed tool emitted data: %s", relay.out.String())
	}
	// Ensure that the static error message is not a serialization of the call.
	var leaked map[string]any
	if json.Unmarshal([]byte(apiErr.Message), &leaked) == nil || strings.Contains(apiErr.Message, "NOT_FOR_THE_CALLER") {
		t.Fatalf("error contains upstream arguments: %s", apiErr.Message)
	}
}

func TestH21StreamRejectedTerminalDoesNotChangeReplayCache(t *testing.T) {
	// Default (= h22, immediate delivery): the call delivered on its own done
	// is what replay history holds; a rejected terminal changes nothing.
	t.Run("default", func(t *testing.T) {
		source := weatherRequest()
		source["input"] = []any{messageItem("user", "h21-stream-cache-integrity-default")}
		good := h21TransportItem("cache_existing", "get_weather", map[string]any{"city": "Paris"})
		changed := h21TransportItem("cache_existing", "get_weather", map[string]any{"city": "Oslo"})
		undelivered := h21TransportItem("cache_undelivered", "get_weather", map[string]any{"city": "Tokyo"})
		result := runStreamTest(t, source, []string{
			h21ItemEvent("response.output_item.done", 0, good), h21Terminal("response.completed", changed, undelivered),
		})
		h21AssertToolFailure(t, result)
		remembered := rememberedNativeCall(streamTestReplayScope(t, source), "call_cache_existing")
		if string(jsonBytes(remembered)) != string(jsonBytes(good)) {
			t.Fatal("rejected terminal replaced an already delivered tool in replay history")
		}
		if rememberedNativeCall(streamTestReplayScope(t, source), "call_cache_undelivered") != nil {
			t.Fatal("rejected terminal cached an undelivered tool")
		}
	})
	t.Run("rc2", h21RejectedTerminalCacheRC2)
}

// relay_tool_delivery: batch (rc2): done alone no longer writes the cache.
func h21RejectedTerminalCacheRC2(t *testing.T) {
	source := weatherRequest()
	source["input"] = []any{messageItem("user", "h21-stream-cache-integrity")}
	good := h21TransportItem("cache_existing", "get_weather", map[string]any{"city": "Paris"})
	previous := h21TransportItem("cache_existing", "get_weather", map[string]any{"city": "London"})
	// A prior successfully completed response must survive a new failed
	// pending batch; done alone no longer writes the replay cache.
	client, reason := decodeTransportItemWithReason(previous, source)
	if reason != "" {
		t.Fatal(reason)
	}
	rememberRelayCall(streamTestReplayScope(t, source), previous, client)
	changed := h21TransportItem("cache_existing", "get_weather", map[string]any{"city": "Oslo"})
	undelivered := h21TransportItem("cache_undelivered", "get_weather", map[string]any{"city": "Tokyo"})
	result := runStreamTestWith(t, s2RC2, source, []string{
		h21ItemEvent("response.output_item.done", 0, good), h21Terminal("response.completed", changed, undelivered),
	})
	h21AssertToolFailure(t, result)
	remembered := rememberedNativeCall(streamTestReplayScope(t, source), "call_cache_existing")
	if string(jsonBytes(remembered)) != string(jsonBytes(previous)) {
		t.Fatal("rejected terminal replaced an already delivered tool in replay history")
	}
	if rememberedNativeCall(streamTestReplayScope(t, source), "call_cache_undelivered") != nil {
		t.Fatal("rejected terminal cached an undelivered tool")
	}
}

func TestH21StreamRejectsDuplicateIdentitiesBeforeSecondDelivery(t *testing.T) {
	for _, native := range []bool{false, true} {
		for _, field := range []string{"call_id", "id"} {
			for _, mode := range s2Modes() {
				name := mode.name + "/" + field
				if native {
					name += " native"
				}
				h21DuplicateIdentityCase(t, name, mode, native, field)
			}
		}
	}
}

func h21DuplicateIdentityCase(t *testing.T, name string, mode s2Mode, native bool, field string) {
	t.Run(name, func(t *testing.T) {
		source := weatherRequest()
		source["input"] = []any{messageItem("user", "h21-stream-duplicate-"+name)}
		one := h21TransportItem("first", "get_weather", map[string]any{"city": "Paris"})
		two := h21TransportItem("second", "get_weather", map[string]any{"city": "Oslo"})
		two[field] = one[field]
		if native {
			one["name"], one["arguments"] = "get_weather", `{"city":"Paris"}`
		}
		result := runStreamTestWith(t, mode.configure, source, []string{
			h21ItemEvent("response.output_item.done", 0, one), h21ItemEvent("response.output_item.done", 1, two),
		})
		h21AssertToolFailure(t, result)
		// Default (= h22): the first call went out on its own done.
		// batch (rc2): a relay call waits; native Lite stays incremental.
		wantCalls := 1
		if mode.rc2 && !native {
			wantCalls = 0
		}
		if strings.Count(result.joined(), "event: response.output_item.done") != wantCalls || strings.Contains(result.joined(), "Oslo") {
			t.Fatalf("duplicate identity delivered twice: %s", result.joined())
		}
		cached := rememberedNativeCall(streamTestReplayScope(t, source), stringValue(two["call_id"]))
		if cached != nil && string(jsonBytes(cached)) == string(jsonBytes(two)) {
			t.Fatal("undelivered duplicate poisoned replay cache")
		}
	})
}

func TestH21StreamPreservesExactCustomInput(t *testing.T) {
	for _, input := range []string{"", " \n\t ", "  first line\n\tsecond line  "} {
		t.Run(input, func(t *testing.T) {
			source := map[string]any{"input": []any{messageItem("user", "h21-stream-custom-"+input)}, "tools": []any{
				map[string]any{"type": "namespace", "name": "functions", "tools": []any{
					map[string]any{"type": "custom", "name": "apply_patch"},
				}},
			}}
			item := h21TransportItem("custom", "functions.apply_patch", input)
			result := runStreamTest(t, source, []string{
				h21ItemEvent("response.output_item.done", 0, item), h21Terminal("response.completed", item),
			})
			if result.closeErr != "" {
				t.Fatal(result.closeErr)
			}
			doneCount := 0
			decoder := newSSEDecoder()
			if err := decoder.feed([]byte(result.joined()), func(event, data string) error {
				if event == "response.custom_tool_call_input.done" {
					doneCount++
					payload := decodeEventPayload(data)
					if payload["input"] != input || !strings.HasPrefix(stringValue(payload["item_id"]), "ctc_") {
						t.Fatalf("custom input or item id changed: %#v", payload)
					}
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if doneCount != 1 {
				t.Fatalf("custom input.done count=%d; empty input still needs one", doneCount)
			}
			changed := h21TransportItem("custom", "functions.apply_patch", input+" ")
			result = runStreamTest(t, source, []string{
				h21ItemEvent("response.output_item.done", 0, item), h21Terminal("response.completed", changed),
			})
			h21AssertToolFailure(t, result)
		})
	}
}

func TestH21StreamCannotDowngradeHeldTransportIdentity(t *testing.T) {
	item := h21TransportItem("one", "get_weather", map[string]any{"city": "Paris"})
	unknown := cloneObject(item)
	delete(unknown, "name")
	result := runStreamTest(t, weatherRequest(), []string{
		h21ItemEvent("response.output_item.added", 0, item), h21ItemEvent("response.output_item.added", 0, unknown),
	})
	h21AssertToolFailure(t, result)
}

func TestH21StreamUsesSSETypeWhenPayloadTypeIsMissing(t *testing.T) {
	item := h21TransportItem("missing_type", "get_weather", map[string]any{"city": "Paris"})
	result := runStreamTest(t, weatherRequest(), []string{
		sseEvent("response.output_item.done", map[string]any{"output_index": 0, "item": item}),
		h21Terminal("response.completed", item),
	})
	if result.closeErr != "" || strings.Contains(result.joined(), "run_officejs") || !strings.Contains(result.joined(), `"name":"get_weather"`) {
		t.Fatalf("missing payload type bypassed conversion: error=%q stream=%s", result.closeErr, result.joined())
	}
}

func TestH21StreamNeverPromotesAnUnfinishedToolToExecutable(t *testing.T) {
	for _, status := range []string{"in_progress", "incomplete", "failed"} {
		t.Run(status, func(t *testing.T) {
			item := h21TransportItem("unfinished", "get_weather", map[string]any{"city": "Paris"})
			item["status"] = status
			// Default (= h22): every way an unfinished call arrives is
			// invalid_tool_call, and none of them becomes executable.
			for _, chunks := range [][]string{
				{h21ItemEvent("response.output_item.done", 0, item)},
				{h21Terminal("response.incomplete", item)},
				{h21Terminal("response.completed", item)},
			} {
				result := runStreamTest(t, weatherRequest(), chunks)
				h21AssertToolFailure(t, result)
				if strings.Contains(result.joined(), "event: response.output_item.done") {
					t.Fatalf("unfinished tool became executable: %s", result.joined())
				}
			}
			// relay_incomplete: passthrough (rc2).
			for _, chunks := range [][]string{
				{h21ItemEvent("response.output_item.done", 0, item)},
				{h21Terminal("response.completed", item)},
			} {
				result := runStreamTestWith(t, s2RC2, weatherRequest(), chunks)
				if strings.HasPrefix(chunks[0], "event: response.output_item.done") {
					// EOF while an unfinished item is held is an upstream
					// truncation, not evidence of invalid client arguments.
					if result.closeErr == "" || strings.Contains(result.closeErr, "invalid_tool_call") {
						t.Fatalf("unfinished EOF was misclassified: %q", result.closeErr)
					}
				} else {
					h21AssertToolFailure(t, result)
				}
				if strings.Contains(result.joined(), "event: response.output_item.done") {
					t.Fatalf("unfinished tool became executable: %s", result.joined())
				}
			}
			// A truthful incomplete terminal stays incomplete, with its
			// undelivered transport omitted rather than promoted to executable.
			incomplete := runStreamTestWith(t, s2RC2, weatherRequest(), []string{h21Terminal("response.incomplete", item)})
			if incomplete.closeErr != "" || !strings.Contains(incomplete.joined(), "event: response.incomplete") || strings.Contains(incomplete.joined(), "run_officejs") || strings.Contains(incomplete.joined(), "event: response.output_item.done") {
				t.Fatalf("incomplete terminal was promoted or reclassified: %q", incomplete.closeErr)
			}
		})
	}
}
