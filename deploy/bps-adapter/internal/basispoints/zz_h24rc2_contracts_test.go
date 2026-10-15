package basispoints

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func preparedDeveloperText(body map[string]any) string {
	var result strings.Builder
	items, _ := body["input"].([]any)
	for _, raw := range items {
		item := objectValue(raw)
		if item["role"] == "developer" {
			result.WriteString(itemText(item["content"]))
		}
	}
	return result.String()
}

func TestH24RC2ModelReceivesFullFunctionAndCustomContract(t *testing.T) {
	function := map[string]any{"type": "function", "name": "deploy", "strict": true, "parameters": map[string]any{
		"type": "object", "additionalProperties": false, "$defs": map[string]any{"target": map[string]any{"type": "string", "enum": []any{"EXACT_SCHEMA_ENUM_SENTINEL"}}},
		"properties": map[string]any{"config": map[string]any{"type": "object", "properties": map[string]any{"target": map[string]any{"$ref": "#/$defs/target"}}, "required": []any{"target"}}},
		"required":   []any{"config"},
	}}
	custom := map[string]any{"type": "custom", "name": "patch", "format": map[string]any{"type": "grammar", "syntax": "lark", "definition": "start: EXACT_GRAMMAR_SENTINEL\nEXACT_GRAMMAR_SENTINEL: /patch [a-z]+/"}}
	source := map[string]any{"model": "gpt-5.6-sol", "input": "synthetic contract", "tools": []any{function, custom}}
	before := cloneValue(source)
	for _, mode := range []string{"legacy", "references"} {
		cfg := defaultConfig()
		cfg.RelayProtocol = mode
		// Complete contracts are opt-in since h25 (default summary = h22).
		cfg.RelayToolContracts = relayToolContractsFull
		body, err := prepareResponsesBody(source, cfg)
		if err != nil {
			t.Fatal(err)
		}
		text := preparedDeveloperText(body)
		for _, definition := range []map[string]any{function, custom} {
			if !strings.Contains(text, string(jsonBytes(definition))) {
				t.Fatalf("%s omitted the complete tool definition", mode)
			}
		}
		if strings.Contains(string(jsonBytes(body)), "cpa_relay_contracts") {
			t.Fatal("private preparation state leaked upstream")
		}
	}
	if !reflect.DeepEqual(source, before) {
		t.Fatal("source definition mutated")
	}
}

// H25-SPEC S1.3 reverses rc2 here: an over-budget catalogue in full mode no
// longer fails the request with 413 (a request h22 served). It falls back to
// the summary directory for that request. What rc2 guarded stays guarded: no
// partial catalogue of complete definitions is ever sent.
func TestH24RC2OversizedContractsFallBackWithoutPartialCatalogue(t *testing.T) {
	source := weatherRequest()
	tool := objectValue(source["tools"].([]any)[0])
	tool["description"] = strings.Repeat("x", relayContractMaxToolBytes+1)
	full := defaultConfig()
	full.RelayToolContracts = relayToolContractsFull
	before := relayCounterSnapshot()["tool_contracts_degraded"]
	body, err := prepareResponsesBody(source, full)
	var api *APIError
	if body == nil || errors.As(err, &api) {
		t.Fatalf("over-budget catalogue failed the request; body present=%v err=%v", body != nil, err)
	}
	summary, err := prepareResponsesBody(source, defaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(preparedDeveloperText(body), "Full tool key:") || string(jsonBytes(body)) != string(jsonBytes(summary)) {
		t.Fatal("over-budget full mode sent a partial or non-summary catalogue")
	}
	if relayCounterSnapshot()["tool_contracts_degraded"] <= before {
		t.Fatal("fallback was not counted")
	}
}

func TestH24RC2ContractRenderingPreservesNumbersAndCannotBeInjected(t *testing.T) {
	source := weatherRequest()
	source[relayContractTextKey] = "INJECTED_CATALOGUE"
	tool := objectValue(source["tools"].([]any)[0])
	tool["parameters"] = map[string]any{"type": "object", "properties": map[string]any{"id": map[string]any{"const": json.Number("9007199254740993")}}}
	text, err := renderRelayToolContracts(source)
	if err != nil || strings.Contains(text, "INJECTED_CATALOGUE") {
		t.Fatal("caller overrode prepared catalogue")
	}
	if !strings.Contains(text, "9007199254740993") {
		t.Fatal("schema numeric value was rounded")
	}
	prepared, err := withPreparedRelayToolContracts(source)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := source[relayContractTextKey].(relayContractText); ok {
		t.Fatal("source mutated")
	}
	if again, err := renderRelayToolContracts(prepared); err != nil || again != text {
		t.Fatal("prepared contract changed")
	}
}
