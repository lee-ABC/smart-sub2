package basispoints

// H25 S3: replay cache and scope (H25-SPEC section 3, S3).

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

type s3Drive struct {
	upstream      string
	upstreamCalls int
	status        int
	kind          string
	closeErr      string
	logs          []map[string]any
	emitted       string
}

// s3DriveTurn runs one request end to end and also captures host.log.
func s3DriveTurn(t *testing.T, service *Service, turn s3Turn) s3Drive {
	t.Helper()
	var (
		mu     sync.Mutex
		out    s3Drive
		next   int
		done   = make(chan struct{})
		closed bool
		emits  []string
	)
	service.SetHost(func(method string, payload any, result any) error {
		request, _ := payload.(map[string]any)
		mu.Lock()
		defer mu.Unlock()
		switch method {
		case "host.log":
			out.logs = append(out.logs, request)
		case "host.http.do_stream", "host.http.do":
			out.upstreamCalls++
			if body, ok := request["body"].([]byte); ok {
				out.upstream = string(body)
			}
			if method == "host.http.do" {
				response := result.(*upstreamResponse)
				response.StatusCode = 200
				response.Headers = http.Header{"Content-Type": []string{"text/event-stream"}}
				response.Body = []byte(strings.Join(turn.Chunks, ""))
				return nil
			}
			stream := result.(*upstreamStream)
			stream.StatusCode, stream.StreamID = 200, "up-s3t"
		case "host.http.stream_read":
			chunk := result.(*streamChunk)
			if next < len(turn.Chunks) {
				chunk.Payload = []byte(turn.Chunks[next])
				next++
			} else {
				chunk.Done = true
			}
		case "host.stream.emit":
			emits = append(emits, string(request["payload"].([]byte)))
		case "host.stream.close":
			if message, ok := request["error"].(string); ok {
				out.closeErr = message
			}
			if !closed {
				closed = true
				close(done)
			}
		}
		return nil
	})
	request := cloneObject(turn.Request)
	request["stream"] = turn.Stream
	envelope := map[string]any{
		"OriginalRequest": jsonBytes(request), "Payload": jsonBytes(request),
		"StorageJSON": streamTestCredential(), "Model": "gpt-6-astra", "Stream": turn.Stream,
		"AuthID": turn.AuthID, "AuthProvider": Provider, "Metadata": turn.Metadata, "Headers": turn.Headers,
		"host_callback_id": "s3-callback",
	}
	method := "executor.execute"
	if turn.Stream {
		envelope["stream_id"] = "client-s3t"
		method = "executor.execute_stream"
	}
	raw, _ := json.Marshal(envelope)
	if _, err := service.Handle(method, raw); err != nil {
		var api *APIError
		if errors.As(err, &api) {
			out.status, out.kind = api.Status, api.Kind
		}
		mu.Lock()
		defer mu.Unlock()
		return out
	}
	if turn.Stream {
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("stream never closed")
		}
	}
	mu.Lock()
	defer mu.Unlock()
	out.emitted = strings.Join(emits, "")
	return out
}

func s3ServiceWith(configure func(*Config)) *Service {
	service := NewService()
	if configure != nil {
		cfg := service.config()
		configure(&cfg)
		service.mu.Lock()
		service.cfg = cfg
		service.mu.Unlock()
	}
	return service
}

func s3HistoryNotes(logs []map[string]any) []string {
	var reasons []string
	for _, entry := range logs {
		fields, _ := entry["fields"].(map[string]any)
		if fields["event"] == "relay_history_note" {
			reasons = append(reasons, stringValue(fields["reason"]))
		}
	}
	return reasons
}

func s3UpstreamCall(upstream, callID string) map[string]any {
	var body map[string]any
	if json.Unmarshal([]byte(upstream), &body) != nil {
		return nil
	}
	items, _ := body["input"].([]any)
	for _, raw := range items {
		item := objectValue(raw)
		if item != nil && item["call_id"] == callID && (item["type"] == "function_call" || item["type"] == "custom_tool_call") {
			return item
		}
	}
	return nil
}

func s3UpstreamItem(upstream, callID, kind string) map[string]any {
	var body map[string]any
	if json.Unmarshal([]byte(upstream), &body) != nil {
		return nil
	}
	items, _ := body["input"].([]any)
	for _, raw := range items {
		item := objectValue(raw)
		if item != nil && item["call_id"] == callID && item["type"] == kind {
			return item
		}
	}
	return nil
}

// S3.1: the replay scope is h22's conversation key, prefixed by caller_scope
// when the host supplies one -- never the credential, never a requirement
// for an explicit session.
func TestH25S3ReplayScopeIsH22KeyPlusCaller(t *testing.T) {
	sources := map[string]map[string]any{
		"prompt_cache_key": {"prompt_cache_key": "pck", "input": []any{s3User("a")}},
		"session_id":       {"session_id": "body-session", "input": []any{s3User("a")}},
		"client_metadata":  {"client_metadata": map[string]any{"session_id": "cm"}, "input": []any{s3User("a")}},
		"first_item":       {"input": []any{s3User("only the first item")}},
		"none":             {"input": "plain string input"},
	}
	for label, source := range sources {
		t.Run(label, func(t *testing.T) {
			h22 := h22ConversationScope(source)
			bare := ExecutorRequest{AuthID: "cred-A", AuthProvider: Provider}
			scope, reason := deriveReplayScope(bare, source)
			if scope != h22 || reason != label {
				t.Fatalf("without caller_scope the scope is not h22's: %q/%q vs %q", scope, reason, h22)
			}
			base := ExecutorRequest{AuthID: "cred-A", AuthProvider: Provider, Metadata: map[string]any{"caller_scope": "caller-A", "execution_session_id": "exec-1"},
				Headers: http.Header{"Session_id": []string{"hdr-1"}}}
			want, _ := deriveReplayScope(base, source)
			if (want == "") != (h22 == "") || (want != "" && (want == h22 || !strings.HasPrefix(want, "replay-v3:"))) {
				t.Fatalf("caller-scoped key %q does not track h22's %q", want, h22)
			}
			// Neither the credential, the provider, a session header nor the
			// host's execution session moves a conversation's replay scope.
			for _, change := range []func(*ExecutorRequest){
				func(r *ExecutorRequest) { r.AuthID = "cred-B" },
				func(r *ExecutorRequest) { r.AuthProvider = "another" },
				func(r *ExecutorRequest) { r.Headers = http.Header{"Session_id": []string{"hdr-2"}} },
				func(r *ExecutorRequest) { r.Headers = nil },
				func(r *ExecutorRequest) { r.Metadata = map[string]any{"caller_scope": "caller-A"} },
			} {
				req := base
				req.Metadata = map[string]any{"caller_scope": "caller-A", "execution_session_id": "exec-1"}
				change(&req)
				if got, _ := deriveReplayScope(req, source); got != want {
					t.Fatal("a non-conversation identity changed the replay scope")
				}
			}
			// A caller_scope only ever separates callers further.
			other := base
			other.Metadata = map[string]any{"caller_scope": "caller-B"}
			if got, _ := deriveReplayScope(other, source); h22 != "" && got == want {
				t.Fatal("two authenticated callers share a replay scope")
			}
		})
	}
}

func TestH25S3ScopeIsFrozenPrivateAndCounted(t *testing.T) {
	source := s3Relay("s3-frozen", s3User("frozen"))
	source["client_metadata"] = map[string]any{"session_id": "cm-frozen"}
	req := ExecutorRequest{AuthID: "cred", AuthProvider: Provider, Model: "gpt-6-astra", Metadata: map[string]any{"caller_scope": "caller"},
		OriginalRequest: jsonBytes(source), Payload: jsonBytes(source), StorageJSON: streamTestCredential()}
	before := relayCounterSnapshot()
	wantScope, _ := deriveReplayScope(req, source)
	wantStrict, _ := deriveStrictReplayScope(req, source)
	prepared, _, _, err := NewService().prepareRequestWithRelayContext(&req)
	if err != nil {
		t.Fatal(err)
	}
	if !req.replayScopeFrozen || req.replayCacheScope != wantScope || req.replayStrictScope != wantStrict || wantStrict == "" || wantScope == wantStrict {
		t.Fatal("request did not freeze both scopes")
	}
	// Counted with relay_requests once the request is admitted, not when it
	// is prepared (review R5): a request the concurrency gate turns away is
	// not counted.
	if prepared := relayCounterSnapshot(); prepared["replay_scope:prompt_cache_key"] != before["replay_scope:prompt_cache_key"] || prepared["replay_scope_caller:present"] != before["replay_scope_caller:present"] {
		t.Fatal("scope source counted before admission")
	}
	countRelayRequest(req)
	after := relayCounterSnapshot()
	if after["replay_scope:prompt_cache_key"] != before["replay_scope:prompt_cache_key"]+1 || after["replay_scope_caller:present"] != before["replay_scope_caller:present"]+1 {
		t.Fatal("scope source was not counted")
	}
	text := string(jsonBytes(prepared))
	if strings.Contains(text, "replay-v3:") || strings.Contains(text, "replay-v2:") || strings.Contains(text, "cpa_trusted_replay_scope") {
		t.Fatal("an internal scope was sent upstream")
	}
	marked := withFrozenReplayScope(source, req)
	if string(jsonBytes(marked[replayScopeSourceKey])) != "null" {
		t.Fatal("the scope marker serialises")
	}
	forged := cloneObject(source)
	forged[replayScopeSourceKey] = map[string]any{"key": "forged", "strict": "forged"}
	if _, present := replayScopeFromSource(forged); present || strictReplayScopeFromSource(forged) != "" {
		t.Fatal("client JSON forged a scope marker")
	}
	var abi map[string]any
	if json.Unmarshal(jsonBytes(req), &abi) != nil || abi["replayStrictScope"] != nil || abi["replayCacheScope"] != nil {
		t.Fatal("private scope fields entered the host ABI")
	}
}

// Review replay-3/-5, encrypt-3 (TestWFEncB6/B10): without any session id
// and across a credential switch, the replay cache and the namespace memory
// work exactly as in h22.
func TestH25S3NoSessionAndCredentialSwitchStillReplay(t *testing.T) {
	for _, credentialSwitch := range []bool{false, true} {
		service := NewService()
		key := fmt.Sprintf("s3-nosession-%v", credentialSwitch)
		callID := "call_s3_nosession_" + key
		first := s3Turn{Request: s3Relay(key, s3User("list files")), Chunks: s3CallChunks("r1", s3Transport(callID, "exec_command", map[string]any{"cmd": "ls"})),
			Stream: true, AuthID: "cred-A", Metadata: s3Meta("caller")}
		if result := s3DriveTurn(t, service, first); result.closeErr != "" {
			t.Fatal(result.closeErr)
		}
		// Next turn: no tools at all (B10) and a stripped namespace (B6).
		second := s3Relay(key, s3User("list files"), s3Call(callID, "exec_command", `{"cmd":"ls"}`), s3Output(callID, "a.txt"), s3User("summarize"))
		delete(second, "tools")
		turn := s3Turn{Request: second, Chunks: s3TextChunks("r2"), Stream: true, AuthID: "cred-A", Metadata: s3Meta("caller")}
		if credentialSwitch {
			turn.AuthID = "cred-B"
		}
		result := s3DriveTurn(t, service, turn)
		call := s3UpstreamCall(result.upstream, callID)
		if call["name"] != transportName || !strings.Contains(stringValue(call["arguments"]), "model summary") {
			t.Fatalf("credentialSwitch=%v: the delivered relay call was not replayed: %v", credentialSwitch, call)
		}
	}
	service := NewService()
	lite := s3Turn{Request: s3Lite("s3-ns-memory", s3LiteDecl(), s3User("hi")), Chunks: s3TextChunks("r1"), Stream: true, AuthID: "cred-A", Metadata: s3Meta("caller")}
	_ = s3DriveTurn(t, service, lite)
	later := s3Lite("s3-ns-memory", s3User("hi"), s3Call("call_s3_ns_memory", "wait_agent", `{"id":"1"}`), s3Output("call_s3_ns_memory", "ok"), s3User("next"))
	result := s3DriveTurn(t, service, s3Turn{Request: later, Chunks: s3TextChunks("r2"), Stream: true, AuthID: "cred-B", Metadata: s3Meta("caller")})
	if call := s3UpstreamCall(result.upstream, "call_s3_ns_memory"); call["namespace"] != "collab" {
		t.Fatalf("namespace memory lost without a session: %v", call)
	}
}

// A caller_scope separates callers (the one deliberate difference from h22,
// which shared a prompt_cache_key across every caller of the process).
func TestH25S3CallersDoNotShareReplay(t *testing.T) {
	service := NewService()
	callID := "call_s3_callers"
	key := "s3-callers"
	_ = s3DriveTurn(t, service, s3Turn{Request: s3Relay(key, s3User("callers")), Chunks: s3CallChunks("r1", s3Transport(callID, "exec_command", map[string]any{"cmd": "PRIVATE_A"})),
		Stream: true, AuthID: "cred", Metadata: s3Meta("caller-A")})
	history := s3Relay(key, s3User("callers"), s3Call(callID, "exec_command", `{"cmd":"CURRENT_B"}`), s3Output(callID, "b"), s3User("next"))
	result := s3DriveTurn(t, service, s3Turn{Request: history, Chunks: s3TextChunks("r2"), Stream: true, AuthID: "cred", Metadata: s3Meta("caller-B")})
	if strings.Contains(result.upstream, "PRIVATE_A") || !strings.Contains(result.upstream, "CURRENT_B") {
		t.Fatal("another caller's delivered call replaced this caller's history")
	}
	result = s3DriveTurn(t, service, s3Turn{Request: history, Chunks: s3TextChunks("r3"), Stream: true, AuthID: "cred", Metadata: s3Meta("caller-A")})
	if !strings.Contains(result.upstream, "PRIVATE_A") {
		t.Fatal("the delivering caller lost its own replay")
	}
}

// S3.2 replay (default): a rewritten relay call replays the model's own
// transport call, a rewritten native call goes as the client sent it, and
// both are logged under a fixed label -- never refused, in any scope.
func TestH25S3ReplayModeNeverRefusesAndLogs(t *testing.T) {
	for _, configure := range []func(*Config){nil, s2ExplicitDefaults} {
		service := s3ServiceWith(configure)
		key := fmt.Sprintf("s3-replay-logs-%v", configure == nil)
		relayID, nativeID := "call_s3_rl_"+key, "call_s3_nl_"+key
		session := http.Header{"Session_id": []string{"strict-session-" + key}}
		turn := func(request map[string]any, chunks []string) s3Turn {
			return s3Turn{Request: request, Chunks: chunks, Stream: true, AuthID: "cred-A", Metadata: s3Meta("caller"), Headers: session}
		}
		_ = s3DriveTurn(t, service, turn(s3Relay(key, s3LiteDecl(), s3User("go")), s3CallChunks("r1", s3Transport(relayID, "exec_command", map[string]any{"cmd": "ls"}))))
		// A declared native tool (additional_tools) called natively.
		native := map[string]any{"type": "function_call", "id": "fc_" + nativeID, "call_id": nativeID, "name": "wait_agent", "namespace": "collab", "status": "completed", "arguments": `{"id":"a"}`}
		_ = s3DriveTurn(t, service, turn(s3Relay(key, s3LiteDecl(), s3User("go"), s3User("plan")), s3CallChunks("r2", native)))
		history := s3Relay(key, s3LiteDecl(), s3User("go"),
			s3Call(relayID, "exec_command", `{"cmd":"ls","added_default":true}`), s3Output(relayID, "files"),
			s3Call(nativeID, "wait_agent", `{"id":"b"}`), s3Output(nativeID, "ok"), s3User("next"))
		for attempt := 0; attempt < 3; attempt++ {
			result := s3DriveTurn(t, service, turn(history, s3TextChunks("r3")))
			if result.status != 0 || result.upstreamCalls != 1 || result.closeErr != "" {
				t.Fatalf("replay refused or failed: %d %s %q", result.status, result.kind, result.closeErr)
			}
			relay := s3UpstreamCall(result.upstream, relayID)
			if relay["name"] != transportName || strings.Contains(stringValue(relay["arguments"]), "added_default") {
				t.Fatalf("the model's own transport call was not replayed: %v", relay)
			}
			if call := s3UpstreamCall(result.upstream, nativeID); call["arguments"] != `{"id":"b"}` || call["name"] != "wait_agent" || call["namespace"] != "collab" {
				t.Fatalf("a native call was not sent as the client sent it: %v", call)
			}
			notes := s3HistoryNotes(result.logs)
			if !reflect.DeepEqual(notes, []string{"relay_history_mismatch", "native_history_mismatch"}) {
				t.Fatalf("history notes = %v", notes)
			}
			for _, entry := range result.logs {
				if entry["host_callback_id"] != "s3-callback" {
					t.Fatal("note not logged under the request's callback id")
				}
				text := string(jsonBytes(entry))
				if strings.Contains(text, "added_default") || strings.Contains(text, relayID) || strings.Contains(text, "wait_agent") {
					t.Fatal("a note carried request content")
				}
			}
		}
	}
}

// S3.2 reject (rc2): refused with 400 only when the contradicted delivery
// was made under the same strict scope (caller + credential + explicit
// session); otherwise logged and sent on h22's cold path.
func TestH25S3RejectModeRefusesOnlyWithinTheStrictScope(t *testing.T) {
	service := s3ServiceWith(s3Reject)
	key := "s3-reject"
	callID := "call_s3_reject"
	session := http.Header{"Session_id": []string{"strict-session"}}
	turn := func(request map[string]any, chunks []string, auth string, headers http.Header) s3Turn {
		return s3Turn{Request: request, Chunks: chunks, Stream: true, AuthID: auth, Metadata: s3Meta("caller"), Headers: headers}
	}
	_ = s3DriveTurn(t, service, turn(s3Relay(key, s3User("go")), s3CallChunks("r1", s3Transport(callID, "exec_command", map[string]any{"cmd": "ls"})), "cred-A", session))
	rewritten := s3Relay(key, s3User("go"), s3Call(callID, "exec_command", `{"cmd":"ls","added_default":true}`), s3Output(callID, "files"), s3User("next"))
	exact := s3Relay(key, s3User("go"), s3Call(callID, "exec_command", `{"cmd":"ls"}`), s3Output(callID, "files"), s3User("next"))
	for attempt := 0; attempt < 2; attempt++ {
		result := s3DriveTurn(t, service, turn(rewritten, s3TextChunks("r2"), "cred-A", session))
		if result.status != 400 || result.kind != "history_conflict" || result.upstreamCalls != 0 {
			t.Fatalf("strict-scope contradiction not refused: %d %s calls=%d", result.status, result.kind, result.upstreamCalls)
		}
		if notes := s3HistoryNotes(result.logs); !reflect.DeepEqual(notes, []string{"history_conflict_rejected"}) {
			t.Fatalf("refusal not logged: %v", notes)
		}
	}
	if result := s3DriveTurn(t, service, turn(exact, s3TextChunks("r3"), "cred-A", session)); result.status != 0 || s3UpstreamCall(result.upstream, callID)["name"] != transportName {
		t.Fatal("the unchanged history did not replay after a refusal")
	}
	for label, other := range map[string]s3Turn{
		"other credential": turn(rewritten, s3TextChunks("r4"), "cred-B", session),
		"other session":    turn(rewritten, s3TextChunks("r5"), "cred-A", http.Header{"Session_id": []string{"another"}}),
		"no session":       turn(rewritten, s3TextChunks("r6"), "cred-A", nil),
	} {
		result := s3DriveTurn(t, service, other)
		if result.status != 0 || result.upstreamCalls != 1 {
			t.Fatalf("%s: a contradiction outside the strict scope was refused", label)
		}
		call := s3UpstreamCall(result.upstream, callID)
		if call["name"] != transportName || !strings.Contains(stringValue(call["arguments"]), "added_default") {
			t.Fatalf("%s: not sent on h22's cold path: %v", label, call)
		}
		if notes := s3HistoryNotes(result.logs); !reflect.DeepEqual(notes, []string{"history_conflict_unscoped"}) {
			t.Fatalf("%s: notes %v", label, notes)
		}
	}
}

// Review verify-encrypt-5: the Lite history-conflict matrix. Default: every
// variant reaches the upstream. reject: refused exactly where rc2 refused
// (same caller, credential and session), never otherwise.
func TestH25S3LiteHistoryMatrix(t *testing.T) {
	args := `{"cmd":"ls -la","timeout":1.0,"id":12345678901234567890}`
	delivered := map[string]any{"type": "function_call", "id": "fc_v5a", "call_id": "call_s3_v5a", "name": "shell", "status": "completed", "arguments": args}
	decl := map[string]any{"type": "additional_tools", "role": "developer", "tools": []any{map[string]any{"type": "function", "name": "shell",
		"parameters": map[string]any{"type": "object", "properties": map[string]any{"cmd": map[string]any{"type": "string"}}}}}}
	history := func(mutate func(map[string]any)) map[string]any {
		item := map[string]any{"type": "function_call", "id": "fc_v5a", "call_id": "call_s3_v5a", "name": "shell", "arguments": args}
		if mutate != nil {
			mutate(item)
		}
		return s3Lite("s3-v5", decl, s3User("hi"), item, s3Output("call_s3_v5a", "done"), s3User("next"))
	}
	meta := map[string]any{"caller_scope": "v5-caller", "execution_session_id": "v5-sess"}
	cases := []struct {
		label    string
		mutate   func(map[string]any)
		auth     string
		meta     map[string]any
		refusing bool
	}{
		{"unchanged", nil, "cred-A", meta, false},
		{"id dropped", func(i map[string]any) { delete(i, "id") }, "cred-A", meta, false},
		{"whitespace and key order", func(i map[string]any) {
			i["arguments"] = `{ "id": 12345678901234567890, "timeout": 1.0, "cmd": "ls -la" }`
		}, "cred-A", meta, false},
		{"1.0 written as 1", func(i map[string]any) { i["arguments"] = `{"cmd":"ls -la","timeout":1,"id":12345678901234567890}` }, "cred-A", meta, false},
		{"bigint precision loss", func(i map[string]any) { i["arguments"] = `{"cmd":"ls -la","timeout":1,"id":12345678901234567000}` }, "cred-A", meta, true},
		{"args truncated", func(i map[string]any) { i["arguments"] = `{"cmd":"ls","timeout":1.0,"id":12345678901234567890}` }, "cred-A", meta, true},
		{"id rewritten", func(i map[string]any) { i["id"] = "fc_rewritten" }, "cred-A", meta, true},
		{"name changed", func(i map[string]any) { i["name"] = "shell2" }, "cred-A", meta, true},
		{"id rewritten, other session", func(i map[string]any) { i["id"] = "fc_rewritten" }, "cred-A", map[string]any{"caller_scope": "v5-caller", "execution_session_id": "v5-sess-2"}, false},
		{"id rewritten, other credential", func(i map[string]any) { i["id"] = "fc_rewritten" }, "cred-B", meta, false},
		{"id rewritten, no session", func(i map[string]any) { i["id"] = "fc_rewritten" }, "cred-A", map[string]any{"caller_scope": "v5-caller"}, false},
	}
	for _, mode := range []struct {
		name      string
		configure func(*Config)
	}{{"default", nil}, {"reject", s3Reject}} {
		s3ResetCaches()
		service := s3ServiceWith(mode.configure)
		first := s3Drive{}
		first = s3DriveTurn(t, service, s3Turn{Request: s3Lite("s3-v5", decl, s3User("hi")), Chunks: s3CallChunks("rv5", delivered), Stream: true, AuthID: "cred-A", Metadata: meta})
		if first.closeErr != "" {
			t.Fatal(first.closeErr)
		}
		for _, c := range cases {
			for _, stream := range []bool{true, false} {
				result := s3DriveTurn(t, service, s3Turn{Request: history(c.mutate), Chunks: s3TextChunks("rv5b"), Stream: stream, AuthID: c.auth, Metadata: c.meta})
				refused := result.status == 400 && result.kind == "history_conflict"
				want := mode.name == "reject" && c.refusing
				if refused != want || (!refused && result.upstreamCalls != 1) {
					t.Errorf("%s/%s stream=%v: refused=%v upstreamCalls=%d", mode.name, c.label, stream, refused, result.upstreamCalls)
				}
				if !refused {
					// A native call goes upstream as the client sent it (both modes;
					// reject only restores an omitted namespace from the delivery).
					want := history(c.mutate)
					items := want["input"].([]any)
					sent := s3UpstreamCall(result.upstream, "call_s3_v5a")
					if sent["arguments"] != objectValue(items[2])["arguments"] {
						t.Errorf("%s/%s: native history changed: %v", mode.name, c.label, sent)
					}
				}
			}
		}
	}
}

// S3.2: under replay the later of two different deliveries of one call_id
// is what replays (h22); reject keeps rc2's tombstone.
func TestH25S3ReplayModeLaterDeliveryWins(t *testing.T) {
	scope := "s3-unit/later-wins"
	first := replayClientCall("s3-reused", "Paris")
	second := replayClientCall("s3-reused", "Oslo")
	before := relayCounterSnapshot()["history_record_overwritten"]
	rememberRelayCall(scope, replayNativeCall(first), first)
	rememberRelayCall(scope, replayNativeCall(second), second)
	if got := rememberedNativeCall(scope, "s3-reused"); string(jsonBytes(got)) != string(jsonBytes(replayNativeCall(second))) {
		t.Fatal("the later delivery does not replay")
	}
	if relayCounterSnapshot()["history_record_overwritten"] != before+1 {
		t.Fatal("overwrite not counted")
	}
	if cached, ok := cachedCallForOutput(scope, "s3-reused"); !ok || cached.Origin != "relay" {
		t.Fatal("output-only continuation lost its relay record")
	}
	// The same call delivered again (immediate delivery writes at done and
	// again at the terminal) is not an overwrite.
	rememberRelayCall(scope, replayNativeCall(second), second)
	if relayCounterSnapshot()["history_record_overwritten"] != before+1 {
		t.Fatal("re-recording the same call counted as an overwrite")
	}
	// Through a stream: two responses deliver one call_id differently.
	service := NewService()
	key := "s3-later-wins-stream"
	callID := "call_s3_later"
	turn := func(request map[string]any, chunks []string) s3Turn {
		return s3Turn{Request: request, Chunks: chunks, Stream: true, AuthID: "cred", Metadata: s3Meta("caller")}
	}
	_ = s3DriveTurn(t, service, turn(s3Relay(key, s3User("one")), s3CallChunks("r1", s3Transport(callID, "exec_command", map[string]any{"cmd": "first"}))))
	result := s3DriveTurn(t, service, turn(s3Relay(key, s3User("one"), s3User("two")), s3CallChunks("r2", s3Transport(callID, "exec_command", map[string]any{"cmd": "second"}))))
	if notes := s3HistoryNotes(result.logs); !reflect.DeepEqual(notes, []string{"relay_record_overwritten"}) {
		t.Fatalf("overwrite not logged: %v", notes)
	}
	// reject (rc2): within one strict scope a tombstone.
	strict := "s3-unit/tombstone"
	rememberRelayCallRejecting(strict, replayNativeCall(first), first)
	storeRelayRecord(replayCacheTarget{scope: strict, mode: historyConflictReject}, replayNativeCall(second), second, "relay")
	if rememberedNativeCall(strict, "s3-reused") != nil {
		t.Fatal("reject mode replayed one of two conflicting deliveries")
	}
}

// S3.3: native (Lite) records live in their own pool and never evict relay
// records; the relay pool keeps h22's capacity and lifetime.
func TestH25S3NativeRecordsNeverEvictRelayRecords(t *testing.T) {
	s3ResetCaches()
	defer s3ResetCaches()
	scope := "s3-unit/pools"
	relay := replayClientCall("s3-relay-kept", "Paris")
	rememberRelayCall(scope, replayNativeCall(relay), relay)
	for index := 0; index < nativeOriginCacheEntries+500; index++ {
		native := map[string]any{"type": "function_call", "name": "exec", "call_id": fmt.Sprintf("native-%d", index), "arguments": `{"cmd":"x"}`}
		rememberDeliveredNativeCall(scope, native)
	}
	if rememberedNativeCall(scope, "s3-relay-kept") == nil {
		t.Fatal("native records evicted a relay record")
	}
	nativeOriginCache.Lock()
	nativeCount := len(nativeOriginCache.items)
	nativeOriginCache.Unlock()
	if nativeCount != nativeOriginCacheEntries {
		t.Fatalf("native pool holds %d", nativeCount)
	}
	if _, state := cachedCallForHistory(scope, map[string]any{"type": "function_call", "name": "exec", "call_id": "native-0", "arguments": `{"cmd":"x"}`}); state != "miss" {
		t.Fatal("the native pool did not evict its own oldest record")
	}
	// The relay pool holds h22's 4096 entries of ordinary size.
	for index := 0; index < nativeCallCacheEntries-1; index++ {
		client := replayClientCall(fmt.Sprintf("relay-%d", index), "Paris")
		rememberRelayCall(scope, replayNativeCall(client), client)
	}
	if rememberedNativeCall(scope, "s3-relay-kept") == nil || rememberedNativeCall(scope, "relay-0") == nil {
		t.Fatal("relay pool is smaller than h22's")
	}
	client := replayClientCall("relay-overflow", "Paris")
	rememberRelayCall(scope, replayNativeCall(client), client)
	if rememberedNativeCall(scope, "s3-relay-kept") != nil || rememberedNativeCall(scope, "relay-overflow") == nil {
		t.Fatal("relay pool does not evict FIFO at h22's capacity")
	}
	// A large relay call (a 1.5 MB patch) is still recorded.
	big := map[string]any{"type": "custom_tool_call", "name": "apply_patch", "call_id": "relay-big", "input": strings.Repeat("+x\n", 500000)}
	bigNative := map[string]any{"type": "function_call", "name": transportName, "call_id": "relay-big",
		"arguments": string(jsonBytes(map[string]any{"code": string(jsonBytes(map[string]any{"tool": "apply_patch", "args": big["input"]}))}))}
	rememberRelayCall(scope, bigNative, big)
	if rememberedNativeCall(scope, "relay-big") == nil {
		t.Fatal("a large relay call was not recorded")
	}
}

func TestH25S3RelayLifetimeIsH22s(t *testing.T) {
	scope := "s3-unit/lifetime"
	client := replayClientCall("s3-life", "Paris")
	native := replayNativeCall(client)
	rememberRelayCall(scope, native, client)
	age := func(d time.Duration) {
		nativeCallCache.Lock()
		key := nativeCallCacheKey(scope, "s3-life")
		entry := nativeCallCache.items[key]
		entry.storedAt = time.Now().Add(-d)
		nativeCallCache.items[key] = entry
		nativeCallCache.Unlock()
	}
	age(119 * time.Minute)
	if rememberedNativeCall(scope, "s3-life") == nil {
		t.Fatal("expired before two hours")
	}
	// A hit does not extend it (h22).
	age(121 * time.Minute)
	if rememberedNativeCall(scope, "s3-life") != nil {
		t.Fatal("outlived two hours")
	}
	// A new delivery restarts it (h22).
	rememberRelayCall(scope, native, client)
	age(60 * time.Minute)
	rememberRelayCall(scope, native, client)
	age(90 * time.Minute)
	if rememberedNativeCall(scope, "s3-life") == nil {
		t.Fatal("a re-delivery did not restart the lifetime")
	}
}

// S3.4 (review encrypt-6/-7, replay-4, deepseek-harness): under reject, a
// call the cache cannot vouch for still takes h22's cold path, and an opaque
// result keeps its content while its type and id follow its call.
func TestH25S3RejectModeColdPathCorrections(t *testing.T) {
	allowed := clientToolSpecs(s3Relay("", s3User("x")))
	cold := []any{
		s3Call("s3c_badjson", "exec_command", `{"cmd":"dir C:\Users"}`), s3Output("s3c_badjson", "a"),
		s3Call("s3c_empty", "exec_command", ``), s3Output("s3c_empty", "b"),
		s3Call("s3c_dup", "exec_command", `{"cmd":"a","cmd":"b"}`), s3Output("s3c_dup", "c"),
		s3With(s3Call("s3c_cipher", "exec_command", `{"cmd":"x"}`), func(c map[string]any) { c["encrypted_function_args"] = "gAAAA" }), s3Output("s3c_cipher", "d"),
		s3Custom("s3c_custom", "apply_patch", "*** Begin Patch\n*** End Patch\n"),
		s3CustomOutput("s3c_custom", []any{map[string]any{"type": "input_text", "text": "child said"}, map[string]any{"type": "encrypted_content", "encrypted_content": "gAAAAfake"}}),
	}
	for _, translate := range []func(any, map[string]toolSpec, string) []any{translateInputItems, translateInputItemsReject} {
		got := translate(cold, allowed, "s3-unit/cold-corrections")
		for index := 0; index < len(got); index += 2 {
			call, output := objectValue(got[index]), objectValue(got[index+1])
			if call["name"] != transportName {
				t.Fatalf("cold call %v went upstream under its client name", call["call_id"])
			}
			if output["type"] != "function_call_output" || output["id"] != functionItemID(stringValue(call["call_id"])) {
				t.Fatalf("result of %v not paired with its run_officejs call: %v", call["call_id"], output)
			}
		}
		opaque := objectValue(got[len(got)-1])
		if !reflect.DeepEqual(opaque["output"], objectValue(cold[len(cold)-1])["output"]) {
			t.Fatal("an opaque result lost its content")
		}
	}
	// Opaque or unverifiable native calls keep their namespace (B5).
	decl := s3LiteDecl()
	for _, args := range []string{``, `{"id":"a","id":"b"}`} {
		history := []any{decl, s3Call("s3c_wait", "wait_agent", args), s3Output("s3c_wait", "ok")}
		history = append(history, s3With(s3Call("s3c_wait_cipher", "wait_agent", `{}`), func(c map[string]any) { c["encrypted_function_args"] = "gAAAA" }))
		for _, translate := range []func(any, map[string]toolSpec, string) []any{translateInputItems, translateInputItemsReject} {
			got := namespaceByCallID(translate(history, map[string]toolSpec{}, "s3-unit/b5"))
			if got["s3c_wait"] != "collab" || got["s3c_wait_cipher"] != "collab" {
				t.Fatalf("args=%q: namespace not restored: %v", args, got)
			}
		}
	}
	// An opaque result of a relay call that is replayed from the cache: type
	// and id follow the call, content kept (reject); h22's rule by default.
	scope := "s3-unit/opaque-output"
	client := map[string]any{"type": "custom_tool_call", "name": "apply_patch", "call_id": "s3c_relay_custom", "input": "patch"}
	native := map[string]any{"type": "function_call", "name": transportName, "call_id": "s3c_relay_custom", "arguments": string(jsonBytes(map[string]any{"code": `{"tool":"apply_patch","args":"patch"}`}))}
	rememberRelayCall(scope, native, client)
	parts := []any{map[string]any{"type": "encrypted_content", "encrypted_content": "gAAAAonly"}}
	history := []any{client, s3CustomOutput("s3c_relay_custom", parts)}
	rejected := objectValue(translateInputItemsReject(history, allowed, scope)[1])
	if rejected["type"] != "function_call_output" || rejected["id"] != functionItemID("s3c_relay_custom") || !reflect.DeepEqual(rejected["output"], parts) {
		t.Fatalf("reject: opaque result of a relay call not normalized with its content kept: %v", rejected)
	}
	replayed := objectValue(translateInputItems(history, allowed, scope)[1])
	if replayed["type"] != "function_call_output" || replayed["output"] != "(tool call succeeded with no output)" {
		t.Fatalf("replay: h22 normalized this result with its placeholder: %v", replayed)
	}
}

// relay_protocol: auto reads the history this request will actually send:
// under replay a rewritten call still replays its cached transport call.
func TestH25S3AutoSelectionFollowsTheReplayedHistory(t *testing.T) {
	native := h24ReferencedCall("s3_auto_refs", []any{"get_weather"}, `{"city":"Paris"}`)
	client := map[string]any{"type": "function_call", "name": "get_weather", "call_id": "s3_auto_refs", "arguments": `{"city":"Paris"}`}
	source := weatherRequest()
	source["prompt_cache_key"] = "s3-auto"
	rewritten := cloneObject(client)
	rewritten["arguments"] = `{"city":"Paris","units":"metric"}`
	source["input"] = []any{rewritten, s3Output("s3_auto_refs", "sunny")}
	scope := conversationScope(source)
	rememberRelayCall(scope, native, client)
	replay := withRelayOptions(source, relayOptions{Protocol: relayProtocolAuto, HistoryConflict: historyConflictReplay})
	if got := selectRelayProtocol(replay, relayProtocolAuto, scope); got != relayProtocolReferences {
		t.Fatalf("replay: selection ignored the replayed references call: %s", got)
	}
	reject := withRelayOptions(source, relayOptions{Protocol: relayProtocolAuto, HistoryConflict: historyConflictReject})
	if got := selectRelayProtocol(reject, relayProtocolAuto, scope); got != relayProtocolLegacy {
		t.Fatalf("reject: an unverified call must keep legacy: %s", got)
	}
	translated := translateInputItems(source["input"], clientToolSpecs(source), scope)
	if !reflect.DeepEqual(translated[0], native) {
		t.Fatal("replay did not send the cached references call")
	}
}

// I4: history_conflict is frozen at request entry like every switch.
func TestH25S3HistoryModeFrozenAtRequestEntry(t *testing.T) {
	service := s3ServiceWith(s3Reject)
	source := s3Relay("s3-frozen-mode", s3User("x"))
	req := ExecutorRequest{AuthID: "cred", AuthProvider: Provider, Model: "gpt-6-astra", Metadata: s3Meta("caller"),
		OriginalRequest: jsonBytes(source), Payload: jsonBytes(source), StorageJSON: streamTestCredential()}
	if _, _, _, err := service.prepareRequestWithRelayContext(&req); err != nil {
		t.Fatal(err)
	}
	service.mu.Lock()
	service.cfg.HistoryConflict = historyConflictReplay
	service.mu.Unlock()
	response := withRelayOptions(withFrozenReplayScope(source, req), service.relayOptionsFor(req))
	if relayOptionsFrom(response).HistoryConflict != historyConflictReject || replayTargetFor(response).mode != historyConflictReject {
		t.Fatal("the response path did not use the request's frozen history_conflict")
	}
}

// Review replay-1/-2/-7 (wf/replay TestWFReplayScenarios, ported): 27
// conversations, each continued for three turns after the client rewrote
// (or did not rewrite) a delivered call. rc2 answered nine of them with a
// 400 on every turn. Default: none is refused and every delivered relay
// call replays as the model's own transport call. reject: exactly rc2's nine.
func TestH25S3ReplayMatrixFromReview(t *testing.T) {
	type options struct {
		session, cmSession, header, auth string
	}
	scoped := options{session: "sess-1"}
	setArgs := func(f func(string) string) func(map[string]any) map[string]any {
		return func(item map[string]any) map[string]any {
			item["arguments"] = f(stringValue(item["arguments"]))
			return item
		}
	}
	setInput := func(f func(string) string) func(map[string]any) map[string]any {
		return func(item map[string]any) map[string]any { item["input"] = f(stringValue(item["input"])); return item }
	}
	same := func(item map[string]any) map[string]any { return item }
	scenarios := []struct {
		name       string
		kind       string
		first      options
		later      *options
		mutate     func(map[string]any) map[string]any
		rc2Refuses bool
	}{
		{"exact echo", "fn", scoped, nil, same, false},
		{"exact echo client_metadata session", "fn", options{cmSession: "sess-1"}, nil, same, false},
		{"keys reordered and whitespace", "fn", scoped, nil, setArgs(func(a string) string {
			return `{ "n": 1000, "city": "Paris", "big": 12345678901234567890 }`
		}), false},
		{"1000 written as 1e3", "fn", scoped, nil, setArgs(func(a string) string { return strings.Replace(a, "1000", "1e3", 1) }), false},
		{"unicode escaped", "fn", scoped, nil, setArgs(func(a string) string { return strings.Replace(a, "Paris", `Paris`, 1) }), false},
		{"bigint precision loss", "fn", scoped, nil, setArgs(func(a string) string {
			return strings.Replace(a, "12345678901234567890", "12345678901234567000", 1)
		}), true},
		{"client adds a key", "fn", scoped, nil, setArgs(func(a string) string { return strings.Replace(a, "{", `{"description":"added by client",`, 1) }), true},
		{"id dropped", "fn", scoped, nil, func(i map[string]any) map[string]any { delete(i, "id"); return i }, false},
		{"id re-minted", "fn", scoped, nil, func(i map[string]any) map[string]any { i["id"] = "fc_client_minted"; return i }, true},
		{"status dropped", "fn", scoped, nil, func(i map[string]any) map[string]any { delete(i, "status"); return i }, false},
		{"user edit", "fn", scoped, nil, setArgs(func(a string) string { return strings.Replace(a, "Paris", "Oslo", 1) }), true},
		{"compaction keeps only the output", "fn", scoped, nil, nil, false},
		{"no session, edited", "fn", options{}, nil, setArgs(func(a string) string { return strings.Replace(a, "Paris", "Oslo", 1) }), false},
		{"no session, exact", "fn", options{}, nil, same, false},
		{"hyphen Session-Id header", "fn", options{session: "sess-1", header: "Session-Id"}, nil, same, false},
		{"credential switch, exact", "fn", scoped, &options{session: "sess-1", auth: "cred-B"}, same, false},
		{"credential switch, edited", "fn", scoped, &options{session: "sess-1", auth: "cred-B"}, setArgs(func(a string) string { return strings.Replace(a, "Paris", "Oslo", 1) }), false},
		{"custom exact", "custom", scoped, nil, same, false},
		{"custom trailing newline stripped", "custom", scoped, nil, setInput(func(s string) string { return strings.TrimRight(s, "\n") }), true},
		{"custom CRLF", "custom", scoped, nil, setInput(func(s string) string { return strings.ReplaceAll(s, "\n", "\r\n") }), true},
		{"custom sent back as function", "custom", scoped, nil, func(i map[string]any) map[string]any {
			i["type"] = "function_call"
			i["arguments"] = string(jsonBytes(map[string]any{"input": i["input"]}))
			delete(i, "input")
			return i
		}, true},
		{"namespace exact", "ns", scoped, nil, same, false},
		{"namespace omitted", "ns", scoped, nil, func(i map[string]any) map[string]any { delete(i, "namespace"); return i }, false},
		{"namespace flattened", "ns", scoped, nil, func(i map[string]any) map[string]any {
			i["name"] = "mcp__srv__lookup"
			delete(i, "namespace")
			return i
		}, true},
		{"namespace, no session", "ns", options{}, nil, same, false},
		{"lite exact", "lite", scoped, nil, same, false},
		{"lite args edited", "lite", scoped, nil, setArgs(func(a string) string { return strings.Replace(a, "ls -la", "ls", 1) }), true},
	}
	patch := "*** Begin Patch\n*** Add File: a.txt\n+hello\n*** End Patch\n"
	request := func(body map[string]any, o options) s3Turn {
		if o.cmSession != "" {
			body["client_metadata"] = map[string]any{"session_id": o.cmSession}
		}
		var headers http.Header
		if o.session != "" {
			name := o.header
			if name == "" {
				name = "Session_id"
			}
			headers = http.Header{name: []string{o.session}}
		}
		auth := o.auth
		if auth == "" {
			auth = "cred-A"
		}
		return s3Turn{Request: body, Stream: true, AuthID: auth, Metadata: s3Meta("caller-main-site"), Headers: headers}
	}
	refusals := map[string]int{}
	for _, mode := range []struct {
		name      string
		configure func(*Config)
	}{{"default", nil}, {"reject", s3Reject}} {
		for _, sc := range scenarios {
			s3ResetCaches()
			service := s3ServiceWith(mode.configure)
			callID := "call_matrix_" + strings.ReplaceAll(strings.ReplaceAll(sc.name, " ", "_"), ",", "")
			key := "matrix-" + callID
			var native, delivered map[string]any
			lite := sc.kind == "lite"
			switch sc.kind {
			case "fn":
				native = s3Transport(callID, "get_weather", map[string]any{"city": "Paris", "n": json.Number("1000"), "big": json.Number("12345678901234567890")})
				delivered = s3Call(callID, "get_weather", `{"big":12345678901234567890,"city":"Paris","n":1000}`)
			case "custom":
				native = s3Transport(callID, "apply_patch", patch)
				delivered = map[string]any{"type": "custom_tool_call", "id": "ctc_" + callID, "call_id": callID, "name": "apply_patch", "input": patch}
			case "ns":
				native = s3Transport(callID, "mcp__srv.lookup", map[string]any{"q": "x"})
				delivered = s3With(s3Call(callID, "lookup", `{"q":"x"}`), func(c map[string]any) { c["namespace"] = "mcp__srv" })
			case "lite":
				native = map[string]any{"type": "function_call", "id": "fc_" + callID, "call_id": callID, "name": "exec_command", "namespace": "shell", "status": "completed", "arguments": `{"cmd":"ls -la"}`}
				delivered = s3With(native, func(c map[string]any) { delete(c, "status") })
			}
			opener := []any{s3User("hi " + sc.name)}
			if lite {
				opener = []any{s3LiteDecl(), s3User("hi " + sc.name)}
			}
			body := func(input []any) map[string]any {
				if lite {
					return s3Lite(key, input...)
				}
				return s3Relay(key, input...)
			}
			first := request(body(opener), sc.first)
			first.Chunks = s3CallChunks("rm1", native)
			if result := s3DriveTurn(t, service, first); result.closeErr != "" || result.status != 0 {
				t.Fatalf("%s: delivery failed: %q", sc.name, result.closeErr)
			}
			later := sc.first
			if sc.later != nil {
				later = *sc.later
			}
			history := append([]any{}, opener...)
			if sc.mutate != nil {
				history = append(history, sc.mutate(cloneObject(delivered)))
			}
			if stringValue(delivered["type"]) == "custom_tool_call" {
				history = append(history, s3CustomOutput(callID, "Done"))
			} else {
				history = append(history, s3Output(callID, "sunny"))
			}
			for turnIndex := 0; turnIndex < 3; turnIndex++ {
				history = append(history, s3User(fmt.Sprintf("next %d", turnIndex)))
				turn := request(body(append([]any{}, history...)), later)
				turn.Chunks = s3TextChunks("rm2")
				result := s3DriveTurn(t, service, turn)
				if result.status == 400 && result.kind == "history_conflict" {
					refusals[mode.name+"/"+sc.name]++
					continue
				}
				if result.status != 0 || result.upstreamCalls != 1 {
					t.Fatalf("%s/%s turn %d failed: %d %s", mode.name, sc.name, turnIndex, result.status, result.kind)
				}
				if mode.name == "default" && !lite && sc.mutate != nil {
					call := s3UpstreamCall(result.upstream, callID)
					if call["name"] != transportName || call["arguments"] != native["arguments"] {
						t.Fatalf("%s turn %d: not replayed warm: %v", sc.name, turnIndex, call)
					}
				}
				if mode.name == "default" && sc.mutate == nil {
					output := s3UpstreamItem(result.upstream, callID, "function_call_output")
					if output == nil || output["id"] != functionItemID(callID) {
						t.Fatalf("%s: output-only continuation not paired: %v", sc.name, output)
					}
				}
			}
		}
	}
	for _, sc := range scenarios {
		if got := refusals["default/"+sc.name]; got != 0 {
			t.Errorf("default refused %s %d times", sc.name, got)
		}
		want := 0
		if sc.rc2Refuses {
			want = 3
		}
		if got := refusals["reject/"+sc.name]; got != want {
			t.Errorf("reject refused %s %d times, rc2 refused %d", sc.name, got, want)
		}
	}
}
