package basispoints

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func h24ReferencedCall(id string, refs any, code any) map[string]any {
	return map[string]any{"type": "function_call", "name": transportName, "id": "fc_" + id, "call_id": id, "status": "completed",
		"arguments": string(jsonBytes(map[string]any{"references": refs, "code": code}))}
}

func TestH24ReferencesCarryExactFunctionAndCustomPayload(t *testing.T) {
	source := map[string]any{"input": "new h24 conversation", "tools": []any{
		map[string]any{"type": "namespace", "name": "workspace", "tools": []any{
			map[string]any{"type": "function", "name": "dispatch", "parameters": map[string]any{"type": "object"}},
			map[string]any{"type": "custom", "name": "patch"},
		}},
		map[string]any{"type": "function", "name": "some_other_tool", "parameters": map[string]any{"type": "object"}},
	}}
	// A payload that is not a routing envelope is carried exactly.
	payload := `{"id":9007199254740993,"n":1e100000000}`
	call, reason := decodeTransportItemWithReason(h24ReferencedCall("h24_function", []any{"workspace.dispatch"}, payload), withRelayProtocolSelection(source, "references"))
	if reason != "" || call["name"] != "dispatch" || call["namespace"] != "workspace" || !equivalentToolArguments(call["arguments"].(string), payload) {
		t.Fatalf("lost reference or exact function payload: reason=%q", reason)
	}
	// H25-SPEC S1.1 reverses rc2 here. rc2 delivered a complete old envelope
	// {"tool":...,"args":...} as the referenced function's arguments (and as a
	// custom tool's raw input). h25 never delivers a routing envelope as a
	// payload in any mode: a complete envelope naming a declared tool is
	// decoded as that call, whatever references says -- which is also exactly
	// what h22 does with it.
	code := `{"tool":"some_other_tool","args":{"id":9007199254740993,"n":1e100000000}}`
	for _, mode := range []string{"auto", "legacy", "references"} {
		call, reason := decodeTransportItemWithReason(h24ReferencedCall("h24_function", []any{"workspace.dispatch"}, code), withRelayProtocolSelection(source, mode))
		if reason != "" || call["name"] != "some_other_tool" || call["namespace"] != nil || !equivalentToolArguments(call["arguments"].(string), `{"id":9007199254740993,"n":1e100000000}`) {
			t.Fatalf("mode=%s delivered the envelope as a payload or lost its exact arguments: reason=%q call=%v", mode, reason, call)
		}
	}
	for _, input := range []string{"", "   \t\n", "  *** Begin Patch\n+\"quote\" C:\\file\n*** End Patch  ", "123"} {
		call, reason := decodeTransportItemWithReason(h24ReferencedCall("h24_custom", []any{"workspace.patch"}, input), withRelayProtocolSelection(source, "references"))
		if reason != "" || call["type"] != "custom_tool_call" || call["input"] != input || call["namespace"] != "workspace" {
			t.Fatalf("custom bytes changed: reason=%q", reason)
		}
	}
	envelope := `{"tool":"some_other_tool","args":{"x":1}}`
	call, reason = decodeTransportItemWithReason(h24ReferencedCall("h24_custom", []any{"workspace.patch"}, envelope), withRelayProtocolSelection(source, "references"))
	if reason != "" || call["type"] != "function_call" || call["name"] != "some_other_tool" || call["arguments"] != `{"x":1}` {
		t.Fatalf("envelope reached a custom tool as raw input: reason=%q call=%v", reason, call)
	}
}

func TestH24LegacyEnvelopeAndReferenceAmbiguity(t *testing.T) {
	for _, mode := range []string{"auto", "legacy", "references"} {
		for _, withEmptyRefs := range []bool{false, true} {
			item := h21Native("h24_old", "get_weather", map[string]any{"city": "Singapore"})
			if withEmptyRefs {
				args := parseArguments(item["arguments"])
				args["references"] = []any{}
				item["arguments"] = string(jsonBytes(args))
			}
			if _, reason := decodeTransportItemWithReason(item, withRelayProtocolSelection(weatherRequest(), mode)); reason != "" {
				t.Fatalf("valid legacy mode=%s failed: %s", mode, reason)
			}
		}
	}
	legacy := h21Native("h24_descriptive", "get_weather", map[string]any{"city": "Singapore"})
	args := parseArguments(legacy["arguments"])
	args["references"] = []any{"Tokyo weather"}
	legacy["arguments"] = string(jsonBytes(args))
	if _, reason := decodeTransportItemWithReason(legacy, withRelayProtocolSelection(weatherRequest(), "legacy")); reason != "" {
		t.Fatal("confirmed legacy descriptive reference rejected", reason)
	}
	// H25-SPEC S1.1: references mode reads code first, so a complete old
	// envelope naming a declared tool decodes whatever references holds.
	if call, reason := decodeTransportItemWithReason(legacy, withRelayProtocolSelection(weatherRequest(), "references")); reason != "" || call["name"] != "get_weather" {
		t.Fatalf("descriptive references broke a valid old envelope: %q", reason)
	}
	for _, tc := range []struct {
		refs   any
		code   any
		reason string
	}{
		// H25-SPEC S1.1: exactly one DECLARED key selects the tool; repeats
		// and undeclared entries (file paths) no longer make it ambiguous.
		{[]any{"get_weather", "get_weather"}, `{"city":"S"}`, ""},
		{[]any{"get_weather", "other"}, `{"city":"S"}`, ""},
		{[]any{"src/a.go", "get_weather", "README.md"}, `{"city":"S"}`, ""},
		{"get_weather", `{"city":"S"}`, "invalid_references"},
		{nil, `{"city":"S"}`, "invalid_references"},
		{[]any{7}, `{"city":"S"}`, "invalid_references"},
		{[]any{" get_weather"}, `{"city":"S"}`, "invalid_references"},
		{[]any{transportName}, `{"city":"S"}`, "invalid_references"},
		{[]any{"get_weather"}, map[string]any{"city": "S"}, "code_json"},
		{[]any{"get_weather"}, `{"city":"S"} {}`, "invalid_arguments_json"},
		{[]any{"get_weather"}, `[]`, "invalid_arguments_json"},
	} {
		if _, got := decodeTransportItemWithReason(h24ReferencedCall("bad", tc.refs, tc.code), withRelayProtocolSelection(weatherRequest(), "references")); got != tc.reason {
			t.Fatalf("reason=%q, want %q", got, tc.reason)
		}
	}
	source := weatherRequest()
	source["tool_choice"] = "none"
	// rc2 asserted this under a frozen legacy selection. Legacy no longer
	// reads references at all (h22), so the reference is checked in
	// references mode; legacy fails it exactly as h22 does.
	if _, reason := decodeTransportItemWithReason(h24ReferencedCall("choice", []any{"get_weather"}, `{"city":"S"}`), withRelayProtocolSelection(source, "references")); reason != "tool_not_allowed" {
		t.Fatalf("valid but excluded reference bypassed tool_choice: %q", reason)
	}
	if _, reason := decodeTransportItemWithReason(h24ReferencedCall("choice", []any{"get_weather"}, `{"city":"S"}`), withRelayProtocolSelection(source, "legacy")); reason != "missing_tool_name" {
		t.Fatalf("legacy read references: %q", reason)
	}
}

func TestH24AutomaticProtocolMigrationAndFrozenSelection(t *testing.T) {
	source := weatherRequest()
	source["prompt_cache_key"] = "h24-protocol-selection"
	if got := selectRelayProtocol(source, "auto", conversationScope(source)); got != "references" {
		t.Fatalf("new conversation=%s", got)
	}
	old := h21Native("h24_history_old", "get_weather", map[string]any{"city": "S"})
	modern := h24ReferencedCall("h24_history_new", []any{"get_weather"}, `{"city":"S"}`)
	for _, tc := range []struct {
		name  string
		input []any
		want  string
	}{
		{"explicit_old", []any{old}, "legacy"},
		{"explicit_new", []any{modern}, "references"},
		{"mixed", []any{old, modern}, "legacy"},
		{"cold_client_call", []any{map[string]any{"type": "function_call", "name": "get_weather", "call_id": "h24_uncached", "arguments": "{}"}}, "legacy"},
		{"orphan_output", []any{map[string]any{"type": "function_call_output", "call_id": "h24_orphan", "output": "done"}}, "legacy"},
		{"opaque_only", []any{map[string]any{"type": "reasoning", "encrypted_content": "OPAQUE"}}, "legacy"},
		{"refs_with_reasoning", []any{modern, map[string]any{"type": "reasoning", "encrypted_content": "OPAQUE"}}, "references"},
		{"item_reference", []any{modern, map[string]any{"type": "item_reference", "id": "opaque"}}, "legacy"},
	} {
		s := cloneObject(source)
		s["input"] = tc.input
		if got := selectRelayProtocol(s, "auto", conversationScope(s)); got != tc.want {
			t.Errorf("%s=%s, want %s", tc.name, got, tc.want)
		}
	}
	call := map[string]any{"type": "function_call", "name": "get_weather", "call_id": "h24_sticky", "arguments": `{"city":"S"}`}
	source["input"] = []any{call}
	scope := conversationScope(source)
	old["call_id"] = "h24_sticky"
	rememberRelayCall(scope, old, call)
	selected := selectRelayProtocol(source, "auto", scope)
	frozen := withRelayProtocolSelection(source, selected)
	modern["call_id"] = "h24_sticky"
	rememberRelayCall(scope, modern, call)
	if selectRelayProtocol(source, "auto", scope) != "references" || relayProtocolForDecode(frozen) != "legacy" {
		t.Fatal("response decoder re-inferred a changed cache selection")
	}
	// auto is opt-in since h25: the forged-key check runs with it switched on.
	spoof := withRelayOptions(cloneObject(source), relayOptions{Protocol: relayProtocolAuto})
	spoof[relayProtocolSelectionKey] = "legacy"
	if relayProtocolForDecode(spoof) != "references" {
		t.Fatal("JSON client forged private protocol selection")
	}
}

func TestH24ProtocolPromptsAndHistoryStaySynchronized(t *testing.T) {
	for _, mode := range []string{"auto", "legacy", "references"} {
		source := weatherRequest()
		source["model"] = "gpt-6-astra"
		source["input"] = []any{messageItem("user", "new")}
		cfg := defaultConfig()
		cfg.RelayProtocol = mode
		body, err := prepareResponsesBody(source, cfg)
		if err != nil {
			t.Fatal(err)
		}
		wire := string(jsonBytes(body))
		if strings.Contains(wire, relayProtocolSelectionKey) || strings.Contains(wire, "relay_protocol") {
			t.Fatal("internal mode leaked outbound")
		}
		wantRefs := mode != "legacy"
		if strings.Contains(wire, "Set references to an array containing exactly one complete catalog tool key") != wantRefs {
			t.Fatalf("mode %s prompt disagrees", mode)
		}
		if wantRefs && strings.Contains(wire, "references=[]") {
			t.Fatal("new protocol mixed old references instructions")
		}
	}
	// New generation policy must not relax h22's namespaced cold-history rule.
	call := map[string]any{"type": "function_call", "name": "exec", "namespace": "workspace", "call_id": "h24_native_cold", "arguments": `{"x":1}`}
	output := map[string]any{"type": "function_call_output", "call_id": "h24_native_cold", "encrypted_content": "OPAQUE"}
	tools := map[string]toolSpec{"workspace.exec": {Key: "workspace.exec", Name: "exec", Namespace: "workspace", Type: "function"}}
	history := []any{call, output, map[string]any{"type": "compaction_trigger"}}
	if got := translateInputItemsFor(history, tools, "h24_native_scope", wireOptions{relayProtocol: "references"}); !reflect.DeepEqual(got, history) {
		t.Fatal("references mode changed opaque/native history or tail")
	}
}

func TestH24UnsuccessfulNonStreamDoesNotDeliverOrCacheTools(t *testing.T) {
	for _, status := range []string{"failed", "incomplete"} {
		source := weatherRequest()
		source["prompt_cache_key"] = "h24_unfinished_" + status
		native := h21Native("h24_not_executed_"+status, "get_weather", map[string]any{"city": "S"})
		plain := map[string]any{"type": "function_call", "name": "native_call", "call_id": "native", "arguments": "{}"}
		upstream := map[string]any{"status": status, "error": map[string]any{"code": "provider_reason"}, "incomplete_details": map[string]any{"reason": "max_output_tokens"}, "output": []any{messageItem("assistant", "partial"), native, plain}, "usage": map[string]any{"input_tokens": json.Number("9007199254740993")}}
		// Default (= h22, relay_incomplete: error): an unsuccessful response
		// is converted like any other; its finished relay call reaches the
		// client and the cause and status are kept.
		h22Source := cloneObject(source)
		h22Source["prompt_cache_key"] = "h24_unfinished_default_" + status
		_, converted, _, err := transformResponseBody(jsonBytes(upstream), h22Source)
		if err != nil || converted["status"] != status || stringValue(objectValue(converted["error"])["code"]) != "provider_reason" {
			t.Fatalf("h22 conversion of an unsuccessful response changed: %v", err)
		}
		if items := converted["output"].([]any); len(items) != 3 || objectValue(items[1])["name"] != "get_weather" || !reflect.DeepEqual(items[2], plain) {
			t.Fatal("h22 delivered the finished call of an unsuccessful response")
		}
		// relay_incomplete: passthrough (rc2).
		source = s2Source(source, s2Passthrough)
		_, result, _, err := transformResponseBody(jsonBytes(upstream), source)
		if err != nil || result["status"] != status || stringValue(objectValue(result["error"])["code"]) != "provider_reason" {
			t.Fatalf("upstream failure changed: %v", err)
		}
		items := result["output"].([]any)
		if len(items) != 2 || !reflect.DeepEqual(items[1], plain) {
			t.Fatal("native output changed or transport was delivered")
		}
		if rememberedNativeCall(conversationScope(source), native["call_id"].(string)) != nil {
			t.Fatal("unsuccessful response cached an undelivered transport")
		}
	}
}

func TestH24ExplicitReferencesDoesNotMigrateUnknownColdHistory(t *testing.T) {
	for _, kind := range []string{"function", "custom"} {
		callType, resultType := "function_call", "function_call_output"
		if kind == "custom" {
			callType, resultType = "custom_tool_call", "custom_tool_call_output"
		}
		call := map[string]any{"type": callType, "name": "local_exec", "id": "fc_cold_unknown_" + kind, "call_id": "cold_unknown_" + kind, "arguments": `{"id":9007199254740993}`, "input": " \tpatch\nraw  "}
		result := map[string]any{"type": resultType, "call_id": call["call_id"], "encrypted_content": "SYNTHETIC_OPAQUE", "output": "opaque"}
		history := []any{call, result}
		before := cloneValue(history)
		allowed := map[string]toolSpec{"local_exec": {Key: "local_exec", Name: "local_exec", Type: kind}}
		legacy := translateInputItemsFor(history, allowed, "h24_cold_legacy_"+kind, wireOptions{relayProtocol: "legacy"})
		refs := translateInputItemsFor(history, allowed, "h24_cold_refs_"+kind, wireOptions{relayProtocol: "references"})
		if !reflect.DeepEqual(legacy, refs) || !reflect.DeepEqual(before, history) {
			t.Fatal("operator generation mode rewrote unknown historical protocol or caller data")
		}
		envelope := transportEnvelope(objectValue(refs[0]))
		if kind == "custom" && envelope["args"] != call["input"] {
			t.Fatal("legacy custom history lost raw bytes")
		}
		if kind == "function" && !strings.Contains(string(jsonBytes(envelope)), "9007199254740993") {
			t.Fatal("legacy function history rounded integer")
		}
	}
	// A known native references call, unlike a cold client-facing call, has
	// provenance and must replay byte-for-byte under either configured mode.
	native := h24ReferencedCall("h24_cached_reference", []any{"get_weather"}, `{"city":"Paris"}`)
	client := map[string]any{"type": "function_call", "name": "get_weather", "call_id": "h24_cached_reference", "arguments": `{"city":"Paris"}`}
	scope := "h24_cached_reference_scope"
	rememberRelayCall(scope, native, client)
	for _, mode := range []string{"legacy", "references"} {
		out := translateInputItemsFor([]any{client}, clientToolSpecs(weatherRequest()), scope, wireOptions{relayProtocol: mode})
		if len(out) != 1 || !reflect.DeepEqual(out[0], native) {
			t.Fatal("known native history was re-encoded")
		}
	}
}
