package basispoints

import (
	"reflect"
	"testing"
)

// The catalogue says which tools exist, not whether a historical call was
// native or translated. A cold cache must not turn an existing namespaced
// call into run_officejs or rewrite the identity of its opaque result.
func TestH22ColdNamespacedHistoryPreservesNativeCallAndEncryptedResult(t *testing.T) {
	for _, kind := range []string{"function", "custom"} {
		t.Run(kind, func(t *testing.T) {
			tool := map[string]any{"type": kind, "name": "exec"}
			source := map[string]any{"tools": []any{map[string]any{
				"type": "namespace", "name": "functions", "tools": []any{tool},
			}}}
			callType, outputType, id := "function_call", "function_call_output", "fc_native_h22"
			if kind == "custom" {
				callType, outputType, id = "custom_tool_call", "custom_tool_call_output", "ctc_native_h22"
			}
			call := map[string]any{
				"type": callType, "name": "exec", "namespace": "functions",
				"call_id": "native_h22_" + kind, "id": id, "status": "completed",
			}
			if kind == "custom" {
				call["input"] = "  synthetic input\nwith trailing whitespace  "
			} else {
				call["arguments"] = `{"synthetic":true}`
			}
			result := map[string]any{
				"type": outputType, "call_id": call["call_id"], "id": "opaque_result_h22_" + kind,
				"encrypted_content": "SYNTHETIC_OPAQUE_CONTENT",
				"output":            []any{map[string]any{"type": "encrypted_text", "text": "SYNTHETIC_OPAQUE_OUTPUT"}},
			}
			history := []any{call, result}
			before := cloneValue(history)
			got := translateInputItems(history, clientToolSpecs(source), "h22-cold-"+kind)
			if !reflect.DeepEqual(got, before) {
				t.Fatal("cold namespaced call or encrypted result was changed")
			}
			if !reflect.DeepEqual(history, before) {
				t.Fatal("caller history was mutated")
			}
		})
	}
}

func TestH22FallbackCustomPayloadStillPreservesRawWhitespace(t *testing.T) {
	input := "  first\n\tlast  "
	item := map[string]any{
		"type": "custom_tool_call", "name": "exec", "namespace": "functions",
		"call_id": "h22-explicit-fallback", "input": input,
	}
	transport := fallbackTransportCall(item)
	envelope := transportEnvelope(transport)
	if envelope["tool"] != "functions.exec" || envelope["args"] != input {
		t.Fatal("explicit fallback lost the full name or raw custom input")
	}
}
