// Package adapter embeds CPA's BPS protocol engine without CPA scheduling,
// account storage, login, token refresh, management or public proxy endpoints.
package adapter

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jaxson-wang/cpa-plugin-oai-basispoints/internal/basispoints"
	"gopkg.in/yaml.v3"
)

const ClientID = "app_fnr0pYvVwwFDocDumLG3H2Bp"
const UpstreamCommit = "4aabe5c3c7543e5c08741eeaa51569d792455a86"
const maxBody = 64 << 20

var models = []string{"gpt-6-sol", "gpt-6.1-sol", "gpt-6-astra", "gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.6-luna", "gpt-6-excel", "gpt-6-astra-excel", "gpt-5.6-sol-excel", "gpt-5.6-terra-excel", "gpt-5.6-luna-excel"}
var aliases = map[string]string{"gpt-6-excel": "gpt-6-sol", "gpt-6-astra-excel": "gpt-6-astra", "gpt-5.6-sol-excel": "gpt-5.6-sol", "gpt-5.6-terra-excel": "gpt-5.6-terra", "gpt-5.6-luna-excel": "gpt-5.6-luna"}

type message struct {
	body   []byte
	closed bool
	err    string
}
type invocation struct {
	started           time.Time
	upstreamBytes     atomic.Int64
	firstReadMS       atomic.Int64
	lastReadAt        atomic.Int64
	accountID         int64
	requestedModel    string
	upstreamTransport string
	wsMu              sync.Mutex
	wsSent            map[[32]byte]bool
	wsSendAttempted   atomic.Bool
	upstreamAttempts  atomic.Uint32
	ctx               context.Context
	cancel            context.CancelFunc
	client            *http.Client
	events            chan message
	closeOnce         sync.Once
}
type upstream struct {
	readErr error // deferred until bytes from the preceding Read have been relayed
	owner   string
	body    io.ReadCloser
	mu      sync.Mutex
	once    sync.Once
}

func (u *upstream) close() { u.once.Do(func() { _ = u.body.Close() }) }

type Adapter struct {
	requestTimeout time.Duration // shared bounded engine, context and HTTP budget
	identityMode   string
	identityLogger *log.Logger
	wsDial         wsDialFunc
	key            string
	imagePath      string
	service        *basispoints.Service
	mu             sync.Mutex
	calls          map[string]*invocation
	streams        map[string]*upstream
	// Tests replace the transport but retain the exact HTTPS host/path guard.
	transport       func(string, string) (http.RoundTripper, error)
	closeTransports func()
}

// New starts a protocol-only engine. Safety invariants override optional relay
// settings: no secondary concurrency gate, quota traffic, accounts or disk data.
func New(key string, relayConfig []byte) (*Adapter, error) {
	if len(strings.TrimSpace(key)) < 32 {
		return nil, errors.New("transport key must contain at least 32 characters")
	}
	cfg := map[string]any{}
	if len(relayConfig) > 0 {
		if err := yaml.Unmarshal(relayConfig, &cfg); err != nil {
			return nil, errors.New("invalid relay configuration")
		}
	}
	budget := struct {
		TimeoutSeconds int `yaml:"timeout_seconds"`
	}{TimeoutSeconds: 300}
	if len(relayConfig) > 0 {
		if err := yaml.Unmarshal(relayConfig, &budget); err != nil {
			return nil, errors.New("timeout_seconds must be an integer between 10 and 1800")
		}
	}
	if budget.TimeoutSeconds < 10 || budget.TimeoutSeconds > 1800 {
		return nil, errors.New("timeout_seconds must be between 10 and 1800")
	}
	mappings := map[string]string{}
	for _, model := range models {
		mappings[model] = model
		if canonical := aliases[model]; canonical != "" {
			mappings[model] = canonical
		}
	}
	cfg["data_dir"] = ""
	cfg["responses_url"] = basispoints.DefaultResponsesURL
	cfg["models"] = models
	cfg["model_upstreams"] = mappings
	cfg["upstream_model"] = "gpt-6-astra"
	cfg["max_concurrency_per_credential"] = 0
	cfg["credential_concurrency"] = map[string]int{}
	cfg["quota_sample_seconds"] = 0
	cfg["downgrade_guard_enabled"] = false
	imageBase, err := configuredImageBase(cfg["public_base_url"])
	if err != nil {
		return nil, err
	}
	cfg["public_base_url"] = imageBase
	cfg["timeout_seconds"] = budget.TimeoutSeconds
	cfg["max_response_bytes"] = maxBody
	configYAML, err := yaml.Marshal(cfg)
	if err != nil {
		return nil, err
	}
	identityMode := "off"
	if identity, ok := cfg["client_identity"].(map[string]any); ok {
		if mode, ok := identity["mode"].(string); ok && strings.TrimSpace(mode) != "" {
			identityMode = strings.ToLower(strings.TrimSpace(mode))
		}
	}
	imagePath := ""
	if imageBase != "" {
		u, _ := url.Parse(imageBase)
		imagePath = strings.TrimRight(u.Path, "/") + "/image"
	}
	a := &Adapter{imagePath: imagePath, requestTimeout: time.Duration(budget.TimeoutSeconds) * time.Second, identityMode: identityMode, key: strings.TrimSpace(key), service: basispoints.NewService(), calls: map[string]*invocation{}, streams: map[string]*upstream{}, transport: nil}
	pool := newTransportPool()
	a.transport, a.closeTransports = pool.get, pool.close
	a.service.SetHost(a.host)
	raw, _ := json.Marshal(map[string]any{"config_yaml": configYAML})
	if _, err = a.service.Handle("plugin.register", raw); err != nil {
		return nil, err
	}
	return a, nil
}

// RequestTimeout also aligns the private server write deadline with the engine.
func (a *Adapter) RequestTimeout() time.Duration { return a.requestTimeout }

func (a *Adapter) Shutdown() {
	a.mu.Lock()
	for _, call := range a.calls {
		call.cancel()
	}
	for _, stream := range a.streams {
		stream.close()
	}
	a.mu.Unlock()
	_, _ = a.service.Handle("plugin.shutdown", nil)
	a.closeTransports()
}

func (a *Adapter) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if (r.URL.Path == "/health" || r.URL.Path == "/healthz") && r.Method == http.MethodGet {
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "upstream_commit": UpstreamCommit, "client_identity_mode": a.identityMode, "identity_logging": a.identityLogger != nil, "upstream_transports": []string{"sse", "websocket"}, "websocket_replay": false})
		return
	}
	if r.URL.Path != "/internal/v1/responses" {
		writeError(w, 404, "not_found", "not found")
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeError(w, 405, "method_not_allowed", "POST required")
		return
	}
	auth := strings.TrimSpace(r.Header.Get("Authorization"))
	if subtle.ConstantTimeCompare([]byte(auth), []byte("Bearer "+a.key)) != 1 {
		writeError(w, 401, "unauthorized", "invalid private transport authorization")
		return
	}
	upstreamTransport := strings.TrimSpace(r.Header.Get("X-Sub2-Excel-Transport"))
	if upstreamTransport == "" {
		upstreamTransport = "sse"
	}
	if upstreamTransport != "sse" && upstreamTransport != "websocket" {
		writeError(w, 400, "invalid_transport", "unsupported private upstream transport")
		return
	}
	token := strings.TrimSpace(r.Header.Get("X-Sub2-Excel-Access-Token"))
	workspace := strings.TrimSpace(r.Header.Get("X-Sub2-Excel-Account-Id"))
	user := strings.TrimSpace(r.Header.Get("X-Sub2-Excel-Account-User-Id"))
	credentialID := strings.TrimSpace(r.Header.Get("X-Sub2-Excel-Credential-Id"))
	scope := strings.TrimSpace(r.Header.Get("X-Sub2-Excel-Scope"))
	id, idErr := strconv.ParseInt(credentialID, 10, 64)
	if idErr != nil || id <= 0 || len(scope) < 16 || len(scope) > 256 || strings.ContainsAny(scope, "\r\n") {
		writeError(w, 400, "missing_selected_identity", "trusted account ID and caller scope are required")
		return
	}
	if err := validateCredential(token, workspace, user); err != nil {
		writeError(w, 401, "invalid_bps_credentials", err.Error())
		return
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
	if err != nil {
		writeError(w, 413, "request_too_large", "request body exceeds limit")
		return
	}
	var body struct {
		Model  string `json:"model"`
		Stream bool   `json:"stream"`
	}
	if json.Unmarshal(raw, &body) != nil {
		writeError(w, 400, "invalid_request", "valid JSON object is required")
		return
	}
	allowed := false
	for _, m := range models {
		if body.Model == m {
			allowed = true
			break
		}
	}
	if !allowed {
		writeError(w, 400, "unsupported_model", "model must use native outbound routing")
		return
	}
	transport, err := a.transport(credentialID+"\x00"+workspace+"\x00"+user, r.Header.Get("X-Sub2-Excel-Proxy"))
	if err != nil {
		writeError(w, 400, "invalid_proxy", "selected-account proxy is invalid")
		return
	}
	client := &http.Client{Transport: transport, Timeout: a.requestTimeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	ctx, cancel := context.WithTimeout(r.Context(), a.requestTimeout)
	call := &invocation{started: time.Now(), accountID: id, requestedModel: body.Model, upstreamTransport: upstreamTransport, ctx: ctx, cancel: cancel, client: client, events: make(chan message, 8)}
	callbackID := randomID()
	a.mu.Lock()
	a.calls[callbackID] = call
	a.mu.Unlock()
	defer func() { cancel(); a.removeCall(callbackID) }()
	storage, _ := json.Marshal(map[string]any{"type": basispoints.Provider, "access_token": token, "account_id": workspace})
	request := basispoints.ExecutorRequest{AuthID: "sub2:" + credentialID, AuthProvider: basispoints.Provider, Model: body.Model, Format: "openai-response", SourceFormat: "openai-response", Stream: body.Stream, OriginalRequest: raw, Payload: raw, StorageJSON: storage, Metadata: map[string]any{"caller_scope": scope}, HostCallbackID: callbackID, StreamID: callbackID}
	payload, _ := json.Marshal(request)
	method := "executor.execute"
	if body.Stream {
		method = "executor.execute_stream"
	}
	result, err := a.service.Handle(method, payload)
	// Trusted private response marker survives core error mapping. A partial WS
	// write may already have executed upstream and must not trigger backend replay.
	if call.upstreamTransport == "websocket" && call.wsSendAttempted.Load() {
		w.Header().Set("X-Sub2-Excel-Upstream-Started", "true")
	}
	if err != nil {
		a.writeAPIError(w, err, token)
		return
	}
	if !body.Stream {
		var response basispoints.ExecutorResponse
		if err := transcode(result, &response); err != nil {
			writeError(w, 502, "invalid_response", "invalid adapter response")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = w.Write(response.Payload)
		return
	}
	a.relayClientStream(w, r, call, callbackID, token)
}

func validateCredential(token, workspace, user string) error {
	parts := strings.Split(token, ".")
	if len(parts) != 3 || len(token) > 128<<10 || workspace == "" || user == "" {
		return errors.New("BPS access token and selected identity are required")
	}
	decoded, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return errors.New("invalid BPS access token")
	}
	var c struct {
		Client string `json:"client_id"`
		Sub    string `json:"sub"`
		Exp    int64  `json:"exp"`
		Auth   struct {
			Account    string `json:"chatgpt_account_id"`
			User       string `json:"chatgpt_user_id"`
			LegacyUser string `json:"user_id"`
		} `json:"https://api.openai.com/auth"`
	}
	if json.Unmarshal(decoded, &c) != nil || c.Client != ClientID || c.Sub == "" {
		return errors.New("access token does not belong to the BPS OAuth client")
	}
	if c.Auth.User == "" {
		c.Auth.User = c.Auth.LegacyUser
	}
	if c.Auth.Account != workspace || c.Auth.User != user {
		return errors.New("access token does not match selected account identity")
	}
	if c.Exp <= time.Now().Unix() {
		return errors.New("BPS access token expired; refresh belongs to Sub2API")
	}
	return nil
}

func (a *Adapter) removeCall(id string) {
	a.mu.Lock()
	delete(a.calls, id)
	for key, stream := range a.streams {
		if stream.owner == id {
			stream.close()
			delete(a.streams, key)
		}
	}
	a.mu.Unlock()
}
func (a *Adapter) invocation(id string) (*invocation, error) {
	a.mu.Lock()
	call := a.calls[id]
	a.mu.Unlock()
	if call == nil {
		return nil, errors.New("request no longer active")
	}
	if err := call.ctx.Err(); err != nil {
		return nil, err
	}
	return call, nil
}
func randomID() string {
	var b [24]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("secure randomness unavailable")
	}
	return hex.EncodeToString(b[:])
}
func transcode(in, out any) error {
	data, err := json.Marshal(in)
	if err != nil {
		return err
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(data, out)
}
func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
func writeError(w http.ResponseWriter, status int, kind, msg string) {
	writeJSON(w, status, map[string]any{"error": map[string]string{"type": kind, "message": msg}})
}
func (a *Adapter) writeAPIError(w http.ResponseWriter, err error, token string) {
	var apiErr *basispoints.APIError
	if errors.As(err, &apiErr) {
		status := apiErr.Status
		if status < 400 || status > 599 {
			status = 502
		}
		if apiErr.Kind == "upstream_transport" {
			w.Header().Set("X-Sub2-Excel-Failure", "transport")
		}
		writeError(w, status, apiErr.Kind, strings.ReplaceAll(apiErr.Message, token, "[REDACTED]"))
		return
	}
	w.Header().Set("X-Sub2-Excel-Failure", "transport")
	writeError(w, 502, "adapter_error", "Basis Points adapter failed")
}

func (a *Adapter) host(method string, payload any, out any) error {
	switch method {
	case "host.log":
		return nil // Never copy arbitrary upstream text/tokens to logs.
	case "host.http.do", "host.http.do_stream":
		var p struct {
			Callback string      `json:"host_callback_id"`
			Method   string      `json:"method"`
			URL      string      `json:"url"`
			Headers  http.Header `json:"headers"`
			Body     []byte      `json:"body"`
		}
		if err := transcode(payload, &p); err != nil {
			return err
		}
		call, err := a.invocation(p.Callback)
		if err != nil {
			return err
		}
		u, err := url.Parse(p.URL)
		if err != nil || u.Scheme != "https" || u.Host != "bps.openai.com" || u.Path != "/basispoints/api/responses" || u.User != nil || u.Fragment != "" || p.Method != http.MethodPost {
			return errors.New("upstream destination is not allowed")
		}
		req, err := http.NewRequestWithContext(call.ctx, p.Method, p.URL, bytes.NewReader(p.Body))
		if err != nil {
			return err
		}
		req.Header = p.Headers.Clone()
		trace := a.identityTrace(call, p.Callback, p.Body)
		var response *http.Response
		if call.upstreamTransport == "websocket" {
			response, err = a.websocketResponse(call, req, p.Body, method == "host.http.do_stream", trace)
		} else {
			a.logIdentity(trace, "bps.upstream.request", 0, 0)
			started := time.Now()
			response, err = call.client.Do(req)
			if err != nil {
				a.logIdentity(trace, "bps.upstream.transport_error", 0, time.Since(started).Milliseconds())
			} else {
				a.logIdentity(trace, "bps.upstream.response", response.StatusCode, time.Since(started).Milliseconds())
			}
		}
		if err != nil {
			return errors.New("selected-account upstream connection failed")
		}
		if method == "host.http.do" {
			defer response.Body.Close()
			raw, err := io.ReadAll(io.LimitReader(response.Body, maxBody+1))
			if err != nil {
				return errors.New("upstream response read failed")
			}
			if len(raw) > maxBody {
				return errors.New("upstream response exceeds limit")
			}
			return transcode(map[string]any{"StatusCode": response.StatusCode, "Headers": response.Header, "Body": raw}, out)
		}
		streamID := randomID()
		a.mu.Lock()
		if a.calls[p.Callback] != call || call.ctx.Err() != nil {
			a.mu.Unlock()
			response.Body.Close()
			return errors.New("request no longer active")
		}
		a.streams[streamID] = &upstream{owner: p.Callback, body: response.Body}
		a.mu.Unlock()
		return transcode(map[string]any{"status_code": response.StatusCode, "headers": response.Header, "stream_id": streamID}, out)
	case "host.http.stream_read", "host.http.stream_close":
		var p struct {
			ID string `json:"stream_id"`
		}
		if err := transcode(payload, &p); err != nil {
			return err
		}
		a.mu.Lock()
		stream := a.streams[p.ID]
		if method == "host.http.stream_close" {
			delete(a.streams, p.ID)
		}
		a.mu.Unlock()
		if stream == nil {
			if method == "host.http.stream_close" {
				return nil
			}
			return errors.New("upstream stream no longer active")
		}
		if method == "host.http.stream_close" {
			stream.close()
			return nil
		}
		owner, err := a.invocation(stream.owner)
		if err != nil {
			return err
		}
		stream.mu.Lock()
		defer stream.mu.Unlock()
		buf := make([]byte, 32<<10)
		n, err := 0, stream.readErr
		if err == nil {
			n, err = stream.body.Read(buf)
		}
		// io.Reader permits data and a non-EOF error in the same call. CPA
		// checks chunk.error before consuming payload, so postpone the error
		// until the next read. A valid terminal in these bytes wins the error.
		if n > 0 && err != nil && err != io.EOF {
			stream.readErr, err = err, nil
		}
		if n > 0 {
			if owner.upstreamBytes.Add(int64(n)) == int64(n) {
				owner.firstReadMS.Store(time.Since(owner.started).Milliseconds())
			}
			owner.lastReadAt.Store(time.Now().UnixNano())
		}
		message := ""
		if err != nil && err != io.EOF {
			message = "upstream stream read failed"
		}
		return transcode(map[string]any{"payload": buf[:n], "done": err != nil, "error": message}, out)
	case "host.stream.emit", "host.stream.close":
		var p struct {
			ID      string `json:"stream_id"`
			Payload []byte `json:"payload"`
			Error   string `json:"error"`
		}
		if err := transcode(payload, &p); err != nil {
			return err
		}
		call, err := a.invocation(p.ID)
		if err != nil {
			return err
		}
		event := message{body: p.Payload, closed: method == "host.stream.close", err: p.Error}
		var sendErr error
		send := func() {
			select {
			case call.events <- event:
			case <-call.ctx.Done():
				sendErr = call.ctx.Err()
			}
		}
		if event.closed {
			call.closeOnce.Do(send)
		} else {
			send()
		}
		return sendErr
	default:
		return fmt.Errorf("host capability %s is not available in selected-account adapter", method)
	}
}
