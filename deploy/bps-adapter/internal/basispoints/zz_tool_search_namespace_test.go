package basispoints

import "testing"

func toolSearchOutput(callID string, groups ...any) map[string]any {
	return map[string]any{
		"type": "tool_search_output", "call_id": callID,
		"status": "completed", "execution": "client", "tools": groups,
	}
}

func searchedNamespace(name string, tools ...string) map[string]any {
	members := []any{}
	for _, tool := range tools {
		members = append(members, map[string]any{
			"type": "function", "name": tool,
			"parameters": map[string]any{"type": "object", "properties": map[string]any{}},
		})
	}
	return map[string]any{"type": "namespace", "name": name, "tools": members}
}

func replayedCall(name, callID string) map[string]any {
	return map[string]any{"type": "function_call", "name": name,
		"call_id": callID, "id": "fc_" + callID, "arguments": "{}"}
}

func namespaceByCallID(items []any) map[string]string {
	got := map[string]string{}
	for _, value := range items {
		item := objectValue(value)
		if item != nil && stringValue(item["type"]) == "function_call" {
			got[stringValue(item["call_id"])] = stringValue(item["namespace"])
		}
	}
	return got
}

// The production failure of 2026-09-29/30: Codex Desktop loaded MCP tools
// through tool_search, the model called them with their namespace, and the
// replay came back without it, so every later turn of the conversation --
// compaction included -- was refused with "Missing namespace for function_call".
func TestToolSearchLoadedCallsRegainTheirNamespace(t *testing.T) {
	blenderCall := replayedCall("get_addon_status", "call_1")
	input := []any{
		map[string]any{"type": "message", "role": "user",
			"content": []any{map[string]any{"type": "input_text", "text": "check blender"}}},
		map[string]any{"type": "tool_search_call", "call_id": "ts_1", "status": "completed",
			"execution": "client", "arguments": map[string]any{"query": "blender"}},
		toolSearchOutput("ts_1", searchedNamespace("mcp__blender", "get_addon_status", "get_scene_info")),
		blenderCall,
		map[string]any{"type": "function_call_output", "call_id": "call_1", "output": "ok"},
		toolSearchOutput("ts_2", searchedNamespace("mcp__unityMCP", "manage_scene")),
		replayedCall("manage_scene", "call_2"),
		// Declared nowhere: must be left exactly as it came.
		replayedCall("never_declared", "call_3"),
	}
	explicit := replayedCall("get_scene_info", "call_4")
	explicit["namespace"] = "explicit"
	input = append(input, explicit)

	translated := translateInputItems(input, map[string]toolSpec{}, "tool-search-namespace")
	got := namespaceByCallID(translated)
	for callID, want := range map[string]string{
		"call_1": "mcp__blender",
		"call_2": "mcp__unityMCP",
		"call_3": "",
		"call_4": "explicit",
	} {
		if got[callID] != want {
			t.Errorf("%s namespace = %q, want %q", callID, got[callID], want)
		}
	}
	if _, mutated := blenderCall["namespace"]; mutated {
		t.Error("the caller's own item was modified; the restored namespace belongs on a copy")
	}
	searches := 0
	for _, value := range translated {
		if item := objectValue(value); item != nil && stringValue(item["type"]) == "tool_search_output" {
			searches++
		}
	}
	if searches != 2 {
		t.Errorf("forwarded %d tool_search_output items, want both untouched", searches)
	}
}

func TestToolSearchNamespaceIsNotGuessedWhenAmbiguous(t *testing.T) {
	input := []any{
		toolSearchOutput("ts_1", searchedNamespace("mcp__git_bash", "run")),
		toolSearchOutput("ts_2", searchedNamespace("mcp__other_shell", "run")),
		replayedCall("run", "call_1"),
		// The same namespace found twice is not a conflict.
		toolSearchOutput("ts_3", searchedNamespace("mcp__blender", "get_addon_status")),
		toolSearchOutput("ts_4", searchedNamespace("mcp__blender", "get_addon_status")),
		replayedCall("get_addon_status", "call_2"),
	}
	got := namespaceByCallID(translateInputItems(input, map[string]toolSpec{}, "tool-search-ambiguous"))
	if got["call_1"] != "" {
		t.Errorf("run was given namespace %q although the search results put it in two", got["call_1"])
	}
	if got["call_2"] != "mcp__blender" {
		t.Errorf("get_addon_status namespace = %q, want mcp__blender", got["call_2"])
	}
}

// h22's test, restored: by default (history_conflict: replay) an
// additional_tools declaration wins over a search result, as in h22.
func TestAdditionalToolsDeclarationWinsOverSearchResults(t *testing.T) {
	input := []any{
		map[string]any{"type": "additional_tools", "tools": []any{
			map[string]any{"type": "namespace", "name": "collaboration", "tools": []any{
				map[string]any{"type": "function", "name": "spawn_agent"},
			}},
		}},
		toolSearchOutput("ts_1", searchedNamespace("mcp__agents", "spawn_agent")),
		replayedCall("spawn_agent", "call_1"),
	}
	got := namespaceByCallID(translateInputItems(input, map[string]toolSpec{}, "tool-search-precedence"))
	if got["call_1"] != "collaboration" {
		t.Errorf("spawn_agent namespace = %q, want the additional_tools declaration \"collaboration\"", got["call_1"])
	}
}

// rc2's version of the test above: history_conflict: reject never guesses
// between two declared namespaces.
func TestDifferentNativeNamespacesAreNotGuessedByDeclarationSource(t *testing.T) {
	input := []any{
		map[string]any{"type": "additional_tools", "tools": []any{
			map[string]any{"type": "namespace", "name": "collaboration", "tools": []any{
				map[string]any{"type": "function", "name": "spawn_agent"},
			}},
		}},
		toolSearchOutput("ts_1", searchedNamespace("mcp__agents", "spawn_agent")),
		replayedCall("spawn_agent", "call_1"),
	}
	got := namespaceByCallID(translateInputItemsReject(input, map[string]toolSpec{}, "tool-search-precedence-reject"))
	if got["call_1"] != "" {
		t.Errorf("ambiguous cross-source namespace was guessed: %q", got["call_1"])
	}
}

func TestToolSearchNamespaceIsRememberedForTheConversation(t *testing.T) {
	scope := "tool-search-remembered"
	_ = translateInputItems([]any{
		toolSearchOutput("ts_1", searchedNamespace("mcp__codebase_memory_mcp", "index_repository")),
		replayedCall("index_repository", "call_1"),
	}, map[string]toolSpec{}, scope)

	later := []any{replayedCall("index_repository", "call_2")}
	if got := namespaceByCallID(translateInputItems(later, map[string]toolSpec{}, scope)); got["call_2"] != "mcp__codebase_memory_mcp" {
		t.Errorf("namespace = %q on a later turn of the same conversation, want mcp__codebase_memory_mcp", got["call_2"])
	}
	if got := namespaceByCallID(translateInputItems(later, map[string]toolSpec{}, "some-other-conversation")); got["call_2"] != "" {
		t.Errorf("another conversation inherited namespace %q", got["call_2"])
	}
}

// End to end through the body the upstream actually receives.
func TestPreparedBodyCarriesToolSearchNamespace(t *testing.T) {
	source := map[string]any{
		"model": "gpt-6-sol", "stream": true, "prompt_cache_key": "tool-search-e2e",
		"input": []any{
			map[string]any{"type": "message", "role": "user",
				"content": []any{map[string]any{"type": "input_text", "text": "scene?"}}},
			toolSearchOutput("ts_1", searchedNamespace("mcp__unityMCP", "manage_scene")),
			replayedCall("manage_scene", "call_1"),
			map[string]any{"type": "function_call_output", "call_id": "call_1", "output": "{}"},
		},
	}
	body, err := prepareResponsesBody(source, defaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	items, _ := body["input"].([]any)
	if got := namespaceByCallID(items)["call_1"]; got != "mcp__unityMCP" {
		t.Errorf("upstream body namespace = %q, want mcp__unityMCP", got)
	}
}
