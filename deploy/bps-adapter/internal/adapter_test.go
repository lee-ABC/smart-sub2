package adapter

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

const testKey = "test-private-transport-key-01234567890123456789"

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func testRoundTripper(t *testing.T) *utlsRoundTripper {
	t.Helper()
	rt, err := newUtlsRoundTripper("")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(rt.closeIdleConnections)
	return rt
}

var testPool = newTransportPool()

func testCachedTransport(id, proxy string) *utlsRoundTripper {
	t, err := testPool.get(id, proxy)
	if err != nil {
		panic(err)
	}
	return t.(*utlsRoundTripper)
}
func testToken(client, workspace, user string) string {
	p, _ := json.Marshal(map[string]any{"client_id": client, "sub": "test-user", "exp": 4102444800, "https://api.openai.com/auth": map[string]any{"chatgpt_account_id": workspace, "chatgpt_user_id": user, "chatgpt_account_user_id": user + "__" + workspace}})
	return "e30." + base64.RawURLEncoding.EncodeToString(p) + ".synthetic"
}
func testRequest(body string) *http.Request {
	r := httptest.NewRequest("POST", "/internal/v1/responses", strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+testKey)
	r.Header.Set("X-Sub2-Excel-Access-Token", testToken(ClientID, "workspace", "user"))
	r.Header.Set("X-Sub2-Excel-Account-Id", "workspace")
	r.Header.Set("X-Sub2-Excel-Account-User-Id", "user")
	r.Header.Set("X-Sub2-Excel-Credential-Id", "11")
	r.Header.Set("X-Sub2-Excel-Scope", "selected-caller-scope-111")
	return r
}
func newTestAdapter(t *testing.T, fn roundTripFunc) *Adapter {
	t.Helper()
	a, err := New(testKey, nil)
	if err != nil {
		t.Fatal(err)
	}
	a.transport = func(string, string) (http.RoundTripper, error) { return fn, nil }
	t.Cleanup(a.Shutdown)
	return a
}
func finalResponse(model, text string) string {
	p, _ := json.Marshal(map[string]any{"id": "resp_test", "object": "response", "status": "completed", "model": model, "output": []any{map[string]any{"type": "message", "id": "msg_test", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": text}}}}, "usage": map[string]int{"input_tokens": 9, "output_tokens": 3, "total_tokens": 12}})
	return string(p)
}
func mockResponse(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(body))}
}

func TestPrivateTransportAndIdentityGuards(t *testing.T) {
	a := newTestAdapter(t, func(*http.Request) (*http.Response, error) {
		t.Fatal("invalid request reached upstream")
		return nil, nil
	})
	cases := []struct {
		name, header, value string
		status              int
	}{{"private key", "Authorization", "Bearer invalid", 401}, {"native token", "X-Sub2-Excel-Access-Token", testToken("native-client", "workspace", "user"), 401}, {"workspace", "X-Sub2-Excel-Account-Id", "other", 401}, {"user", "X-Sub2-Excel-Account-User-Id", "other", 401}, {"missing selection", "X-Sub2-Excel-Credential-Id", "", 400}, {"missing scope", "X-Sub2-Excel-Scope", "", 400}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := testRequest(`{"model":"gpt-6-astra","input":"hi"}`)
			r.Header.Set(tc.header, tc.value)
			w := httptest.NewRecorder()
			a.ServeHTTP(w, r)
			if w.Code != tc.status {
				t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
			}
			if strings.Contains(w.Body.String(), r.Header.Get("X-Sub2-Excel-Access-Token")) {
				t.Fatal("token exposed")
			}
		})
	}
	for _, model := range []string{"gpt-5.5", "other"} {
		w := httptest.NewRecorder()
		a.ServeHTTP(w, testRequest(`{"model":"`+model+`","input":"hi"}`))
		if w.Code != 400 {
			t.Fatalf("unsupported model status=%d", w.Code)
		}
	}
}

func TestAllCanonicalAndExcelModelsPassThroughExplicitly(t *testing.T) {
	for _, model := range models {
		t.Run(model, func(t *testing.T) {
			want := model
			if aliases[model] != "" {
				want = aliases[model]
			}
			a := newTestAdapter(t, func(r *http.Request) (*http.Response, error) {
				if r.URL.String() != "https://bps.openai.com/basispoints/api/responses" {
					t.Fatalf("unexpected destination %s", r.URL)
				}
				if r.Header.Get("Authorization") != "Bearer "+testToken(ClientID, "workspace", "user") {
					t.Fatal("selected BPS bearer not forwarded")
				}
				if r.Header.Get("X-Sub2-Excel-Credential-Id") != "" || strings.Contains(r.Header.Get("Authorization"), testKey) {
					t.Fatal("private headers leaked")
				}
				var body map[string]any
				_ = json.NewDecoder(r.Body).Decode(&body)
				if body["model"] != want {
					t.Fatalf("model=%v want=%s", body["model"], want)
				}
				return mockResponse(200, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":"+finalResponse(want, "OK")+"}\n\ndata: [DONE]\n\n"), nil
			})
			w := httptest.NewRecorder()
			a.ServeHTTP(w, testRequest(`{"model":"`+model+`","input":"hi"}`))
			if w.Code != 200 || !strings.Contains(w.Body.String(), "OK") || w.Header().Get("Content-Type") != "application/json" {
				t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
			}
		})
	}
}

func TestStreamingEmitsAndClearsRequestState(t *testing.T) {
	sse := "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"output_index\":0,\"item_id\":\"msg_test\",\"content_index\":0,\"delta\":\"hello\"}\n\n" + "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":" + finalResponse("gpt-6-astra", "hello") + "}\n\ndata: [DONE]\n\n"
	a := newTestAdapter(t, func(*http.Request) (*http.Response, error) { return mockResponse(200, sse), nil })
	w := httptest.NewRecorder()
	a.ServeHTTP(w, testRequest(`{"model":"gpt-6-astra","input":"hi","stream":true}`))
	if w.Code != 200 || !w.Flushed || !strings.Contains(w.Body.String(), "response.completed") || !strings.Contains(w.Body.String(), "[DONE]") {
		t.Fatalf("bad SSE %d %s", w.Code, w.Body.String())
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.calls) != 0 || len(a.streams) != 0 {
		t.Fatal("request resources leaked")
	}
}

func TestUpstreamRedirectNeverForwardsBearer(t *testing.T) {
	count := 0
	a := newTestAdapter(t, func(*http.Request) (*http.Response, error) {
		count++
		r := mockResponse(302, `{"error":{"message":"redirect"}}`)
		r.Header.Set("Location", "https://not-bps.invalid/stolen")
		return r, nil
	})
	w := httptest.NewRecorder()
	a.ServeHTTP(w, testRequest(`{"model":"gpt-6-astra","input":"hi"}`))
	if count != 1 || w.Code == 200 {
		t.Fatalf("redirect followed or successful: count=%d status=%d", count, w.Code)
	}
}

func TestHostCapabilitiesAndDestinationsFailClosed(t *testing.T) {
	a := newTestAdapter(t, func(*http.Request) (*http.Response, error) {
		t.Fatal("unapproved destination reached network")
		return nil, nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a.calls["known"] = &invocation{ctx: ctx, cancel: cancel, client: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) { t.Fatal("unexpected network"); return nil, nil })}}
	for _, target := range []string{"https://auth.openai.com/oauth/token", "https://example.com/basispoints/api/responses", "http://bps.openai.com/basispoints/api/responses", "https://bps.openai.com:443/basispoints/api/responses", "https://bps.openai.com/basispoints/api/user"} {
		err := a.host("host.http.do", map[string]any{"host_callback_id": "known", "method": "POST", "url": target}, nil)
		if err == nil {
			t.Fatalf("destination allowed: %s", target)
		}
	}
	for _, method := range []string{"host.auth.get", "host.auth.list", "auth.refresh", "host.unknown"} {
		if a.host(method, map[string]any{}, nil) == nil {
			t.Fatalf("unexpected capability %s", method)
		}
	}
}

type cancelBody struct {
	ctx    context.Context
	closed chan struct{}
	once   sync.Once
}

func (b *cancelBody) Read(p []byte) (int, error) {
	select {
	case <-b.ctx.Done():
		return 0, b.ctx.Err()
	case <-b.closed:
		return 0, io.EOF
	}
}
func (b *cancelBody) Close() error { b.once.Do(func() { close(b.closed) }); return nil }
func TestClientCancellationClosesUpstream(t *testing.T) {
	started := make(chan *cancelBody, 1)
	a := newTestAdapter(t, func(r *http.Request) (*http.Response, error) {
		b := &cancelBody{ctx: r.Context(), closed: make(chan struct{})}
		started <- b
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: b}, nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	r := testRequest(`{"model":"gpt-6-astra","input":"hi","stream":true}`).WithContext(ctx)
	done := make(chan struct{})
	go func() { defer close(done); a.ServeHTTP(httptest.NewRecorder(), r) }()
	body := <-started
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("handler ignored cancellation")
	}
	select {
	case <-body.closed:
	case <-time.After(time.Second):
		t.Fatal("upstream body not closed")
	}
}

func TestToolRoundtripKeepsSelectedScope(t *testing.T) {
	arguments := `{"summary":"Read value","code":"{\"tool\":\"read_value\",\"args\":{\"cell\":\"A1\"}}"}`
	native := map[string]any{"type": "function_call", "id": "fc_tool", "call_id": "call_tool", "name": "run_officejs", "status": "completed", "arguments": arguments}
	nativeResponse, _ := json.Marshal(map[string]any{"id": "resp_tool", "status": "completed", "output": []any{native}})
	var seen []map[string]any
	a := newTestAdapter(t, func(r *http.Request) (*http.Response, error) {
		var b map[string]any
		if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
			t.Fatal(err)
		}
		seen = append(seen, b)
		if len(seen) == 1 {
			return mockResponse(200, string(nativeResponse)), nil
		}
		return mockResponse(200, finalResponse("gpt-6-astra", "done")), nil
	})
	source := map[string]any{"model": "gpt-6-astra", "input": []any{map[string]any{"role": "user", "content": "read A1"}}, "tools": []any{map[string]any{"type": "function", "name": "read_value", "parameters": map[string]any{"type": "object", "properties": map[string]any{"cell": map[string]any{"type": "string"}}, "required": []any{"cell"}}}}}
	raw, _ := json.Marshal(source)
	w := httptest.NewRecorder()
	a.ServeHTTP(w, testRequest(string(raw)))
	if w.Code != 200 || strings.Contains(w.Body.String(), "run_officejs") || !strings.Contains(w.Body.String(), "read_value") {
		t.Fatalf("tool decode failed %d %s", w.Code, w.Body.String())
	}
	var response map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &response)
	source["input"] = append(source["input"].([]any), response["output"].([]any)[0], map[string]any{"type": "function_call_output", "call_id": "call_tool", "output": "42"})
	raw, _ = json.Marshal(source)
	next := httptest.NewRecorder()
	a.ServeHTTP(next, testRequest(string(raw)))
	if next.Code != 200 || len(seen) != 2 {
		t.Fatalf("tool return failed %d %s", next.Code, next.Body.String())
	}
	replay, _ := json.Marshal(seen[1]["input"])
	if !bytes.Contains(replay, []byte("run_officejs")) || !bytes.Contains(replay, []byte("42")) {
		t.Fatalf("tool replay missing native call/result: %s", replay)
	}
}

func TestTransportPoolIsolatesCredentialsAndProxy(t *testing.T) {
	p := newTransportPool()
	defer p.close()
	a, err := p.get("one", "")
	if err != nil {
		t.Fatal(err)
	}
	again, _ := p.get("one", "")
	other, _ := p.get("two", "")
	proxied, _ := p.get("one", "http://127.0.0.1:10809")
	if a != again || a == other || a == proxied {
		t.Fatal("transport pooling crossed identity/proxy boundary")
	}
	for _, bad := range []string{"broken", "ftp://localhost:1", "http://localhost:1/path", "http://localhost:1?secret=a"} {
		if _, err = p.get("one", bad); err == nil {
			t.Fatalf("bad proxy accepted %s", bad)
		}
	}
	p.close()
	if _, err = p.get("one", ""); err == nil {
		t.Fatal("closed pool accepted request")
	}
}

// A connected upstream can stall after sending HTTP 200. The adapter must end
// the response protocol explicitly instead of leaving Sub2 with a bare EOF.
func TestStreamingDeadlineHasFailedTerminalAndDoesNotReplay(t *testing.T) {
	var attempts int
	a := newTestAdapter(t, func(r *http.Request) (*http.Response, error) {
		attempts++
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: &cancelBody{ctx: r.Context(), closed: make(chan struct{})}}, nil
	})
	a.requestTimeout = 30 * time.Millisecond
	w := httptest.NewRecorder()
	a.ServeHTTP(w, testRequest(`{"model":"gpt-6-astra","input":"hi","stream":true}`))
	body := w.Body.String()
	if !strings.Contains(body, `"type":"response.failed"`) || !strings.Contains(body, `"code":"upstream_timeout"`) || !strings.Contains(body, "data: [DONE]") {
		t.Fatalf("deadline lost its protocol terminal: %s", body)
	}
	if strings.Contains(body, `"type":"response.completed"`) || attempts != 1 {
		t.Fatalf("deadline must not fabricate success or replay, attempts=%d body=%s", attempts, body)
	}
}

func TestStreamingBrokenBodyHasFailedTerminal(t *testing.T) {
	a := newTestAdapter(t, func(*http.Request) (*http.Response, error) {
		return mockResponse(200, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_broken\",\"status\":\"in_progress\"}}\n\n"), nil
	})
	w := httptest.NewRecorder()
	a.ServeHTTP(w, testRequest(`{"model":"gpt-6-astra","input":"hi","stream":true}`))
	if !strings.Contains(w.Body.String(), `"type":"response.failed"`) || !strings.Contains(w.Body.String(), "data: [DONE]") {
		t.Fatalf("broken stream has no failed terminal: %s", w.Body.String())
	}
	if strings.Contains(w.Body.String(), `"type":"response.completed"`) {
		t.Fatal("broken stream reported success")
	}
}

type finalChunkErrorBody struct {
	raw  []byte
	used bool
}

func (b *finalChunkErrorBody) Read(p []byte) (int, error) {
	if b.used {
		return 0, io.EOF
	}
	b.used = true
	return copy(p, b.raw), io.ErrUnexpectedEOF
}
func (*finalChunkErrorBody) Close() error { return nil }

func TestStreamingTerminalBytesSurviveAccompanyingReadError(t *testing.T) {
	sse := "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":" + finalResponse("gpt-6-astra", "OK") + "}\n\n"
	a := newTestAdapter(t, func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: &finalChunkErrorBody{raw: []byte(sse)}}, nil
	})
	w := httptest.NewRecorder()
	a.ServeHTTP(w, testRequest(`{"model":"gpt-6-astra","input":"hi","stream":true}`))
	if !strings.Contains(w.Body.String(), `"type":"response.completed"`) || strings.Contains(w.Body.String(), `"type":"response.failed"`) {
		t.Fatalf("valid terminal discarded with read error: %s", w.Body.String())
	}
}

func TestQueuedCompletedWinsDeadline(t *testing.T) {
	a := newTestAdapter(t, func(*http.Request) (*http.Response, error) { t.Fatal("must not replay"); return nil, nil })
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	call := &invocation{ctx: ctx, started: time.Now(), requestedModel: "gpt-6-astra", events: make(chan message, 2)}
	call.events <- message{body: []byte("event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":" + finalResponse("gpt-6-astra", "OK") + "}\n\ndata: [DONE]\n\n")}
	call.events <- message{closed: true}
	w := httptest.NewRecorder()
	a.relayClientStream(w, testRequest(`{}`), call, "test", "synthetic-token")
	if !strings.Contains(w.Body.String(), "response.completed") || strings.Contains(w.Body.String(), "response.failed") {
		t.Fatal(w.Body.String())
	}
}

func TestCancelledClientDoesNotReceiveSyntheticFailure(t *testing.T) {
	a := newTestAdapter(t, func(*http.Request) (*http.Response, error) { t.Fatal("must not replay"); return nil, nil })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	call := &invocation{ctx: ctx, started: time.Now(), events: make(chan message, 1)}
	w := httptest.NewRecorder()
	a.relayClientStream(w, testRequest(`{}`).WithContext(ctx), call, "test", "synthetic-token")
	if w.Body.Len() != 0 {
		t.Fatal(w.Body.String())
	}
}
