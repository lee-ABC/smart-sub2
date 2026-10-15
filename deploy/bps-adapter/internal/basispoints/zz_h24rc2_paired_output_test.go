package basispoints

import (
	"reflect"
	"strings"
	"testing"
)

// rc2's paired-output rules are history_conflict: reject's; the default
// pairs outputs as h22 did (TestH25S3ReplayModeTranslationIsH22).
//
// H25-SPEC S3.4 changes what happens to the current call in four variants:
// an opaque, payload-less, non-JSON or contradicted call is no longer sent
// upstream under its bare client name but takes h22's cold path, which wraps
// it into a run_officejs call of its own. Its output then pairs with that
// wrapper (type function_call_output, the wrapper's item id, content kept), so
// the rc2 assertion "the output stays as the client sent it" is replaced for
// those four by: the stale relay record is not replayed, the call is the cold
// wrapper of the current call, and the output keeps its content. duplicate is
// unchanged.
func TestH24RC2PairedOutputRequiresCurrentCallProof(t *testing.T) {
	for _, kind := range []string{"function", "custom"} {
		for _, variant := range []string{"opaque", "missing", "invalid", "conflict", "duplicate"} {
			for _, outputFirst := range []bool{false, true} {
				scope := "paired-proof-" + kind + variant
				if outputFirst {
					scope += "-output-first"
				}
				source := map[string]any{"tools": []any{map[string]any{"type": kind, "name": "probe"}}}
				var args any = map[string]any{"value": "original"}
				if kind == "custom" {
					args = "original raw input"
				}
				native := h21Native(scope, "probe", args)
				client, reason := decodeTransportItemWithReason(native, source)
				if reason != "" {
					t.Fatal(reason)
				}
				rememberRelayCall(scope, native, client)
				current := cloneObject(client)
				payloadKey := "arguments"
				outputType := "function_call_output"
				if kind == "custom" {
					payloadKey = "input"
					outputType = "custom_tool_call_output"
				}
				switch variant {
				case "opaque", "duplicate":
					current["encrypted_content"] = "SYNTHETIC_OPAQUE"
				case "missing":
					delete(current, payloadKey)
				case "invalid":
					current[payloadKey] = 7
				case "conflict":
					current[payloadKey] = `{"value":"different"}`
				}
				output := map[string]any{"type": outputType, "call_id": scope, "id": "keep-output-id", "output": "current result"}
				history := []any{current, output}
				if outputFirst {
					history = []any{output, current}
				}
				if variant == "duplicate" {
					history = append(history, cloneObject(client))
				}
				before := cloneValue(history)
				got := translateInputItemsReject(history, clientToolSpecs(source), scope)
				index, callIndex := 1, 0
				if outputFirst {
					index, callIndex = 0, 1
				}
				if variant == "duplicate" {
					if !reflect.DeepEqual(got[index], output) {
						t.Errorf("%s/%s first=%v output was rebound to stale relay", kind, variant, outputFirst)
					}
				} else {
					call := objectValue(got[callIndex])
					if reflect.DeepEqual(call, native) || call["name"] != transportName || stringValue(call["call_id"]) != scope {
						t.Errorf("%s/%s first=%v: stale relay replayed or current call not wrapped: %v", kind, variant, outputFirst, call)
					}
					if variant == "conflict" && strings.Contains(stringValue(call["arguments"]), "original") {
						t.Errorf("%s/%s: stale arguments replayed", kind, variant)
					}
					paired := objectValue(got[index])
					if paired["type"] != "function_call_output" || paired["id"] != functionItemID(scope) || paired["output"] != output["output"] || paired["call_id"] != scope {
						t.Errorf("%s/%s first=%v: output not paired with its wrapped call: %v", kind, variant, outputFirst, paired)
					}
				}
				if !reflect.DeepEqual(history, before) {
					t.Fatal("caller history mutated")
				}
			}
		}
	}
}

// history_conflict: reject (rc2). h22 pairs an output that precedes a
// run_officejs call seen only in this request's history with nothing.
func TestH24RC2ProvenOutputsWorkInEitherOrderAndOutputOnly(t *testing.T) {
	for _, variant := range []string{"verified-before", "verified-after", "transport-before", "transport-after", "output-only"} {
		scope := "paired-positive-" + variant
		native := h21Native(scope, "get_weather", map[string]any{"city": "S"})
		client, reason := decodeTransportItemWithReason(native, weatherRequest())
		if reason != "" {
			t.Fatal(reason)
		}
		if !strings.HasPrefix(variant, "transport") {
			rememberRelayCall(scope, native, client)
		}
		output := map[string]any{"type": "custom_tool_call_output", "call_id": scope, "id": "old-output-id", "output": "value"}
		call := client
		if strings.HasPrefix(variant, "transport") {
			call = native
		}
		history := []any{call, output}
		index := 1
		if strings.HasSuffix(variant, "after") {
			history = []any{output, call}
			index = 0
		}
		if variant == "output-only" {
			history = []any{output}
			index = 0
		}
		got := objectValue(translateInputItemsReject(history, clientToolSpecs(weatherRequest()), scope)[index])
		if got["type"] != "function_call_output" || got["id"] != functionItemID(scope) || got["output"] != "value" {
			t.Errorf("proven %s output was not normalized", variant)
		}
	}
}

// history_conflict: reject (rc2). By default a relayed result with no text
// is replaced by h22's placeholder, as in h22 (TestH25S3ReplayModeTranslationIsH22).
func TestH24RC2RelayResultPreservesStructuredAndMultimodalContent(t *testing.T) {
	for _, value := range []any{
		[]any{map[string]any{"type": "input_image", "image_url": "https://example.invalid/image.png"}},
		[]any{map[string]any{"type": "input_file", "file_id": "file_synthetic"}},
		map[string]any{"error": map[string]any{"code": "E_SYNTHETIC", "message": "operation failed"}},
		[]any{map[string]any{"type": "input_text", "text": ""}},
		map[string]any{}, []any{}, false, 0, "  meaningful text  ", " \t\n",
	} {
		native := h21Native("structured-result", "get_weather", map[string]any{"city": "S"})
		output := map[string]any{"type": "function_call_output", "call_id": "structured-result", "output": value}
		got := objectValue(translateInputItemsReject([]any{native, output}, nil, "structured-scope")[1])
		if !reflect.DeepEqual(got["output"], value) {
			t.Errorf("structured/nonempty %T result was replaced: %v", value, got["output"])
		}
	}
	for _, value := range []any{nil, ""} {
		native := h21Native("empty-result", "get_weather", map[string]any{"city": "S"})
		output := map[string]any{"type": "function_call_output", "call_id": "empty-result", "output": value}
		got := objectValue(translateInputItemsReject([]any{native, output}, nil, "empty-scope")[1])
		if got["output"] != "(empty tool output)" {
			t.Errorf("empty output claimed a status or changed compatibility: %v", got["output"])
		}
	}
	native := h21Native("missing-result", "get_weather", map[string]any{"city": "S"})
	missing := map[string]any{"type": "function_call_output", "call_id": "missing-result"}
	got := objectValue(translateInputItemsReject([]any{native, missing}, nil, "missing-scope")[1])
	if got["output"] != "(empty tool output)" {
		t.Fatal("missing output did not receive a neutral placeholder")
	}
}
