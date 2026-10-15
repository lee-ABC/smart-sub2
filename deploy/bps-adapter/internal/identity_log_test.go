package adapter

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestThreadIdentityLogsActualWireIDsWithoutCredentials(t *testing.T) {
	a, err := New(testKey, []byte("client_identity:\n  mode: thread\n  pepper: stable-synthetic-test-pepper\n"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Shutdown)
	var logs bytes.Buffer
	a.SetIdentityLogger(log.New(&logs, "", 0))
	var wire []map[string]any
	a.transport = func(string, string) (http.RoundTripper, error) {
		return roundTripFunc(func(r *http.Request) (*http.Response, error) {
			var b map[string]any
			if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
				t.Fatal(err)
			}
			wire = append(wire, b)
			return mockResponse(200, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":"+finalResponse("gpt-6-astra", "done")+"}\n\ndata: [DONE]\n\n"), nil
		}), nil
	}
	user := map[string]any{"role": "user", "content": "PRIVATE-PROMPT-CANARY"}
	call := map[string]any{"type": "function_call", "call_id": "call-a", "name": "read_value", "arguments": "{}"}
	output := map[string]any{"type": "function_call_output", "call_id": "call-a", "output": "PRIVATE-TOOL-OUTPUT"}
	send := func(id, scope, session string, input []any) {
		b, _ := json.Marshal(map[string]any{"model": "gpt-6-astra", "input": input, "prompt_cache_key": session, "tools": []any{map[string]any{"type": "function", "name": "read_value", "parameters": map[string]any{"type": "object", "properties": map[string]any{}}}}})
		r := testRequest(string(b))
		r.Header.Set("X-Sub2-Excel-Credential-Id", id)
		r.Header.Set("X-Sub2-Excel-Scope", scope)
		r.Header.Set("Cookie", "PRIVATE-COOKIE-CANARY")
		w := httptest.NewRecorder()
		a.ServeHTTP(w, r)
		if w.Code != 200 {
			t.Fatalf("request failed HTTP%d", w.Code)
		}
	}
	send("11", "caller-scope-11111111", "session-one", []any{user})
	send("11", "caller-scope-11111111", "session-one", []any{user, call, output})
	send("11", "caller-scope-11111111", "session-one", []any{user, call, output, map[string]any{"role": "user", "content": "NEXT-PRIVATE-TURN"}})
	send("11", "caller-scope-11111111", "session-two", []any{map[string]any{"role": "user", "content": "different conversation"}})
	send("12", "caller-scope-11111111", "session-one", []any{user})
	send("11", "caller-scope-22222222", "session-one", []any{user})
	if len(wire) != 6 {
		t.Fatalf("wire count=%d", len(wire))
	}
	meta := func(n int) map[string]any { return wire[n]["metadata"].(map[string]any) }
	for _, n := range []int{1, 2, 3, 5} {
		if meta(n)["task_id"] != meta(0)["task_id"] {
			t.Fatal("same credential task_id changed")
		}
	}
	if meta(4)["task_id"] == meta(0)["task_id"] {
		t.Fatal("different credentials share task_id")
	}
	if meta(1)["turn_id"] != meta(0)["turn_id"] || meta(1)["agent_iteration"] != "2" {
		t.Fatal("tool loop turn/iteration incorrect")
	}
	if meta(2)["turn_id"] == meta(0)["turn_id"] || meta(2)["agent_iteration"] != "1" {
		t.Fatal("new user turn did not change")
	}
	if meta(5)["turn_id"] == meta(0)["turn_id"] {
		t.Fatal("caller isolation missing")
	}
	lines := strings.Split(strings.TrimSpace(logs.String()), "\n")
	if len(lines) != 12 {
		t.Fatalf("log count=%d", len(lines))
	}
	for i, line := range lines {
		var record identityLogRecord
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatal(err)
		}
		if record.TaskID != meta(i / 2)["task_id"] || record.TurnID != meta(i / 2)["turn_id"] || record.ConfiguredMode != "thread" {
			t.Fatal("log does not reflect wire metadata")
		}
		if i%2 == 1 && record.HTTPStatus != 200 {
			t.Fatal("missing upstream HTTP status")
		}
	}
	for _, secret := range []string{testKey, testToken(ClientID, "workspace", "user"), "PRIVATE-PROMPT-CANARY", "PRIVATE-TOOL-OUTPUT", "PRIVATE-COOKIE-CANARY", "NEXT-PRIVATE-TURN", "stable-synthetic-test-pepper"} {
		if strings.Contains(logs.String(), secret) {
			t.Fatal("secret appeared in identity logs")
		}
	}
	health := httptest.NewRecorder()
	a.ServeHTTP(health, httptest.NewRequest("GET", "/healthz", nil))
	if !strings.Contains(health.Body.String(), `"client_identity_mode":"thread"`) || !strings.Contains(health.Body.String(), `"identity_logging":true`) {
		t.Fatal("missing runtime diagnostics")
	}
}

func TestIdentityLog403AndMalformedFieldsAreSafe(t *testing.T) {
	a, err := New(testKey, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Shutdown)
	var logs bytes.Buffer
	a.SetIdentityLogger(log.New(&logs, "", 0))
	a.transport = func(string, string) (http.RoundTripper, error) {
		return roundTripFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 403, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"error":{"message":"denied"}}`))}, nil
		}), nil
	}
	w := httptest.NewRecorder()
	a.ServeHTTP(w, testRequest(`{"model":"gpt-6-astra","input":"SECRET-PROMPT","stream":true}`))
	if w.Code != 403 {
		t.Fatalf("status=%d", w.Code)
	}
	if !strings.Contains(logs.String(), `"http_status":403`) || strings.Contains(logs.String(), "SECRET-PROMPT") {
		t.Fatal("HTTP failure diagnostics missing or unsafe")
	}
	record := a.identityTrace(&invocation{accountID: 1, requestedModel: "gpt-6-astra"}, "local-generated-id", []byte(`{"model":"SECRET-MODEL","metadata":{"task_id":"SECRET-ID","turn_id":"SECRET-ID","agent_iteration":"SECRET-ITERATION"}}`))
	if record.TaskID != "" || record.TurnID != "" || record.UpstreamModel != "" || record.AgentIteration != 0 {
		t.Fatal("unvalidated strings retained")
	}
}
