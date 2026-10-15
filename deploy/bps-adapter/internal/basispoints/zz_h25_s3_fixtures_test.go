package basispoints

// H25 S3 fixtures: multi-turn conversations driven end to end through
// Service.Handle (executor.execute_stream / executor.execute) against a fake
// host. Each turn records the exact upstream body the plugin sent and the
// tool calls the client received. The file compiles unchanged in the
// h22-live tree, where testdata/h22_s3_golden_gen.go.txt records h22's own
// outputs into testdata/h22_s3_golden.json.
//
// Every scenario uses its own prompt_cache_key / first message and call ids:
// the replay caches are process-wide in both trees, so scenarios must not
// share a scope with each other or with any other test.

import (
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

type s3Turn struct {
	Request  map[string]any
	Chunks   []string
	Stream   bool
	AuthID   string
	Metadata map[string]any
	Headers  http.Header
}

type s3Scenario struct {
	Name     string
	Identity string // "" = default config (identity off); "hygiene"/"thread" with plain_prologue
	Turns    []s3Turn
}

type s3TurnResult struct {
	Upstream  string   `json:"upstream"`
	Status    int      `json:"status"`
	Kind      string   `json:"kind"`
	CloseErr  bool     `json:"close_err"`
	Delivered []string `json:"delivered"`
}

func s3User(text string) map[string]any {
	return map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": text}}}
}

func s3RelayTools() []any {
	return []any{
		map[string]any{"type": "function", "name": "get_weather", "description": "Weather <now> & later", "parameters": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"city": map[string]any{"type": "string"},
				"n":    map[string]any{"type": "number"},
				"big":  map[string]any{"type": "integer"},
			},
			"required": []any{"city"},
		}},
		map[string]any{"type": "custom", "name": "apply_patch", "description": "apply a patch"},
		map[string]any{"type": "function", "name": "exec_command", "parameters": map[string]any{
			"type": "object", "properties": map[string]any{"cmd": map[string]any{"type": "string"}}, "required": []any{"cmd"}}},
		map[string]any{"type": "namespace", "name": "mcp__srv", "description": "srv", "tools": []any{
			map[string]any{"type": "function", "name": "lookup", "parameters": map[string]any{
				"type": "object", "properties": map[string]any{"q": map[string]any{"type": "string"}},
			}},
		}},
	}
}

func s3LiteDecl() map[string]any {
	return map[string]any{"type": "additional_tools", "role": "developer", "tools": []any{
		map[string]any{"type": "namespace", "name": "shell", "description": "shell", "tools": []any{
			map[string]any{"type": "function", "name": "exec_command", "parameters": map[string]any{
				"type": "object", "properties": map[string]any{"cmd": map[string]any{"type": "string"}}}},
		}},
		map[string]any{"type": "namespace", "name": "collab", "description": "agents", "tools": []any{
			map[string]any{"type": "function", "name": "wait_agent", "parameters": map[string]any{
				"type": "object", "properties": map[string]any{"id": map[string]any{"type": "string"}}}},
		}},
	}}
}

// s3Relay is a relay-mode request (top-level tools).
func s3Relay(key string, input ...any) map[string]any {
	body := map[string]any{"model": "gpt-6-astra", "tool_choice": "auto", "tools": s3RelayTools(), "input": input}
	if key != "" {
		body["prompt_cache_key"] = key
	}
	return body
}

// s3Lite is a Lite request (additional_tools only).
func s3Lite(key string, input ...any) map[string]any {
	body := map[string]any{"model": "gpt-6-astra", "tool_choice": "auto", "parallel_tool_calls": false, "input": input}
	if key != "" {
		body["prompt_cache_key"] = key
	}
	return body
}

func s3Transport(callID, tool string, args any) map[string]any {
	code := string(jsonBytes(map[string]any{"tool": tool, "args": args}))
	return map[string]any{"type": "function_call", "id": "fc_" + callID, "call_id": callID, "name": "run_officejs", "status": "completed",
		"arguments": string(jsonBytes(map[string]any{"summary": "model summary", "extended_summary": "model words", "code": code, "destructive": false, "references": []any{"Sheet1!A1"}}))}
}

func s3Call(callID, name, args string) map[string]any {
	return map[string]any{"type": "function_call", "id": "fc_" + callID, "call_id": callID, "name": name, "arguments": args}
}

func s3Custom(callID, name, input string) map[string]any {
	return map[string]any{"type": "custom_tool_call", "id": "ctc_" + callID, "call_id": callID, "name": name, "input": input}
}

func s3Output(callID string, output any) map[string]any {
	return map[string]any{"type": "function_call_output", "call_id": callID, "output": output}
}

func s3CustomOutput(callID string, output any) map[string]any {
	return map[string]any{"type": "custom_tool_call_output", "call_id": callID, "output": output}
}

func s3With(item map[string]any, edit func(map[string]any)) map[string]any {
	copy := cloneObject(item)
	edit(copy)
	return copy
}

// s3CallChunks streams one tool item (transport or native).
func s3CallChunks(responseID string, item map[string]any) []string {
	added := cloneObject(item)
	delete(added, "status")
	kind := stringValue(item["type"])
	if kind == "function_call" {
		added["arguments"] = ""
	} else {
		added["input"] = ""
	}
	chunks := []string{
		sseEvent("response.created", map[string]any{"type": "response.created", "response": map[string]any{"id": responseID, "status": "in_progress", "output": []any{}}}),
		sseEvent("response.output_item.added", map[string]any{"type": "response.output_item.added", "output_index": 0, "item": added}),
	}
	if kind == "function_call" {
		chunks = append(chunks, sseEvent("response.function_call_arguments.delta", map[string]any{"type": "response.function_call_arguments.delta", "output_index": 0, "item_id": item["id"], "delta": item["arguments"]}))
	} else {
		chunks = append(chunks, sseEvent("response.custom_tool_call_input.delta", map[string]any{"type": "response.custom_tool_call_input.delta", "output_index": 0, "item_id": item["id"], "delta": item["input"]}))
	}
	return append(chunks,
		sseEvent("response.output_item.done", map[string]any{"type": "response.output_item.done", "output_index": 0, "item": item}),
		sseEvent("response.completed", map[string]any{"type": "response.completed", "response": map[string]any{"id": responseID, "status": "completed", "output": []any{item}}})+"data: [DONE]\n\n",
	)
}

func s3TextChunks(responseID string) []string {
	text := map[string]any{"type": "message", "id": "msg_" + responseID, "role": "assistant", "status": "completed",
		"content": []any{map[string]any{"type": "output_text", "text": "ok"}}}
	return []string{
		sseEvent("response.created", map[string]any{"type": "response.created", "response": map[string]any{"id": responseID, "status": "in_progress", "output": []any{}}}),
		sseEvent("response.output_item.added", map[string]any{"type": "response.output_item.added", "output_index": 0, "item": map[string]any{"type": "message", "id": "msg_" + responseID, "role": "assistant", "content": []any{}}}),
		sseEvent("response.output_item.done", map[string]any{"type": "response.output_item.done", "output_index": 0, "item": text}),
		sseEvent("response.completed", map[string]any{"type": "response.completed", "response": map[string]any{"id": responseID, "status": "completed", "output": []any{text}}}) + "data: [DONE]\n\n",
	}
}

func s3Meta(caller string) map[string]any {
	if caller == "" {
		return map[string]any{}
	}
	return map[string]any{"caller_scope": caller}
}

// s3Scenarios is the fixture set. Names are stable keys into the golden.
func s3Scenarios() []s3Scenario {
	var all []s3Scenario
	add := func(name, identity string, turns ...s3Turn) {
		all = append(all, s3Scenario{Name: name, Identity: identity, Turns: turns})
	}
	turn := func(request map[string]any, chunks []string) s3Turn {
		return s3Turn{Request: request, Chunks: chunks, Stream: true, AuthID: "cred-A", Metadata: s3Meta("caller-main")}
	}
	// Relay function call, then the client's history comes back in several
	// shapes. h22 replays its cached run_officejs for every one of them.
	weatherArgs := map[string]any{"city": "Paris", "n": json.Number("1000"), "big": json.Number("12345678901234567890")}
	for _, variant := range []struct {
		name string
		echo func(map[string]any) map[string]any
	}{
		{"exact", func(m map[string]any) map[string]any { return m }},
		{"status_added", func(m map[string]any) map[string]any {
			return s3With(m, func(c map[string]any) { c["status"] = "completed" })
		}},
		{"key_added_by_client", func(m map[string]any) map[string]any {
			return s3With(m, func(c map[string]any) {
				c["arguments"] = `{"big":12345678901234567890,"city":"Paris","description":"added","n":1000}`
			})
		}},
		{"user_edit", func(m map[string]any) map[string]any {
			return s3With(m, func(c map[string]any) { c["arguments"] = `{"big":12345678901234567890,"city":"Oslo","n":1000}` })
		}},
		{"bigint_rounded", func(m map[string]any) map[string]any {
			return s3With(m, func(c map[string]any) { c["arguments"] = `{"big":12345678901234567000,"city":"Paris","n":1000}` })
		}},
		{"id_dropped", func(m map[string]any) map[string]any { return s3With(m, func(c map[string]any) { delete(c, "id") }) }},
		{"id_reminted", func(m map[string]any) map[string]any {
			return s3With(m, func(c map[string]any) { c["id"] = "fc_client_minted" })
		}},
		{"args_not_json", func(m map[string]any) map[string]any {
			return s3With(m, func(c map[string]any) { c["arguments"] = `{"city":"Paris"` })
		}},
		{"name_changed", func(m map[string]any) map[string]any {
			return s3With(m, func(c map[string]any) { c["name"] = "exec_command" })
		}},
	} {
		callID := "call_s3_relay_" + variant.name
		key := "s3-relay-" + variant.name
		delivered := s3Call(callID, "get_weather", `{"big":12345678901234567890,"city":"Paris","n":1000}`)
		echo := variant.echo(delivered)
		add("relay_fn_"+variant.name, "",
			turn(s3Relay(key, s3User("weather?")), s3CallChunks("r1", s3Transport(callID, "get_weather", weatherArgs))),
			turn(s3Relay(key, s3User("weather?"), echo, s3Output(callID, "sunny"), s3User("and tomorrow?")), s3TextChunks("r2")),
			turn(s3Relay(key, s3User("weather?"), echo, s3Output(callID, "sunny"), s3User("and tomorrow?"), s3User("thanks")), s3TextChunks("r3")),
		)
	}
	// Scope sources: h22 keys on prompt_cache_key, then session ids, then the
	// first input item. None of them involves the upstream credential.
	{
		callID := "call_s3_fp"
		delivered := s3Call(callID, "exec_command", `{"cmd":"ls"}`)
		first := s3User("s3 first-item scope: unique opener")
		add("scope_first_item", "",
			turn(s3Relay("", first), s3CallChunks("r1", s3Transport(callID, "exec_command", map[string]any{"cmd": "ls"}))),
			turn(s3Relay("", first, delivered, s3Output(callID, "a.txt"), s3User("next")), s3TextChunks("r2")),
		)
	}
	{
		callID := "call_s3_cm"
		delivered := s3Call(callID, "exec_command", `{"cmd":"pwd"}`)
		withSession := func(body map[string]any) map[string]any {
			body["client_metadata"] = map[string]any{"session_id": "s3-cm-session"}
			return body
		}
		t1 := turn(withSession(s3Relay("", s3User("cm"))), s3CallChunks("r1", s3Transport(callID, "exec_command", map[string]any{"cmd": "pwd"})))
		t1.Headers = http.Header{"Session_id": []string{"s3-cm-session"}}
		t2 := turn(withSession(s3Relay("", s3User("cm"), delivered, s3Output(callID, "/"), s3User("next"))), s3TextChunks("r2"))
		t2.Headers = http.Header{"Session_id": []string{"s3-cm-session"}}
		add("scope_client_metadata_session", "", t1, t2)
	}
	{
		callID := "call_s3_cred"
		key := "s3-credential-switch"
		delivered := s3Call(callID, "exec_command", `{"cmd":"whoami"}`)
		t2 := turn(s3Relay(key, s3User("cred"), delivered, s3Output(callID, "me"), s3User("next")), s3TextChunks("r2"))
		t2.AuthID = "cred-B"
		t3 := turn(s3Relay(key, s3User("cred"), delivered, s3Output(callID, "me"), s3User("next"), s3User("more")), s3TextChunks("r3"))
		t3.AuthID = "cred-A"
		add("scope_credential_switch", "",
			turn(s3Relay(key, s3User("cred")), s3CallChunks("r1", s3Transport(callID, "exec_command", map[string]any{"cmd": "whoami"}))), t2, t3)
	}
	{
		callID := "call_s3_nocaller"
		key := "s3-no-caller"
		delivered := s3Call(callID, "exec_command", `{"cmd":"date"}`)
		t1 := turn(s3Relay(key, s3User("nocaller")), s3CallChunks("r1", s3Transport(callID, "exec_command", map[string]any{"cmd": "date"})))
		t1.Metadata = s3Meta("")
		t2 := turn(s3Relay(key, s3User("nocaller"), delivered, s3Output(callID, "today"), s3User("next")), s3TextChunks("r2"))
		t2.Metadata = s3Meta("")
		add("scope_no_caller", "", t1, t2)
	}
	// Custom relay tool.
	for _, variant := range []struct {
		name string
		echo func(string) string
	}{
		{"exact", func(s string) string { return s }},
		{"trailing_newline_stripped", func(s string) string { return strings.TrimRight(s, "\n") }},
		{"crlf", func(s string) string { return strings.ReplaceAll(s, "\n", "\r\n") }},
	} {
		callID := "call_s3_patch_" + variant.name
		key := "s3-patch-" + variant.name
		patch := "*** Begin Patch\n*** Add File: a.txt\n+hello\n*** End Patch\n"
		echo := s3Custom(callID, "apply_patch", variant.echo(patch))
		add("relay_custom_"+variant.name, "",
			turn(s3Relay(key, s3User("patch")), s3CallChunks("r1", s3Transport(callID, "apply_patch", patch))),
			turn(s3Relay(key, s3User("patch"), echo, s3CustomOutput(callID, "Done"), s3User("next")), s3TextChunks("r2")),
		)
	}
	// Namespaced relay tool.
	for _, variant := range []struct {
		name string
		echo func(map[string]any) map[string]any
	}{
		{"exact", func(m map[string]any) map[string]any { return m }},
		{"namespace_omitted", func(m map[string]any) map[string]any {
			return s3With(m, func(c map[string]any) { delete(c, "namespace") })
		}},
		{"flattened", func(m map[string]any) map[string]any {
			return s3With(m, func(c map[string]any) { c["name"] = "mcp__srv__lookup"; delete(c, "namespace") })
		}},
	} {
		callID := "call_s3_ns_" + variant.name
		key := "s3-ns-" + variant.name
		delivered := s3With(s3Call(callID, "lookup", `{"q":"x"}`), func(c map[string]any) { c["namespace"] = "mcp__srv" })
		add("relay_namespace_"+variant.name, "",
			turn(s3Relay(key, s3User("ns")), s3CallChunks("r1", s3Transport(callID, "mcp__srv.lookup", map[string]any{"q": "x"}))),
			turn(s3Relay(key, s3User("ns"), variant.echo(delivered), s3Output(callID, "found"), s3User("next")), s3TextChunks("r2")),
		)
	}
	// Compaction kept the output and dropped the call; a later request has
	// no tools at all (summary turn); tool_choice none.
	{
		callID := "call_s3_compact"
		key := "s3-compact"
		delivered := s3Call(callID, "exec_command", `{"cmd":"cat a"}`)
		noTools := s3Relay(key, s3User("compact"), delivered, s3Output(callID, "aaa"), s3User("summarize"))
		delete(noTools, "tools")
		delete(noTools, "tool_choice")
		none := s3Relay(key, s3User("compact"), delivered, s3Output(callID, "aaa"), s3User("no tools now"))
		none["tool_choice"] = "none"
		add("relay_compaction_and_toolless", "",
			turn(s3Relay(key, s3User("compact")), s3CallChunks("r1", s3Transport(callID, "exec_command", map[string]any{"cmd": "cat a"}))),
			turn(s3Relay(key, s3User("compact"), s3Output(callID, "aaa"), s3User("next")), s3TextChunks("r2")),
			turn(noTools, s3TextChunks("r3")),
			turn(none, s3TextChunks("r4")),
		)
	}
	// Output before its call; empty and structured outputs; the same call_id
	// delivered twice with different content (later delivery replays).
	{
		callID := "call_s3_order"
		key := "s3-order"
		delivered := s3Call(callID, "exec_command", `{"cmd":"id"}`)
		add("relay_output_before_call", "",
			turn(s3Relay(key, s3User("order")), s3CallChunks("r1", s3Transport(callID, "exec_command", map[string]any{"cmd": "id"}))),
			turn(s3Relay(key, s3User("order"), s3Output(callID, "uid=0"), delivered, s3User("next")), s3TextChunks("r2")),
		)
	}
	{
		key := "s3-outputs"
		ids := []string{"call_s3_out_empty", "call_s3_out_space", "call_s3_out_image", "call_s3_out_mixed", "call_s3_out_cipher"}
		var history []any
		history = append(history, s3User("outputs"))
		turns := []s3Turn{}
		for index, callID := range ids {
			prefix := append([]any{}, history...)
			turns = append(turns, turn(s3Relay(key, prefix...), s3CallChunks("r"+callID, s3Transport(callID, "exec_command", map[string]any{"cmd": "echo"}))))
			var output any
			switch index {
			case 0:
				output = ""
			case 1:
				output = " \n"
			case 2:
				output = []any{map[string]any{"type": "input_image", "image_url": "https://example.invalid/a.png"}}
			case 3:
				output = []any{map[string]any{"type": "input_text", "text": "child said"}, map[string]any{"type": "encrypted_content", "encrypted_content": "gAAAAABfake"}}
			case 4:
				output = []any{map[string]any{"type": "encrypted_content", "encrypted_content": "gAAAAABfake2"}}
			}
			history = append(history, s3Call(callID, "exec_command", `{"cmd":"echo"}`), s3Output(callID, output))
		}
		history = append(history, s3User("next"))
		turns = append(turns, turn(s3Relay(key, history...), s3TextChunks("r-final")))
		add("relay_outputs_empty_structured_opaque", "", turns...)
	}
	{
		callID := "call_s3_twice"
		key := "s3-twice"
		add("relay_same_call_id_delivered_twice", "",
			turn(s3Relay(key, s3User("twice")), s3CallChunks("r1", s3Transport(callID, "exec_command", map[string]any{"cmd": "first"}))),
			turn(s3Relay(key, s3User("twice"), s3User("again")), s3CallChunks("r2", s3Transport(callID, "exec_command", map[string]any{"cmd": "second"}))),
			turn(s3Relay(key, s3User("twice"), s3Call(callID, "exec_command", `{"cmd":"first"}`), s3Output(callID, "1"), s3User("next")), s3TextChunks("r3")),
		)
	}
	// A run_officejs call that reached the client (h17 leak) comes back in
	// its history; a later request carries only its output.
	{
		callID := "call_s3_leaked"
		key := "s3-leaked"
		leaked := s3Transport(callID, "exec_command", map[string]any{"cmd": "leak"})
		delete(leaked, "status")
		add("relay_leaked_transport_in_history", "",
			turn(s3Relay(key, s3User("leak"), leaked, s3Output(callID, "x"), s3User("next")), s3TextChunks("r1")),
			turn(s3Relay(key, s3User("leak"), s3Output(callID, "x"), s3User("next"), s3User("more")), s3TextChunks("r2")),
		)
	}
	// Cold history (never delivered by this process): a declared tool is
	// wrapped as run_officejs whatever its shape -- namespace, arguments that
	// are not JSON (deepseek-harness), duplicate keys, empty arguments,
	// ciphertext fields -- and an undeclared upstream-native call is sent as
	// it is, with its namespace restored where known.
	{
		key := "s3-cold"
		history := []any{s3User("cold"),
			s3Call("call_s3_cold_plain", "exec_command", `{"cmd":"ls"}`), s3Output("call_s3_cold_plain", "a"),
			s3With(s3Call("call_s3_cold_ns", "exec_command", `{"cmd":"ls"}`), func(c map[string]any) { c["namespace"] = "functions" }), s3Output("call_s3_cold_ns", "b"),
			s3Call("call_s3_cold_badjson", "exec_command", `{"cmd":"dir C:\Users"}`), s3Output("call_s3_cold_badjson", "c"),
			s3Call("call_s3_cold_dupkey", "exec_command", `{"cmd":"a","cmd":"b"}`), s3Output("call_s3_cold_dupkey", "d"),
			s3Call("call_s3_cold_empty", "exec_command", ``), s3Output("call_s3_cold_empty", "e"),
			s3With(s3Call("call_s3_cold_cipher", "exec_command", `{"cmd":"x"}`), func(c map[string]any) { c["encrypted_function_args"] = "gAAAAcipher" }), s3Output("call_s3_cold_cipher", "f"),
			s3Custom("call_s3_cold_custom", "apply_patch", "*** Begin Patch\n*** End Patch\n"), s3CustomOutput("call_s3_cold_custom", "g"),
			s3Call("call_s3_cold_native", "update_plan", `{"plan":[]}`), s3Output("call_s3_cold_native", "h"),
			s3Call("call_s3_cold_lookup", "lookup", `{"q":"y"}`), s3Output("call_s3_cold_lookup", "i"),
			s3User("next"),
		}
		add("relay_cold_history_shapes", "", turn(s3Relay(key, history...), s3TextChunks("r1")))
	}
	// Lite (native) conversations: namespace memory across turns, a client
	// that edits a delivered native call, unverifiable native history, and
	// h22's declaration precedence.
	{
		callID := "call_s3_lite"
		key := "s3-lite"
		native := map[string]any{"type": "function_call", "id": "fc_" + callID, "call_id": callID, "name": "exec_command", "namespace": "shell",
			"status": "completed", "arguments": `{"cmd":"ls -la"}`}
		echo := s3Call(callID, "exec_command", `{"cmd":"ls -la"}`)
		edited := s3Call(callID, "exec_command", `{"cmd":"ls"}`)
		add("lite_namespace_memory_and_edits", "",
			turn(s3Lite(key, s3LiteDecl(), s3User("lite")), s3CallChunks("r1", native)),
			turn(s3Lite(key, s3User("lite"), echo, s3Output(callID, "files"), s3User("next")), s3TextChunks("r2")),
			turn(s3Lite(key, s3User("lite"), edited, s3Output(callID, "files"), s3User("next")), s3TextChunks("r3")),
			turn(s3Lite(key, s3User("lite"), s3With(echo, func(c map[string]any) { c["id"] = "fc_rewritten" }), s3Output(callID, "files"), s3User("next")), s3TextChunks("r4")),
		)
	}
	{
		key := "s3-lite-unverified"
		history := []any{s3LiteDecl(), s3User("lite unverified"),
			s3Call("call_s3_lu_empty", "wait_agent", ``), s3Output("call_s3_lu_empty", "ok"),
			s3Call("call_s3_lu_dup", "wait_agent", `{"id":"a","id":"b"}`), s3Output("call_s3_lu_dup", "ok"),
			s3With(s3Call("call_s3_lu_cipher", "wait_agent", `{"id":"a"}`), func(c map[string]any) { c["encrypted_function_args"] = "gAAAAcipher" }), s3Output("call_s3_lu_cipher", "ok"),
			s3User("next"),
		}
		add("lite_unverified_history_namespaces", "", turn(s3Lite(key, history...), s3TextChunks("r1")))
	}
	{
		key := "s3-lite-precedence"
		searched := map[string]any{"type": "tool_search_output", "call_id": "ts_s3", "status": "completed", "execution": "client", "tools": []any{
			map[string]any{"type": "namespace", "name": "mcp__agents", "tools": []any{map[string]any{"type": "function", "name": "wait_agent"}}},
			map[string]any{"type": "namespace", "name": "mcp__a", "tools": []any{map[string]any{"type": "function", "name": "run"}}},
			map[string]any{"type": "namespace", "name": "mcp__b", "tools": []any{map[string]any{"type": "function", "name": "run"}}},
		}}
		twice := map[string]any{"type": "additional_tools", "role": "developer", "tools": []any{
			map[string]any{"type": "namespace", "name": "first_ns", "tools": []any{map[string]any{"type": "function", "name": "twice"}}},
			map[string]any{"type": "namespace", "name": "second_ns", "tools": []any{map[string]any{"type": "function", "name": "twice"}}},
		}}
		history := []any{s3LiteDecl(), twice, s3User("precedence"), searched,
			s3Call("call_s3_pr_wait", "wait_agent", `{}`), s3Output("call_s3_pr_wait", "ok"),
			s3Call("call_s3_pr_run", "run", `{}`), s3Output("call_s3_pr_run", "ok"),
			s3Call("call_s3_pr_twice", "twice", `{}`), s3Output("call_s3_pr_twice", "ok"),
			s3User("next"),
		}
		add("lite_namespace_precedence", "", turn(s3Lite(key, history...), s3TextChunks("r1")))
	}
	// A relay conversation whose later request has no declarations at all:
	// h22's per-scope namespace memory restores what earlier turns declared.
	{
		key := "s3-ns-memory-relay"
		first := s3Lite(key, s3LiteDecl(), s3User("mem"))
		later := s3Relay(key, s3User("mem"), s3Call("call_s3_mem_wait", "wait_agent", `{"id":"1"}`), s3Output("call_s3_mem_wait", "done"), s3User("next"))
		add("namespace_memory_without_declarations", "", turn(first, s3TextChunks("r1")), turn(later, s3TextChunks("r2")))
	}
	// Non-streaming delivery, then a streamed turn.
	{
		callID := "call_s3_nonstream"
		key := "s3-nonstream"
		delivered := s3Call(callID, "exec_command", `{"cmd":"uptime"}`)
		t1 := turn(s3Relay(key, s3User("nonstream")), s3CallChunks("r1", s3Transport(callID, "exec_command", map[string]any{"cmd": "uptime"})))
		t1.Stream = false
		add("relay_non_stream_delivery", "", t1,
			turn(s3Relay(key, s3User("nonstream"), s3With(delivered, func(c map[string]any) { c["arguments"] = `{"cmd":"uptime","extra":true}` }), s3Output(callID, "up"), s3User("next")), s3TextChunks("r2")),
		)
	}
	// The production identity profile: plain_prologue, hygiene and thread.
	for _, identity := range []string{"hygiene", "thread"} {
		callID := "call_s3_identity_" + identity
		key := "s3-identity-" + identity
		delivered := s3Call(callID, "exec_command", `{"cmd":"hostname"}`)
		add("identity_"+identity, identity,
			turn(s3Relay(key, s3User("identity")), s3CallChunks("r1", s3Transport(callID, "exec_command", map[string]any{"cmd": "hostname"}))),
			turn(s3Relay(key, s3User("identity"), delivered, s3Output(callID, "box"), s3Call("call_s3_identity_cold_"+identity, "exec_command", `{"cmd":"x"}`), s3Output("call_s3_identity_cold_"+identity, ""), s3User("next")), s3TextChunks("r2")),
		)
	}
	return all
}

// s3RunScenario runs every turn of a scenario. configure (nil = defaults)
// edits the service configuration first.
func s3RunScenario(t *testing.T, scenario s3Scenario, configure func(*Config)) []s3TurnResult {
	t.Helper()
	service := NewService()
	cfg := service.config()
	if scenario.Identity != "" {
		cfg.ClientIdentity.Mode = scenario.Identity
		cfg.ClientIdentity.PlainPrologue = true
	}
	if configure != nil {
		configure(&cfg)
	}
	service.mu.Lock()
	service.cfg = cfg
	service.mu.Unlock()
	results := make([]s3TurnResult, 0, len(scenario.Turns))
	for _, turn := range scenario.Turns {
		result := s3RunTurn(t, service, turn)
		if scenario.Identity != "" {
			result.Upstream = s3MaskTurnID(result.Upstream)
		}
		results = append(results, result)
	}
	return results
}

func s3RunTurn(t *testing.T, service *Service, turn s3Turn) s3TurnResult {
	t.Helper()
	var (
		mu       sync.Mutex
		next     int
		done     = make(chan struct{})
		closed   bool
		emits    []string
		upstream string
		result   s3TurnResult
	)
	service.SetHost(func(method string, payload any, out any) error {
		request, _ := payload.(map[string]any)
		switch method {
		case "host.http.do_stream", "host.http.do":
			if body, ok := request["body"].([]byte); ok {
				mu.Lock()
				upstream = string(body)
				mu.Unlock()
			}
			if method == "host.http.do" {
				response := out.(*upstreamResponse)
				response.StatusCode = 200
				response.Headers = http.Header{"Content-Type": []string{"text/event-stream"}}
				response.Body = []byte(strings.Join(turn.Chunks, ""))
				return nil
			}
			stream := out.(*upstreamStream)
			stream.StatusCode = 200
			stream.StreamID = "up-s3"
			return nil
		case "host.http.stream_read":
			chunk := out.(*streamChunk)
			mu.Lock()
			defer mu.Unlock()
			if next < len(turn.Chunks) {
				chunk.Payload = []byte(turn.Chunks[next])
				next++
				return nil
			}
			chunk.Done = true
			return nil
		case "host.stream.emit":
			mu.Lock()
			emits = append(emits, string(request["payload"].([]byte)))
			mu.Unlock()
			return nil
		case "host.stream.close":
			mu.Lock()
			if message, ok := request["error"].(string); ok && message != "" {
				result.CloseErr = true
			}
			if !closed {
				closed = true
				close(done)
			}
			mu.Unlock()
			return nil
		}
		return nil
	})
	request := cloneObject(turn.Request)
	request["stream"] = turn.Stream
	envelope := map[string]any{
		"OriginalRequest": jsonBytes(request), "Payload": jsonBytes(request),
		"StorageJSON": streamTestCredential(), "Model": "gpt-6-astra", "Stream": turn.Stream,
		"AuthID": turn.AuthID, "AuthProvider": Provider, "Metadata": turn.Metadata, "Headers": turn.Headers,
	}
	method := "executor.execute"
	if turn.Stream {
		envelope["stream_id"] = "client-s3"
		method = "executor.execute_stream"
	}
	raw, _ := json.Marshal(envelope)
	response, err := service.Handle(method, raw)
	if err != nil {
		var api *APIError
		if errors.As(err, &api) {
			result.Status, result.Kind = api.Status, api.Kind
		} else {
			result.Kind = "error"
		}
		mu.Lock()
		result.Upstream = upstream
		mu.Unlock()
		return result
	}
	var final map[string]any
	if turn.Stream {
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("stream never closed")
		}
		mu.Lock()
		joined := strings.Join(emits, "")
		mu.Unlock()
		for _, block := range strings.Split(joined, "\n\n") {
			for _, line := range strings.Split(block, "\n") {
				var event map[string]any
				if strings.HasPrefix(line, "data: ") && json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event) == nil && event["type"] == "response.completed" {
					final, _ = event["response"].(map[string]any)
				}
			}
		}
	} else if body, ok := response.(map[string]any); ok {
		if payload, ok := body["Payload"].([]byte); ok {
			_ = json.Unmarshal(payload, &final)
		}
	}
	output, _ := final["output"].([]any)
	for _, value := range output {
		item, _ := value.(map[string]any)
		if kind := stringValue(item["type"]); kind == "function_call" || kind == "custom_tool_call" {
			result.Delivered = append(result.Delivered, s3CanonicalCall(item))
		}
	}
	mu.Lock()
	result.Upstream = upstream
	mu.Unlock()
	return result
}

// s3CanonicalCall keeps the fields a client executes, as sorted JSON.
func s3CanonicalCall(item map[string]any) string {
	kept := map[string]any{}
	for _, key := range []string{"type", "id", "call_id", "name", "namespace", "arguments", "input"} {
		if value, ok := item[key]; ok {
			kept[key] = value
		}
	}
	// encoding/json writes map keys sorted.
	return string(jsonBytes(kept))
}

var s3TurnIDPattern = regexp.MustCompile(`"turn_id":"[0-9a-f-]{36}"`)

// s3MaskTurnID hides the one value an identity mode derives from the arrival
// time of a turn (turnIDFor); everything else in the body is deterministic.
func s3MaskTurnID(body string) string {
	return s3TurnIDPattern.ReplaceAllString(body, `"turn_id":"<arrival-time>"`)
}
