package adapter

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/net/websocket"
)

type wireCapture struct {
	mu     sync.Mutex
	bodies []map[string]any
}

func (c *wireCapture) add(b map[string]any) {
	c.mu.Lock()
	c.bodies = append(c.bodies, b)
	c.mu.Unlock()
}
func (c *wireCapture) snapshot() []map[string]any {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]map[string]any(nil), c.bodies...)
}

func useMockWebSocket(t *testing.T, a *Adapter, capture *wireCapture, respond func(*websocket.Conn, map[string]any)) {
	t.Helper()
	server := httptest.NewServer(websocket.Server{
		Handshake: func(cfg *websocket.Config, r *http.Request) error {
			if !strings.Contains(r.Header.Get("Sec-Websocket-Protocol"), "openai-bearer.") {
				return errors.New("missing selected bearer")
			}
			cfg.Protocol = []string{"responses"}
			return nil
		},
		Handler: func(ws *websocket.Conn) {
			defer ws.Close()
			var b map[string]any
			if err := websocket.JSON.Receive(ws, &b); err != nil {
				return
			}
			capture.add(b)
			if respond != nil {
				respond(ws, b)
				return
			}
			_ = websocket.JSON.Send(ws, map[string]any{"type": "basispoints.response.resume_token", "request_id": b["basispoints_request_id"], "resume_token": b["basispoints_resume_token"]})
			_ = websocket.JSON.Send(ws, map[string]any{"type": "basispoints.response.metadata", "headers": map[string]string{"X-OpenAI-Internal-Basispoints-Tools-Version-Id": "test-tools"}})
			_ = websocket.JSON.Send(ws, map[string]any{"type": "basispoints.response.upstream_sent"})
			_ = websocket.JSON.Send(ws, map[string]any{"type": "basispoints.response.heartbeat"})
			var response map[string]any
			_ = json.Unmarshal([]byte(finalResponse(b["model"].(string), "OK")), &response)
			response["metadata"] = b["metadata"]
			_ = websocket.JSON.Send(ws, map[string]any{"type": "response.completed", "response": response})
		},
	})
	t.Cleanup(server.Close)
	a.wsDial = func(ctx context.Context, cfg *websocket.Config, _ http.RoundTripper) (*websocket.Conn, int, http.Header, error) {
		if cfg.Location.Scheme != "wss" || cfg.Location.Host != "bps.openai.com" || cfg.Location.Path != "/basispoints/api/responses" {
			t.Error("unexpected upstream destination")
		}
		q := cfg.Location.Query()
		if q.Get("bps_auth_mode") != "chatgpt" || !strings.HasPrefix(q.Get("bps_ws_affinity"), "affinity_") {
			t.Error("missing WS query context")
		}
		if cfg.Header.Get("Authorization") != "" {
			t.Error("bearer repeated in generic headers")
		}
		conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", strings.TrimPrefix(server.URL, "http://"))
		if err != nil {
			return nil, 0, nil, err
		}
		_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
		ws, err := websocket.NewClient(cfg, conn)
		if err != nil {
			conn.Close()
			return nil, 0, nil, err
		}
		return ws, 101, http.Header{}, nil
	}
}
func threadAdapter(t *testing.T) *Adapter {
	t.Helper()
	a, err := New(testKey, []byte("client_identity:\n  mode: thread\n  pepper: synthetic-ws-test-pepper\n"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Shutdown)
	return a
}

func TestWebSocketModelEffortMatrixPreservesCallerValues(t *testing.T) {
	for _, model := range []string{"gpt-6-sol", "gpt-6-astra", "gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.6-luna"} {
		for _, effort := range []string{"low", "medium", "high", "xhigh"} {
			for _, nested := range []bool{true, false} {
				t.Run(model+"/"+effort+map[bool]string{true: "/nested", false: "/flat"}[nested], func(t *testing.T) {
					a := threadAdapter(t)
					capture := &wireCapture{}
					useMockWebSocket(t, a, capture, nil)
					raw := map[string]any{"model": model, "input": "PRIVATE-PROMPT", "stream": true}
					if nested {
						raw["reasoning"] = map[string]string{"effort": effort}
					} else {
						raw["reasoning_effort"] = effort
					}
					data, _ := json.Marshal(raw)
					r := testRequest(string(data))
					r.Header.Set("X-Sub2-Excel-Transport", "websocket")
					w := httptest.NewRecorder()
					a.ServeHTTP(w, r)
					if w.Code != 200 || !strings.Contains(w.Body.String(), "response.completed") {
						t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
					}
					if w.Header().Get("X-Sub2-Excel-Upstream-Started") != "true" {
						t.Fatal("missing post-send replay barrier")
					}
					bodies := capture.snapshot()
					if len(bodies) != 1 {
						t.Fatalf("wire requests=%d", len(bodies))
					}
					b := bodies[0]
					if b["model"] != model || b["reasoning_effort"] != effort {
						t.Fatalf("model/effort changed: %v %v", b["model"], b["reasoning_effort"])
					}
					if b["type"] != "response.create" || b["stream"] != true {
						t.Fatal("invalid WS envelope")
					}
					if strings.Contains(w.Body.String(), "basispoints.response.") || strings.Contains(w.Body.String(), "resume_") {
						t.Fatal("WS private control frame leaked to caller")
					}
				})
			}
		}
	}
}

func TestWebSocketSharesSSETaskTurnAndToolIteration(t *testing.T) {
	a := threadAdapter(t)
	capture := &wireCapture{}
	useMockWebSocket(t, a, capture, nil)
	a.transport = func(string, string) (http.RoundTripper, error) {
		return roundTripFunc(func(r *http.Request) (*http.Response, error) {
			var b map[string]any
			_ = json.NewDecoder(r.Body).Decode(&b)
			capture.add(b)
			return mockResponse(200, "data: {\"type\":\"response.completed\",\"response\":"+finalResponse(b["model"].(string), "OK")+"}\n\ndata: [DONE]\n\n"), nil
		}), nil
	}
	var logs bytes.Buffer
	a.SetIdentityLogger(log.New(&logs, "", 0))
	user := map[string]any{"role": "user", "content": "PRIVATE-USER-PROMPT"}
	tool := map[string]any{"type": "function_call", "call_id": "tool-1", "name": "read_value", "arguments": "{}"}
	result := map[string]any{"type": "function_call_output", "call_id": "tool-1", "output": "PRIVATE-TOOL-OUTPUT"}
	send := func(transport, account, scope string, input []any) {
		raw, _ := json.Marshal(map[string]any{"model": "gpt-5.6-sol", "reasoning": map[string]string{"effort": "high"}, "prompt_cache_key": "shared-conversation", "input": input, "stream": true, "tools": []any{map[string]any{"type": "function", "name": "read_value", "parameters": map[string]any{"type": "object", "properties": map[string]any{}}}}})
		r := testRequest(string(raw))
		r.Header.Set("X-Sub2-Excel-Transport", transport)
		r.Header.Set("X-Sub2-Excel-Credential-Id", account)
		r.Header.Set("X-Sub2-Excel-Scope", scope)
		w := httptest.NewRecorder()
		a.ServeHTTP(w, r)
		if w.Code != 200 {
			t.Fatalf("status=%d", w.Code)
		}
	}
	send("sse", "11", "caller-scope-11111111", []any{user})
	send("websocket", "11", "caller-scope-11111111", []any{user})
	send("websocket", "11", "caller-scope-11111111", []any{user, tool, result})
	send("sse", "11", "caller-scope-11111111", []any{user, tool, result})
	send("websocket", "11", "caller-scope-11111111", []any{user, tool, result, map[string]any{"role": "user", "content": "NEW-PRIVATE-TURN"}})
	send("websocket", "12", "caller-scope-11111111", []any{user})
	send("websocket", "11", "caller-scope-22222222", []any{user})
	bs := capture.snapshot()
	if len(bs) != 7 {
		t.Fatalf("requests=%d", len(bs))
	}
	meta := func(i int) map[string]any { return bs[i]["metadata"].(map[string]any) }
	for _, i := range []int{1, 2, 3, 4, 6} {
		if meta(i)["task_id"] != meta(0)["task_id"] {
			t.Fatal("same credential task changed")
		}
	}
	for _, i := range []int{1, 2, 3} {
		if meta(i)["turn_id"] != meta(0)["turn_id"] {
			t.Fatal("transport/tool loop changed turn")
		}
	}
	if meta(2)["agent_iteration"] != "2" || meta(3)["agent_iteration"] != "2" {
		t.Fatal("tool loop iteration did not advance")
	}
	if meta(4)["turn_id"] == meta(0)["turn_id"] || meta(4)["agent_iteration"] != "1" {
		t.Fatal("new user turn not isolated")
	}
	if meta(5)["task_id"] == meta(0)["task_id"] || meta(6)["turn_id"] == meta(0)["turn_id"] {
		t.Fatal("credential/caller isolation lost")
	}
	seen := map[string]bool{}
	for _, i := range []int{1, 2, 4, 5, 6} {
		id := bs[i]["basispoints_request_id"].(string)
		if seen[id] {
			t.Fatal("WS request ID reused across distinct requests")
		}
		seen[id] = true
	}
	for _, secret := range []string{testKey, testToken(ClientID, "workspace", "user"), "PRIVATE-USER-PROMPT", "PRIVATE-TOOL-OUTPUT", "NEW-PRIVATE-TURN", "synthetic-ws-test-pepper", "resume_"} {
		if strings.Contains(logs.String(), secret) {
			t.Fatal("secret leaked into identity log")
		}
	}
	if !strings.Contains(logs.String(), `"transport":"wss"`) || !strings.Contains(logs.String(), `"reasoning_effort":"high"`) {
		t.Fatal("wire transport/effort missing from diagnostics")
	}
}

func TestWebSocketNonstreamAndInvalidTransport(t *testing.T) {
	a := threadAdapter(t)
	capture := &wireCapture{}
	useMockWebSocket(t, a, capture, nil)
	r := testRequest(`{"model":"gpt-6-sol","reasoning_effort":"low","input":"hi","stream":false}`)
	r.Header.Set("X-Sub2-Excel-Transport", "websocket")
	w := httptest.NewRecorder()
	a.ServeHTTP(w, r)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "OK") || w.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("nonstream=%d %s", w.Code, w.Body.String())
	}
	r = testRequest(`{"model":"gpt-6-sol","input":"hi"}`)
	r.Header.Set("X-Sub2-Excel-Transport", "bogus")
	w = httptest.NewRecorder()
	a.ServeHTTP(w, r)
	if w.Code != 400 || len(capture.snapshot()) != 1 {
		t.Fatal("invalid mode reached upstream")
	}
}

func TestWebSocketHandshake403KeepsStatusWithoutLeakingBearer(t *testing.T) {
	a := threadAdapter(t)
	var count atomic.Int32
	a.wsDial = func(context.Context, *websocket.Config, http.RoundTripper) (*websocket.Conn, int, http.Header, error) {
		count.Add(1)
		return nil, 403, nil, errors.New("SECRET-ERROR")
	}
	r := testRequest(`{"model":"gpt-6-astra","input":"hi","stream":true}`)
	r.Header.Set("X-Sub2-Excel-Transport", "websocket")
	w := httptest.NewRecorder()
	a.ServeHTTP(w, r)
	if w.Header().Get("X-Sub2-Excel-Upstream-Started") != "" {
		t.Fatal("pre-send rejection marked committed")
	}
	if w.Code != 403 || strings.Contains(w.Body.String(), "SECRET-ERROR") {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestWebSocketDisconnectDoesNotReplayFrame(t *testing.T) {
	a := threadAdapter(t)
	capture := &wireCapture{}
	useMockWebSocket(t, a, capture, func(ws *websocket.Conn, _ map[string]any) { _ = ws.Close() })
	r := testRequest(`{"model":"gpt-5.6-sol","input":"hi","stream":true}`)
	r.Header.Set("X-Sub2-Excel-Transport", "websocket")
	w := httptest.NewRecorder()
	a.ServeHTTP(w, r)
	if w.Header().Get("X-Sub2-Excel-Upstream-Started") != "true" {
		t.Fatal("post-send disconnect lost trusted replay barrier")
	}
	if len(capture.snapshot()) != 1 {
		t.Fatalf("unexpected create replay count=%d", len(capture.snapshot()))
	}
	if strings.Contains(w.Body.String(), `"status":"completed"`) {
		t.Fatal("disconnect fabricated completion")
	}
}

func TestWebSocketCancellationClosesRead(t *testing.T) {
	a := threadAdapter(t)
	capture := &wireCapture{}
	arrived := make(chan struct{})
	closed := make(chan struct{})
	useMockWebSocket(t, a, capture, func(ws *websocket.Conn, _ map[string]any) {
		close(arrived)
		var b []byte
		_ = websocket.Message.Receive(ws, &b)
		close(closed)
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := testRequest(`{"model":"gpt-6-astra","input":"hi","stream":true}`).WithContext(ctx)
	r.Header.Set("X-Sub2-Excel-Transport", "websocket")
	done := make(chan struct{})
	go func() { a.ServeHTTP(httptest.NewRecorder(), r); close(done) }()
	select {
	case <-arrived:
	case <-time.After(3 * time.Second):
		t.Fatal("no WS request")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("request ignored cancellation")
	}
	select {
	case <-closed:
	case <-time.After(3 * time.Second):
		t.Fatal("upstream connection remained open")
	}
}

func TestWebSocketBrowserTLSForcesHTTP1WithoutChangingSSEALPN(t *testing.T) {
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "ok") }))
	server.EnableHTTP2 = true
	server.TLS = &tls.Config{NextProtos: []string{"h2", "http/1.1"}}
	server.StartTLS()
	defer server.Close()
	old := utlsRootCAs
	pool := x509.NewCertPool()
	pool.AddCert(server.Certificate())
	utlsRootCAs = pool
	t.Cleanup(func() { utlsRootCAs = old })
	u, _ := url.Parse(server.URL)
	rt := testRoundTripper(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	conn, err := rt.createConnectionWithALPN(ctx, u.Hostname(), u.Host, []string{"http/1.1"})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if conn.ConnectionState().NegotiatedProtocol != "http/1.1" {
		t.Fatalf("WS ALPN=%q; server offers=%v", conn.ConnectionState().NegotiatedProtocol, server.TLS.NextProtos)
	}
	sse, err := rt.createConnection(ctx, u.Hostname(), u.Host)
	if err != nil {
		t.Fatal(err)
	}
	defer sse.Close()
	if sse.ConnectionState().NegotiatedProtocol != "h2" {
		t.Fatal("SSE ALPN changed")
	}
}
