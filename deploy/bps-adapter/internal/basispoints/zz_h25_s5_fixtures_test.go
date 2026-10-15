package basispoints

// H25 S5 fixtures: the whole plugin, end to end, under the production
// configuration.
//
// Every case registers a fresh Service through plugin.register with a
// config.yaml -- the same parse, reset list and fallback path production
// runs -- and drives it through executor.execute_stream / executor.execute
// against a fake host. Each turn records the exact upstream request (body
// bytes and headers), everything the client received (normalized events and
// the close message, or the whole non-stream response) and any executor
// error.
//
// This file compiles unchanged in the h22-live tree, next to the S2, S3 and
// S4 fixture files, where testdata/h22_s5_golden_gen.go.txt records h22's own
// outputs into testdata/h22_s5_golden.json. Keep it free of anything h22 does
// not have. Every cache key and call id here is unique to this file: the
// replay caches are process-wide in both trees.

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---------------------------------------------------------------- profiles

// s5Profile is one plugin configuration. production is the oai-basispoints
// section of the production config.yaml of 2026-10-07 (tools/equiv
// profiles.json, base): the fake responses_url is never contacted, the
// pepper and public_base_url are test values, the concurrency caps are left
// out and quota sampling is off (it is background traffic that never touches
// a request). identity_off is a minimal file with every identity switch off.
type s5Profile struct {
	Name string
	YAML string
	// Plain: the relay prologue uses the plain wording (plain_prologue from
	// hygiene upwards).
	Plain bool
}

const s5Pepper = "5e5e000000000000000000000000000000000000000000000000000000000005"

func s5Profiles() []s5Profile {
	production := `responses_url: https://bps.openai.com/basispoints/api/responses
upstream_model: gpt-6-astra
models: [gpt-6-astra, gpt-5.6-sol, gpt-5.6-terra, gpt-5.6-luna, gpt-6-sol]
model_upstreams:
  gpt-6-sol: gpt-6-sol
timeout_seconds: 1800
max_response_bytes: 134217728
public_base_url: https://s5.invalid/bp-img
quota_sample_seconds: 0
downgrade_guard_enabled: true
client_identity:
  mode: thread
  pepper: "` + s5Pepper + `"
  accept_language: en-US,en;q=0.9
  send_browser_headers: true
  send_sentry_trace: true
  keep_cookies: true
  plain_prologue: true
  client_shapes: true
  ua_profile: chrome133
`
	minimal := `timeout_seconds: 300
quota_sample_seconds: 0
client_identity:
  pepper: "` + s5Pepper + `"
`
	return []s5Profile{{Name: "production", YAML: production, Plain: true}, {Name: "identity_off", YAML: minimal}}
}

// ---------------------------------------------------------------- cases

type s5Case struct {
	Name  string
	Turns []s5Turn
}

type s5Turn struct {
	Request map[string]any
	// Chunks are the upstream reads, in order.
	Chunks []string
	Stream bool
	// Split delivers every chunk in two reads.
	Split bool
	// Status is the upstream HTTP status (0 = 200) and Body its error body.
	Status int
	Body   string
	// AuthID: "" = cred-s5-a. Caller: "" = caller-s5, "-" = no caller_scope.
	AuthID  string
	Caller  string
	Headers http.Header

	// What h25 changes on this turn on purpose (H25-SPEC section 1). The
	// h22 generator ignores these.
	//
	// Hidden: E1, the catalogue keys left out of the relay prologue;
	// AllHidden: none is left. Searched: E2. Converted: E3, h22 failed this
	// stream with invalid_tool_call and h25 delivers exactly this call;
	// Coerced: by coerce_scalar_strings. Blocked: E4, the call ids handed
	// back without their namespace.
	Hidden    []string
	AllHidden bool
	Searched  bool
	Converted *s5Call
	Coerced   bool
	Blocked   []string
}

type s5Call struct{ ID, Arguments string }

// s5Result is what one turn produced.
type s5Result struct {
	Upstream      string   `json:"upstream"`
	Headers       string   `json:"headers"`
	UpstreamCalls int      `json:"upstream_calls"`
	Status        int      `json:"status"`
	Kind          string   `json:"kind"`
	Message       string   `json:"message"`
	Events        []string `json:"events"`
	CloseErr      string   `json:"close_err"`
	Payload       string   `json:"payload"`
	// raw is the client's stream as emitted (not recorded).
	raw string
}

// ---------------------------------------------------------------- tools

func s5User(text string) map[string]any {
	return map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": text}}}
}

func s5Assistant(id, text string) map[string]any {
	return map[string]any{"type": "message", "id": id, "role": "assistant", "status": "completed",
		"content": []any{map[string]any{"type": "output_text", "text": text, "annotations": []any{}}}}
}

func s5Reasoning(id string) map[string]any {
	return map[string]any{"type": "reasoning", "id": id, "summary": []any{map[string]any{"type": "summary_text", "text": "Considered it."}},
		"encrypted_content": "gAAAA-s5-" + id}
}

func s5Weather() map[string]any {
	return map[string]any{"type": "function", "name": "get_weather", "description": "Weather <now> & later \u2014 天气。", "strict": false,
		"parameters": map[string]any{"type": "object", "properties": map[string]any{
			"city": map[string]any{"type": "string"},
			"unit": map[string]any{"type": "string", "enum": []any{"c", "f"}},
		}, "required": []any{"city"}, "additionalProperties": false}}
}

func s5Patch() map[string]any {
	return map[string]any{"type": "custom", "name": "apply_patch", "description": "Apply a patch in the V4A format."}
}

func s5ExecTool() map[string]any {
	return map[string]any{"type": "function", "name": "exec_command", "description": "Run a shell command.",
		"parameters": map[string]any{"type": "object", "properties": map[string]any{
			"cmd":        map[string]any{"type": "string"},
			"timeout_ms": map[string]any{"type": "integer"},
		}, "required": []any{"cmd"}}}
}

func s5ListFiles() map[string]any {
	return map[string]any{"type": "function", "name": "list_files", "description": "List files.",
		"parameters": map[string]any{"type": "object", "properties": map[string]any{
			"path":      map[string]any{"type": "string"},
			"limit":     map[string]any{"type": "integer"},
			"recursive": map[string]any{"type": "boolean"},
			"ratio":     map[string]any{"type": "number"},
		}, "required": []any{"path"}}}
}

func s5MCP() map[string]any {
	return map[string]any{"type": "namespace", "name": "mcp__srv", "description": "Docs server.", "tools": []any{
		map[string]any{"type": "function", "name": "lookup", "description": "Look a term up.", "parameters": map[string]any{
			"type": "object", "properties": map[string]any{
				"q":       map[string]any{"type": "string"},
				"filters": map[string]any{"type": "object", "patternProperties": map[string]any{"^x-": map[string]any{"type": "string"}}},
			}, "required": []any{"q"}}},
		map[string]any{"type": "function", "name": "fetch", "parameters": map[string]any{
			"type": "object", "properties": map[string]any{"url": map[string]any{"type": "string"}}}},
	}}
}

func s5RelayTools() []any {
	return []any{s5Weather(), s5Patch(), s5ExecTool(), s5ListFiles(), s5MCP()}
}

// s5LiteDecl is a Responses-Lite (Codex Desktop) declaration.
func s5LiteDecl() map[string]any {
	return map[string]any{"type": "additional_tools", "role": "developer", "tools": []any{
		map[string]any{"type": "namespace", "name": "shell", "description": "Shell.", "tools": []any{
			map[string]any{"type": "function", "name": "exec_command", "parameters": map[string]any{
				"type": "object", "properties": map[string]any{"cmd": map[string]any{"type": "string"}}, "required": []any{"cmd"}}},
		}},
		map[string]any{"type": "namespace", "name": "collab", "description": "Agents.", "tools": []any{
			map[string]any{"type": "function", "name": "wait_agent", "parameters": map[string]any{
				"type": "object", "properties": map[string]any{"id": map[string]any{"type": "string"}}}},
		}},
		map[string]any{"type": "function", "name": "view_image", "parameters": map[string]any{
			"type": "object", "properties": map[string]any{"path": map[string]any{"type": "string"}}}},
		map[string]any{"type": "custom", "name": "apply_patch", "description": "Apply a patch."},
	}}
}

// ---------------------------------------------------------------- requests

func s5Body(key string, input ...any) map[string]any {
	body := map[string]any{"model": "gpt-6-astra", "input": input}
	if key != "" {
		body["prompt_cache_key"] = key
	}
	return body
}

func s5Relay(key string, input ...any) map[string]any {
	body := s5Body(key, input...)
	body["tools"], body["tool_choice"], body["parallel_tool_calls"] = s5RelayTools(), "auto", true
	return body
}

func s5RelayWith(key string, tools []any, input ...any) map[string]any {
	body := s5Body(key, input...)
	body["tools"], body["tool_choice"], body["parallel_tool_calls"] = tools, "auto", false
	return body
}

func s5Lite(key string, input ...any) map[string]any {
	body := s5Body(key, input...)
	body["tool_choice"], body["parallel_tool_calls"] = "auto", true
	return body
}

func s5Set(body map[string]any, pairs ...any) map[string]any {
	copy := cloneObject(body)
	for index := 0; index+1 < len(pairs); index += 2 {
		copy[pairs[index].(string)] = pairs[index+1]
	}
	return copy
}

// ---------------------------------------------------------------- items

// s5NoReferences leaves the references key out of a transport call.
const s5NoReferences = "\x00none"

func s5Code(tool string, args any) string {
	return string(jsonBytes(map[string]any{"tool": tool, "args": args}))
}

// s5Transport is the model's run_officejs call carrying code.
func s5Transport(callID string, code any, references any) map[string]any {
	outer := map[string]any{"summary": "s5 summary", "extended_summary": "s5 words", "destructive": false, "code": code}
	if references != s5NoReferences {
		outer["references"] = references
	}
	return map[string]any{"type": "function_call", "id": "fc_" + callID, "call_id": callID, "name": "run_officejs",
		"status": "completed", "arguments": string(jsonBytes(outer))}
}

func s5Native(callID, namespace, name, arguments string) map[string]any {
	call := map[string]any{"type": "function_call", "id": "fc_" + callID, "call_id": callID, "name": name, "arguments": arguments, "status": "completed"}
	if namespace != "" {
		call["namespace"] = namespace
	}
	return call
}

func s5NativeCustom(callID, name, input string) map[string]any {
	return map[string]any{"type": "custom_tool_call", "id": "ctc_" + callID, "call_id": callID, "name": name, "input": input, "status": "completed"}
}

// s5Echo is a call as a client sends it back in its history.
func s5Echo(callID, namespace, name, arguments string) map[string]any {
	call := map[string]any{"type": "function_call", "id": "fc_" + callID, "call_id": callID, "name": name, "arguments": arguments}
	if namespace != "" {
		call["namespace"] = namespace
	}
	return call
}

func s5Output(callID string, output any) map[string]any {
	return map[string]any{"type": "function_call_output", "call_id": callID, "output": output}
}

func s5CustomOutput(callID string, output any) map[string]any {
	return map[string]any{"type": "custom_tool_call_output", "call_id": callID, "output": output}
}

// ---------------------------------------------------------------- upstream streams

// s5SSE builds one upstream event stream. Events carry sequence numbers, as
// the upstream's do (h25 may renumber them; the comparison ignores them).
type s5SSE struct {
	id       string
	chunks   []string
	output   []any
	index    int
	sequence int
}

func s5Stream(id string) *s5SSE {
	b := &s5SSE{id: id}
	b.event("response.created", map[string]any{"response": map[string]any{"id": id, "object": "response", "status": "in_progress", "output": []any{}}})
	return b
}

func (b *s5SSE) event(name string, payload map[string]any) *s5SSE {
	payload["type"] = name
	payload["sequence_number"] = b.sequence
	b.sequence++
	b.chunks = append(b.chunks, sseEvent(name, payload))
	return b
}

func (b *s5SSE) inProgress() *s5SSE {
	return b.event("response.in_progress", map[string]any{"response": map[string]any{"id": b.id, "object": "response", "status": "in_progress", "output": []any{}}})
}

func (b *s5SSE) reasoning(text string) *s5SSE {
	index := b.index
	b.index++
	id := fmt.Sprintf("rs_%s_%d", b.id, index)
	done := map[string]any{"type": "reasoning", "id": id, "summary": []any{map[string]any{"type": "summary_text", "text": text}}, "encrypted_content": "gAAAA-" + id}
	b.event("response.output_item.added", map[string]any{"output_index": index, "item": map[string]any{"type": "reasoning", "id": id, "summary": []any{}}})
	b.event("response.reasoning_summary_text.delta", map[string]any{"output_index": index, "item_id": id, "summary_index": 0, "delta": text})
	b.event("response.reasoning_summary_text.done", map[string]any{"output_index": index, "item_id": id, "summary_index": 0, "text": text})
	b.event("response.output_item.done", map[string]any{"output_index": index, "item": done})
	b.output = append(b.output, done)
	return b
}

func (b *s5SSE) text(text string) *s5SSE {
	index := b.index
	b.index++
	id := fmt.Sprintf("msg_%s_%d", b.id, index)
	b.event("response.output_item.added", map[string]any{"output_index": index, "item": map[string]any{"type": "message", "id": id, "role": "assistant", "status": "in_progress", "content": []any{}}})
	b.event("response.content_part.added", map[string]any{"output_index": index, "item_id": id, "content_index": 0, "part": map[string]any{"type": "output_text", "text": "", "annotations": []any{}}})
	half := len(text) / 2
	b.event("response.output_text.delta", map[string]any{"output_index": index, "item_id": id, "content_index": 0, "delta": text[:half]})
	b.event("response.output_text.delta", map[string]any{"output_index": index, "item_id": id, "content_index": 0, "delta": text[half:]})
	b.event("response.output_text.done", map[string]any{"output_index": index, "item_id": id, "content_index": 0, "text": text})
	b.event("response.content_part.done", map[string]any{"output_index": index, "item_id": id, "content_index": 0, "part": map[string]any{"type": "output_text", "text": text, "annotations": []any{}}})
	done := s5Assistant(id, text)
	b.event("response.output_item.done", map[string]any{"output_index": index, "item": done})
	b.output = append(b.output, done)
	return b
}

// textDelta streams the start of a message and stops there.
func (b *s5SSE) textDelta(text string) *s5SSE {
	index := b.index
	b.index++
	id := fmt.Sprintf("msg_%s_%d", b.id, index)
	b.event("response.output_item.added", map[string]any{"output_index": index, "item": map[string]any{"type": "message", "id": id, "role": "assistant", "status": "in_progress", "content": []any{}}})
	return b.event("response.output_text.delta", map[string]any{"output_index": index, "item_id": id, "content_index": 0, "delta": text})
}

// call streams one function or custom call through its whole lifecycle.
func (b *s5SSE) call(item map[string]any) *s5SSE {
	index := b.index
	b.index++
	added := cloneObject(item)
	added["status"] = "in_progress"
	if item["type"] == "custom_tool_call" {
		input := stringValue(item["input"])
		added["input"] = ""
		b.event("response.output_item.added", map[string]any{"output_index": index, "item": added})
		b.event("response.custom_tool_call_input.delta", map[string]any{"output_index": index, "item_id": item["id"], "delta": input[:len(input)/2]})
		b.event("response.custom_tool_call_input.delta", map[string]any{"output_index": index, "item_id": item["id"], "delta": input[len(input)/2:]})
		b.event("response.custom_tool_call_input.done", map[string]any{"output_index": index, "item_id": item["id"], "input": input})
	} else {
		arguments := stringValue(item["arguments"])
		added["arguments"] = ""
		b.event("response.output_item.added", map[string]any{"output_index": index, "item": added})
		b.event("response.function_call_arguments.delta", map[string]any{"output_index": index, "item_id": item["id"], "delta": arguments[:len(arguments)/2]})
		b.event("response.function_call_arguments.delta", map[string]any{"output_index": index, "item_id": item["id"], "delta": arguments[len(arguments)/2:]})
		b.event("response.function_call_arguments.done", map[string]any{"output_index": index, "item_id": item["id"], "arguments": arguments})
	}
	b.event("response.output_item.done", map[string]any{"output_index": index, "item": item})
	b.output = append(b.output, item)
	return b
}

// added streams only the start of a call (a truncated item).
func (b *s5SSE) added(item map[string]any) *s5SSE {
	index := b.index
	b.index++
	added := cloneObject(item)
	added["status"], added["arguments"] = "in_progress", ""
	b.event("response.output_item.added", map[string]any{"output_index": index, "item": added})
	arguments := stringValue(item["arguments"])
	return b.event("response.function_call_arguments.delta", map[string]any{"output_index": index, "item_id": item["id"], "delta": arguments[:len(arguments)/2]})
}

// done streams only the completed item.
func (b *s5SSE) done(item map[string]any) *s5SSE {
	index := b.index
	b.index++
	b.event("response.output_item.done", map[string]any{"output_index": index, "item": item})
	b.output = append(b.output, item)
	return b
}

// terminalOnly lists an item in the terminal snapshot without streaming it.
func (b *s5SSE) terminalOnly(item map[string]any) *s5SSE {
	b.output = append(b.output, item)
	return b
}

// replaceOutput swaps the terminal snapshot's copy of item number index.
func (b *s5SSE) replaceOutput(index int, item map[string]any) *s5SSE {
	b.output[index] = item
	return b
}

func s5Usage() map[string]any {
	return map[string]any{"input_tokens": 1200, "input_tokens_details": map[string]any{"cached_tokens": 1024},
		"output_tokens": 40, "output_tokens_details": map[string]any{"reasoning_tokens": 12}, "total_tokens": 1240}
}

// finish ends the stream with a terminal of kind (completed / incomplete /
// failed) and the [DONE] sentinel.
func (b *s5SSE) finish(kind string, extra map[string]any) []string {
	status := strings.TrimPrefix(kind, "response.")
	response := map[string]any{"id": b.id, "object": "response", "status": status, "output": append([]any{}, b.output...), "usage": s5Usage()}
	for key, value := range extra {
		response[key] = value
	}
	b.event(kind, map[string]any{"response": response})
	return append(append([]string(nil), b.chunks...), "data: [DONE]\n\n")
}

func (b *s5SSE) completed() []string { return b.finish("response.completed", nil) }

// open ends the stream without a terminal (the upstream hung up).
func (b *s5SSE) open() []string { return append([]string(nil), b.chunks...) }

func (b *s5SSE) errorEvent(code, message string) []string {
	b.event("error", map[string]any{"error": map[string]any{"code": code, "message": message}})
	return append([]string(nil), b.chunks...)
}

// ---------------------------------------------------------------- the case list

func s5Cases() []s5Case {
	var all []s5Case
	add := func(name string, turns ...s5Turn) { all = append(all, s5Case{Name: name, Turns: turns}) }
	turn := func(request map[string]any, chunks []string) s5Turn {
		return s5Turn{Request: request, Chunks: chunks, Stream: true}
	}
	nonStream := func(request map[string]any, chunks []string) s5Turn {
		return s5Turn{Request: request, Chunks: chunks}
	}
	text := func(id string) []string { return s5Stream(id).reasoning("Thinking.").text("Here you go.").completed() }

	// ---- no tools
	add("plain_string_input", turn(map[string]any{"model": "gpt-6-astra", "input": "Say hello."}, s5Stream("r_plain_1").inProgress().text("Hello there.").completed()))
	add("plain_non_stream", nonStream(s5Body("s5-plain-ns", s5User("Say hello.")), s5Stream("r_plain_ns").reasoning("Hm.").text("Hello.").completed()))
	add("plain_request_fields", turn(map[string]any{
		"model": "gpt-6-astra", "instructions": "Be terse. <b>Never</b> use & signs.", "prompt_cache_key": "s5-plain-fields",
		"input":     []any{s5User("Summarise the report.")},
		"reasoning": map[string]any{"effort": "xhigh", "summary": "auto"}, "include": []any{"reasoning.encrypted_content"},
		"store": true, "text": map[string]any{"verbosity": "low"}, "service_tier": "priority", "truncation": "auto",
		"max_output_tokens": 4000, "metadata": map[string]any{"user": "u-1", "turn_id": "client-turn", "bps_tools_version_id": "v9"},
		"client_metadata":    map[string]any{"session_id": "s5-client-session"},
		"context_management": []any{map[string]any{"type": "compaction", "compact_threshold": 250000}},
	}, text("r_plain_fields")))
	add("plain_history_shapes", turn(s5Set(s5Body("s5-plain-history",
		s5User("first"), s5Reasoning("rs_hist_1"), s5Assistant("msg_hist_1", "first answer"),
		map[string]any{"type": "item_reference", "id": "msg_hist_0"},
		map[string]any{"type": "reasoning", "id": "rs_hist_plain", "summary": []any{}},
		map[string]any{"type": "message", "role": "developer", "content": "Developer note."},
		map[string]any{"type": "compaction", "encrypted_content": "gAAAA-s5-compaction"},
		s5User("second")), "reasoning_effort", "low"), text("r_plain_history")))
	add("plain_multi_turn",
		turn(s5Body("s5-plain-multi", s5User("one")), text("r_plain_multi_1")),
		turn(s5Body("s5-plain-multi", s5User("one"), s5Assistant("msg_r_plain_multi_1_1", "Here you go."), s5User("two")), text("r_plain_multi_2")),
	)
	add("plain_model_upstream_mapping", turn(s5Set(s5Body("s5-plain-sol", s5User("map me")), "model", "gpt-6-sol"), text("r_plain_sol")))
	add("plain_model_unsupported", turn(s5Set(s5Body("s5-plain-bad-model", s5User("x")), "model", "gpt-4o-mini"), text("r_unused")))
	add("plain_empty_input", turn(map[string]any{"model": "gpt-6-astra", "input": []any{}}, text("r_unused")))
	add("plain_upstream_400", s5Turn{Request: s5Body("s5-plain-400", s5User("x")), Stream: true, Status: 400,
		Body: `{"error":{"message":"Invalid value for input","type":"invalid_request_error","code":"invalid_value"}}`})
	add("plain_upstream_500_non_stream", s5Turn{Request: s5Body("s5-plain-500", s5User("x")), Status: 500, Body: `{"error":{"message":"boom","type":"server_error"}}`})
	add("plain_failed_terminal_zero_output", turn(s5Body("s5-plain-failed", s5User("x")),
		s5Stream("r_plain_failed").inProgress().finish("response.failed", map[string]any{"error": map[string]any{"code": "server_error", "message": "The server had an error."}})))
	add("plain_incomplete_terminal", turn(s5Body("s5-plain-incomplete", s5User("x")),
		s5Stream("r_plain_incomplete").text("Partial answ").finish("response.incomplete", map[string]any{"incomplete_details": map[string]any{"reason": "max_output_tokens"}})))
	add("plain_eof_zero_output", turn(s5Body("s5-plain-eof0", s5User("x")), s5Stream("r_plain_eof0").inProgress().open()))
	add("plain_eof_after_text", turn(s5Body("s5-plain-eof1", s5User("x")), s5Stream("r_plain_eof1").textDelta("Half a sent").open()))
	add("plain_error_event_zero_output", turn(s5Body("s5-plain-err0", s5User("x")), s5Stream("r_plain_err0").errorEvent("server_error", "upstream overloaded")))
	add("plain_error_event_after_text", turn(s5Body("s5-plain-err1", s5User("x")), s5Stream("r_plain_err1").textDelta("Partial").errorEvent("server_error", "upstream overloaded")))
	split := turn(s5Body("s5-plain-split", s5User("split")), s5Stream("r_plain_split").reasoning("Split.").text("Read in halves.").completed())
	split.Split = true
	add("plain_split_reads", split)

	// ---- relay (top-level tools)
	weatherArgs := map[string]any{"city": "Paris", "unit": "c"}
	weather := s5Transport("s5_rl_weather", s5Code("get_weather", weatherArgs), []any{"Sheet1!A1"})
	add("relay_function_replayed",
		turn(s5Relay("s5-rl-replay", s5User("weather?")), s5Stream("r_rl_replay_1").reasoning("Need weather.").call(weather).completed()),
		turn(s5Relay("s5-rl-replay", s5User("weather?"), s5Reasoning("rs_r_rl_replay_1_0"),
			s5Echo("s5_rl_weather", "", "get_weather", `{"city":"Paris","unit":"c"}`), s5Output("s5_rl_weather", "18C"), s5User("and Oslo?")), text("r_rl_replay_2")),
	)
	rewritten := s5Transport("s5_rl_rewritten", s5Code("get_weather", weatherArgs), []any{})
	add("relay_function_rewritten_by_client",
		turn(s5Relay("s5-rl-rewritten", s5User("weather?")), s5Stream("r_rl_rw_1").call(rewritten).completed()),
		turn(s5Relay("s5-rl-rewritten", s5User("weather?"),
			s5Echo("s5_rl_rewritten", "", "get_weather", `{"city":"Paris","unit":"f","note":"client edit"}`), s5Output("s5_rl_rewritten", ""), s5User("thanks")), text("r_rl_rw_2")),
	)
	patchInput := "*** Begin Patch\n*** Add File: a.txt\n+\"quoted\" C:\\path\\file\n+\ttabbed\n*** End Patch\n"
	patch := s5Transport("s5_rl_patch", s5Code("apply_patch", patchInput), []any{})
	add("relay_custom_replayed",
		turn(s5Relay("s5-rl-patch", s5User("patch it")), s5Stream("r_rl_patch_1").call(patch).completed()),
		turn(s5Relay("s5-rl-patch", s5User("patch it"),
			map[string]any{"type": "custom_tool_call", "id": "ctc_s5_rl_patch", "call_id": "s5_rl_patch", "name": "apply_patch", "input": patchInput},
			s5CustomOutput("s5_rl_patch", "Done!"), s5User("next")), text("r_rl_patch_2")),
	)
	lookup := s5Transport("s5_rl_lookup", s5Code("mcp__srv.lookup", map[string]any{"q": "pricing", "filters": map[string]any{"x-lang": "en"}}), []any{"mcp__srv.lookup"})
	add("relay_namespace_replayed",
		turn(s5Relay("s5-rl-ns", s5User("look it up")), s5Stream("r_rl_ns_1").call(lookup).completed()),
		turn(s5Relay("s5-rl-ns", s5User("look it up"),
			s5Echo("s5_rl_lookup", "mcp__srv", "lookup", `{"filters":{"x-lang":"en"},"q":"pricing"}`),
			s5Output("s5_rl_lookup", []any{map[string]any{"type": "input_text", "text": "found"}}), s5User("ok")), text("r_rl_ns_2")),
	)
	add("relay_parallel_calls_and_text", turn(s5Relay("s5-rl-parallel", s5User("both")),
		s5Stream("r_rl_parallel").text("Running both.").
			call(s5Transport("s5_rl_par_exec", s5Code("exec_command", map[string]any{"cmd": "ls -la", "timeout_ms": 5000}), []any{})).
			call(s5Transport("s5_rl_par_weather", s5Code("get_weather", map[string]any{"city": "Oslo"}), []any{})).completed()))
	add("relay_call_then_text", turn(s5Relay("s5-rl-call-text", s5User("x")),
		s5Stream("r_rl_call_text").call(s5Transport("s5_rl_ct", s5Code("exec_command", map[string]any{"cmd": "pwd"}), []any{})).text("Started.").completed()))
	add("relay_terminal_only_call", turn(s5Relay("s5-rl-terminal", s5User("x")),
		s5Stream("r_rl_terminal").terminalOnly(s5Transport("s5_rl_term", s5Code("exec_command", map[string]any{"cmd": "date"}), []any{})).completed()))
	// legacy reads code and nothing else: references in every shape.
	refs := s5Stream("r_rl_refs")
	for index, references := range []any{s5NoReferences, nil, "Sheet1!A1", []any{}, []any{"a.go", "b.go"}, []any{"get_weather"}, []any{"get_weather", "exec_command"}, []any{"Sheet1!A1", "Sheet2!B2", "Sheet3!C3"}, 7} {
		refs.call(s5Transport(fmt.Sprintf("s5_rl_ref_%d", index), s5Code("exec_command", map[string]any{"cmd": fmt.Sprintf("echo %d", index)}), references))
	}
	add("relay_references_ignored", turn(s5Relay("s5-rl-refs", s5User("refs")), refs.completed()))
	add("relay_code_variants", turn(s5Relay("s5-rl-code", s5User("code")), s5Stream("r_rl_code").
		call(s5Transport("s5_rl_code_obj", map[string]any{"tool": "exec_command", "args": map[string]any{"cmd": "whoami"}}, []any{})).
		call(s5Transport("s5_rl_code_fence", "```json\n"+s5Code("exec_command", map[string]any{"cmd": "id"})+"\n```", []any{})).
		call(s5Transport("s5_rl_code_space", "  "+s5Code("get_weather", map[string]any{"city": "Rome"})+"\n", []any{})).completed()))
	add("relay_tool_choice_none", turn(s5Set(s5Relay("s5-rl-none", s5User("no tools please")), "tool_choice", "none"), text("r_rl_none")))
	add("relay_tool_choice_required_text_only", turn(s5Set(s5Relay("s5-rl-required", s5User("x")), "tool_choice", "required"), text("r_rl_required")))
	add("relay_forced_function", turn(s5Set(s5Relay("s5-rl-forced", s5User("x")), "tool_choice", map[string]any{"type": "function", "name": "exec_command"}),
		s5Stream("r_rl_forced").call(s5Transport("s5_rl_forced", s5Code("exec_command", map[string]any{"cmd": "uname"}), []any{})).completed()))
	add("relay_forced_other_tool", turn(s5Set(s5Relay("s5-rl-forced-other", s5User("x")), "tool_choice", map[string]any{"type": "function", "name": "exec_command"}),
		s5Stream("r_rl_forced_other").call(s5Transport("s5_rl_forced_other", s5Code("get_weather", map[string]any{"city": "Lima"}), []any{})).completed()))
	add("relay_allowed_tools", turn(s5Set(s5Relay("s5-rl-allowed", s5User("x")), "tool_choice", map[string]any{"type": "allowed_tools", "mode": "auto",
		"tools": []any{map[string]any{"type": "function", "name": "get_weather"}}}),
		s5Stream("r_rl_allowed").call(s5Transport("s5_rl_allowed", s5Code("get_weather", map[string]any{"city": "Kyiv"}), []any{})).completed()))
	add("relay_parallel_false_two_calls", turn(s5RelayWith("s5-rl-ptc", s5RelayTools(), s5User("x")), s5Stream("r_rl_ptc").
		call(s5Transport("s5_rl_ptc_1", s5Code("exec_command", map[string]any{"cmd": "a"}), []any{})).
		call(s5Transport("s5_rl_ptc_2", s5Code("exec_command", map[string]any{"cmd": "b"}), []any{})).completed()))
	add("relay_non_stream_call", nonStream(s5Relay("s5-rl-ns-call", s5User("x")),
		s5Stream("r_rl_ns_call").reasoning("Hm.").call(s5Transport("s5_rl_ns_call", s5Code("get_weather", map[string]any{"city": "Bern"}), []any{})).completed()))
	add("relay_non_stream_bad_call", nonStream(s5Relay("s5-rl-ns-bad", s5User("x")),
		s5Stream("r_rl_ns_bad").call(s5Transport("s5_rl_ns_bad", `{"tool":"get_weather","args":{"city":"Bern"`, []any{})).completed()))
	add("relay_code_json_broken_zero_output", turn(s5Relay("s5-rl-broken0", s5User("x")),
		s5Stream("r_rl_broken0").call(s5Transport("s5_rl_broken0", `{"tool":"exec_command","args":{"cmd":"echo "hi""}}`, []any{})).completed()))
	add("relay_code_json_broken_after_text", turn(s5Relay("s5-rl-broken1", s5User("x")),
		s5Stream("r_rl_broken1").text("Let me run it.").call(s5Transport("s5_rl_broken1", `{"tool":"exec_command","args":{"cmd":`, []any{})).completed()))
	add("relay_undeclared_tool", turn(s5Relay("s5-rl-undeclared", s5User("x")),
		s5Stream("r_rl_undeclared").call(s5Transport("s5_rl_undeclared", s5Code("rm_everything", map[string]any{"path": "/"}), []any{})).completed()))
	add("relay_schema_mismatch", turn(s5Relay("s5-rl-mismatch", s5User("x")),
		s5Stream("r_rl_mismatch").call(s5Transport("s5_rl_mismatch", s5Code("get_weather", map[string]any{"unit": "c"}), []any{})).completed()))
	add("relay_truncated_eof", turn(s5Relay("s5-rl-trunc", s5User("x")),
		s5Stream("r_rl_trunc").added(s5Transport("s5_rl_trunc", s5Code("exec_command", map[string]any{"cmd": "make"}), []any{})).open()))
	unfinished := s5Transport("s5_rl_unfinished", s5Code("exec_command", map[string]any{"cmd": "make"}), []any{})
	unfinished["status"] = "incomplete"
	add("relay_unfinished_item", turn(s5Relay("s5-rl-unfinished", s5User("x")), s5Stream("r_rl_unfinished").done(unfinished).completed()))
	failedAfter := s5Transport("s5_rl_failed_after", s5Code("exec_command", map[string]any{"cmd": "true"}), []any{})
	add("relay_failed_after_call", turn(s5Relay("s5-rl-failed-after", s5User("x")),
		s5Stream("r_rl_failed_after").call(failedAfter).finish("response.failed", map[string]any{"error": map[string]any{"code": "server_error", "message": "late failure"}})))
	add("relay_incomplete_after_call", turn(s5Relay("s5-rl-incomplete-after", s5User("x")),
		s5Stream("r_rl_incomplete_after").call(s5Transport("s5_rl_incomplete_after", s5Code("exec_command", map[string]any{"cmd": "true"}), []any{})).
			finish("response.incomplete", map[string]any{"incomplete_details": map[string]any{"reason": "max_output_tokens"}})))
	add("relay_error_event_after_call", turn(s5Relay("s5-rl-error-after", s5User("x")),
		s5Stream("r_rl_error_after").call(s5Transport("s5_rl_error_after", s5Code("exec_command", map[string]any{"cmd": "true"}), []any{})).errorEvent("server_error", "gone")))
	terminalChanged := s5Stream("r_rl_term_changed").call(s5Transport("s5_rl_term_changed", s5Code("get_weather", map[string]any{"city": "Paris"}), []any{}))
	terminalChanged.replaceOutput(0, s5Transport("s5_rl_term_changed", s5Code("get_weather", map[string]any{"city": "Oslo"}), []any{}))
	add("relay_terminal_arguments_changed", turn(s5Relay("s5-rl-term-changed", s5User("x")), terminalChanged.completed()))
	splitCall := turn(s5Relay("s5-rl-split", s5User("x")), s5Stream("r_rl_split").call(s5Transport("s5_rl_split", s5Code("exec_command", map[string]any{"cmd": "ls"}), []any{})).completed())
	splitCall.Split = true
	add("relay_split_reads", splitCall)

	// ---- E3: h22 rejected these; h25 converts them safely.
	coerce := s5Transport("s5_e3_coerce", s5Code("list_files", map[string]any{"path": "/tmp", "limit": "20", "recursive": "true", "ratio": "0.5"}), []any{})
	add("e3_scalar_strings", s5Turn{Request: s5Relay("s5-e3-coerce", s5User("x")), Chunks: s5Stream("r_e3_coerce").call(coerce).completed(), Stream: true,
		Converted: &s5Call{ID: "fc_s5_e3_coerce", Arguments: `{"limit":20,"path":"/tmp","ratio":0.5,"recursive":true}`}, Coerced: true})
	numberDone := s5Transport("s5_e3_number", `{"tool":"exec_command","args":{"cmd":"sleep","timeout_ms":1e3}}`, []any{})
	numberTerminal := s5Transport("s5_e3_number", s5Code("exec_command", map[string]any{"cmd": "sleep", "timeout_ms": 1000}), []any{})
	add("e3_equivalent_number_in_terminal", s5Turn{Request: s5Relay("s5-e3-number", s5User("x")),
		Chunks: s5Stream("r_e3_number").done(numberDone).replaceOutput(0, numberTerminal).completed(), Stream: true,
		Converted: &s5Call{ID: "fc_s5_e3_number", Arguments: `{"cmd":"sleep","timeout_ms":1e3}`}})
	// The upstream item id is not fc_<call_id>, so the id h22 derives for the
	// terminal copy that lacks one differs from the id it already delivered.
	missingID := s5Transport("s5_e3_missing_id", s5Code("get_weather", map[string]any{"city": "Paris"}), []any{})
	missingID["id"] = "fc_upstream_s5_7"
	missingIDTerminal := cloneObject(missingID)
	delete(missingIDTerminal, "id")
	add("e3_terminal_missing_id", s5Turn{Request: s5Relay("s5-e3-missing-id", s5User("x")),
		Chunks: s5Stream("r_e3_missing_id").done(missingID).replaceOutput(0, missingIDTerminal).completed(), Stream: true,
		Converted: &s5Call{ID: "fc_upstream_s5_7", Arguments: `{"city":"Paris"}`}})
	// The same terminal without an id where the derived id agrees: h22
	// already delivered this one.
	sameID := s5Transport("s5_e3_same_id", s5Code("get_weather", map[string]any{"city": "Paris"}), []any{})
	sameIDTerminal := cloneObject(sameID)
	delete(sameIDTerminal, "id")
	add("relay_terminal_missing_derivable_id", turn(s5Relay("s5-rl-same-id", s5User("x")),
		s5Stream("r_rl_same_id").done(sameID).replaceOutput(0, sameIDTerminal).completed()))

	// ---- relay history
	add("relay_cold_history", turn(s5Relay("s5-rl-cold", s5User("before"),
		s5Echo("s5_cold_exec", "", "exec_command", `{"cmd":"ls"}`), s5Output("s5_cold_exec", "a\nb"),
		map[string]any{"type": "custom_tool_call", "id": "ctc_s5_cold_patch", "call_id": "s5_cold_patch", "name": "apply_patch", "input": "*** Begin Patch\n*** End Patch"},
		s5CustomOutput("s5_cold_patch", ""),
		s5Echo("s5_cold_lookup", "mcp__srv", "lookup", `{"q":"x"}`), s5Output("s5_cold_lookup", []any{map[string]any{"type": "input_text", "text": "hit"}}),
		s5Echo("s5_cold_badjson", "", "exec_command", `{"cmd":`), s5Output("s5_cold_badjson", "err"),
		s5Echo("s5_cold_native", "", "web_search_native", `{"q":"x"}`), s5Output("s5_cold_native", "results"),
		s5Echo("s5_cold_enc", "", "exec_command", `{"cmd":"ls"}`), s5Output("s5_cold_enc", "  "),
		s5User("after")), text("r_rl_cold")))
	add("relay_leaked_transport_in_history", turn(s5Relay("s5-rl-leak", s5User("before"),
		s5Transport("s5_leak", s5Code("exec_command", map[string]any{"cmd": "ls"}), []any{}), s5Output("s5_leak", "files"),
		s5User("after")), text("r_rl_leak")))
	sessionHeaders := http.Header{"Session_id": []string{"s5-session-header"}}
	sessionCall := s5Transport("s5_rl_session", s5Code("exec_command", map[string]any{"cmd": "hostname"}), []any{})
	add("relay_scope_from_first_item_with_session_header",
		s5Turn{Request: s5Relay("", s5User("s5 first item scope")), Chunks: s5Stream("r_rl_session_1").call(sessionCall).completed(), Stream: true, Headers: sessionHeaders},
		s5Turn{Request: s5Relay("", s5User("s5 first item scope"), s5Echo("s5_rl_session", "", "exec_command", `{"cmd":"hostname","extra":1}`), s5Output("s5_rl_session", "box"), s5User("next")),
			Chunks: text("r_rl_session_2"), Stream: true, Headers: sessionHeaders},
	)
	noCaller := s5Transport("s5_rl_nocaller", s5Code("exec_command", map[string]any{"cmd": "id -u"}), []any{})
	add("relay_without_caller_scope",
		s5Turn{Request: s5Relay("s5-rl-nocaller", s5User("x")), Chunks: s5Stream("r_rl_nocaller_1").call(noCaller).completed(), Stream: true, Caller: "-"},
		s5Turn{Request: s5Relay("s5-rl-nocaller", s5User("x"), s5Echo("s5_rl_nocaller", "", "exec_command", `{"cmd":"id -u "}`), s5Output("s5_rl_nocaller", "0"), s5User("y")),
			Chunks: text("r_rl_nocaller_2"), Stream: true, Caller: "-"},
	)
	switched := s5Transport("s5_rl_switch", s5Code("exec_command", map[string]any{"cmd": "uptime"}), []any{})
	add("relay_credential_switch",
		s5Turn{Request: s5Relay("s5-rl-switch", s5User("x")), Chunks: s5Stream("r_rl_switch_1").call(switched).completed(), Stream: true},
		s5Turn{Request: s5Relay("s5-rl-switch", s5User("x"), s5Echo("s5_rl_switch", "", "exec_command", `{"cmd":"uptime"}`), s5Output("s5_rl_switch", "up"), s5User("y")),
			Chunks: text("r_rl_switch_2"), Stream: true, AuthID: "cred-s5-b"},
	)
	nonStreamDelivered := s5Transport("s5_rl_ns_replay", s5Code("get_weather", map[string]any{"city": "Bern"}), []any{})
	add("relay_non_stream_then_replay",
		nonStream(s5Relay("s5-rl-ns-replay", s5User("x")), s5Stream("r_rl_ns_replay_1").call(nonStreamDelivered).completed()),
		nonStream(s5Relay("s5-rl-ns-replay", s5User("x"), s5Echo("s5_rl_ns_replay", "", "get_weather", `{"city":"Bern"}`), s5Output("s5_rl_ns_replay", "cold"), s5User("y")), text("r_rl_ns_replay_2")),
	)

	// ---- Responses-Lite (additional_tools)
	liteExec := s5Native("s5_lt_exec", "shell", "exec_command", `{"cmd":"git status"}`)
	add("lite_native_call_replayed",
		turn(s5Lite("s5-lt-1", s5LiteDecl(), s5User("status?")), s5Stream("r_lt_1").reasoning("Checking.").call(liteExec).completed()),
		turn(s5Lite("s5-lt-1", s5LiteDecl(), s5User("status?"), s5Reasoning("rs_r_lt_1_0"),
			s5Echo("s5_lt_exec", "", "exec_command", `{"cmd":"git status"}`), s5Output("s5_lt_exec", "clean"), s5User("thanks")), text("r_lt_2")),
	)
	add("lite_namespace_memory_without_declaration",
		turn(s5Lite("s5-lt-mem", s5LiteDecl(), s5User("x")), s5Stream("r_lt_mem_1").call(s5Native("s5_lt_mem", "collab", "wait_agent", `{"id":"a1"}`)).completed()),
		turn(s5Lite("s5-lt-mem", s5User("x"), s5Echo("s5_lt_mem", "", "wait_agent", `{"id":"a1"}`), s5Output("s5_lt_mem", "done"), s5User("y")), text("r_lt_mem_2")),
	)
	add("lite_tool_choice_none", turn(s5Set(s5Lite("s5-lt-none", s5LiteDecl(), s5User("x")), "tool_choice", "none"), text("r_lt_none")))
	add("lite_allowed_tools_none", turn(s5Set(s5Lite("s5-lt-allowed-none", s5LiteDecl(), s5User("x")), "tool_choice", map[string]any{"type": "allowed_tools", "mode": "none", "tools": []any{}}), text("r_lt_allowed_none")))
	add("lite_parallel_false_two_native", turn(s5Set(s5Lite("s5-lt-ptc", s5LiteDecl(), s5User("x")), "parallel_tool_calls", false), s5Stream("r_lt_ptc").
		call(s5Native("s5_lt_ptc_1", "shell", "exec_command", `{"cmd":"a"}`)).call(s5Native("s5_lt_ptc_2", "shell", "exec_command", `{"cmd":"b"}`)).completed()))
	add("lite_native_not_declared", turn(s5Lite("s5-lt-undeclared", s5LiteDecl(), s5User("x")),
		s5Stream("r_lt_undeclared").call(s5Native("s5_lt_undeclared", "", "spreadsheet_write", `{"cell":"A1"}`)).completed()))
	nativeChanged := s5Stream("r_lt_changed").call(s5Native("s5_lt_changed", "shell", "exec_command", `{"cmd":"a"}`))
	nativeChanged.replaceOutput(0, s5Native("s5_lt_changed", "shell", "exec_command", `{"cmd":"b"}`))
	add("lite_native_terminal_changed", turn(s5Lite("s5-lt-changed", s5LiteDecl(), s5User("x")), nativeChanged.completed()))
	add("lite_native_invalid_json", turn(s5Lite("s5-lt-badjson", s5LiteDecl(), s5User("x")),
		s5Stream("r_lt_badjson").call(s5Native("s5_lt_badjson", "shell", "exec_command", `{"cmd":`)).completed()))
	add("lite_native_custom", turn(s5Lite("s5-lt-custom", s5LiteDecl(), s5User("x")),
		s5Stream("r_lt_custom").call(s5NativeCustom("s5_lt_custom", "apply_patch", "*** Begin Patch\n*** End Patch\n")).completed()))
	add("lite_transport_call", turn(s5Lite("s5-lt-transport", s5LiteDecl(), s5User("x")),
		s5Stream("r_lt_transport").call(s5Transport("s5_lt_transport", s5Code("shell.exec_command", map[string]any{"cmd": "ls"}), []any{})).completed()))
	add("lite_non_stream_native", nonStream(s5Lite("s5-lt-ns", s5LiteDecl(), s5User("x")),
		s5Stream("r_lt_ns").call(s5Native("s5_lt_ns", "shell", "exec_command", `{"cmd":"pwd"}`)).completed()))
	add("lite_with_top_level_tools", turn(s5Set(s5Relay("s5-lt-both", s5LiteDecl(), s5User("x"))),
		s5Stream("r_lt_both").call(s5Transport("s5_lt_both", s5Code("exec_command", map[string]any{"cmd": "ls"}), []any{})).completed()))

	// ---- tool_search_output (dynamic tools)
	searched := toolSearchOutput("s5_ts_only", searchedNamespace("mcp__docs", "search_docs", "read_doc"))
	add("search_only_native_call", s5Turn{Request: s5Lite("s5-ts-only", searched, s5User("find docs")), Stream: true, Searched: true,
		Chunks: s5Stream("r_ts_only").call(s5Native("s5_ts_only_call", "mcp__docs", "search_docs", `{}`)).completed()})
	add("search_only_text", s5Turn{Request: s5Lite("s5-ts-only-text", toolSearchOutput("s5_ts_only_text", searchedNamespace("mcp__docs", "search_docs")), s5User("x")),
		Stream: true, Searched: true, Chunks: text("r_ts_only_text")})
	add("search_only_tool_choice_none", turn(s5Set(s5Lite("s5-ts-none", toolSearchOutput("s5_ts_none", searchedNamespace("mcp__docs", "search_docs")), s5User("x")), "tool_choice", "none"), text("r_ts_none")))
	add("search_plus_additional", turn(s5Lite("s5-ts-add", s5LiteDecl(), toolSearchOutput("s5_ts_add", searchedNamespace("mcp__docs", "search_docs")), s5User("x")),
		s5Stream("r_ts_add").call(s5Native("s5_ts_add_call", "mcp__docs", "search_docs", `{}`)).completed()))
	add("search_plus_relay", turn(s5Relay("s5-ts-relay", toolSearchOutput("s5_ts_relay", searchedNamespace("mcp__docs", "search_docs")), s5User("x")),
		s5Stream("r_ts_relay").call(s5Transport("s5_ts_relay_call", s5Code("mcp__docs.search_docs", map[string]any{}), []any{})).completed()))

	// ---- E1 / E4: tools whose parameters demand ciphertext
	collaboration := []any{s5ExecTool(), s4Collaboration()}
	add("enc_relay_other_tool", s5Turn{Request: s5RelayWith("s5-enc-exec", collaboration, s5User("x")), Stream: true, Hidden: s4CollaborationHidden,
		Chunks: s5Stream("r_enc_exec").call(s5Transport("s5_enc_exec", s5Code("exec_command", map[string]any{"cmd": "ls"}), []any{})).completed()})
	spawnArgs := map[string]any{"message": "PLAINTEXT review src", "task_name": "rev"}
	add("enc_relay_spawn_conversation",
		s5Turn{Request: s5RelayWith("s5-enc-spawn", collaboration, s5User("delegate")), Stream: true, Hidden: s4CollaborationHidden, Blocked: []string{"s5_enc_spawn"},
			Chunks: s5Stream("r_enc_spawn_1").call(s5Transport("s5_enc_spawn", s5Code("collaboration.spawn_agent", spawnArgs), []any{})).completed()},
		s5Turn{Request: s5RelayWith("s5-enc-spawn", collaboration, s5User("delegate"),
			s5Echo("s5_enc_spawn", "", "spawn_agent", string(jsonBytes(spawnArgs))), s5Output("s5_enc_spawn", "unsupported call: spawn_agent"), s5User("do it yourself")),
			Stream: true, Hidden: s4CollaborationHidden, Chunks: text("r_enc_spawn_2")},
	)
	add("enc_relay_list_agents_keeps_namespace", s5Turn{Request: s5RelayWith("s5-enc-list", collaboration, s5User("x")), Stream: true, Hidden: s4CollaborationHidden,
		Chunks: s5Stream("r_enc_list").call(s5Transport("s5_enc_list", s5Code("collaboration.list_agents", map[string]any{"all": true}), []any{})).completed()})
	add("enc_relay_non_stream_spawn", s5Turn{Request: s5RelayWith("s5-enc-ns", collaboration, s5User("x")), Hidden: s4CollaborationHidden, Blocked: []string{"s5_enc_ns"},
		Chunks: s5Stream("r_enc_ns").call(s5Transport("s5_enc_ns", s5Code("collaboration.send_message", map[string]any{"message": "PLAINTEXT", "target": "rev"}), []any{})).completed()})
	add("enc_all_hidden", s5Turn{Request: s5RelayWith("s5-enc-all", []any{s4EncryptedOnly()}, s5User("x")), Stream: true,
		Hidden: []string{"collaboration.send_message", "collaboration.spawn_agent"}, AllHidden: true, Chunks: text("r_enc_all")})
	add("enc_lite_native_spawn", turn(s5Lite("s5-enc-lite", map[string]any{"type": "additional_tools", "role": "developer", "tools": []any{s4Collaboration()}}, s5User("x")),
		s5Stream("r_enc_lite").call(s5Native("s5_enc_lite", "collaboration", "spawn_agent", `{"message":"gAAAAAciphertext","task_name":"rev"}`)).completed()))
	add("enc_lite_spawn_through_run_officejs", s5Turn{Request: s5Lite("s5-enc-lite-rl", map[string]any{"type": "additional_tools", "role": "developer", "tools": []any{s4Collaboration()}}, s5User("x")),
		Stream: true, Blocked: []string{"s5_enc_lite_rl"},
		Chunks: s5Stream("r_enc_lite_rl").call(s5Transport("s5_enc_lite_rl", s5Code("collaboration.spawn_agent", spawnArgs), []any{})).completed()})
	add("enc_flat_name_untouched", turn(s5RelayWith("s5-enc-flat", []any{s4FlatEncrypted()}, s5User("x")),
		s5Stream("r_enc_flat").call(s5Transport("s5_enc_flat", s5Code("collaboration__spawn_agent", map[string]any{"message": "PLAINTEXT"}), []any{})).completed()))
	return all
}

// ---------------------------------------------------------------- the runner

const s5ClientStream = "client-s5"

var (
	s5TurnIDTime   = regexp.MustCompile(`"turn_id":"[0-9a-f]{8}-[0-9a-f]{4}-`)
	s5SentrySpanID = regexp.MustCompile(`^([0-9a-f]{32})-[0-9a-f]{16}-1$`)
)

// s5MaskBody hides the one value an identity mode derives from the arrival
// time of a turn: the timestamp half of a UUIDv7 turn_id (turnIDFor). The
// rest of it, and everything else in the body, is deterministic.
func s5MaskBody(body string) string {
	return s5TurnIDTime.ReplaceAllString(body, `"turn_id":"<arrival-ms>-`)
}

// s5HeaderText is the request headers as sorted lines. The Sentry span id is
// drawn at random for every request.
func s5HeaderText(headers http.Header) string {
	keys := make([]string, 0, len(headers))
	for key := range headers {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var lines []string
	for _, key := range keys {
		values := append([]string(nil), headers[key]...)
		if http.CanonicalHeaderKey(key) == "Sentry-Trace" {
			for index, value := range values {
				values[index] = s5SentrySpanID.ReplaceAllString(value, "$1-<span>-1")
			}
		}
		lines = append(lines, key+": "+strings.Join(values, " | "))
	}
	return strings.Join(lines, "\n")
}

// s5TurnState is the fake host's view of one turn.
type s5TurnState struct {
	mu       sync.Mutex
	turn     s5Turn
	reads    []string
	emits    []string
	closeMsg string
	closed   chan struct{}
	isClosed bool
	result   s5Result
}

// s5RunCase registers a fresh plugin with profile (plus overlay, extra
// top-level config.yaml lines) and runs every turn of c.
func s5RunCase(t *testing.T, profile s5Profile, c s5Case, overlay string) []s5Result {
	t.Helper()
	service := NewService()
	var current struct {
		sync.Mutex
		state *s5TurnState
	}
	service.SetHost(func(method string, payload any, out any) error {
		request, _ := payload.(map[string]any)
		current.Lock()
		state := current.state
		current.Unlock()
		if state == nil {
			return errors.New("s5: no turn in progress")
		}
		state.mu.Lock()
		defer state.mu.Unlock()
		switch method {
		case "host.http.do", "host.http.do_stream":
			if stringValue(request["url"]) != DefaultResponsesURL {
				return errors.New("s5: network disabled")
			}
			body, _ := request["body"].([]byte)
			headers, _ := request["headers"].(http.Header)
			state.result.Upstream = s5MaskBody(string(body))
			state.result.Headers = s5HeaderText(headers)
			state.result.UpstreamCalls++
			status := state.turn.Status
			if status == 0 {
				status = 200
			}
			if method == "host.http.do" {
				response := out.(*upstreamResponse)
				response.StatusCode = status
				if status == 200 {
					response.Headers = http.Header{"Content-Type": []string{"text/event-stream"}}
					response.Body = []byte(strings.Join(state.turn.Chunks, ""))
				} else {
					response.Headers = http.Header{"Content-Type": []string{"application/json"}}
					response.Body = []byte(state.turn.Body)
				}
				return nil
			}
			stream := out.(*upstreamStream)
			stream.StatusCode = status
			stream.StreamID = "upstream-s5"
			if status == 200 {
				state.reads = nil
				for _, chunk := range state.turn.Chunks {
					if state.turn.Split && len(chunk) > 1 {
						state.reads = append(state.reads, chunk[:len(chunk)/2], chunk[len(chunk)/2:])
					} else {
						state.reads = append(state.reads, chunk)
					}
				}
			} else {
				state.reads = []string{state.turn.Body}
			}
			return nil
		case "host.http.stream_read":
			chunk := out.(*streamChunk)
			if len(state.reads) > 0 {
				chunk.Payload = []byte(state.reads[0])
				state.reads = state.reads[1:]
				return nil
			}
			chunk.Done = true
			return nil
		case "host.stream.emit":
			if data, ok := request["payload"].([]byte); ok {
				state.emits = append(state.emits, string(data))
			}
			return nil
		case "host.stream.close":
			if message, ok := request["error"].(string); ok {
				state.closeMsg = message
			}
			if !state.isClosed {
				state.isClosed = true
				close(state.closed)
			}
			return nil
		}
		return nil
	})
	config := "data_dir: \"" + filepath.ToSlash(t.TempDir()) + "\"\n" + profile.YAML + overlay
	raw, _ := json.Marshal(map[string]any{"config_yaml": []byte(config)})
	if _, err := service.Handle("plugin.register", raw); err != nil {
		t.Fatalf("%s/%s: plugin.register: %v", profile.Name, c.Name, err)
	}
	results := make([]s5Result, 0, len(c.Turns))
	for index, turn := range c.Turns {
		state := &s5TurnState{turn: turn, closed: make(chan struct{})}
		current.Lock()
		current.state = state
		current.Unlock()
		results = append(results, s5RunTurn(t, service, state, fmt.Sprintf("%s/%s turn %d", profile.Name, c.Name, index+1)))
	}
	return results
}

func s5RunTurn(t *testing.T, service *Service, state *s5TurnState, label string) s5Result {
	t.Helper()
	turn := state.turn
	request := cloneObject(turn.Request)
	request["stream"] = turn.Stream
	authID := turn.AuthID
	if authID == "" {
		authID = "cred-s5-a"
	}
	metadata := map[string]any{}
	switch turn.Caller {
	case "":
		metadata["caller_scope"] = "caller-s5"
	case "-":
	default:
		metadata["caller_scope"] = turn.Caller
	}
	envelope := map[string]any{
		"OriginalRequest": jsonBytes(request), "Payload": jsonBytes(request),
		"StorageJSON": streamTestCredential(), "Model": stringValue(request["model"]), "Stream": turn.Stream,
		"AuthID": authID, "AuthProvider": Provider, "Metadata": metadata, "Headers": turn.Headers,
		"AuthMetadata": map[string]any{"file_name": authID + ".json"}, "host_callback_id": "cb-s5",
	}
	method := "executor.execute"
	if turn.Stream {
		envelope["stream_id"] = s5ClientStream
		method = "executor.execute_stream"
	}
	raw, _ := json.Marshal(envelope)
	response, err := service.Handle(method, raw)
	if err != nil {
		state.mu.Lock()
		defer state.mu.Unlock()
		result := state.result
		var api *APIError
		if errors.As(err, &api) {
			result.Status, result.Kind, result.Message = api.Status, api.Kind, api.Message
		} else {
			result.Kind, result.Message = "error", err.Error()
		}
		return result
	}
	if turn.Stream {
		select {
		case <-state.closed:
		case <-time.After(10 * time.Second):
			t.Fatalf("%s: stream never closed", label)
		}
		state.mu.Lock()
		defer state.mu.Unlock()
		result := state.result
		result.raw = strings.Join(state.emits, "")
		result.Events = s2NormalizeStream(result.raw)
		result.CloseErr = state.closeMsg
		return result
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	result := state.result
	if body, ok := response.(map[string]any); ok {
		if payload, ok := body["Payload"].([]byte); ok {
			result.Payload = string(payload)
		}
	}
	return result
}
