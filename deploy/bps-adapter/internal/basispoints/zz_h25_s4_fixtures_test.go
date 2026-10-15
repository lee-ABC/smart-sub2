package basispoints

// H25 S4 fixtures: tools whose parameters demand ciphertext (E1/E4). This file
// compiles unchanged in the h22-live tree, next to zz_h25_s2_fixtures_test.go
// and zz_h25_s3_fixtures_test.go, where testdata/h22_s4_golden_gen.go.txt
// records h22's own outputs into testdata/h22_s4_golden.json. Keep it free of
// anything h22 does not have.
//
// Every call id and cache key is unique to this file: the replay caches are
// process-wide in both trees.

type s4PromptFixture struct {
	Name   string
	Source map[string]any
	// Hidden: the catalogue keys E1 leaves out of the relay prompt (sorted).
	Hidden []string
	// AllHidden: no relay tool is left, so the prompt is the one h22 gives
	// the same request without its top-level tools, plus the E1 sentence.
	AllHidden bool
}

type s4DecodeFixture struct {
	Name   string
	Source string
	Native map[string]any
	// Blocked: E4 hands this call back without its namespace.
	Blocked bool
}

type s4StreamFixture struct {
	Name    string
	Request map[string]any
	Chunks  []string
	// Blocked: the call ids E4 hands back without their namespace.
	Blocked []string
}

type s4TransformFixture struct {
	Name     string
	Source   map[string]any
	Response map[string]any
	Blocked  []string
}

func s4User(text string) map[string]any {
	return map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": text}}}
}

func s4Function(name, description string, parameters map[string]any) map[string]any {
	tool := map[string]any{"type": "function", "name": name, "description": description, "strict": false}
	if parameters != nil {
		tool["parameters"] = parameters
	}
	return tool
}

func s4Object(required []any, properties map[string]any) map[string]any {
	schema := map[string]any{"type": "object", "properties": properties, "additionalProperties": false}
	if required != nil {
		schema["required"] = required
	}
	return schema
}

// s4Collaboration is the Codex multi-agent namespace: three tools whose
// message is marked encrypted, and one that carries no mark at all.
func s4Collaboration() map[string]any {
	return map[string]any{"type": "namespace", "name": "collaboration", "description": "Multi-agent collaboration tools.", "tools": []any{
		s4Function("spawn_agent", "Spawn a sub-agent.", s4Object([]any{"message", "task_name"}, map[string]any{
			"message":   map[string]any{"type": "string", "description": "Task for the sub-agent.", "encrypted": true},
			"task_name": map[string]any{"type": "string"},
		})),
		s4Function("send_message", "Message an agent.", s4Object([]any{"message", "target"}, map[string]any{
			"message": map[string]any{"type": "string", "encrypted": true},
			"target":  map[string]any{"type": "string"},
		})),
		s4Function("followup_task", "Follow up on a task.", s4Object([]any{"message", "target"}, map[string]any{
			"message": map[string]any{"type": "string", "encrypted": true},
			"target":  map[string]any{"type": "string"},
		})),
		s4Function("list_agents", "List the running agents.", s4Object(nil, map[string]any{
			"all": map[string]any{"type": "boolean"},
		})),
	}}
}

// s4EncryptedOnly is a namespace with nothing but marked tools.
func s4EncryptedOnly() map[string]any {
	return map[string]any{"type": "namespace", "name": "collaboration", "tools": []any{
		s4Function("spawn_agent", "Spawn a sub-agent.", s4Object([]any{"message"}, map[string]any{
			"message": map[string]any{"type": "string", "encrypted": true},
		})),
		s4Function("send_message", "Message an agent.", s4Object([]any{"message"}, map[string]any{
			"message": map[string]any{"type": "string", "encrypted": true},
		})),
	}}
}

func s4Exec() map[string]any {
	return s4Function("exec_command", "Run a shell command.", s4Object([]any{"cmd"}, map[string]any{"cmd": map[string]any{"type": "string"}}))
}

func s4ApplyPatch() map[string]any {
	return map[string]any{"type": "custom", "name": "apply_patch", "description": "Apply a patch."}
}

// s4FlatEncrypted: the mark, but no namespace (a flat name). Not hidden.
func s4FlatEncrypted() map[string]any {
	return s4Function("collaboration__spawn_agent", "Spawn a sub-agent (flat name).", s4Object([]any{"message"}, map[string]any{
		"message": map[string]any{"type": "string", "encrypted": true},
	}))
}

// s4DeepMarks: marks nested inside items, anyOf, $defs and an inputSchema.
func s4DeepMarks() map[string]any {
	return map[string]any{"type": "namespace", "name": "vault", "tools": []any{
		s4Function("store_items", "Store items.", s4Object(nil, map[string]any{
			"items": map[string]any{"type": "array", "items": map[string]any{"type": "object", "properties": map[string]any{
				"secret": map[string]any{"type": "string", "encrypted": true},
			}}},
		})),
		s4Function("store_choice", "Store one of two shapes.", map[string]any{"type": "object", "properties": map[string]any{
			"value": map[string]any{"anyOf": []any{map[string]any{"type": "integer"}, map[string]any{"type": "string", "encrypted": true}}},
		}}),
		s4Function("store_ref", "Store by reference.", map[string]any{"type": "object",
			"properties": map[string]any{"blob": map[string]any{"$ref": "#/$defs/blob"}},
			"$defs":      map[string]any{"blob": map[string]any{"type": "string", "encrypted": true}},
		}),
		map[string]any{"type": "function", "name": "store_schema", "description": "Store via inputSchema.", "inputSchema": map[string]any{
			"type": "object", "properties": map[string]any{"token": map[string]any{"type": "string", "encrypted": true}},
		}},
	}}
}

// s4NotMarks: namespaced tools that look related but carry no mark.
func s4NotMarks() map[string]any {
	return map[string]any{"type": "namespace", "name": "lookalike", "tools": []any{
		s4Function("named_property", "A parameter called encrypted.", s4Object(nil, map[string]any{
			"encrypted": true,
			"mode":      map[string]any{"type": "string"},
		})),
		s4Function("named_flag", "A boolean parameter called encrypted.", s4Object(nil, map[string]any{
			"encrypted": map[string]any{"type": "boolean"},
		})),
		s4Function("string_mark", "The mark as a string.", s4Object(nil, map[string]any{
			"message": map[string]any{"type": "string", "encrypted": "true"},
		})),
		s4Function("false_mark", "The mark set to false.", s4Object(nil, map[string]any{
			"message": map[string]any{"type": "string", "encrypted": false},
		})),
		s4Function("described", "Mentions encrypted in a description only.", s4Object(nil, map[string]any{
			"message": map[string]any{"type": "string", "description": "\"encrypted\": true"},
		})),
	}}
}

func s4MultiAgentV1() map[string]any {
	return map[string]any{"type": "namespace", "name": "multi_agent_v1", "tools": []any{
		s4Function("spawn_agent", "Spawn (no mark).", s4Object([]any{"message"}, map[string]any{"message": map[string]any{"type": "string"}})),
	}}
}

func s4Additional(tools ...any) map[string]any {
	return map[string]any{"type": "additional_tools", "role": "developer", "tools": tools}
}

func s4SearchOutput(callID string, tools ...any) map[string]any {
	return map[string]any{"type": "tool_search_output", "call_id": callID, "status": "completed", "execution": "client", "tools": tools}
}

func s4Relay(key string, tools []any, input ...any) map[string]any {
	body := map[string]any{"model": "gpt-6-astra", "tool_choice": "auto", "parallel_tool_calls": false, "tools": tools, "input": input}
	if key != "" {
		body["prompt_cache_key"] = key
	}
	return body
}

func s4Lite(key string, input ...any) map[string]any {
	body := map[string]any{"model": "gpt-6-astra", "tool_choice": "auto", "parallel_tool_calls": false, "input": input}
	if key != "" {
		body["prompt_cache_key"] = key
	}
	return body
}

var s4CollaborationHidden = []string{"collaboration.followup_task", "collaboration.send_message", "collaboration.spawn_agent"}

func s4PromptFixtures() []s4PromptFixture {
	user := s4User("Delegate the review to a sub-agent.")
	history := []any{
		user,
		map[string]any{"type": "function_call", "id": "fc_s4_hist_spawn", "call_id": "s4_hist_spawn", "name": "spawn_agent", "namespace": "collaboration",
			"arguments": `{"message":"review src","task_name":"rev"}`},
		map[string]any{"type": "function_call_output", "call_id": "s4_hist_spawn", "output": "unsupported call: spawn_agent"},
		map[string]any{"type": "function_call", "id": "fc_s4_hist_exec", "call_id": "s4_hist_exec", "name": "exec_command", "arguments": `{"cmd":"ls"}`},
		map[string]any{"type": "function_call_output", "call_id": "s4_hist_exec", "output": "a.go"},
		map[string]any{"type": "function_call", "id": "fc_s4_hist_bare", "call_id": "s4_hist_bare", "name": "send_message",
			"arguments": `{"message":"hi","target":"rev"}`},
		map[string]any{"type": "function_call_output", "call_id": "s4_hist_bare", "output": "unsupported call: send_message"},
		s4User("continue"),
	}
	withInstructions := s4Relay("s4-prompt-instructions", []any{s4Exec(), s4Collaboration()}, user)
	withInstructions["instructions"] = "You are Codex. Use spawn_agent for parallel work."
	choiceNone := s4Relay("s4-prompt-none", []any{s4Exec(), s4Collaboration()}, user)
	choiceNone["tool_choice"] = "none"
	allowed := s4Relay("s4-prompt-allowed", []any{s4Exec(), s4Collaboration()}, user)
	allowed["tool_choice"] = map[string]any{"type": "allowed_tools", "mode": "auto", "tools": []any{
		map[string]any{"type": "function", "name": "spawn_agent", "namespace": "collaboration"},
		map[string]any{"type": "function", "name": "exec_command"},
	}}
	return []s4PromptFixture{
		{Name: "relay_mixed", Source: s4Relay("s4-prompt-mixed", []any{s4Exec(), s4Collaboration(), s4ApplyPatch()}, user), Hidden: s4CollaborationHidden},
		{Name: "relay_collaboration_first", Source: s4Relay("", []any{s4Collaboration(), s4Exec()}, user), Hidden: s4CollaborationHidden},
		{Name: "relay_instructions", Source: withInstructions, Hidden: s4CollaborationHidden},
		{Name: "relay_history", Source: s4Relay("s4-prompt-history", []any{s4Exec(), s4Collaboration()}, history...), Hidden: s4CollaborationHidden},
		{Name: "relay_allowed_tools", Source: allowed, Hidden: s4CollaborationHidden},
		{Name: "relay_deep_marks", Source: s4Relay("s4-prompt-deep", []any{s4Exec(), s4DeepMarks()}, user),
			Hidden: []string{"vault.store_choice", "vault.store_items", "vault.store_ref", "vault.store_schema"}},
		{Name: "relay_only_encrypted", Source: s4Relay("s4-prompt-only", []any{s4EncryptedOnly()}, user),
			Hidden: []string{"collaboration.send_message", "collaboration.spawn_agent"}, AllHidden: true},
		{Name: "relay_only_encrypted_with_additional", Source: s4Relay("s4-prompt-only-additional", []any{s4EncryptedOnly()}, s4Additional(s4Exec()), user),
			Hidden: []string{"collaboration.send_message", "collaboration.spawn_agent"}, AllHidden: true},
		// Nothing hidden: these must stay h22's bytes with the switch on.
		{Name: "relay_flat_name", Source: s4Relay("s4-prompt-flat", []any{s4Exec(), s4FlatEncrypted()}, user)},
		{Name: "relay_not_marks", Source: s4Relay("s4-prompt-notmarks", []any{s4Exec(), s4NotMarks(), s4MultiAgentV1()}, user)},
		{Name: "relay_choice_none", Source: choiceNone},
		{Name: "lite_collaboration", Source: s4Lite("s4-prompt-lite", s4Additional(s4Exec(), s4Collaboration()), user)},
		{Name: "relay_plus_lite_collaboration", Source: s4Relay("s4-prompt-mixed-lite", []any{s4Exec()}, s4Additional(s4Collaboration()), user)},
		{Name: "lite_search_collaboration", Source: s4Lite("s4-prompt-search", s4Additional(s4Exec()), s4SearchOutput("s4_ts", s4Collaboration()), user)},
	}
}

// s4Transport is a run_officejs call carrying the old envelope.
func s4Transport(callID, tool string, args any) map[string]any {
	code := string(jsonBytes(map[string]any{"tool": tool, "args": args}))
	return map[string]any{"type": "function_call", "id": "fc_" + callID, "call_id": callID, "name": "run_officejs", "status": "completed",
		"arguments": string(jsonBytes(map[string]any{"summary": "s", "extended_summary": "e", "destructive": false, "references": []any{}, "code": code}))}
}

func s4DecodeSources() map[string]map[string]any {
	user := s4User("decode")
	return map[string]map[string]any{
		"relay":        s4Relay("", []any{s4Exec(), s4Collaboration(), s4ApplyPatch()}, user),
		"lite":         s4Lite("", s4Additional(s4Exec(), s4Collaboration()), user),
		"relay_lite":   s4Relay("", []any{s4Exec()}, s4Additional(s4Collaboration()), user),
		"search":       s4Lite("", s4SearchOutput("s4_dec_ts", s4Collaboration()), user),
		"flat":         s4Relay("", []any{s4FlatEncrypted()}, user),
		"deep":         s4Relay("", []any{s4DeepMarks()}, user),
		"not_marks":    s4Relay("", []any{s4NotMarks(), s4MultiAgentV1()}, user),
		"choice_none":  map[string]any{"model": "gpt-6-astra", "tool_choice": "none", "tools": []any{s4Collaboration()}, "input": []any{user}},
		"forced_spawn": map[string]any{"model": "gpt-6-astra", "tools": []any{s4Exec(), s4Collaboration()}, "input": []any{user}, "tool_choice": map[string]any{"type": "function", "namespace": "collaboration", "name": "spawn_agent"}},
	}
}

func s4DecodeFixtures() []s4DecodeFixture {
	spawn := map[string]any{"message": "PLAINTEXT review src/main.go", "task_name": "rev"}
	send := map[string]any{"message": "PLAINTEXT ping", "target": "rev"}
	followup := map[string]any{"message": "PLAINTEXT again", "target": "rev"}
	var fixtures []s4DecodeFixture
	add := func(name, source string, native map[string]any, blocked bool) {
		fixtures = append(fixtures, s4DecodeFixture{Name: source + "/" + name, Source: source, Native: native, Blocked: blocked})
	}
	for _, source := range []string{"relay", "lite", "relay_lite", "search"} {
		add("spawn_agent", source, s4Transport("s4_dec_spawn", "collaboration.spawn_agent", spawn), true)
		add("send_message", source, s4Transport("s4_dec_send", "collaboration.send_message", send), true)
		add("followup_task", source, s4Transport("s4_dec_follow", "collaboration.followup_task", followup), true)
		add("list_agents", source, s4Transport("s4_dec_list", "collaboration.list_agents", map[string]any{"all": true}), false)
		add("spawn_missing_required", source, s4Transport("s4_dec_spawn_bad", "collaboration.spawn_agent", map[string]any{"message": "x"}), false)
		add("spawn_short_name", source, s4Transport("s4_dec_spawn_short", "spawn_agent", spawn), false)
		add("exec_command", source, s4Transport("s4_dec_exec", "exec_command", map[string]any{"cmd": "ls"}), false)
	}
	noCallID := s4Transport("s4_dec_noid", "collaboration.spawn_agent", spawn)
	delete(noCallID, "call_id")
	add("spawn_without_call_id", "relay", noCallID, true)
	add("flat_spawn", "flat", s4Transport("s4_dec_flat", "collaboration__spawn_agent", map[string]any{"message": "PLAINTEXT"}), false)
	add("store_items", "deep", s4Transport("s4_dec_items", "vault.store_items", map[string]any{"items": []any{map[string]any{"secret": "x"}}}), true)
	add("store_choice", "deep", s4Transport("s4_dec_choice", "vault.store_choice", map[string]any{"value": "x"}), true)
	add("store_ref", "deep", s4Transport("s4_dec_ref", "vault.store_ref", map[string]any{"blob": "x"}), true)
	add("store_schema", "deep", s4Transport("s4_dec_schema", "vault.store_schema", map[string]any{"token": "x"}), true)
	for _, name := range []string{"named_property", "named_flag", "string_mark", "false_mark", "described"} {
		add(name, "not_marks", s4Transport("s4_dec_"+name, "lookalike."+name, map[string]any{}), false)
	}
	add("multi_agent_v1", "not_marks", s4Transport("s4_dec_mav1", "multi_agent_v1.spawn_agent", map[string]any{"message": "x"}), false)
	add("spawn_agent", "choice_none", s4Transport("s4_dec_none", "collaboration.spawn_agent", spawn), false)
	add("spawn_agent", "forced_spawn", s4Transport("s4_dec_forced", "collaboration.spawn_agent", spawn), true)
	add("exec_command", "forced_spawn", s4Transport("s4_dec_forced_exec", "exec_command", map[string]any{"cmd": "ls"}), false)
	return fixtures
}

// s4ToolChunks streams the given tool items (transport or native), one output
// index each, then completes. terminalOnly items appear only in the terminal.
func s4ToolChunks(responseID string, items []map[string]any, terminalOnly []map[string]any) []string {
	chunks := []string{sseEvent("response.created", map[string]any{"type": "response.created",
		"response": map[string]any{"id": responseID, "status": "in_progress", "output": []any{}}})}
	output := []any{}
	for index, item := range items {
		added := cloneObject(item)
		delete(added, "status")
		added["arguments"] = ""
		chunks = append(chunks,
			sseEvent("response.output_item.added", map[string]any{"type": "response.output_item.added", "output_index": index, "item": added}),
			sseEvent("response.function_call_arguments.delta", map[string]any{"type": "response.function_call_arguments.delta", "output_index": index, "item_id": item["id"], "delta": item["arguments"]}),
			sseEvent("response.output_item.done", map[string]any{"type": "response.output_item.done", "output_index": index, "item": item}),
		)
		output = append(output, item)
	}
	for _, item := range terminalOnly {
		output = append(output, item)
	}
	return append(chunks, sseEvent("response.completed", map[string]any{"type": "response.completed",
		"response": map[string]any{"id": responseID, "status": "completed", "output": output}})+"data: [DONE]\n\n")
}

func s4NativeCall(callID, namespace, name, arguments string) map[string]any {
	call := map[string]any{"type": "function_call", "id": "fc_" + callID, "call_id": callID, "name": name, "arguments": arguments, "status": "completed"}
	if namespace != "" {
		call["namespace"] = namespace
	}
	return call
}

func s4StreamFixtures() []s4StreamFixture {
	user := s4User("stream")
	spawn := map[string]any{"message": "PLAINTEXT review", "task_name": "rev"}
	relay := func(key string) map[string]any {
		return s4Relay(key, []any{s4Exec(), s4Collaboration(), s4ApplyPatch()}, user)
	}
	spawnCall := s4Transport("s4_st_spawn", "collaboration.spawn_agent", spawn)
	execCall := s4Transport("s4_st_exec", "exec_command", map[string]any{"cmd": "ls"})
	sendCall := s4Transport("s4_st_send", "collaboration.send_message", map[string]any{"message": "PLAINTEXT ping", "target": "rev"})
	liteSpawn := s4Transport("s4_st_lite_spawn", "collaboration.spawn_agent", spawn)
	searchSpawn := s4Transport("s4_st_search_spawn", "collaboration.spawn_agent", spawn)
	terminalSpawn := s4Transport("s4_st_terminal_spawn", "collaboration.spawn_agent", spawn)
	nativeSpawn := s4NativeCall("s4_st_native_spawn", "collaboration", "spawn_agent", `{"message":"gAAAAAciphertext","task_name":"rev"}`)
	listCall := s4Transport("s4_st_list", "collaboration.list_agents", map[string]any{"all": false})
	return []s4StreamFixture{
		{Name: "relay_spawn", Request: relay("s4-st-1"), Chunks: s4ToolChunks("r_s4_1", []map[string]any{spawnCall}, nil), Blocked: []string{"s4_st_spawn"}},
		{Name: "relay_exec_then_send", Request: relay("s4-st-2"), Chunks: s4ToolChunks("r_s4_2", []map[string]any{execCall, sendCall}, nil), Blocked: []string{"s4_st_send"}},
		{Name: "relay_list_agents_keeps_namespace", Request: relay("s4-st-3"), Chunks: s4ToolChunks("r_s4_3", []map[string]any{listCall}, nil)},
		{Name: "lite_spawn_through_run_officejs", Request: s4Lite("s4-st-4", s4Additional(s4Exec(), s4Collaboration()), user),
			Chunks: s4ToolChunks("r_s4_4", []map[string]any{liteSpawn}, nil), Blocked: []string{"s4_st_lite_spawn"}},
		{Name: "search_spawn_through_run_officejs", Request: s4Lite("s4-st-5", s4SearchOutput("s4_st_ts", s4Collaboration()), user),
			Chunks: s4ToolChunks("r_s4_5", []map[string]any{searchSpawn}, nil), Blocked: []string{"s4_st_search_spawn"}},
		{Name: "relay_terminal_only_spawn", Request: relay("s4-st-6"), Chunks: s4ToolChunks("r_s4_6", nil, []map[string]any{terminalSpawn}), Blocked: []string{"s4_st_terminal_spawn"}},
		// Lite native call: never through the relay decoder, never changed.
		{Name: "lite_native_spawn", Request: s4Lite("s4-st-7", s4Additional(s4Exec(), s4Collaboration()), user),
			Chunks: s4ToolChunks("r_s4_7", []map[string]any{nativeSpawn}, nil)},
	}
}

func s4TransformFixtures() []s4TransformFixture {
	user := s4User("transform")
	spawn := s4Transport("s4_tr_spawn", "collaboration.spawn_agent", map[string]any{"message": "PLAINTEXT", "task_name": "rev"})
	exec := s4Transport("s4_tr_exec", "exec_command", map[string]any{"cmd": "pwd"})
	response := func(items ...any) map[string]any {
		return map[string]any{"id": "r_s4_tr", "object": "response", "status": "completed", "output": items}
	}
	return []s4TransformFixture{
		{Name: "relay_spawn_and_exec", Source: s4Relay("", []any{s4Exec(), s4Collaboration()}, user), Response: response(exec, spawn), Blocked: []string{"s4_tr_spawn"}},
		{Name: "lite_spawn", Source: s4Lite("", s4Additional(s4Collaboration()), user), Response: response(spawn), Blocked: []string{"s4_tr_spawn"}},
	}
}

// s4Scenarios: the conversation around a blocked call, through Service.Handle
// in both trees. The client's history is given as the client would send it
// after an h25 delivery (no namespace, unsupported call) and after an h22
// delivery (namespace); both replay the model's own run_officejs call from the
// cache, and a cold history goes upstream as h22 sends it.
func s4Scenarios() []s3Scenario {
	turn := func(request map[string]any, chunks []string) s3Turn {
		return s3Turn{Request: request, Chunks: chunks, Stream: true, AuthID: "cred-s4", Metadata: s3Meta("caller-s4")}
	}
	tools := func() []any { return []any{s4Exec(), s4Collaboration()} }
	spawnArgs := map[string]any{"message": "PLAINTEXT review", "task_name": "rev"}
	var all []s3Scenario
	for _, variant := range []struct {
		name      string
		namespace string
		stream    bool
	}{{"h25_echo", "", true}, {"h22_echo", "collaboration", true}, {"h25_echo_nonstream", "", false}} {
		key := "s4-conv-" + variant.name
		callID := "s4_conv_" + variant.name
		user := s4User("Delegate " + variant.name)
		echo := map[string]any{"type": "function_call", "id": "fc_" + callID, "call_id": callID, "name": "spawn_agent", "arguments": string(jsonBytes(spawnArgs))}
		if variant.namespace != "" {
			echo["namespace"] = variant.namespace
		}
		first := turn(s4Relay(key, tools(), user), s3CallChunks("r_s4c_1", s4Transport(callID, "collaboration.spawn_agent", spawnArgs)))
		second := turn(s4Relay(key, tools(), user, echo, s3Output(callID, "unsupported call: spawn_agent"), s4User("then do it yourself")), s3TextChunks("r_s4c_2"))
		first.Stream, second.Stream = variant.stream, variant.stream
		all = append(all, s3Scenario{Name: "conversation_" + variant.name, Turns: []s3Turn{first, second}})
	}
	// Cold: the replay cache never saw the call (expired, restarted).
	coldCall := map[string]any{"type": "function_call", "id": "fc_s4_cold", "call_id": "s4_cold", "name": "spawn_agent", "arguments": `{"message":"PLAINTEXT","task_name":"rev"}`}
	all = append(all, s3Scenario{Name: "conversation_cold", Turns: []s3Turn{
		turn(s4Relay("s4-conv-cold", tools(), s4User("cold"), coldCall, s3Output("s4_cold", "unsupported call: spawn_agent"), s4User("next")), s3TextChunks("r_s4c_cold")),
	}})
	// Lite: the native call and its history are untouched.
	native := s4NativeCall("s4_conv_native", "collaboration", "spawn_agent", `{"message":"gAAAAAciphertext","task_name":"rev"}`)
	nativeEcho := cloneObject(native)
	delete(nativeEcho, "status")
	all = append(all, s3Scenario{Name: "conversation_lite_native", Turns: []s3Turn{
		turn(s4Lite("s4-conv-lite", s4Additional(s4Exec(), s4Collaboration()), s4User("lite")), s3CallChunks("r_s4c_lite_1", native)),
		turn(s4Lite("s4-conv-lite", s4Additional(s4Exec(), s4Collaboration()), s4User("lite"), nativeEcho, s3Output("s4_conv_native", "spawned"), s4User("next")), s3TextChunks("r_s4c_lite_2")),
	}})
	return all
}
