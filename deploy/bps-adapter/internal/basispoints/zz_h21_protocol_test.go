package basispoints

import (
	"strings"
	"testing"
)

func h21Native(id, tool string, args any) map[string]any {
	return map[string]any{
		"type": "function_call", "name": transportName, "id": "fc_" + id,
		"call_id": id, "status": "completed",
		"arguments": string(jsonBytes(map[string]any{"code": string(jsonBytes(map[string]any{"tool": tool, "args": args}))})),
	}
}

func h21Response(items ...any) []byte {
	return jsonBytes(map[string]any{"id": "resp_h21", "status": "completed", "output": items})
}

func TestH21TransformsAllCallsPreservingNativeOrder(t *testing.T) {
	source := weatherRequest()
	native := map[string]any{"type": "function_call", "name": "native_search", "call_id": "direct", "arguments": "{}"}
	text := messageItem("assistant", "Working")
	a := h21Native("one", "get_weather", map[string]any{"city": "Singapore"})
	b := h21Native("two", "get_weather", map[string]any{"city": "Tokyo"})
	body, response, changed, err := transformResponseBody(h21Response(text, a, native, b), source)
	if err != nil || !changed || strings.Contains(string(body), `"name":"run_officejs"`) {
		t.Fatalf("batch conversion failed: changed=%v err=%v", changed, err)
	}
	items := response["output"].([]any)
	if len(items) != 4 || stringValue(objectValue(items[1])["call_id"]) != "one" || stringValue(objectValue(items[3])["call_id"]) != "two" || string(jsonBytes(items[2])) != string(jsonBytes(native)) {
		t.Fatal("call order, count, or native item changed")
	}
}

func TestH21RejectsBadBatchWithoutCachingOrPayloadLeak(t *testing.T) {
	for _, kind := range []string{"unknown", "wrong_namespace", "javascript", "trailing_json", "wrong_schema", "incomplete_item", "no_tools", "choice_none", "choice_other"} {
		t.Run(kind, func(t *testing.T) {
			source := weatherRequest()
			source["input"] = []any{messageItem("user", "h21-"+kind)}
			first := h21Native("undelivered_"+kind, "get_weather", map[string]any{"city": "Singapore"})
			bad := h21Native("bad", "not_declared_PRIVATE", map[string]any{"city": "PRIVATE_ARGUMENT"})
			switch kind {
			case "wrong_namespace":
				bad = h21Native("bad", "functions.get_weather", map[string]any{"city": "Singapore"})
			case "javascript":
				bad["arguments"] = `{"code":"PRIVATE_JAVASCRIPT()"}`
			case "trailing_json":
				bad["arguments"] = string(jsonBytes(map[string]any{"code": `{"tool":"get_weather","args":{"city":"Singapore"}} {"private":1}`}))
			case "wrong_schema":
				bad = h21Native("bad", "get_weather", map[string]any{"city": 42})
			case "incomplete_item":
				bad = h21Native("bad", "get_weather", map[string]any{"city": "Singapore"})
				bad["status"] = "incomplete"
			case "no_tools":
				delete(source, "tools")
			case "choice_none":
				source["tool_choice"] = "none"
			case "choice_other":
				source["tool_choice"] = map[string]any{"type": "function", "name": "not_weather"}
			}
			body, response, _, err := transformResponseBody(h21Response(first, bad), source)
			if err == nil || body != nil || response != nil || strings.Contains(err.Error(), "PRIVATE") || strings.Contains(err.Error(), "run_officejs") {
				t.Fatalf("invalid call must fail without payload: err=%v", err)
			}
			if rememberedNativeCall(conversationScope(source), stringValue(first["call_id"])) != nil {
				t.Fatal("failed batch cached an undelivered call")
			}
		})
	}
}

func TestH21DynamicCatalogueOnlyUsedForOutput(t *testing.T) {
	for _, itemType := range []string{"additional_tools", "tool_search_output"} {
		t.Run(itemType, func(t *testing.T) {
			source := weatherRequest()
			source["input"] = []any{map[string]any{"type": itemType, "tools": source["tools"]}, messageItem("user", "weather")}
			delete(source, "tools")
			before := string(jsonBytes(source))
			if len(clientToolSpecs(source)) != 0 || len(relayOutputToolSpecs(source)) != 1 {
				t.Fatal("native and relay catalogues were not kept separate")
			}
			// h22's assertion, restored: the Lite prologue is h22's text. The one
			// change H25 allows (E2) is tool_search_output without
			// additional_tools, where h22 said "answer as text".
			if itemType == "additional_tools" && clientToolProtocolInstructionsFor(source, true) != plainAdditional {
				t.Fatal("Lite prologue changed")
			}
			if itemType == "tool_search_output" && clientToolProtocolInstructionsFor(source, true) != plainSearchedTools {
				t.Fatal("discovered-only tools did not get the E2 prologue")
			}
			// rc2's assertion, with rc2's prologue switched on
			// (relay_tool_contracts: full).
			full := s2Source(source, func(cfg *Config) { cfg.RelayToolContracts = relayToolContractsFull })
			if hint := clientToolProtocolInstructionsFor(full, true); !strings.Contains(hint, "Use these directly") || strings.Contains(hint, "Return the answer as assistant text") {
				t.Fatal("declared native/discovered tools were treated as unavailable or relayed")
			}
			body, _, changed, err := transformResponseBody(h21Response(h21Native("dynamic", "get_weather", map[string]any{"city": "Singapore"})), source)
			if err != nil || !changed || strings.Contains(string(body), `"name":"run_officejs"`) || string(jsonBytes(source)) != before {
				t.Fatalf("dynamic tool conversion failed or mutated request: %v", err)
			}
		})
	}
}

func TestH21CustomNamespaceIDAndReplay(t *testing.T) {
	input := "  first line\n\tsecond line  "
	source := map[string]any{"input": []any{messageItem("user", "h21-custom")}, "tools": []any{map[string]any{"type": "namespace", "name": "functions", "tools": []any{map[string]any{"type": "custom", "name": "apply_patch"}}}}}
	native := h21Native("custom_"+strings.Repeat("a", 80), "functions.apply_patch", input)
	_, response, _, err := transformResponseBody(h21Response(native), source)
	if err != nil {
		t.Fatal(err)
	}
	call := objectValue(response["output"].([]any)[0])
	id := stringValue(call["id"])
	if !strings.HasPrefix(id, "ctc_") || len(id) > 64 || stringValue(call["name"]) != "apply_patch" || stringValue(call["namespace"]) != "functions" || call["input"] != input || call["call_id"] != native["call_id"] {
		t.Fatal("custom tool identity, namespace, or whitespace was corrupted")
	}
	// A cold cache cannot prove that a namespaced call used the relay. Keep
	// the caller's item and result intact rather than guessing their origin.
	history := []any{call, map[string]any{"type": "custom_tool_call_output", "call_id": call["call_id"], "output": "ok"}}
	replayed := translateInputItems(history, clientToolSpecs(source), "h21-uncached-scope")
	if string(jsonBytes(replayed)) != string(jsonBytes(history)) {
		t.Fatal("unknown-origin namespaced history was rewritten without a cached relay identity")
	}
	// The original conversation still has a proven native relay mapping.
	warm := translateInputItems(history, clientToolSpecs(source), conversationScope(source))
	if string(jsonBytes(warm[0])) != string(jsonBytes(native)) || stringValue(objectValue(warm[1])["type"]) != "function_call_output" {
		t.Fatal("cached namespaced relay history lost its original native call or result pairing")
	}
	// A native custom call without a relay catalogue must remain native.
	nativeCustom := map[string]any{"type": "custom_tool_call", "name": "native_patch", "id": "ctc_native", "call_id": "native_id", "input": input}
	nativeHistory := translateInputItems([]any{nativeCustom}, map[string]toolSpec{}, "h21-native-scope")
	if string(jsonBytes(nativeHistory[0])) != string(jsonBytes(nativeCustom)) {
		t.Fatal("native custom history was incorrectly converted to relay history")
	}
}

func TestH21RejectsDuplicateCallIDsBeforeCaching(t *testing.T) {
	source := weatherRequest()
	source["input"] = []any{messageItem("user", "h21-duplicate-calls")}
	one := h21Native("duplicate", "get_weather", map[string]any{"city": "Singapore"})
	two := h21Native("duplicate", "get_weather", map[string]any{"city": "Tokyo"})
	body, _, _, err := transformResponseBody(h21Response(one, two), source)
	if err == nil || body != nil || rememberedNativeCall(conversationScope(source), "duplicate") != nil {
		t.Fatal("conflicting call ids must not be delivered or cached")
	}
	// A native call can collide with a relay call too.
	two["name"] = "native_tool"
	body, _, _, err = transformResponseBody(h21Response(one, two), source)
	if err == nil || body != nil {
		t.Fatal("native/relay call id collision accepted")
	}
}

func TestH21NumericEnumsRemainExact(t *testing.T) {
	for _, tc := range []struct {
		left, right string
		equal       bool
	}{
		{"1e3", "1000", true}, {"-0", "0.0", true}, {"0.100", "1e-1", true},
		{"9007199254740993", "9007199254740992", false},
		{"1e100000000", "10e99999999", true},
	} {
		left, right := parseArguments(`{"n":` + tc.left + `}`)["n"], parseArguments(`{"n":` + tc.right + `}`)["n"]
		if enumValuesEqual(left, right) != tc.equal {
			t.Fatalf("incorrect comparison for %s, %s", tc.left, tc.right)
		}
	}
	if !schemaMatches(parseArguments(`{"n":1e3}`), map[string]any{"type": "object", "properties": map[string]any{"n": map[string]any{"enum": []any{float64(1000)}}}}) {
		t.Fatal("scientific notation regressed after preserving JSON numbers")
	}
}

func TestH21PreservesLargeIntegersInCompletedResponse(t *testing.T) {
	source := map[string]any{"tools": []any{map[string]any{"type": "function", "name": "lookup", "parameters": map[string]any{"type": "object"}}}}
	native := h21Native("large", "lookup", parseArguments(`{"id":9007199254740993}`))
	raw := map[string]any{"status": "completed", "output": []any{native}}
	body, response, _, err := transformResponseBody(jsonBytes(raw), source)
	if err != nil || !strings.Contains(string(body), "9007199254740993") || response["status"] != "completed" {
		t.Fatalf("number or completed status changed: %v", err)
	}
}

func TestH21DynamicPrecedenceAndToolChoice(t *testing.T) {
	decl := func(name, typ string) any { return map[string]any{"type": typ, "name": name} }
	source := map[string]any{"tools": []any{decl("same", "function")}, "input": []any{
		map[string]any{"type": "additional_tools", "tools": []any{decl("same", "custom")}},
		map[string]any{"type": "tool_search_output", "tools": []any{decl("same", "function"), decl("searched", "custom")}},
	}}
	if specs := relayOutputToolSpecs(source); specs["same"].Type != "custom" || specs["searched"].Type != "custom" {
		t.Fatal("explicit dynamic declarations must override top-level and search fallback")
	}
	source["tool_choice"] = map[string]any{"type": "allowed_tools", "mode": "auto", "tools": []any{decl("searched", "custom")}}
	if specs := relayOutputToolSpecs(source); len(specs) != 1 || specs["searched"].Name != "searched" {
		t.Fatal("tool_choice was not enforced")
	}
}

func TestH21InstructionsDoNotInventExampleTools(t *testing.T) {
	source := map[string]any{"tools": []any{map[string]any{"type": "namespace", "name": "workspace", "tools": []any{
		map[string]any{"type": "function", "name": "run", "parameters": map[string]any{"type": "object", "properties": map[string]any{"script": map[string]any{"type": "string"}}}},
		map[string]any{"type": "custom", "name": "patch"},
	}}}}
	for _, text := range []string{clientToolProtocolInstructionsFor(source, true), clientToolProtocolInstructionsFor(source, false), clientToolProtocolReminder(source)} {
		if strings.Contains(text, "exec_command") || strings.Contains(text, "pwd") || !strings.Contains(text, "workspace.run") || !strings.Contains(text, "workspace.patch") || !strings.Contains(text, "args") {
			t.Fatal("tool instructions invented an unavailable tool or lost the full namespace")
		}
	}
}
