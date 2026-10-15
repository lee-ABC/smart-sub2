package basispoints

// H25 S4: relay_hide_encrypted_tools (E1/E4) and the relay observability.

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// S4.1 which tools are hidden

func TestH25S4EncryptedToolDetection(t *testing.T) {
	marked := map[string]any{"type": "string", "encrypted": true}
	object := func(properties map[string]any) map[string]any {
		return map[string]any{"type": "object", "properties": properties}
	}
	deep := any(marked)
	for i := 0; i < encryptedSchemaMaxDepth+4; i++ {
		deep = object(map[string]any{"next": deep})
	}
	wide := map[string]any{}
	for i := 0; i < encryptedSchemaMaxNodes+10; i++ {
		wide[string(rune('a'+i%26))+strings.Repeat("x", i/26)] = map[string]any{"type": "string"}
	}
	wide["zzzz_last"] = marked
	cases := []struct {
		name      string
		namespace string
		spec      map[string]any
		want      bool
	}{
		{"namespaced property mark", "collaboration", map[string]any{"parameters": object(map[string]any{"message": marked})}, true},
		{"flat name with mark", "", map[string]any{"parameters": object(map[string]any{"message": marked})}, false},
		{"mark on the root schema", "ns", map[string]any{"parameters": map[string]any{"type": "string", "encrypted": true}}, true},
		{"array items", "ns", map[string]any{"parameters": object(map[string]any{"list": map[string]any{"type": "array", "items": marked}})}, true},
		{"tuple items", "ns", map[string]any{"parameters": object(map[string]any{"list": map[string]any{"type": "array", "prefixItems": []any{map[string]any{}, marked}}})}, true},
		{"anyOf", "ns", map[string]any{"parameters": object(map[string]any{"v": map[string]any{"anyOf": []any{map[string]any{"type": "integer"}, marked}}})}, true},
		{"additionalProperties", "ns", map[string]any{"parameters": map[string]any{"type": "object", "additionalProperties": marked}}, true},
		{"patternProperties", "ns", map[string]any{"parameters": map[string]any{"type": "object", "patternProperties": map[string]any{"^m": marked}}}, true},
		{"$defs", "ns", map[string]any{"parameters": map[string]any{"type": "object", "$defs": map[string]any{"m": marked}}}, true},
		{"inputSchema", "ns", map[string]any{"inputSchema": object(map[string]any{"m": marked})}, true},
		{"input_schema", "ns", map[string]any{"input_schema": object(map[string]any{"m": marked})}, true},
		{"property named encrypted (schema true)", "ns", map[string]any{"parameters": object(map[string]any{"encrypted": true})}, false},
		{"property named encrypted (boolean param)", "ns", map[string]any{"parameters": object(map[string]any{"encrypted": map[string]any{"type": "boolean"}})}, false},
		{"string mark", "ns", map[string]any{"parameters": object(map[string]any{"m": map[string]any{"encrypted": "true"}})}, false},
		{"false mark", "ns", map[string]any{"parameters": object(map[string]any{"m": map[string]any{"encrypted": false}})}, false},
		{"mark in a description", "ns", map[string]any{"parameters": object(map[string]any{"m": map[string]any{"description": `"encrypted": true`}})}, false},
		{"mark on the tool, not its parameters", "ns", map[string]any{"encrypted": true, "parameters": object(map[string]any{"m": map[string]any{"type": "string"}})}, false},
		{"custom tool", "ns", map[string]any{"type": "custom", "format": map[string]any{"encrypted": true}}, false},
		{"beyond the depth bound", "ns", map[string]any{"parameters": deep}, false},
		{"beyond the node budget", "ns", map[string]any{"parameters": object(wide)}, false},
	}
	for _, c := range cases {
		spec := toolSpec{Key: "k", Name: "k", Namespace: c.namespace, Type: "function", Spec: c.spec}
		for round := 0; round < 20; round++ {
			if got := encryptedNamespacedTool(spec); got != c.want {
				t.Fatalf("%s: hidden = %v, want %v", c.name, got, c.want)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// S4.1 the other prompt shapes: full contracts and references

func TestH25S4FullAndReferencesPromptsHideEncryptedTools(t *testing.T) {
	source := s4Relay("s4-modes", []any{s4Exec(), s4Collaboration()}, s4User("modes"))
	type mode struct {
		name      string
		configure func(*Config)
	}
	modes := []mode{
		{"summary_legacy", nil},
		{"full_legacy", func(c *Config) { c.RelayToolContracts = relayToolContractsFull }},
		{"summary_references", func(c *Config) { c.RelayProtocol = relayProtocolReferences }},
		{"full_references", func(c *Config) {
			c.RelayToolContracts = relayToolContractsFull
			c.RelayProtocol = relayProtocolReferences
		}},
	}
	for _, m := range modes {
		for _, hide := range []bool{true, false} {
			cfg := defaultConfig()
			if m.configure != nil {
				m.configure(&cfg)
			}
			cfg.RelayHideEncryptedTools = hide
			for _, plain := range []bool{false, true} {
				cfg.ClientIdentity.PlainPrologue = plain
				in := cloneObject(source)
				body, err := prepareResponsesBodyFor(in, cfg, conversationScope(in), persona{}, identityHygiene)
				if err != nil {
					t.Fatal(m.name, err)
				}
				text := string(jsonBytes(body))
				sentence := s2JSONText(s4Sentence(s4CollaborationHidden, plain))
				for _, key := range s4CollaborationHidden {
					listed := strings.Contains(text, "- "+key+" (") || strings.Contains(text, "Full tool key: "+key+";")
					if listed == hide {
						t.Fatalf("%s hide=%v plain=%v: %s listed=%v", m.name, hide, plain, key, listed)
					}
				}
				if strings.Contains(text, sentence) != hide {
					t.Fatalf("%s hide=%v plain=%v: sentence present=%v", m.name, hide, plain, !hide)
				}
				if hide && strings.Contains(text, `\"encrypted\":true`) {
					t.Fatalf("%s: a hidden definition reached the prompt", m.name)
				}
				if !strings.Contains(text, "exec_command") || !strings.Contains(text, "collaboration.list_agents") {
					t.Fatalf("%s: a visible tool went missing", m.name)
				}
			}
		}
	}
	// Every relay tool hidden: no catalogue in any mode, the sentence, and no
	// reminder that names a tool.
	only := s4Relay("s4-modes-only", []any{s4EncryptedOnly()}, s4User("only"))
	for _, m := range modes {
		cfg := defaultConfig()
		if m.configure != nil {
			m.configure(&cfg)
		}
		body, err := prepareResponsesBody(cloneObject(only), cfg)
		if err != nil {
			t.Fatal(m.name, err)
		}
		text := string(jsonBytes(body))
		if strings.Contains(text, "Available client tools:") || !strings.Contains(text, s2JSONText(s4Sentence([]string{"collaboration.send_message", "collaboration.spawn_agent"}, false))) ||
			strings.Contains(text, "Client tools: ") {
			t.Fatalf("%s: all-hidden prompt %s", m.name, text)
		}
	}
	// auto treats it as the Lite request its prompt describes: legacy.
	frozen := withRelayOptions(cloneObject(only), defaultRelayOptions())
	if got := selectRelayProtocol(frozen, relayProtocolAuto, ""); got != relayProtocolLegacy {
		t.Fatalf("auto selected %q for an all-hidden relay catalogue", got)
	}
	visible := withRelayOptions(s4Relay("", []any{s4Exec(), s4EncryptedOnly()}, s4User("auto")), defaultRelayOptions())
	if got := selectRelayProtocol(visible, relayProtocolAuto, ""); got != relayProtocolReferences {
		t.Fatalf("auto selected %q for a new relay conversation", got)
	}
}

// The switch is frozen at request entry like every other decode switch: a
// reconfigure in the middle of a stream cannot make the terminal check decode
// the call differently from its done event.
func TestH25S4SwitchFrozenAtRequestEntry(t *testing.T) {
	spawn := s4Transport("s4_frozen_spawn", "collaboration.spawn_agent", map[string]any{"message": "PLAINTEXT", "task_name": "rev"})
	chunks := s4ToolChunks("r_s4_frozen", []map[string]any{spawn}, nil)
	for _, start := range []bool{true, false} {
		flip := func(service *Service, index int) {
			if index == 2 {
				service.mu.Lock()
				service.cfg.RelayHideEncryptedTools = !start
				service.mu.Unlock()
			}
		}
		run := s2RunStream(t, s2HostScript{configure: func(c *Config) { c.RelayHideEncryptedTools = start }, beforeRead: flip},
			s4Relay("s4-frozen", []any{s4Exec(), s4Collaboration()}, s4User("frozen")), chunks)
		if run.closeErr != "" {
			t.Fatalf("start=%v: a reconfigure failed the stream: %q", start, run.closeErr)
		}
		withNamespace := strings.Contains(run.joined(), `"namespace":"collaboration"`)
		if withNamespace == start {
			t.Fatalf("start=%v: delivered with namespace = %v", start, withNamespace)
		}
	}
}

// ---------------------------------------------------------------------------
// The review's reproduction (wf/encrypt TestWFEncA1/A3 and the additional_tools
// re-check), turned into assertions.

func TestH25S4ReviewRelaySpawnAgentDelivery(t *testing.T) {
	request := map[string]any{
		"model": "gpt-6-astra", "tool_choice": "auto", "parallel_tool_calls": false,
		"tools": []any{s4Exec(), s4Collaboration()},
		"input": []any{s4User("Delegate the review to a sub-agent (review A1).")},
	}
	legacy := s4Transport("s4_rev_a1_legacy", "collaboration.spawn_agent", map[string]any{"message": "PLAINTEXT review src/main.go", "task_name": "rev"})
	references := map[string]any{"type": "function_call", "id": "fc_s4_rev_a1_refs", "call_id": "s4_rev_a1_refs", "name": "run_officejs", "status": "completed",
		"arguments": string(jsonBytes(map[string]any{"summary": "s", "extended_summary": "e", "destructive": false,
			"references": []any{"collaboration.spawn_agent"}, "code": `{"message":"PLAINTEXT review src/main.go","task_name":"rev"}`}))}
	for _, c := range []struct {
		name      string
		item      map[string]any
		configure func(*Config)
	}{
		{"legacy envelope", legacy, nil},
		{"references envelope", references, func(cfg *Config) { cfg.RelayProtocol = relayProtocolReferences }},
	} {
		service := NewService()
		if c.configure != nil {
			cfg := service.config()
			c.configure(&cfg)
			service.mu.Lock()
			service.cfg = cfg
			service.mu.Unlock()
		}
		result := s3RunTurn(t, service, s3Turn{Request: cloneObject(request), Chunks: s3CallChunks("r_s4_rev_a1", c.item), Stream: true,
			AuthID: "cred-s4-review", Metadata: s3Meta("caller-s4-review-a1")})
		if len(result.Delivered) != 1 || result.CloseErr {
			t.Fatalf("%s: delivered %v", c.name, result.Delivered)
		}
		upstream := result.Upstream
		var call map[string]any
		_ = json.Unmarshal([]byte(result.Delivered[0]), &call)
		if call["name"] != "spawn_agent" || call["namespace"] != nil || !strings.Contains(stringValue(call["arguments"]), "PLAINTEXT review src/main.go") {
			t.Fatalf("%s: delivered %v", c.name, call)
		}
		if strings.Contains(upstream, `\"encrypted\":true`) || strings.Contains(upstream, "- collaboration.spawn_agent (") ||
			!strings.Contains(upstream, "unavailable through this relay, so do not call them: collaboration.followup_task, collaboration.send_message, collaboration.spawn_agent.") {
			t.Fatalf("%s: upstream prompt still offers the encrypted tools", c.name)
		}
	}
}

// The review's second re-check: collaboration declared in additional_tools,
// the upstream wraps spawn_agent in run_officejs anyway. h22 handed it back
// with its namespace; it is blocked too. A native Lite call is not.
func TestH25S4ReviewAdditionalToolsSpawnThroughRunOfficejs(t *testing.T) {
	request := s4Lite("s4-rev-additional", s4Additional(s4Exec(), s4Collaboration()), s4User("Delegate (review additional)."))
	wrapped := s4Transport("s4_rev_add_wrapped", "collaboration.spawn_agent", map[string]any{"message": "PLAINTEXT", "task_name": "rev"})
	native := s4NativeCall("s4_rev_add_native", "collaboration", "spawn_agent", `{"message":"gAAAAAciphertext","task_name":"rev"}`)
	for _, c := range []struct {
		item          map[string]any
		wantNamespace bool
	}{{wrapped, false}, {native, true}} {
		result := runStreamTest(t, cloneObject(request), s3CallChunks("r_s4_rev_add", c.item))
		if result.closeErr != "" {
			t.Fatalf("%v: %q", c.item["call_id"], result.closeErr)
		}
		for _, event := range h23Events(t, result.joined()) {
			if event["type"] != "response.output_item.done" {
				continue
			}
			item := objectValue(event["item"])
			if item["name"] != "spawn_agent" || (item["namespace"] == "collaboration") != c.wantNamespace {
				t.Fatalf("%v: delivered %v", c.item["call_id"], item)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// S4.2 observability

func s4CounterDelta(before, after map[string]uint64) map[string]uint64 {
	delta := map[string]uint64{}
	for key, value := range after {
		if value != before[key] {
			delta[key] = value - before[key]
		}
	}
	return delta
}

func s4ReadStatus(t *testing.T, service *Service) map[string]any {
	t.Helper()
	page, err := service.Handle("management.handle", jsonBytes(map[string]any{"Method": "GET", "Path": "/v0/resource/plugins/oai-basispoints/" + relayStatusPath}))
	if err != nil {
		t.Fatal(err)
	}
	var status map[string]any
	decoder := json.NewDecoder(strings.NewReader(string(page.(map[string]any)["Body"].([]byte))))
	decoder.UseNumber()
	if err := decoder.Decode(&status); err != nil {
		t.Fatal(err)
	}
	return status
}

func s4RelayRequestWithCall(key string, item map[string]any) (map[string]any, []string) {
	return s4Relay(key, []any{s4Exec(), s4Collaboration(), s4ListFiles()}, s4User("observe "+key)), s3CallChunks("r_"+key, item)
}

func s4ListFiles() map[string]any {
	return s4Function("list_files", "List files.", map[string]any{"type": "object", "additionalProperties": false,
		"properties":        map[string]any{"path": map[string]any{"type": "string"}, "limit": map[string]any{"type": "integer"}},
		"patternProperties": map[string]any{"^x_": map[string]any{"type": "string"}},
	})
}

func s4TransportWithReferences(callID, tool string, args any, references any, present bool) map[string]any {
	item := s4Transport(callID, tool, args)
	outer := parseArguments(item["arguments"])
	if present {
		outer["references"] = references
	} else {
		delete(outer, "references")
	}
	item["arguments"] = string(jsonBytes(outer))
	return item
}

// Requests, outcomes, run_officejs calls, references counts and every
// exception are counted once each, read back through relay-status, and no
// counter or log carries request content.
func TestH25S4RelayStatusCountsRequestsOutcomesAndExceptions(t *testing.T) {
	before := relayCounterSnapshot()
	var logs []map[string]any
	run := func(request map[string]any, chunks []string) streamTestResult {
		result := runStreamTest(t, request, chunks)
		logs = append(logs, result.logs...)
		return result
	}
	// A blocked spawn_agent (E1 + E4), references [] -> "0".
	ok := run(s4RelayRequestWithCall("s4-obs-spawn", s4Transport("s4_obs_spawn", "collaboration.spawn_agent", map[string]any{"message": "PLAINTEXT", "task_name": "r"})))
	if ok.closeErr != "" {
		t.Fatal(ok.closeErr)
	}
	// A broken envelope: code_json, zero output, so the retry hint (E5).
	broken := s4Transport("s4_obs_broken", "exec_command", map[string]any{"cmd": "ls"})
	outer := parseArguments(broken["arguments"])
	outer["code"] = `{"tool":"exec_command","args":{"cmd":"C:\path"}}`
	broken["arguments"] = string(jsonBytes(outer))
	failed := run(s4RelayRequestWithCall("s4-obs-broken", broken))
	if !strings.Contains(failed.closeErr, "reason=code_json") || !strings.HasSuffix(failed.closeErr, retryableErrorHint) {
		t.Fatalf("broken envelope: %q", failed.closeErr)
	}
	// E3 in the decoder: a coerced scalar and a schema only rc2's checker accepts.
	run(s4RelayRequestWithCall("s4-obs-coerce", s4Transport("s4_obs_coerce", "list_files", map[string]any{"path": "/", "limit": "20"})))
	run(s4RelayRequestWithCall("s4-obs-corrected", s4Transport("s4_obs_corrected", "list_files", map[string]any{"path": "/", "x_tag": "a"})))
	// references entries: absent, not a list, 2, 5.
	for name, refs := range map[string]struct {
		value   any
		present bool
	}{"absent": {nil, false}, "null": {nil, true}, "two": {[]any{"Sheet1!A1", "b"}, true}, "three": {[]any{"a", "b", "c"}, true}, "five": {[]any{"a", "b", "c", "d", "e"}, true}} {
		run(s4RelayRequestWithCall("s4-obs-refs-"+name, s4TransportWithReferences("s4_obs_refs_"+name, "exec_command", map[string]any{"cmd": "ls"}, refs.value, refs.present)))
	}
	// E3 at the terminal: a missing terminal id, an equivalent number.
	idItem := s4Transport("s4_obs_id", "exec_command", map[string]any{"cmd": "ls"})
	idTerminal := cloneObject(idItem)
	delete(idTerminal, "id")
	request, _ := s4RelayRequestWithCall("s4-obs-id", idItem)
	run(request, []string{s2Created(), h21ItemEvent("response.output_item.done", 0, idItem), h21Terminal("response.completed", idTerminal)})
	numberDone := h21TransportItem("s4_obs_number", "lookup", map[string]any{"n": 1000})
	numberDone["arguments"] = `{"code":"{\"tool\":\"lookup\",\"args\":{\"n\":1e3}}"}`
	numberTerminal := h21TransportItem("s4_obs_number", "lookup", map[string]any{"n": 1000})
	run(h23NumericSource(), []string{s2Created(), h21ItemEvent("response.output_item.done", 0, numberDone), h21Terminal("response.completed", numberTerminal)})
	// A Lite native call and a tool-less request.
	run(s4Lite("s4-obs-lite", s4Additional(s4Exec()), s4User("lite")), s3CallChunks("r_s4_obs_lite", s4NativeCall("s4_obs_lite", "", "exec_command", `{"cmd":"ls"}`)))
	run(map[string]any{"model": "gpt-6-astra", "input": []any{s4User("plain")}}, s3TextChunks("r_s4_obs_plain"))
	// A non-streaming relay request.
	service := NewService()
	nonStream := s3RunTurn(t, service, s3Turn{Request: s4Relay("s4-obs-nonstream", []any{s4Exec()}, s4User("ns")),
		Chunks: s3CallChunks("r_s4_obs_ns", s4Transport("s4_obs_ns", "exec_command", map[string]any{"cmd": "ls"})), AuthID: "cred-s4-obs", Metadata: s3Meta("caller-s4-obs")})
	if len(nonStream.Delivered) != 1 {
		t.Fatalf("non-stream: %+v", nonStream)
	}

	delta := s4CounterDelta(before, relayCounterSnapshot())
	for key, want := range map[string]uint64{
		"relay_requests:legacy:relay":          12,
		"relay_requests:legacy:lite":           1,
		"relay_requests:legacy:none":           1,
		"relay_outcome:legacy:relay:succeeded": 11,
		"relay_outcome:legacy:relay:code_json": 1,
		"relay_outcome:legacy:lite:succeeded":  1,
		"relay_outcome:legacy:none:succeeded":  1,
		"relay_call:legacy:ok":                 11,
		"relay_call:legacy:code_json":          1,
		"references_entries:legacy:0":          6,
		"references_entries:legacy:absent":     2,
		"references_entries:legacy:not_list":   1,
		"references_entries:legacy:2":          1,
		"references_entries:legacy:3":          1,
		"references_entries:legacy:4+":         1,
		"encrypted_tools_hidden":               10,
		"encrypted_tool_blocked":               1,
		"scalar_coerced":                       1,
		"schema_corrected":                     1,
		"terminal_id_restored":                 1,
		"terminal_args_equivalent":             1,
		"retryable_error_hint":                 1,
		"replay_scope:prompt_cache_key":        12,
		"replay_scope:first_item":              2,
	} {
		if delta[key] != want {
			t.Errorf("%s: +%d, want +%d (all: %v)", key, delta[key], want, delta)
		}
	}
	for key := range relayCounterSnapshot() {
		for _, content := range []string{"PLAINTEXT", "spawn", "exec_command", "s4_obs", "s4-obs", "Sheet1", "lookup", "list_files"} {
			if strings.Contains(key, content) {
				t.Fatalf("counter name %q carries request content", key)
			}
		}
	}
	for _, entry := range logs {
		text := string(jsonBytes(entry))
		for _, content := range []string{"PLAINTEXT", "spawn_agent", "collaboration", "s4_obs", "s4-obs", "Sheet1", "C:\\\\path"} {
			if strings.Contains(text, content) {
				t.Fatalf("log line carries request content %q: %s", content, text)
			}
		}
	}

	status := s4ReadStatus(t, service)
	summary := objectValue(status["summary"])
	exceptions := objectValue(summary["exceptions"])
	counters := objectValue(status["counters"])
	for _, exception := range relayExceptionCounters {
		if exceptions[exception.name] == nil || stringValue(exceptions[exception.name]) != stringValue(counters[exception.counter]) && counters[exception.counter] != nil {
			t.Fatalf("exception %s = %v, counter %s = %v", exception.name, exceptions[exception.name], exception.counter, counters[exception.counter])
		}
	}
	relay := objectValue(objectValue(objectValue(summary["requests_by_protocol"])["legacy"])["relay"])
	if stringValue(relay["requests"]) != stringValue(counters["relay_requests:legacy:relay"]) ||
		stringValue(objectValue(relay["outcomes"])["code_json"]) != stringValue(counters["relay_outcome:legacy:relay:code_json"]) {
		t.Fatalf("summary %v disagrees with counters", relay)
	}
	if objectValue(objectValue(summary["references_entries_by_protocol"])["legacy"])["4+"] == nil ||
		objectValue(summary["replay_scope"])["replay_scope:prompt_cache_key"] == nil {
		t.Fatalf("summary lacks references or scope: %v", summary)
	}
}

// relayOutcomeLabel uses only code-owned values: a tool reason code, or an
// error kind that looks like one plus its status, never a message.
func TestH25S4OutcomeLabels(t *testing.T) {
	for _, c := range []struct {
		err  error
		want string
	}{
		{nil, "succeeded"},
		{toolReject("code_json"), "code_json"},
		{&toolRejection{cause: &APIError{Status: 422, Kind: "invalid_tool_call"}, reason: "PLAINTEXT secret"}, "unspecified"},
		{fail(502, "upstream_transport", "Basis Points stream read failed: PLAINTEXT"), "error:upstream_transport:502"},
		{fail(499, "client_disconnected", "x"), "error:client_disconnected:499"},
		{fail(503, "Upstream Said: PLAINTEXT", "x"), "error:other:503"},
		{fail(1000, "weird_status", "x"), "error:weird_status:0"},
		{http.ErrHandlerTimeout, "error:other:0"},
	} {
		if got := relayOutcomeLabel(c.err); got != c.want {
			t.Fatalf("%v: label %q, want %q", c.err, got, c.want)
		}
	}
}

// One outcome per stream, also when the relay panics after it has closed
// with an error once.
func TestH25S4StreamOutcomeCountedOnce(t *testing.T) {
	recorder := newRelayOutcomeRecorder(ExecutorRequest{selectedRelayProtocol: relayProtocolLegacy, relayKind: "relay"})
	before := relayCounterSnapshot()
	recorder.record(toolReject("code_json"))
	recorder.record(fail(500, "stream_panic", "x"))
	recorder.record(nil)
	delta := s4CounterDelta(before, relayCounterSnapshot())
	if len(delta) != 1 || delta["relay_outcome:legacy:relay:code_json"] != 1 {
		t.Fatalf("outcomes %v", delta)
	}
}
