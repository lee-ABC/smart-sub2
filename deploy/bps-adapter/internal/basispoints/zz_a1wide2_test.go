package basispoints

// a1wide2: relay_no_catalog_transport (no_catalog_transport.go) and the
// promotion of forced tool choices (a1_promotion.go).

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// ---------------------------------------------------------------- helpers

type a2Event struct {
	name    string
	payload map[string]any
}

// a2Events parses what the client received, sequence numbers included.
func a2Events(t *testing.T, raw string) []a2Event {
	t.Helper()
	var events []a2Event
	for _, block := range strings.Split(strings.ReplaceAll(raw, "\r\n", "\n"), "\n\n") {
		name, data := "", ""
		for _, line := range strings.Split(block, "\n") {
			switch {
			case strings.HasPrefix(line, "event: "):
				name = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				data = strings.TrimPrefix(line, "data: ")
			}
		}
		if data == "" {
			continue
		}
		if strings.TrimSpace(data) == "[DONE]" {
			events = append(events, a2Event{name: "[DONE]"})
			continue
		}
		decoder := json.NewDecoder(strings.NewReader(data))
		decoder.UseNumber()
		var payload map[string]any
		if err := decoder.Decode(&payload); err != nil {
			t.Fatalf("client event is not JSON: %q", data)
		}
		events = append(events, a2Event{name: name, payload: payload})
	}
	return events
}

func a2Count(events []a2Event, name string) int {
	count := 0
	for _, event := range events {
		if event.name == name {
			count++
		}
	}
	return count
}

func a2Terminal(t *testing.T, events []a2Event, name string) map[string]any {
	t.Helper()
	for _, event := range events {
		if event.name == name {
			return objectValue(event.payload["response"])
		}
	}
	t.Fatalf("no %s in %v", name, events)
	return nil
}

func a2Body(t *testing.T, body string) map[string]any {
	t.Helper()
	decoder := json.NewDecoder(strings.NewReader(body))
	decoder.UseNumber()
	var document map[string]any
	if err := decoder.Decode(&document); err != nil {
		t.Fatalf("upstream body is not JSON: %v", err)
	}
	return document
}

// a2JSCall is a run_officejs call carrying Office.js, as a tool-less request
// gets it when the model reaches for the workbook.
func a2JSCall(callID string) map[string]any {
	return s5Transport(callID, "await Excel.run(async (context) => { const s = context.workbook.worksheets.getActiveWorksheet(); s.getRange(\"A1\").values = [[\"x\"]]; await context.sync(); });", []any{})
}

// a2Compaction is a Codex compaction request: no tools, a history of tool
// calls, the summary prompt last.
func a2Compaction(key string) map[string]any {
	return s5Body(key, s5User("fix the build"),
		s5Echo("call_a2_h1", "", "exec_command", `{"cmd":"go build ./..."}`), s5Output("call_a2_h1", "ok"),
		s5Assistant("msg_a2_h1", "Built."),
		s5User("You are performing a CONTEXT CHECKPOINT COMPACTION. Create a handoff summary."))
}

const a2Continue = "relay_no_catalog_transport: continue\n"

// ---------------------------------------------------------------- no-catalog transport

func TestA2NoCatalogTransportContinuesTheTurn(t *testing.T) {
	s5ResetCaches()
	for _, stream := range []bool{true, false} {
		h := newA1Harness(t, a2Continue)
		before := relayCounterSnapshot()
		first := s5Stream("r_a2_1").reasoning("Let me check the sheet.").call(a2JSCall("call_a2_js")).completed()
		second := s5Stream("r_a2_2").reasoning("No workbook.").text("Summary: the build is fixed.").completed()
		result := h.run(a2Compaction("a2-continue"), stream, func(call int, _ string) a1Reply {
			if call == 0 {
				return a1Reply{chunks: first}
			}
			return a1Reply{chunks: second}
		})
		if result.closeErr != "" || result.kind != "" || len(result.bodies) != 2 {
			t.Fatalf("stream=%v: close %q kind %q message %q, %d upstream calls", stream, result.closeErr, result.kind, result.message, len(result.bodies))
		}
		if strings.Contains(result.raw+result.payload, "run_officejs") || strings.Contains(result.raw+result.payload, "Excel.run") {
			t.Fatalf("stream=%v: the withheld call reached the client: %s%s", stream, result.raw, result.payload)
		}
		var response map[string]any
		if stream {
			events := a2Events(t, result.raw)
			if a2Count(events, "response.created") != 1 || a2Count(events, "response.in_progress") != 0 ||
				a2Count(events, "response.completed") != 1 || a2Count(events, "[DONE]") != 1 {
				t.Fatalf("lifecycle: %s", result.raw)
			}
			// Sequence numbers run on; output indexes follow what the client
			// already holds.
			var indexes []int
			for position, event := range events {
				if event.name == "[DONE]" {
					continue
				}
				if got := int(numberValue(event.payload["sequence_number"])); got != position {
					t.Fatalf("sequence_number %d at position %d", got, position)
				}
				if event.name == "response.output_item.added" {
					indexes = append(indexes, eventOutputIndex(event.payload))
				}
			}
			if !reflect.DeepEqual(indexes, []int{0, 1, 2}) {
				t.Fatalf("output indexes %v", indexes)
			}
			response = a2Terminal(t, events, "response.completed")
		} else {
			response = a2Body(t, result.payload)
		}
		if response["id"] != "r_a2_1" {
			t.Fatalf("stream=%v: response id %v, want the first one", stream, response["id"])
		}
		output, _ := response["output"].([]any)
		var ids []string
		for _, item := range output {
			ids = append(ids, stringValue(objectValue(item)["id"]))
		}
		if strings.Join(ids, " ") != "rs_r_a2_1_0 rs_r_a2_2_0 msg_r_a2_2_1" {
			t.Fatalf("stream=%v: output %v", stream, ids)
		}
		usage := objectValue(response["usage"])
		if numberValue(usage["input_tokens"]) != 2400 || numberValue(usage["output_tokens"]) != 80 || numberValue(usage["total_tokens"]) != 2480 ||
			numberValue(objectValue(usage["input_tokens_details"])["cached_tokens"]) != 2048 ||
			numberValue(objectValue(usage["output_tokens_details"])["reasoning_tokens"]) != 24 {
			t.Fatalf("stream=%v: usage %v", stream, usage)
		}
		// The continuation is the first body carried on: same everything,
		// the response's output and one answer appended, one more iteration.
		one, two := a2Body(t, result.bodies[0]), a2Body(t, result.bodies[1])
		inputOne, _ := one["input"].([]any)
		inputTwo, _ := two["input"].([]any)
		if len(inputTwo) != len(inputOne)+3 || !reflect.DeepEqual(inputTwo[:len(inputOne)], inputOne) {
			t.Fatalf("stream=%v: continuation input does not extend the first (%d -> %d items)", stream, len(inputOne), len(inputTwo))
		}
		tail := inputTwo[len(inputOne):]
		reasoning, call, answer := objectValue(tail[0]), objectValue(tail[1]), objectValue(tail[2])
		if !reflect.DeepEqual(reasoning, map[string]any{"type": "reasoning", "summary": []any{}, "encrypted_content": "gAAAA-rs_r_a2_1_0"}) {
			t.Fatalf("stream=%v: reasoning carried as %v", stream, reasoning)
		}
		if call["name"] != "run_officejs" || call["call_id"] != "call_a2_js" {
			t.Fatalf("stream=%v: call carried as %v", stream, call)
		}
		if !reflect.DeepEqual(answer, map[string]any{"type": "function_call_output", "call_id": "call_a2_js", "output": noCatalogToolOutput}) {
			t.Fatalf("stream=%v: answer %v", stream, answer)
		}
		for key := range one {
			if key == "input" || key == "metadata" {
				continue
			}
			if !reflect.DeepEqual(one[key], two[key]) {
				t.Fatalf("stream=%v: %s changed: %v -> %v", stream, key, one[key], two[key])
			}
		}
		metaOne, metaTwo := objectValue(one["metadata"]), objectValue(two["metadata"])
		if metaOne["agent_iteration"] == nil || metaTwo["agent_iteration"] == metaOne["agent_iteration"] ||
			metaTwo["turn_id"] != metaOne["turn_id"] || metaTwo["task_id"] != metaOne["task_id"] {
			t.Fatalf("stream=%v: metadata %v -> %v", stream, metaOne, metaTwo)
		}
		delta := a1CounterDelta(before)
		for key, want := range map[string]uint64{
			"no_catalog_transport:officejs": 1, "no_catalog_continuation:started": 1, "no_catalog_continuation:answered": 1,
			"relay_requests:legacy:none": 1, "relay_outcome:legacy:none:succeeded": 1,
		} {
			if delta[key] != want {
				t.Errorf("stream=%v: %s = %d, want %d (all: %v)", stream, key, delta[key], want, delta)
			}
		}
		for key := range delta {
			if strings.HasPrefix(key, "relay_call:") || strings.Contains(key, "code_json") {
				t.Errorf("stream=%v: the withheld call was decoded as a relay call: %s", stream, key)
			}
		}
	}
}

// With client_shapes (production: client_identity thread), the continuation
// carries the reasoning item in the add-in's own shape, as the history does.
func TestA2NoCatalogTransportClientShapes(t *testing.T) {
	s5ResetCaches()
	h := newA1Harness(t, "  mode: thread\n  client_shapes: true\n"+a2Continue)
	first := s5Stream("r_a2_cs").reasoning("Let me check.").call(a2JSCall("call_a2_cs")).completed()
	result := h.run(a2Compaction("a2-shapes"), true, func(call int, _ string) a1Reply {
		if call == 0 {
			return a1Reply{chunks: first}
		}
		return a1Text("r_a2_cs2", "Summary.")
	})
	if result.closeErr != "" || len(result.bodies) != 2 {
		t.Fatalf("close %q, %d calls", result.closeErr, len(result.bodies))
	}
	one, two := a2Body(t, result.bodies[0]), a2Body(t, result.bodies[1])
	inputOne, _ := one["input"].([]any)
	inputTwo, _ := two["input"].([]any)
	reasoning := objectValue(inputTwo[len(inputOne)])
	if reasoning["id"] != "rs_r_a2_cs_0" || reasoning["encrypted_content"] != "gAAAA-rs_r_a2_cs_0" {
		t.Fatalf("reasoning %v", reasoning)
	}
	if summary, _ := reasoning["summary"].([]any); len(summary) != 1 {
		t.Fatalf("reasoning summary %v", reasoning["summary"])
	}
}

// error (the default) is h22: the call is decoded, fails, one upstream call.
func TestA2NoCatalogTransportErrorIsH22(t *testing.T) {
	s5ResetCaches()
	for _, config := range []string{"", "relay_no_catalog_transport: error\n"} {
		h := newA1Harness(t, config)
		first := s5Stream("r_a2_err").reasoning("Hm.").call(a2JSCall("call_a2_err")).completed()
		result := h.run(a2Compaction("a2-error"), true, a1Always(a1Reply{chunks: first}))
		if !strings.Contains(result.closeErr, "reason=code_json") || len(result.bodies) != 1 {
			t.Fatalf("config %q: close %q, %d upstream calls", config, result.closeErr, len(result.bodies))
		}
	}
}

// What a continuation cannot answer fails as h22 did, without a second call.
func TestA2NoCatalogTransportFallsBackToH22(t *testing.T) {
	read := s5Native("call_a2_read", "", "read_ranges", `{"ranges":["A1"]}`)
	for name, chunks := range map[string][]string{
		"not_trailing": s5Stream("r_a2_nt").reasoning("Hm.").call(a2JSCall("call_a2_nt")).text("Done anyway.").completed(),
		"call_open":    s5Stream("r_a2_co").reasoning("Hm.").call(read).call(a2JSCall("call_a2_co")).completed(),
	} {
		s5ResetCaches()
		h := newA1Harness(t, a2Continue)
		before := relayCounterSnapshot()
		result := h.run(a2Compaction("a2-fallback-"+name), true, a1Always(a1Reply{chunks: chunks}))
		if !strings.Contains(result.closeErr, "reason=code_json") || len(result.bodies) != 1 {
			t.Fatalf("%s: close %q, %d upstream calls", name, result.closeErr, len(result.bodies))
		}
		if delta := a1CounterDelta(before); delta["no_catalog_continuation:"+name] != 1 || delta["no_catalog_continuation:started"] != 0 {
			t.Fatalf("%s: counters %v", name, delta)
		}
	}
}

// The continuation limit: a model that keeps calling run_officejs gets two
// more upstream calls, then the request fails as h22 did.
func TestA2NoCatalogTransportLimit(t *testing.T) {
	s5ResetCaches()
	h := newA1Harness(t, a2Continue)
	before := relayCounterSnapshot()
	result := h.run(a2Compaction("a2-limit"), true, func(call int, _ string) a1Reply {
		id := "r_a2_limit_" + string(rune('a'+call))
		return a1Reply{chunks: s5Stream(id).reasoning("Again.").call(a2JSCall("call_" + id)).completed()}
	})
	if !strings.Contains(result.closeErr, "invalid_tool_call") || len(result.bodies) != noCatalogContinuationLimit+1 {
		t.Fatalf("close %q, %d upstream calls", result.closeErr, len(result.bodies))
	}
	events := a2Events(t, result.raw)
	if a2Count(events, "response.created") != 1 || a2Count(events, "response.completed") != 0 || strings.Contains(result.raw, "run_officejs") {
		t.Fatalf("client stream: %s", result.raw)
	}
	delta := a1CounterDelta(before)
	if delta["no_catalog_continuation:started"] != noCatalogContinuationLimit || delta["no_catalog_continuation:limit"] != 1 || delta["no_catalog_transport:officejs"] != 3 {
		t.Fatalf("counters %v", delta)
	}
}

// A failed or incomplete upstream turn is passed on without the call.
func TestA2NoCatalogTransportUnsuccessfulTerminal(t *testing.T) {
	s5ResetCaches()
	h := newA1Harness(t, a2Continue)
	chunks := s5Stream("r_a2_inc").reasoning("Hm.").call(a2JSCall("call_a2_inc")).finish("response.incomplete", map[string]any{"incomplete_details": map[string]any{"reason": "max_output_tokens"}})
	result := h.run(a2Compaction("a2-incomplete"), true, a1Always(a1Reply{chunks: chunks}))
	if result.closeErr != "" || len(result.bodies) != 1 || strings.Contains(result.raw, "run_officejs") {
		t.Fatalf("close %q, %d calls: %s", result.closeErr, len(result.bodies), result.raw)
	}
	response := a2Terminal(t, a2Events(t, result.raw), "response.incomplete")
	if output, _ := response["output"].([]any); len(output) != 1 {
		t.Fatalf("output %v", response["output"])
	}
}

// A request with client tools is untouched by the switch: its run_officejs
// call is a relay call, decoded and delivered (or failed) as before.
func TestA2NoCatalogTransportLeavesRelayAndLiteAlone(t *testing.T) {
	s5ResetCaches()
	h := newA1Harness(t, a2Continue)
	bad := s5Transport("call_a2_relay_bad", "{not json", []any{})
	result := h.run(a1Relay("a2-relay", s5User("go")), true, a1Always(a1Reply{chunks: s5Stream("r_a2_relay").call(bad).completed()}))
	if !strings.Contains(result.closeErr, "reason=code_json") || len(result.bodies) != 1 {
		t.Fatalf("relay: close %q, %d upstream calls", result.closeErr, len(result.bodies))
	}
	good := s5Transport("call_a2_relay_ok", s5Code("exec_command", map[string]any{"cmd": "ls"}), []any{})
	result = h.run(a1Relay("a2-relay-ok", s5User("go")), true, a1Always(a1Reply{chunks: s5Stream("r_a2_relay_ok").call(good).completed()}))
	if result.closeErr != "" || !strings.Contains(result.raw, `"name":"exec_command"`) || len(result.bodies) != 1 {
		t.Fatalf("relay ok: close %q: %s", result.closeErr, result.raw)
	}
	lite := s5Lite("a2-lite", s5LiteDecl(), s5User("go"))
	result = h.run(lite, true, a1Always(a1Reply{chunks: s5Stream("r_a2_lite").call(bad).completed()}))
	if !strings.Contains(result.closeErr, "reason=code_json") || len(result.bodies) != 1 {
		t.Fatalf("lite: close %q, %d upstream calls", result.closeErr, len(result.bodies))
	}
}

// tool_choice "none" with tools declared leaves nothing callable either.
func TestA2NoCatalogTransportToolChoiceNone(t *testing.T) {
	s5ResetCaches()
	h := newA1Harness(t, a2Continue)
	request := s5Set(a1Relay("a2-none", s5User("just answer")), "tool_choice", "none")
	result := h.run(request, true, func(call int, _ string) a1Reply {
		if call == 0 {
			return a1Reply{chunks: s5Stream("r_a2_tcn").call(s5Transport("call_a2_tcn", s5Code("exec_command", map[string]any{"cmd": "ls"}), []any{})).completed()}
		}
		return a1Text("r_a2_tcn2", "Here is the answer.")
	})
	if result.closeErr != "" || len(result.bodies) != 2 || strings.Contains(result.raw, "exec_command") {
		t.Fatalf("close %q, %d calls: %s", result.closeErr, len(result.bodies), result.raw)
	}
	// Nothing reached the client before the continuation: its message is the
	// first item.
	response := a2Terminal(t, a2Events(t, result.raw), "response.completed")
	if output, _ := response["output"].([]any); len(output) != 1 || stringValue(objectValue(output[0])["type"]) != "message" {
		t.Fatalf("output %v", response["output"])
	}
}

// The continuation cannot be opened: the stream fails with the upstream's
// error, and the client has the first response's output only.
func TestA2NoCatalogTransportContinuationRefused(t *testing.T) {
	s5ResetCaches()
	h := newA1Harness(t, a2Continue)
	before := relayCounterSnapshot()
	result := h.run(a2Compaction("a2-refused"), true, func(call int, _ string) a1Reply {
		if call == 0 {
			return a1Reply{chunks: s5Stream("r_a2_ref").reasoning("Hm.").call(a2JSCall("call_a2_ref")).completed()}
		}
		return a1Reply{status: 400, body: `{"error":{"message":"bad input","type":"invalid_request_error","code":"invalid_value"}}`}
	})
	if result.closeErr == "" || len(result.bodies) != 2 || strings.Contains(result.raw, "run_officejs") {
		t.Fatalf("close %q, %d calls: %s", result.closeErr, len(result.bodies), result.raw)
	}
	if delta := a1CounterDelta(before); delta["no_catalog_continuation:open_failed"] != 1 {
		t.Fatalf("counters %v", delta)
	}
}

func TestA2NoCatalogShape(t *testing.T) {
	envelope := s5Code("exec_command", map[string]any{"cmd": "ls"})
	for code, want := range map[any]string{
		envelope:                          "json_envelope",
		`{"a":1}`:                         "json_other",
		`{"tool":"exec_command","args":{"cmd":"a\"b"}` + "x": "json_broken",
		"await Excel.run(async (c) => {})": "officejs",
		"const fs = require('fs');":        "node_js",
		"return 1 + 1;":                    "javascript",
		"just words":                       "text",
		"   ":                              "code_empty",
	} {
		if got := noCatalogShape(s5Transport("c", code, []any{})); got != want {
			t.Errorf("%q: %s, want %s", code, got, want)
		}
	}
	if got := noCatalogShape(map[string]any{"type": "function_call", "name": "run_officejs", "arguments": "{oops"}); got != "outer_unreadable" {
		t.Errorf("outer: %s", got)
	}
	if got := noCatalogShape(map[string]any{"type": "function_call", "name": "run_officejs", "arguments": `{"summary":"x"}`}); got != "code_missing" {
		t.Errorf("missing: %s", got)
	}
}

// ---------------------------------------------------------------- tool_choice promotion

func a2PromotedInput(t *testing.T, body string) []map[string]any {
	t.Helper()
	return a1BodyInput(t, body)
}

// a2CatalogueKeys lists "<namespace>.<name>" for every tool in the promoted
// additional_tools item.
func a2CatalogueKeys(t *testing.T, items []map[string]any) []string {
	t.Helper()
	_, declaration := a1FindItem(items, "additional_tools")
	if declaration == nil {
		t.Fatal("no additional_tools item")
	}
	var keys []string
	tools, _ := declaration["tools"].([]any)
	for _, raw := range tools {
		namespace := objectValue(raw)
		members, _ := namespace["tools"].([]any)
		for _, member := range members {
			keys = append(keys, stringValue(namespace["name"])+"."+stringValue(objectValue(member)["name"]))
		}
	}
	return keys
}

func a2LastMessageText(items []map[string]any) (string, string) {
	if len(items) == 0 {
		return "", ""
	}
	last := items[len(items)-1]
	content, _ := last["content"].([]any)
	if len(content) == 0 {
		return stringValue(last["role"]), ""
	}
	return stringValue(last["role"]), stringValue(objectValue(content[0])["text"])
}

func TestA2ToolChoicePromotion(t *testing.T) {
	a1ResetDenyCache()
	ns := func(name string) map[string]any { return map[string]any{"type": "function", "name": name} }
	cases := []struct {
		name    string
		choice  any
		keys    string
		notice  string
		counter string
	}{
		{"required", "required", "functions.exec_command functions.apply_patch functions.list_files mcp__docs.lookup",
			"Tool choice for this response is required: call exactly one of the client tools declared in additional_tools, directly as declared. A text-only answer does not satisfy this request.", "required"},
		{"function", map[string]any{"type": "function", "name": "list_files"}, "functions.list_files",
			"Tool choice for this response requires exactly one call to the client tool list_files, directly as declared in additional_tools. A text-only answer does not satisfy this request.", "function"},
		{"namespaced", map[string]any{"type": "function", "name": "lookup", "namespace": "mcp__docs"}, "mcp__docs.lookup",
			"Tool choice for this response requires exactly one call to the client tool lookup (namespace mcp__docs), directly as declared in additional_tools. A text-only answer does not satisfy this request.", "function"},
		{"custom", map[string]any{"type": "custom", "name": "apply_patch"}, "functions.apply_patch",
			"Tool choice for this response requires exactly one call to the client tool apply_patch, directly as declared in additional_tools. A text-only answer does not satisfy this request.", "custom"},
		{"allowed_auto", map[string]any{"type": "allowed_tools", "mode": "auto", "tools": []any{ns("exec_command"), map[string]any{"type": "function", "name": "lookup", "namespace": "mcp__docs"}, map[string]any{"type": "web_search"}}},
			"functions.exec_command mcp__docs.lookup", "", "allowed_tools_auto"},
		{"allowed_required", map[string]any{"type": "allowed_tools", "mode": "required", "tools": []any{ns("exec_command")}}, "functions.exec_command",
			"Tool choice for this response is required: call exactly one of the client tools declared in additional_tools, directly as declared. A text-only answer does not satisfy this request.", "allowed_tools_required"},
	}
	for _, c := range cases {
		s5ResetCaches()
		h := newA1Harness(t, "relay_native_promotion: on\nrelay_native_promotion_history: h2\n")
		before := relayCounterSnapshot()
		request := s5Set(a1Relay("a2-tc-"+c.name, s5User("do it")), "tool_choice", c.choice)
		result := h.run(request, true, a1Always(a1Text("r_a2_tc_"+c.name, "ok")))
		if result.closeErr != "" || len(result.bodies) != 1 {
			t.Fatalf("%s: close %q, %d calls", c.name, result.closeErr, len(result.bodies))
		}
		items := a2PromotedInput(t, result.bodies[0])
		if keys := strings.Join(a2CatalogueKeys(t, items), " "); keys != c.keys {
			t.Errorf("%s: catalogue %q, want %q", c.name, keys, c.keys)
		}
		role, text := a2LastMessageText(items)
		if c.notice == "" {
			if role == "developer" {
				t.Errorf("%s: unexpected notice %q", c.name, text)
			}
		} else if role != "developer" || text != c.notice {
			t.Errorf("%s: last item %s %q", c.name, role, text)
		}
		if strings.Contains(result.bodies[0], `"tool_choice"`) {
			t.Errorf("%s: tool_choice went upstream", c.name)
		}
		delta := a1CounterDelta(before)
		if delta["a1_requests:on:promoted"] != 1 || delta["a1_tool_choice:on:"+c.counter] != 1 || delta["a1_feature:on:tool_choice"] != 1 {
			t.Errorf("%s: counters %v", c.name, delta)
		}
	}
}

// Blocked, the request is the a1wide relay request, byte for byte.
func TestA2ToolChoiceBlockedIsA1wide(t *testing.T) {
	request := s5Set(a1Relay("a2-tc-blocked", s5User("do it")), "tool_choice", "required")
	bodies := map[string]string{}
	for name, config := range map[string]string{
		"blocked": "relay_native_promotion: on\nrelay_native_promotion_block: custom_format_other,tool_choice\n",
		"off":     "relay_native_promotion: off\n",
	} {
		s5ResetCaches()
		a1ResetDenyCache()
		h := newA1Harness(t, config)
		result := h.run(request, true, a1Always(a1Text("r_a2_tcb", "ok")))
		if len(result.bodies) != 1 {
			t.Fatalf("%s: %d calls", name, len(result.bodies))
		}
		bodies[name] = result.bodies[0]
	}
	if bodies["blocked"] != bodies["off"] {
		t.Fatal("a blocked tool_choice request is not the relay request")
	}
	if strings.Contains(bodies["blocked"], "additional_tools") || strings.Contains(bodies["blocked"], "Tool choice for this response") {
		t.Fatal("blocked request was promoted")
	}
}

// The notice goes before a trailing compaction_trigger, and the relay
// fallback body (a 400 before the stream) carries none of it.
func TestA2ToolChoiceNoticePlacementAndFallback(t *testing.T) {
	s5ResetCaches()
	a1ResetDenyCache()
	h := newA1Harness(t, "relay_native_promotion: on\nrelay_native_promotion_history: h2\n")
	request := s5Set(a1Relay("a2-tc-place", s5User("do it"), map[string]any{"type": "compaction_trigger"}), "tool_choice", "required")
	result := h.run(request, true, a1RefusePromoted("r_a2_tcf"))
	if len(result.bodies) != 2 {
		t.Fatalf("%d upstream calls", len(result.bodies))
	}
	promoted := a1BodyInput(t, result.bodies[0])
	if len(promoted) < 2 || stringValue(promoted[len(promoted)-1]["type"]) != "compaction_trigger" {
		t.Fatalf("compaction_trigger is not last: %v", promoted[len(promoted)-1])
	}
	role, text := a2LastMessageText(promoted[:len(promoted)-1])
	if role != "developer" || !strings.HasPrefix(text, "Tool choice for this response is required") {
		t.Fatalf("notice not before the trigger: %s %q", role, text)
	}
	if strings.Contains(result.bodies[1], "Tool choice for this response") || strings.Contains(result.bodies[1], "additional_tools") {
		t.Fatal("the relay fallback body carries the promotion")
	}
}

func TestA2ParseToolChoice(t *testing.T) {
	for name, c := range map[string]struct {
		raw  any
		kind string
	}{
		"absent":         {nil, a1ChoiceAuto},
		"auto":           {" Auto ", a1ChoiceAuto},
		"required":       {"required", a1ChoiceRequired},
		"none":           {"none", a1ChoiceUnsupported},
		"function":       {map[string]any{"type": "function", "name": "x"}, a1ChoiceFunction},
		"bad name":       {map[string]any{"type": "function", "name": "a.b"}, a1ChoiceUnsupported},
		"chat shape":     {map[string]any{"type": "function", "function": map[string]any{"name": "x"}}, a1ChoiceUnsupported},
		"allowed none":   {map[string]any{"type": "allowed_tools", "mode": "none", "tools": []any{}}, a1ChoiceUnsupported},
		"allowed broken": {map[string]any{"type": "allowed_tools", "mode": "auto"}, a1ChoiceUnsupported},
		"hosted":         {map[string]any{"type": "file_search"}, a1ChoiceUnsupported},
		"number":         {42, a1ChoiceUnsupported},
	} {
		if got := a1ParseToolChoice(c.raw).kind; got != c.kind {
			t.Errorf("%s: %q, want %q", name, got, c.kind)
		}
	}
}
