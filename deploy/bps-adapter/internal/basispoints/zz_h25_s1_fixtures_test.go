package basispoints

// H25 S1 equivalence fixtures. This file compiles unchanged in the h22-live
// tree, where a generator test recorded testdata/h22_s1_golden.json from h22's
// own functions. zz_h25_s1_golden_test.go replays the same fixtures here and
// compares byte for byte. Keep it free of anything h22 does not have.

import "fmt"

type s1PromptFixture struct {
	Name   string
	Source map[string]any
}

type s1DecodeFixture struct {
	Name   string
	Source string
	Native map[string]any
}

func s1Function(name, description string, parameters map[string]any) map[string]any {
	tool := map[string]any{"type": "function", "name": name}
	if description != "" {
		tool["description"] = description
	}
	if parameters != nil {
		tool["parameters"] = parameters
	}
	return tool
}

func s1Custom(name, description string) map[string]any {
	tool := map[string]any{"type": "custom", "name": name}
	if description != "" {
		tool["description"] = description
	}
	return tool
}

func s1WeatherTool() map[string]any {
	return s1Function("get_weather", "Get current weather.", map[string]any{
		"type":       "object",
		"properties": map[string]any{"city": map[string]any{"type": "string"}},
		"required":   []any{"city"},
	})
}

func s1MultiTools() []any {
	return []any{
		s1WeatherTool(),
		s1Function("list_files", "List files <b>&amp;</b> folders 天气 \"quoted\"", map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path":      map[string]any{"type": "string"},
				"limit":     map[string]any{"type": "integer"},
				"ratio":     map[string]any{"type": "number"},
				"recursive": map[string]any{"type": "boolean"},
				"mode":      map[string]any{"enum": []any{1, 2}},
				"tags":      map[string]any{"type": "array", "items": map[string]any{"type": "integer"}},
			},
			"required": []any{"path"},
		}),
		s1Function("no_params", "", nil),
		map[string]any{"type": "function", "name": "uses_input_schema", "inputSchema": map[string]any{
			"type": "object", "properties": map[string]any{"q": map[string]any{"type": "string"}},
		}},
		s1Function("loose", "Schema h22 accepts more of than rc2.", map[string]any{
			"properties":           map[string]any{"n": map[string]any{"type": "integer"}},
			"required":             []any{"must"},
			"additionalProperties": map[string]any{"type": "string"},
		}),
		s1Custom("apply_patch", "Apply a patch."),
		map[string]any{"type": "namespace", "name": "workspace", "tools": []any{
			s1Function("exec_command", "Run a command.", map[string]any{
				"type": "object", "properties": map[string]any{"cmd": map[string]any{"type": "string"}},
				"required": []any{"cmd"},
			}),
			s1Custom("write", ""),
		}},
	}
}

func s1UserInput(text string) []any {
	return []any{map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": text}}}}
}

func s1PromptFixtures() []s1PromptFixture {
	with := func(base map[string]any, key string, value any) map[string]any {
		copy := map[string]any{}
		for k, v := range base {
			copy[k] = v
		}
		copy[key] = value
		return copy
	}
	weather := map[string]any{"model": "gpt-6-astra", "input": s1UserInput("weather in Paris"), "tools": []any{s1WeatherTool()}}
	multi := map[string]any{"model": "gpt-6-astra", "input": s1UserInput("list the files"), "tools": s1MultiTools()}
	additional := []any{
		map[string]any{"type": "additional_tools", "tools": []any{
			s1Function("native_lookup", "Native.", map[string]any{"type": "object"}),
			s1WeatherTool(),
		}},
		map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "mixed"}}},
	}
	search := []any{
		map[string]any{"type": "tool_search_output", "call_id": "s1_search", "status": "completed", "execution": "client", "tools": []any{
			map[string]any{"type": "namespace", "name": "mcp_docs", "tools": []any{s1Function("lookup", "Docs.", map[string]any{"type": "object"})}},
		}},
		map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "search"}}},
	}
	return []s1PromptFixture{
		{"weather", weather},
		{"weather_string_input", with(weather, "input", "weather in Paris")},
		{"multi", multi},
		{"multi_instructions_session", with(with(multi, "instructions", "You are terse."), "prompt_cache_key", "s1-session-key")},
		{"forced_function", with(multi, "tool_choice", map[string]any{"type": "function", "name": "get_weather"})},
		{"forced_namespaced", with(multi, "tool_choice", map[string]any{"type": "function", "name": "exec_command", "namespace": "workspace"})},
		{"allowed_tools_auto", with(multi, "tool_choice", map[string]any{"type": "allowed_tools", "mode": "auto", "tools": []any{map[string]any{"type": "function", "name": "get_weather"}}})},
		{"allowed_tools_required", with(multi, "tool_choice", map[string]any{"type": "allowed_tools", "mode": "required", "tools": []any{map[string]any{"type": "custom", "name": "apply_patch"}}})},
		{"required", with(multi, "tool_choice", "required")},
		{"auto_choice", with(multi, "tool_choice", "auto")},
		{"parallel_false", with(multi, "parallel_tool_calls", false)},
		{"top_and_additional", with(weather, "input", additional)},
		{"top_and_search", with(multi, "input", search)},
		{"namespace_only", map[string]any{"model": "gpt-6-astra", "input": s1UserInput("ns"), "tools": []any{s1MultiTools()[6]}}},
	}
}

// Two top-level custom tools: h22 appended their reminder sentences in map
// order. The generator records every order h22 produced.
func s1TwoCustomSource() map[string]any {
	return map[string]any{"model": "gpt-6-astra", "input": s1UserInput("two"), "tools": []any{
		s1Custom("zeta_patch", "Z."), s1Custom("alpha_patch", "A."), s1WeatherTool(),
	}}
}

func s1DecodeSources() map[string]map[string]any {
	weather := map[string]any{"model": "gpt-6-astra", "input": s1UserInput("w"), "tools": []any{s1WeatherTool()}}
	multi := map[string]any{"model": "gpt-6-astra", "input": s1UserInput("m"), "tools": s1MultiTools()}
	none := map[string]any{"model": "gpt-6-astra", "input": s1UserInput("n"), "tools": []any{s1WeatherTool()}, "tool_choice": "none"}
	forced := map[string]any{"model": "gpt-6-astra", "input": s1UserInput("f"), "tools": s1MultiTools(), "tool_choice": map[string]any{"type": "function", "name": "get_weather"}}
	return map[string]map[string]any{"weather": weather, "multi": multi, "none": none, "forced": forced}
}

func s1Transport(id string, outer map[string]any) map[string]any {
	return map[string]any{"type": "function_call", "name": transportName, "id": "fc_" + id, "call_id": id, "status": "completed", "arguments": string(jsonBytes(outer))}
}

func s1Envelope(tool string, args any) string {
	return string(jsonBytes(map[string]any{"tool": tool, "args": args}))
}

// s1DecodeFixtures crosses every code shape with every references shape. The
// references values are what models actually put there (paths, names) plus
// the malformed ones (null, a string, a number).
func s1DecodeFixtures() []s1DecodeFixture {
	nested := string(jsonBytes(map[string]any{"name": transportName, "arguments": string(jsonBytes(map[string]any{"code": s1Envelope("get_weather", map[string]any{"city": "Nested"})}))}))
	codes := []struct {
		name string
		code any
	}{
		{"legacy_function", s1Envelope("get_weather", map[string]any{"city": "Paris"})},
		{"legacy_custom", s1Envelope("apply_patch", "*** Begin Patch\n+x\n*** End Patch")},
		{"legacy_namespaced", s1Envelope("workspace.exec_command", map[string]any{"cmd": "ls"})},
		{"legacy_broken_escape", `{"tool":"get_weather","args":{"city":"Pa"ris"}}`},
		{"legacy_broken_custom", `{"tool":"apply_patch","args":"line "quoted" end"}`},
		{"payload_only", `{"city":"Paris"}`},
		{"raw_custom_text", "*** Begin Patch\n+raw\n*** End Patch"},
		{"nested_transport", nested},
		{"fenced", "```json\n" + s1Envelope("get_weather", map[string]any{"city": "Fenced"}) + "\n```"},
		{"undeclared_tool", s1Envelope("not_declared", map[string]any{"x": 1})},
		{"arguments_key", string(jsonBytes(map[string]any{"tool": "get_weather", "arguments": map[string]any{"city": "Args"}}))},
		{"name_key", string(jsonBytes(map[string]any{"name": "get_weather", "args": map[string]any{"city": "Name"}}))},
		{"object_code", map[string]any{"tool": "get_weather", "args": map[string]any{"city": "Object"}}},
		{"missing_tool", `{"args":{"city":"Paris"}}`},
		{"string_integer", s1Envelope("list_files", map[string]any{"path": "/", "limit": "20"})},
		{"string_bool_number", s1Envelope("list_files", map[string]any{"path": "/", "recursive": "true", "ratio": "0.5", "tags": []any{"1", 2}})},
		{"string_integer_fraction", s1Envelope("list_files", map[string]any{"path": "/", "limit": "1.0"})},
		{"integer_fraction", s1Envelope("list_files", map[string]any{"path": "/", "limit": 1.5})},
		{"enum_string", s1Envelope("list_files", map[string]any{"path": "/", "mode": "1"})},
		{"loose_schema", s1Envelope("loose", map[string]any{"n": 2, "extra": 7})},
		{"schema_mismatch", s1Envelope("get_weather", map[string]any{"city": 7})},
		{"empty_code", ""},
	}
	refs := []struct {
		name    string
		value   any
		present bool
	}{
		{"absent", nil, false},
		{"empty", []any{}, true},
		{"one_path", []any{"src/app.go"}, true},
		{"two_paths", []any{"src/app.go", "README.md"}, true},
		{"three_paths", []any{"a.txt", "b.txt", "c.txt"}, true},
		{"null", nil, true},
		{"string", "src/app.go", true},
		{"number", []any{7}, true},
		{"tool_name", []any{"get_weather"}, true},
		{"tool_name_twice", []any{"get_weather", "get_weather"}, true},
		{"two_tool_names", []any{"workspace.exec_command", "get_weather"}, true},
		{"custom_name", []any{"apply_patch"}, true},
	}
	var fixtures []s1DecodeFixture
	for _, source := range []string{"weather", "multi", "none", "forced"} {
		for _, code := range codes {
			for _, ref := range refs {
				outer := map[string]any{"summary": "s", "extended_summary": "e", "destructive": false, "code": code.code}
				if ref.present {
					outer["references"] = ref.value
				}
				name := fmt.Sprintf("%s/%s/%s", source, code.name, ref.name)
				fixtures = append(fixtures, s1DecodeFixture{Name: name, Source: source, Native: s1Transport("s1_"+code.name, outer)})
			}
		}
	}
	return fixtures
}
