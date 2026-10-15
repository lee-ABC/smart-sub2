package basispoints

import (
	"reflect"
	"strings"
	"testing"
)

func catalogPolicyFixtureTool(name string) map[string]any {
	return map[string]any{"type": "function", "name": name, "parameters": map[string]any{"type": "object"}}
}
func catalogPolicyDeveloperText(body map[string]any) string {
	var all []string
	for _, raw := range body["input"].([]any) {
		item := objectValue(raw)
		if item == nil || item["role"] != "developer" {
			continue
		}
		for _, part := range item["content"].([]any) {
			all = append(all, stringValue(objectValue(part)["text"]))
		}
	}
	return strings.Join(all, "\n")
}
func TestH24RC2CatalogDiscoveredOnlyNotTextOnly(t *testing.T) {
	source := map[string]any{"model": "gpt-6-astra", "input": []any{toolSearchOutput("audit_search", searchedNamespace("mcp_docs", "lookup")), messageItem("user", "use lookup")}}
	if _, ok := relayOutputToolSpecs(source)["mcp_docs.lookup"]; !ok {
		t.Fatal("fixture has no callable discovered tool")
	}
	body, err := prepareResponsesBody(source, defaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	hints := catalogPolicyDeveloperText(body)
	if strings.Contains(hints, "Return the answer as assistant text") {
		t.Fatal("callable tool_search_output exists but developer prologue forbids tool continuation")
	}
}
func TestH24RC2CatalogSearchAnnotationChangesKeepTool(t *testing.T) {
	first := catalogPolicyFixtureTool("lookup")
	first["description"] = "Read documents"
	second := cloneObject(first)
	second["description"] = "Read the matching documents"
	source := map[string]any{"input": []any{map[string]any{"type": "tool_search_output", "tools": []any{first}}, map[string]any{"type": "tool_search_output", "tools": []any{second}}}}
	if _, ok := relayOutputToolSpecs(source)["lookup"]; !ok {
		t.Fatal("description-only discovery update deleted a valid tool from output catalogue")
	}
}

// rc2's namespace ambiguity rule is history_conflict: reject's; the default
// keeps h22's (last additional_tools declaration wins).
func TestH24RC2CatalogAdditionalNamespaceCollisionNeverGuessed(t *testing.T) {
	scope := "audit_additional_ambiguous"
	input := []any{map[string]any{"type": "additional_tools", "tools": []any{searchedNamespace("shell_a", "run"), searchedNamespace("shell_b", "run")}}, replayedCall("run", "audit_ambiguous_call")}
	got := namespaceByCallID(translateInputItemsReject(input, map[string]toolSpec{}, scope))
	if got["audit_ambiguous_call"] != "" {
		t.Fatalf("ambiguous short name was routed to %q", got["audit_ambiguous_call"])
	}
}

// history_conflict: reject (rc2's ambiguity tombstone); see above.
func TestH24RC2CatalogCurrentAmbiguityInvalidatesOldNamespace(t *testing.T) {
	scope := "audit_cached_ambiguity"
	_ = translateInputItemsReject([]any{toolSearchOutput("audit_first", searchedNamespace("shell_a", "run"))}, map[string]toolSpec{}, scope)
	later := []any{toolSearchOutput("audit_second", searchedNamespace("shell_a", "run")), toolSearchOutput("audit_third", searchedNamespace("shell_b", "run")), replayedCall("run", "audit_cached_call")}
	got := namespaceByCallID(translateInputItemsReject(later, map[string]toolSpec{}, scope))
	if got["audit_cached_call"] != "" {
		t.Fatalf("current ambiguous declaration inherited stale namespace %q", got["audit_cached_call"])
	}
}
func TestH24RC2CatalogForcedToolPromptMustMatchAllowedCatalogue(t *testing.T) {
	source := map[string]any{"tools": []any{catalogPolicyFixtureTool("allowed"), catalogPolicyFixtureTool("forbidden")}, "tool_choice": map[string]any{"type": "function", "name": "allowed"}}
	if len(relayOutputToolSpecs(source)) != 1 {
		t.Fatal("bad fixture")
	}
	if strings.Contains(clientToolCatalogText(source), "forbidden") {
		t.Fatal("prompt advertises a tool that the response path will reject under forced choice")
	}
}
func TestH24RC2CatalogRequiredChoiceCannotSucceedWithTextOnly(t *testing.T) {
	choices := []any{"required", map[string]any{"type": "function", "name": "allowed"}, map[string]any{"type": "allowed_tools", "mode": "required", "tools": []any{map[string]any{"type": "function", "name": "allowed"}}}}
	for _, choice := range choices {
		source := map[string]any{"tools": []any{catalogPolicyFixtureTool("allowed")}, "tool_choice": choice}
		// tool_policy_checks: enforce (rc2).
		_, _, _, err := transformResponseBody(h21Response(messageItem("assistant", "text only")), s2Source(source, s2Enforce))
		if err == nil {
			t.Errorf("required/forced successful response accepted without any call: %v", choice)
		}
		// Default (= h22): logged, delivered.
		if _, _, _, err := transformResponseBody(h21Response(messageItem("assistant", "text only")), source); err != nil {
			t.Errorf("h22 delivered this text answer: %v", err)
		}
	}
}
func TestH24RC2CatalogNoneBlocksDeclaredNativeToolCall(t *testing.T) {
	source := map[string]any{"tools": []any{catalogPolicyFixtureTool("local_exec")}, "tool_choice": "none"}
	native := map[string]any{"type": "function_call", "name": "local_exec", "call_id": "audit_native", "id": "fc_audit_native", "arguments": "{}", "status": "completed"}
	// tool_policy_checks: enforce (rc2).
	_, _, _, err := transformResponseBody(h21Response(native), s2Source(source, s2Enforce))
	if err == nil {
		t.Fatal("tool_choice none was bypassed by a direct declared native call")
	}
	// Default (= h22): logged, delivered.
	if _, _, _, err := transformResponseBody(h21Response(native), source); err != nil {
		t.Fatalf("h22 delivered this native call: %v", err)
	}
}
func TestH24RC2CatalogControls(t *testing.T) {
	t.Run("auto_text_allowed", func(t *testing.T) {
		source := map[string]any{"tools": []any{catalogPolicyFixtureTool("allowed")}, "tool_choice": "auto"}
		if _, _, _, err := transformResponseBody(h21Response(messageItem("assistant", "text")), source); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("forced_namespace_exact", func(t *testing.T) {
		source := map[string]any{"tools": []any{searchedNamespace("namespace_a", "run")}, "tool_choice": map[string]any{"type": "function", "name": "run", "namespace": "namespace_a"}}
		if _, ok := relayOutputToolSpecs(source)["namespace_a.run"]; !ok {
			t.Fatal("exact namespace forced choice failed")
		}
	})
	t.Run("real_schema_conflict_excluded", func(t *testing.T) {
		a := catalogPolicyFixtureTool("lookup")
		b := cloneObject(a)
		b["parameters"] = map[string]any{"type": "object", "required": []any{"x"}}
		s := map[string]any{"input": []any{map[string]any{"type": "tool_search_output", "tools": []any{a}}, map[string]any{"type": "tool_search_output", "tools": []any{b}}}}
		if _, ok := relayOutputToolSpecs(s)["lookup"]; ok {
			t.Fatal("true conflict was ignored")
		}
	})
	t.Run("explicit_namespace_preserved", func(t *testing.T) {
		call := replayedCall("run", "audit_explicit")
		call["namespace"] = "explicit"
		out := translateInputItems([]any{call}, map[string]toolSpec{}, "audit_explicit_scope")
		if !reflect.DeepEqual(out[0], call) {
			t.Fatal("explicit namespace changed")
		}
	})
}
