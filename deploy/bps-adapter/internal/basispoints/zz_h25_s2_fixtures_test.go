package basispoints

// H25 S2 equivalence fixtures: Lite prologues, streams and whole responses.
// This file compiles unchanged in the h22-live tree, where
// testdata/h22_s2_golden_gen.go.txt recorded testdata/h22_s2_golden.json from
// h22's own code (prepareResponsesBody, the stream relay through the same
// runStreamTest harness, transformResponseBody). zz_h25_s2_golden_test.go
// replays them here with the default configuration. Keep this file free of
// anything h22 does not have.

import (
	"encoding/json"
	"strings"
)

// s2NormalizeStream turns what a client received into comparable events:
// each event's name and its JSON with the top-level sequence_number removed
// (H25-SPEC I2 lets h25 renumber) and "type" filled from the event name when
// the upstream left it out, re-encoded canonically; "[DONE]" for the
// sentinel. SSE comments carry nothing and are skipped.
func s2NormalizeStream(text string) []string {
	var events []string
	name := ""
	var data []string
	dispatch := func() {
		if len(data) == 0 {
			name = ""
			return
		}
		payload := strings.Join(data, "\n")
		data = nil
		if strings.TrimSpace(payload) == "[DONE]" {
			events = append(events, "[DONE]")
			name = ""
			return
		}
		decoder := json.NewDecoder(strings.NewReader(payload))
		decoder.UseNumber()
		var value map[string]any
		if err := decoder.Decode(&value); err != nil || value == nil {
			events = append(events, name+"\x00RAW "+payload)
			name = ""
			return
		}
		delete(value, "sequence_number")
		if _, ok := value["type"]; !ok && name != "" {
			value["type"] = name
		}
		canonical, _ := json.Marshal(value)
		events = append(events, name+"\x00"+string(canonical))
		name = ""
	}
	for _, line := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
		switch {
		case line == "":
			dispatch()
		case strings.HasPrefix(line, ":"):
		case strings.HasPrefix(line, "event: "):
			name = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			data = append(data, strings.TrimPrefix(line, "data: "))
		}
	}
	dispatch()
	return events
}

type s2PromptFixture struct {
	Name   string
	Source map[string]any
}

type s2StreamFixture struct {
	Name    string
	Request map[string]any
	Chunks  []string
}

type s2TransformFixture struct {
	Name     string
	Source   map[string]any
	Response map[string]any
}

func s2Weather() map[string]any {
	return map[string]any{
		"type": "function", "name": "get_weather", "description": "Get current weather.",
		"parameters": map[string]any{
			"type":       "object",
			"properties": map[string]any{"city": map[string]any{"type": "string"}},
			"required":   []any{"city"},
		},
	}
}

func s2LiteSource(input ...any) map[string]any {
	return map[string]any{"model": "gpt-6-astra", "input": input}
}

func s2Additional(tools ...any) map[string]any {
	return map[string]any{"type": "additional_tools", "tools": tools}
}

func s2PromptFixtures() []s2PromptFixture {
	user := messageItem("user", "s2 lite")
	namespaced := map[string]any{"type": "namespace", "name": "workspace", "tools": []any{s2Weather(),
		map[string]any{"type": "custom", "name": "apply_patch", "description": "Apply a patch."}}}
	history := []any{
		map[string]any{"type": "function_call", "name": "get_weather", "namespace": "workspace", "call_id": "s2_hist", "id": "fc_s2_hist", "arguments": `{"city":"Oslo"}`},
		map[string]any{"type": "function_call_output", "call_id": "s2_hist", "output": "sunny"},
	}
	withHistory := append([]any{s2Additional(namespaced), user}, history...)
	fixtures := []s2PromptFixture{
		{"lite_additional", s2LiteSource(s2Additional(s2Weather()), user)},
		{"lite_additional_namespace", s2LiteSource(s2Additional(namespaced), user)},
		{"lite_additional_empty", s2LiteSource(s2Additional(), user)},
		{"lite_additional_history", s2LiteSource(withHistory...)},
		{"no_tools", s2LiteSource(user)},
		{"no_tools_string_input", map[string]any{"model": "gpt-6-astra", "input": "s2 plain"}},
		{"search_plus_additional", s2LiteSource(s2Additional(s2Weather()), toolSearchOutput("s2_search", searchedNamespace("mcp_docs", "lookup")), user)},
		{"search_only_choice_none", s2LiteSource(toolSearchOutput("s2_search", searchedNamespace("mcp_docs", "lookup")), user)},
		{"search_only_forced_other", s2LiteSource(toolSearchOutput("s2_search", searchedNamespace("mcp_docs", "lookup")), user)},
		{"top_tools_choice_none", map[string]any{"model": "gpt-6-astra", "input": []any{user}, "tools": []any{s2Weather()}, "tool_choice": "none"}},
		{"top_tools_choice_none_additional", map[string]any{"model": "gpt-6-astra", "input": []any{s2Additional(s2Weather()), user}, "tools": []any{s2Weather()}, "tool_choice": "none"}},
		{"lite_choice_none", s2LiteSource(s2Additional(s2Weather()), user)},
		{"lite_choice_allowed_none", s2LiteSource(s2Additional(s2Weather()), user)},
		{"lite_parallel_false_required", s2LiteSource(s2Additional(s2Weather()), user)},
		{"lite_instructions_cache_key", s2LiteSource(s2Additional(s2Weather()), user)},
		// E2: the only fixtures whose h25 prologue may differ from h22's.
		{"e2_search_only", s2LiteSource(toolSearchOutput("s2_search", searchedNamespace("mcp_docs", "lookup", "fetch")), user)},
		{"e2_search_only_allowed", s2LiteSource(toolSearchOutput("s2_search", searchedNamespace("mcp_docs", "lookup")), user)},
		{"e2_search_only_flat", s2LiteSource(map[string]any{"type": "tool_search_output", "call_id": "s2_flat", "status": "completed", "execution": "client", "tools": []any{s2Weather()}}, user)},
	}
	for _, fixture := range fixtures {
		switch fixture.Name {
		case "search_only_choice_none", "lite_choice_none":
			fixture.Source["tool_choice"] = "none"
		case "search_only_forced_other":
			fixture.Source["tool_choice"] = map[string]any{"type": "function", "name": "not_discovered"}
		case "lite_choice_allowed_none":
			fixture.Source["tool_choice"] = map[string]any{"type": "allowed_tools", "mode": "none", "tools": []any{map[string]any{"type": "function", "name": "get_weather"}}}
		case "lite_parallel_false_required":
			fixture.Source["parallel_tool_calls"] = false
			fixture.Source["tool_choice"] = "required"
		case "lite_instructions_cache_key":
			fixture.Source["instructions"] = "Be brief."
			fixture.Source["prompt_cache_key"] = "s2-lite-cache-key"
		case "e2_search_only_allowed":
			fixture.Source["tool_choice"] = map[string]any{"type": "allowed_tools", "mode": "auto", "tools": []any{map[string]any{"type": "function", "namespace": "mcp_docs", "name": "lookup"}}}
		}
	}
	return fixtures
}

// s2E2Fixtures are the prompt fixtures where H25 E2 applies: h22 told the
// model to answer as text although tool_search_output found callable tools.
var s2E2Fixtures = map[string]bool{"e2_search_only": true, "e2_search_only_allowed": true, "e2_search_only_flat": true}

func s2NumericRequest() map[string]any {
	return map[string]any{"model": "gpt-6-astra", "input": "s2 numbers", "tools": []any{
		map[string]any{"type": "function", "name": "lookup", "parameters": map[string]any{"type": "object"}},
		map[string]any{"type": "function", "name": "list_files", "parameters": map[string]any{
			"type": "object", "properties": map[string]any{"path": map[string]any{"type": "string"}, "limit": map[string]any{"type": "integer"}},
			"required": []any{"path"},
		}},
	}}
}

func s2CustomRequest() map[string]any {
	return map[string]any{"model": "gpt-6-astra", "input": "s2 custom", "tools": []any{
		map[string]any{"type": "custom", "name": "patch"},
	}}
}

func s2LiteWeatherRequest() map[string]any {
	return map[string]any{"model": "gpt-6-astra", "input": []any{s2Additional(s2Weather()), messageItem("user", "s2 native")}}
}

func s2NativeCall(id, arguments string) map[string]any {
	return map[string]any{"type": "function_call", "name": "get_weather", "id": "fc_" + id, "call_id": "call_" + id, "arguments": arguments, "status": "completed"}
}

func s2Created() string {
	return sseEvent("response.created", map[string]any{"type": "response.created", "response": map[string]any{"id": "resp_s2", "status": "in_progress", "output": []any{}}})
}

func s2Message(id, text string) map[string]any {
	return map[string]any{"type": "message", "id": id, "role": "assistant", "status": "completed", "content": []any{map[string]any{"type": "output_text", "text": text}}}
}

func s2Response(kind string, response map[string]any) string {
	return sseEvent(kind, map[string]any{"type": kind, "response": response})
}

func s2StreamFixtures() []s2StreamFixture {
	paris := h21TransportItem("paris", "get_weather", map[string]any{"city": "Paris"})
	oslo := h21TransportItem("oslo", "get_weather", map[string]any{"city": "Oslo"})
	parisChanged := h21TransportItem("paris", "get_weather", map[string]any{"city": "Rome"})
	bad := h21TransportItem("bad", "NOT_DECLARED", map[string]any{"city": "Paris"})
	unfinished := cloneObject(paris)
	unfinished["status"] = "incomplete"
	duplicate := cloneObject(oslo)
	duplicate["call_id"] = paris["call_id"]
	added := cloneObject(paris)
	added["status"], added["arguments"] = "in_progress", ""
	message := s2Message("msg_s2", "TEXT AFTER TOOL")
	messageAdded := cloneObject(message)
	messageAdded["status"], messageAdded["content"] = "in_progress", []any{}
	equivalentA := h21TransportItem("num", "lookup", map[string]any{"n": 1000})
	equivalentA["arguments"] = `{"code":"{\"tool\":\"lookup\",\"args\":{\"n\":1e3}}"}`
	equivalentB := h21TransportItem("num", "lookup", map[string]any{"n": 1000})
	missingID := cloneObject(paris)
	delete(missingID, "id")
	coerced := h21TransportItem("coerce", "list_files", map[string]any{"path": "/tmp", "limit": "20"})
	customFirst := h21TransportItem("alias", "patch", "raw text")
	customLast := cloneObject(customFirst)
	customLast["id"] = "ctc_alias"
	native := s2NativeCall("native", `{"city":"Paris"}`)
	nativeAdded := cloneObject(native)
	nativeAdded["status"], nativeAdded["arguments"] = "in_progress", ""
	nativeTwo := s2NativeCall("native_two", `{"city":"Oslo"}`)
	nativeChanged := s2NativeCall("native", `{"city":"Rome"}`)
	nativeBadJSON := s2NativeCall("native_bad", `{"city":`)
	nativeDelta := func(itemID, delta string) string {
		return sseEvent("response.function_call_arguments.delta", map[string]any{"type": "response.function_call_arguments.delta", "output_index": 0, "item_id": itemID, "delta": delta})
	}
	reasoning := sseEvent("response.reasoning_summary_text.delta", map[string]any{"type": "response.reasoning_summary_text.delta", "output_index": 0, "delta": "thinking"})
	errorEvent := sseEvent("error", map[string]any{"type": "error", "error": map[string]any{"code": "server_error", "message": "upstream said no"}})
	parallelFalse := weatherRequest()
	parallelFalse["parallel_tool_calls"] = false
	required := weatherRequest()
	required["tool_choice"] = "required"
	forcedOther := s2LiteWeatherRequest()
	forcedOther["tools"] = []any{map[string]any{"type": "function", "name": "other", "parameters": map[string]any{"type": "object"}}}
	forcedOther["tool_choice"] = map[string]any{"type": "function", "name": "other"}
	choiceNone := weatherRequest()
	choiceNone["tool_choice"] = "none"
	liteParallelFalse := s2LiteWeatherRequest()
	liteParallelFalse["parallel_tool_calls"] = false
	textDone := sseEvent("response.output_text.delta", map[string]any{"type": "response.output_text.delta", "output_index": 0, "item_id": "msg_s2", "content_index": 0, "delta": "hello"})
	return []s2StreamFixture{
		{"text", map[string]any{"model": "gpt-6-astra", "input": "hi"}, textChunks()},
		{"relay_full_lifecycle", weatherRequest(), transportChunks(stringValue(paris["arguments"]))},
		{"relay_done_then_terminal", weatherRequest(), []string{s2Created(), h21ItemEvent("response.output_item.done", 0, paris), h21Terminal("response.completed", paris)}},
		{"relay_added_done_terminal", weatherRequest(), []string{h21ItemEvent("response.output_item.added", 0, added), h21ItemEvent("response.output_item.done", 0, paris), h21Terminal("response.completed", paris)}},
		{"relay_terminal_only_two", weatherRequest(), []string{h21Terminal("response.completed", paris, oslo)}},
		{"relay_two_done", weatherRequest(), []string{h21ItemEvent("response.output_item.done", 0, paris), h21ItemEvent("response.output_item.done", 1, oslo), h21Terminal("response.completed", paris, oslo)}},
		{"relay_then_text", weatherRequest(), []string{h21ItemEvent("response.output_item.done", 0, paris), h21ItemEvent("response.output_item.added", 1, messageAdded), h21ItemEvent("response.output_item.done", 1, message), h21Terminal("response.completed", paris, message)}},
		{"relay_good_then_bad_done", weatherRequest(), []string{h21ItemEvent("response.output_item.done", 0, paris), h21ItemEvent("response.output_item.done", 1, bad)}},
		{"relay_bad_done", weatherRequest(), []string{s2Created(), h21ItemEvent("response.output_item.done", 0, bad)}},
		{"relay_bad_terminal_only", weatherRequest(), []string{s2Created(), h21Terminal("response.completed", bad)}},
		{"relay_terminal_changed", weatherRequest(), []string{h21ItemEvent("response.output_item.done", 0, paris), h21Terminal("response.completed", parisChanged)}},
		{"relay_held_eof", weatherRequest(), []string{s2Created(), h21ItemEvent("response.output_item.added", 0, added)}},
		{"relay_held_sentinel", weatherRequest(), []string{h21ItemEvent("response.output_item.added", 0, added), "data: [DONE]\n\n"}},
		{"relay_done_eof", weatherRequest(), []string{h21ItemEvent("response.output_item.done", 0, paris)}},
		{"relay_unfinished_done", weatherRequest(), []string{h21ItemEvent("response.output_item.done", 0, unfinished)}},
		{"relay_failed_terminal_calls", weatherRequest(), []string{s2Response("response.failed", map[string]any{"status": "failed", "error": map[string]any{"code": "server_error"}, "output": []any{paris, oslo}})}},
		{"relay_incomplete_terminal_calls", weatherRequest(), []string{s2Response("response.incomplete", map[string]any{"status": "incomplete", "incomplete_details": map[string]any{"reason": "max_output_tokens"}, "output": []any{paris}})}},
		{"relay_incomplete_terminal_unfinished", weatherRequest(), []string{s2Response("response.incomplete", map[string]any{"status": "incomplete", "output": []any{unfinished}})}},
		{"relay_failed_after_delivery_empty", weatherRequest(), []string{h21ItemEvent("response.output_item.done", 0, paris), s2Response("response.failed", map[string]any{"status": "failed", "output": []any{}, "error": map[string]any{"code": "server_error"}})}},
		{"relay_failed_after_delivery_same", weatherRequest(), []string{h21ItemEvent("response.output_item.done", 0, paris), s2Response("response.failed", map[string]any{"status": "failed", "output": []any{paris}, "error": map[string]any{"code": "server_error"}})}},
		{"relay_then_error_event", weatherRequest(), []string{h21ItemEvent("response.output_item.done", 0, paris), errorEvent}},
		{"error_event_only", weatherRequest(), []string{s2Created(), errorEvent}},
		{"bare_terminal", weatherRequest(), []string{s2Created(), sseEvent("response.completed", map[string]any{"type": "response.completed"})}},
		{"bare_terminal_after_relay", weatherRequest(), []string{h21ItemEvent("response.output_item.done", 0, paris), sseEvent("response.completed", map[string]any{"type": "response.completed"})}},
		{"completed_status_mismatch", weatherRequest(), []string{s2Created(), s2Response("response.completed", map[string]any{"status": "in_progress", "output": []any{message}})}},
		{"trailing_junk_event", weatherRequest(), []string{s2Created(), "event: response.output_text.delta\ndata: " + string(jsonBytes(map[string]any{"type": "response.output_text.delta", "output_index": 0, "delta": "x"})) + " {}\n\n", h21Terminal("response.completed")}},
		{"relay_parallel_false_two", parallelFalse, []string{h21ItemEvent("response.output_item.done", 0, paris), h21ItemEvent("response.output_item.done", 1, oslo), h21Terminal("response.completed", paris, oslo)}},
		{"required_text_only", required, []string{textDone, h21Terminal("response.completed", message)}},
		{"relay_custom", s2CustomRequest(), []string{h21ItemEvent("response.output_item.done", 0, customFirst), h21Terminal("response.completed", customFirst)}},
		{"relay_custom_alias_id", s2CustomRequest(), []string{h21ItemEvent("response.output_item.done", 0, customFirst), h21Terminal("response.completed", customLast)}},
		{"relay_duplicate_call_id", weatherRequest(), []string{h21ItemEvent("response.output_item.done", 0, paris), h21ItemEvent("response.output_item.done", 1, duplicate)}},
		{"reasoning_then_bad", weatherRequest(), []string{reasoning, h21ItemEvent("response.output_item.done", 1, bad)}},
		{"native_lite_call", s2LiteWeatherRequest(), []string{s2Created(), h21ItemEvent("response.output_item.added", 0, nativeAdded), nativeDelta("fc_native", `{"city":"Paris"}`), h21ItemEvent("response.output_item.done", 0, native), h21Terminal("response.completed", native)}},
		{"native_choice_none", choiceNone, []string{h21ItemEvent("response.output_item.added", 0, nativeAdded), h21ItemEvent("response.output_item.done", 0, native), h21Terminal("response.completed", native)}},
		{"native_forced_other", forcedOther, []string{h21ItemEvent("response.output_item.done", 0, native), h21Terminal("response.completed", native)}},
		{"native_parallel_false_two", liteParallelFalse, []string{h21ItemEvent("response.output_item.done", 0, native), h21ItemEvent("response.output_item.done", 1, nativeTwo), h21Terminal("response.completed", native, nativeTwo)}},
		{"native_delta_wrong_id", s2LiteWeatherRequest(), []string{h21ItemEvent("response.output_item.added", 0, nativeAdded), nativeDelta("fc_WRONG", "x"), h21ItemEvent("response.output_item.done", 0, native), h21Terminal("response.completed", native)}},
		{"native_orphan_delta_wrong_id", s2LiteWeatherRequest(), []string{nativeDelta("fc_WRONG", "x"), h21ItemEvent("response.output_item.done", 0, native), h21Terminal("response.completed", native)}},
		{"native_terminal_changed", s2LiteWeatherRequest(), []string{h21ItemEvent("response.output_item.done", 0, native), h21Terminal("response.completed", nativeChanged)}},
		{"native_terminal_missing", s2LiteWeatherRequest(), []string{h21ItemEvent("response.output_item.done", 0, native), h21Terminal("response.completed")}},
		{"native_invalid_json_args", s2LiteWeatherRequest(), []string{h21ItemEvent("response.output_item.done", 0, nativeBadJSON), h21Terminal("response.completed", nativeBadJSON)}},
		{"native_then_relay", weatherRequest(), []string{h21ItemEvent("response.output_item.done", 0, native), h21ItemEvent("response.output_item.done", 1, oslo), h21Terminal("response.completed", native, oslo)}},
		// E3: h22 rejected these; h25 converts them safely.
		{"e3_equivalent_number", s2NumericRequest(), []string{h21ItemEvent("response.output_item.done", 0, equivalentA), h21Terminal("response.completed", equivalentB)}},
		{"e3_terminal_missing_id", weatherRequest(), []string{h21ItemEvent("response.output_item.done", 0, paris), h21Terminal("response.completed", missingID)}},
		{"e3_scalar_string", s2NumericRequest(), []string{h21ItemEvent("response.output_item.done", 0, coerced), h21Terminal("response.completed", coerced)}},
	}
}

func s2TransformFixtures() []s2TransformFixture {
	paris := h21Native("t_paris", "get_weather", map[string]any{"city": "Paris"})
	oslo := h21Native("t_oslo", "get_weather", map[string]any{"city": "Oslo"})
	unfinished := cloneObject(paris)
	unfinished["status"] = "incomplete"
	native := s2NativeCall("t_native", `{"city":"Paris"}`)
	choiceNone := weatherRequest()
	choiceNone["tool_choice"] = "none"
	required := weatherRequest()
	required["tool_choice"] = "required"
	forced := weatherRequest()
	forced["tool_choice"] = map[string]any{"type": "function", "name": "get_weather"}
	parallelFalse := weatherRequest()
	parallelFalse["parallel_tool_calls"] = false
	response := func(status string, items ...any) map[string]any {
		return map[string]any{"id": "resp_s2_t", "status": status, "output": items}
	}
	return []s2TransformFixture{
		{"completed_relay", weatherRequest(), response("completed", paris)},
		{"completed_native_choice_none", choiceNone, response("completed", native)},
		{"completed_required_text_only", required, response("completed", messageItem("assistant", "text only"))},
		{"completed_forced_two", forced, response("completed", paris, oslo)},
		{"completed_parallel_false_two", parallelFalse, response("completed", paris, oslo)},
		{"failed_with_finished_relay", weatherRequest(), response("failed", messageItem("assistant", "partial"), paris)},
		{"incomplete_with_finished_relay", weatherRequest(), response("incomplete", paris)},
		{"incomplete_with_unfinished_relay", weatherRequest(), response("incomplete", unfinished)},
		{"completed_bad_relay", weatherRequest(), response("completed", h21Native("t_bad", "NOT_DECLARED", map[string]any{}))},
	}
}
