package basispoints

import (
	"encoding/json"
	"os"
	"testing"
)

// TestWriteLiveProbeBody writes the upstream body this build produces for a
// minimal conversation that replays a tool_search-loaded call without its
// namespace, so it can be sent to the real upstream by hand. Set
// LIVE_BODY_OUT to run it.
func TestWriteLiveProbeBody(t *testing.T) {
	out := os.Getenv("LIVE_BODY_OUT")
	if out == "" {
		t.Skip("LIVE_BODY_OUT not set")
	}
	source := map[string]any{
		"model": "gpt-6-sol", "stream": true, "prompt_cache_key": os.Getenv("LIVE_BODY_KEY"),
		"reasoning": map[string]any{"effort": "low"},
		"input": []any{
			map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text",
				"text": "Check whether the demo add-on is enabled, then answer in one short sentence."}}},
			map[string]any{"type": "tool_search_call", "call_id": "call_probe_search", "status": "completed",
				"execution": "client", "arguments": map[string]any{"query": "demo add-on status"}},
			map[string]any{"type": "tool_search_output", "id": "tso_probe_search", "call_id": "call_probe_search",
				"status": "completed", "execution": "client", "tools": []any{
					map[string]any{"type": "namespace", "name": "mcp__probe_demo",
						"description": "Demo add-on tools.", "tools": []any{
							map[string]any{"type": "function", "name": "get_addon_status",
								"description": "Report whether the demo add-on is enabled.",
								"parameters": map[string]any{"type": "object", "properties": map[string]any{}, "additionalProperties": false},
								"strict": false, "defer_loading": true},
						}},
				}},
			// Replayed the way production clients send it: no namespace.
			map[string]any{"type": "function_call", "id": "fc_probe_status", "call_id": "call_probe_status",
				"name": "get_addon_status", "arguments": "{}"},
			map[string]any{"type": "function_call_output", "call_id": "call_probe_status",
				"output": `{"enabled":true}`},
		},
	}
	body, err := prepareResponsesBody(source, defaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(body)
	if err := os.WriteFile(out, raw, 0o600); err != nil {
		t.Fatal(err)
	}
}
