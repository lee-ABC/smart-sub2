package basispoints

import (
	"errors"
	"strings"
	"testing"
)

func rc2NativeWeather() map[string]any {
	return map[string]any{"type": "function_call", "name": "get_weather", "id": "fc_rc2_native", "call_id": "call_rc2_native", "arguments": `{"city":"Paris"}`, "status": "completed"}
}

func TestH24RC2NativeSingleCallPoliciesBeforeSecondExposure(t *testing.T) {
	for _, policy := range []string{"forced", "parallel-false", "auto"} {
		for _, secondRelay := range []bool{false, true} {
			t.Run(policy+map[bool]string{false: "/native", true: "/relay"}[secondRelay], func(t *testing.T) {
				source := weatherRequest()
				switch policy {
				case "forced":
					source["tool_choice"] = map[string]any{"type": "function", "name": "get_weather"}
				case "parallel-false":
					source["parallel_tool_calls"] = false
				case "auto":
					delete(source, "tool_choice")
					delete(source, "parallel_tool_calls")
				}
				first := rc2NativeWeather()
				firstAdded := cloneObject(first)
				firstAdded["arguments"], firstAdded["status"] = "", "in_progress"
				second := cloneObject(first)
				second["id"], second["call_id"] = "fc_second", "call_second"
				if secondRelay {
					second = h21TransportItem("second", "get_weather", map[string]any{"city": "Paris"})
				}
				secondAdded := cloneObject(second)
				secondAdded["arguments"], secondAdded["status"] = "", "in_progress"
				chunks := []string{h21ItemEvent("response.output_item.added", 0, firstAdded),
					sseEvent("response.function_call_arguments.done", map[string]any{"type": "response.function_call_arguments.done", "output_index": 0, "item_id": first["id"], "arguments": first["arguments"]}),
					h21ItemEvent("response.output_item.done", 0, first), h21ItemEvent("response.output_item.added", 1, secondAdded), h21ItemEvent("response.output_item.done", 1, second), h21Terminal("response.completed", first, second)}
				// Default (= h22): the upstream was never told about the
				// single-call policy; both calls are delivered and it is logged.
				// enforce (rc2 switches): the first call was already delivered
				// when the second appeared, so the stream must not fail any more
				// (H25-SPEC S2.2) -- logged and delivered as well. rc2 failed the
				// stream here after delivering the first call.
				for _, mode := range s2Modes() {
					result := runStreamTestWith(t, mode.configure, source, chunks)
					if result.closeErr != "" || strings.Count(result.joined(), "event: response.output_item.done") != 2 {
						t.Fatalf("%s: delivered stream failed or a call was lost: %q", mode.name, result.closeErr)
					}
				}
				if policy == "auto" {
					return
				}
				wantReason := "forced_tool_count"
				if policy == "parallel-false" {
					wantReason = "parallel_tool_count"
				}
				// enforce before anything was delivered: both calls are
				// announced before either completes, so the second one is
				// refused before any executable call exists.
				early := []string{h21ItemEvent("response.output_item.added", 0, firstAdded), h21ItemEvent("response.output_item.added", 1, secondAdded),
					h21ItemEvent("response.output_item.done", 0, first), h21ItemEvent("response.output_item.done", 1, second), h21Terminal("response.completed", first, second)}
				result := runStreamTestWith(t, s2RC2, source, early)
				if !strings.Contains(result.closeErr, wantReason) || strings.Contains(result.joined(), "fc_second") || strings.Count(result.joined(), "event: response.output_item.done") != 0 {
					t.Fatalf("second tool was exposed or the stream was not refused: %q", result.closeErr)
				}
			})
		}
	}
}

func TestH24RC2NativeProvenanceCommitsOnlyAfterHostAcceptance(t *testing.T) {
	for _, rejected := range []bool{false, true} {
		t.Run(map[bool]string{false: "accepted", true: "rejected"}[rejected], func(t *testing.T) {
			service := NewService()
			service.SetHost(func(method string, payload any, out any) error {
				if rejected && method == "host.stream.emit" {
					return errors.New("synthetic rejection")
				}
				return nil
			})
			scope := "trusted-native-" + t.Name()
			source := withReplayScope(weatherRequest(), scope)
			native := rc2NativeWeather()
			relay := newStreamRelay(service, "synthetic", source, usageTarget{}, "gpt-6-astra")
			if err := relay.feed([]byte(h21ItemEvent("response.output_item.done", 0, native))); err != nil {
				t.Fatal(err)
			}
			if _, state := cachedCallForHistory(scope, native); state == "hit" {
				t.Fatal("native provenance recorded before host acceptance")
			}
			err := relay.flush()
			cached, state := cachedCallForHistory(scope, native)
			if rejected {
				if err == nil || state == "hit" {
					t.Fatal("rejected host write produced delivered provenance")
				}
				return
			}
			if err != nil || state != "hit" || cached.Origin != "native" {
				t.Fatalf("accepted native provenance missing: state=%s err=%v", state, err)
			}
			if err := relay.feed([]byte(h23TerminalResponse("response.failed", map[string]any{"status": "failed", "output": []any{}, "error": map[string]any{"code": "server_error"}}))); err != nil {
				t.Fatal(err)
			}
			if err := relay.flush(); err != nil {
				t.Fatal(err)
			}
			if _, state := cachedCallForHistory(scope, native); state != "hit" {
				t.Fatal("later failure erased already delivered native provenance")
			}
			history := []any{native, map[string]any{"type": "function_call_output", "id": "fco_original", "call_id": native["call_id"], "output": "opaque", "encrypted_content": "SYNTHETIC_OPAQUE"}}
			// Replaying a delivered native call from its provenance record is
			// what history_conflict: reject does (rc2). Under replay (h22) a
			// name that is also a top-level tool is wrapped, as h22 did; see
			// TestH25S3ReplayModeTranslationIsH22.
			got := translateInputItemsReject(history, clientToolSpecs(source), scope)
			if string(jsonBytes(got)) != string(jsonBytes(history)) {
				t.Fatal("delivered native history or opaque result was rewrapped")
			}
		})
	}
}

func TestH24RC2NativeSchemaObservationDoesNotEnforce(t *testing.T) {
	source := weatherRequest()
	observed := 0
	source[schemaObserverKey] = &selectedSchemaObserver{remaining: 2, observe: func(spec toolSpec, value any) { observed++ }}
	native := rc2NativeWeather()
	native["arguments"] = `{"city":7}`
	before := string(jsonBytes(native))
	if err := validateNativeToolCall(native, source); err != nil {
		t.Fatal("native schema was hard rejected")
	}
	observeNativeToolSchema(native, source)
	if observed != 1 || string(jsonBytes(native)) != before {
		t.Fatal("native shadow was not bounded observation-only")
	}
	native["encrypted_content"] = "SYNTHETIC_OPAQUE"
	observeNativeToolSchema(native, source)
	if observed != 1 {
		t.Fatal("opaque native content was inspected as ordinary arguments")
	}
}

func rc2HasExecutableItem(t *testing.T, text string) bool {
	t.Helper()
	for _, event := range h23Events(t, text) {
		if isStreamToolItem(objectValue(event["item"])) {
			return true
		}
		output, _ := objectValue(event["response"])["output"].([]any)
		for _, value := range output {
			if isStreamToolItem(objectValue(value)) {
				return true
			}
		}
	}
	return false
}

func TestH24RC2NativeChoiceRejectsBeforeFirstExposure(t *testing.T) {
	choices := map[string]any{
		"none":          "none",
		"forced-other":  map[string]any{"type": "function", "name": "other"},
		"allowed-other": map[string]any{"type": "allowed_tools", "mode": "auto", "tools": []any{map[string]any{"type": "function", "name": "other"}}},
	}
	for name, choice := range choices {
		for _, phase := range []string{"snapshot", "added", "done"} {
			t.Run(name+"/"+phase, func(t *testing.T) {
				source := weatherRequest()
				source["tool_choice"] = choice
				source["tools"] = append(source["tools"].([]any), map[string]any{"type": "function", "name": "other", "parameters": map[string]any{"type": "object"}})
				item := rc2NativeWeather()
				first := h21ItemEvent("response.output_item."+phase, 0, item)
				if phase == "snapshot" {
					first = sseEvent("response.created", map[string]any{"type": "response.created", "response": map[string]any{"status": "in_progress", "output": []any{item}}})
				}
				// tool_policy_checks: enforce (rc2).
				result := runStreamTestWith(t, s2Enforce, source, []string{first, h21Terminal("response.completed", item)})
				if result.closeErr == "" || rc2HasExecutableItem(t, result.joined()) {
					t.Fatalf("forbidden native tool reached caller: %q", result.closeErr)
				}
				// Default (= h22): logged, delivered.
				h22 := runStreamTest(t, source, []string{first, h21Terminal("response.completed", item)})
				if h22.closeErr != "" || !rc2HasExecutableItem(t, h22.joined()) {
					t.Fatalf("h22 delivered this native call: %q", h22.closeErr)
				}
			})
		}
	}
}

func TestH24RC2KnownNativeExclusionIsNotWidenedByUnknownBuiltin(t *testing.T) {
	source := weatherRequest()
	source["tool_choice"] = map[string]any{"type": "allowed_tools", "mode": "auto", "tools": []any{map[string]any{"type": "function", "name": "other_function"}, map[string]any{"type": "web_search"}}}
	item := rc2NativeWeather()
	if validateNativeToolCall(item, source) == nil {
		t.Fatal("unrelated builtin re-enabled an explicitly excluded function")
	}
	chunks := []string{h21ItemEvent("response.output_item.added", 0, item), h21ItemEvent("response.output_item.done", 0, item), h21Terminal("response.completed", item)}
	// tool_policy_checks: enforce (rc2).
	result := runStreamTestWith(t, s2Enforce, source, chunks)
	if result.closeErr == "" || rc2HasExecutableItem(t, result.joined()) {
		t.Fatal("excluded function reached caller through mixed allowed_tools")
	}
	// Default (= h22): logged, delivered.
	if h22 := runStreamTest(t, source, chunks); h22.closeErr != "" || !rc2HasExecutableItem(t, h22.joined()) {
		t.Fatalf("h22 delivered this native call: %q", h22.closeErr)
	}
	// An actually unknown non-function native item is still not coerced into
	// a function or rejected by this function/custom metadata guard.
	opaque := map[string]any{"type": "future_native_item", "encrypted_content": "SYNTHETIC_OPAQUE"}
	if err := validateNativeToolCall(opaque, source); err != nil {
		t.Fatal("unknown native kind was treated as a forbidden function")
	}
}

func TestH24RC2NativeDeltaIdentityIsChecked(t *testing.T) {
	for _, scenario := range []string{"wrong-id", "wrong-kind", "orphan-wrong-id"} {
		t.Run(scenario, func(t *testing.T) {
			item := rc2NativeWeather()
			added := cloneObject(item)
			added["arguments"], added["status"] = "", "in_progress"
			kind := "response.function_call_arguments.delta"
			payload := map[string]any{"type": kind, "output_index": 0, "item_id": "fc_WRONG", "delta": "PRIVATE_WRONG_TARGET"}
			if scenario == "wrong-kind" {
				kind = "response.custom_tool_call_input.delta"
				payload["type"], payload["item_id"] = kind, item["id"]
			}
			chunks := []string{h21ItemEvent("response.output_item.added", 0, added), sseEvent(kind, payload), h21ItemEvent("response.output_item.done", 0, item), h21Terminal("response.completed", item)}
			if scenario == "orphan-wrong-id" {
				chunks = chunks[1:]
			}
			// tool_policy_checks: enforce (rc2).
			result := runStreamTestWith(t, s2Enforce, weatherRequest(), chunks)
			if result.closeErr == "" || strings.Contains(result.joined(), "PRIVATE_WRONG_TARGET") {
				t.Fatalf("contradictory native delta forwarded: %q", result.closeErr)
			}
			// Default (= h22): forwarded as the upstream sent it, and logged.
			if h22 := runStreamTest(t, weatherRequest(), chunks); h22.closeErr != "" || !strings.Contains(h22.joined(), "PRIVATE_WRONG_TARGET") {
				t.Fatalf("h22 forwarded this native delta: %q", h22.closeErr)
			}
		})
	}
}

func TestH24RC2NativeCustomOpaquePathKeepsExactContent(t *testing.T) {
	raw := "  preserve\n\tcustom  "
	source := weatherRequest()
	source["tools"] = []any{map[string]any{"type": "namespace", "name": "mcp__fixture", "tools": []any{map[string]any{"type": "custom", "name": "exec"}}}}
	source["tool_choice"] = map[string]any{"type": "custom", "namespace": "mcp__fixture", "name": "exec"}
	item := map[string]any{"type": "custom_tool_call", "name": "exec", "namespace": "mcp__fixture", "id": "ctc_native_rc2", "call_id": "native_custom_rc2", "input": raw, "encrypted_content": "SYNTHETIC_OPAQUE"}
	result := runStreamTest(t, source, []string{h21ItemEvent("response.output_item.done", 0, item), h21Terminal("response.completed", item)})
	if result.closeErr != "" {
		t.Fatal(result.closeErr)
	}
	for _, event := range h23Events(t, result.joined()) {
		if event["type"] == "response.output_item.done" {
			if string(jsonBytes(event["item"])) != string(jsonBytes(item)) {
				t.Fatal("native custom/opaque item changed")
			}
		}
	}
}
