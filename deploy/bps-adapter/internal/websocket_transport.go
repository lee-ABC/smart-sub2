package adapter

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/websocket"
)

const bpsWebSocketURL = "wss://bps.openai.com/basispoints/api/responses"

type wsRequestIdentity struct{ requestID, affinity, resumeToken string }
type wsDialFunc func(context.Context, *websocket.Config, http.RoundTripper) (*websocket.Conn, int, http.Header, error)

func wsRandomIdentity() (wsRequestIdentity, error) {
	var ids [3][16]byte
	for i := range ids {
		if _, err := rand.Read(ids[i][:]); err != nil {
			return wsRequestIdentity{}, errors.New("websocket request identity unavailable")
		}
	}
	ids[0][6] = (ids[0][6] & 15) | 0x40
	ids[0][8] = (ids[0][8] & 63) | 0x80
	b := ids[0]
	return wsRequestIdentity{fmt.Sprintf("%x-%x-%x-%x-%x", b[:4], b[4:6], b[6:8], b[8:10], b[10:]), "affinity_" + hex.EncodeToString(ids[1][:]), "resume_" + hex.EncodeToString(ids[2][:])}, nil
}

// Consume the SAME CPA-prepared body as SSE. Never construct or substitute a
// model/effort/task/turn here. Only transport fields are added to the wire frame.
func prepareBPSWebSocket(headers http.Header, raw []byte) (*websocket.Config, []byte, wsRequestIdentity, error) {
	var body map[string]json.RawMessage
	if json.Unmarshal(raw, &body) != nil || body == nil {
		return nil, nil, wsRequestIdentity{}, errors.New("invalid prepared websocket body")
	}
	auth := strings.Fields(headers.Get("Authorization"))
	if len(auth) != 2 || !strings.EqualFold(auth[0], "Bearer") {
		return nil, nil, wsRequestIdentity{}, errors.New("websocket bearer unavailable")
	}
	ids, err := wsRandomIdentity()
	if err != nil {
		return nil, nil, ids, err
	}
	clientInfo := map[string]string{}
	for name, values := range headers {
		if strings.HasPrefix(strings.ToLower(name), "x-openai-internal-basispoints-") && len(values) > 0 {
			clientInfo[http.CanonicalHeaderKey(name)] = values[0]
		}
	}
	info, _ := json.Marshal(clientInfo)
	u, _ := url.Parse(bpsWebSocketURL)
	q := u.Query()
	q.Set("bps_client_info", string(info))
	q.Set("bps_auth_mode", "chatgpt")
	q.Set("bps_ws_affinity", ids.affinity)
	// Mirror the observed control capabilities. This first transport is deliberately
	// single-shot: disconnect/draining fail closed; no unverified resume or replay.
	q.Set("bps_control_frames", "upstream_sent,heartbeat,resume,cancel,server_draining")
	u.RawQuery = q.Encode()
	cfg, err := websocket.NewConfig(u.String(), "https://bps.openai.com")
	if err != nil {
		return nil, nil, ids, err
	}
	cfg.Protocol = []string{"responses", "openai-bearer." + auth[1]}
	for _, key := range []string{"User-Agent", "X-Basispoints-Auth-Mode", "Chatgpt-Account-Id", "X-Openai-Account-Id", "X-Openai-Account-User-Id"} {
		if value := headers.Get(key); value != "" {
			cfg.Header.Set(key, value)
		}
	}
	trimmed := bytes.TrimSpace(raw)
	// Most requests are streaming. Preserve CPA's original body ordering verbatim.
	if string(bytes.TrimSpace(body["stream"])) != "true" {
		body["stream"] = json.RawMessage("true")
		trimmed, err = json.Marshal(body)
		if err != nil {
			return nil, nil, ids, err
		}
	}
	extra, _ := json.Marshal(map[string]string{"type": "response.create", "basispoints_request_id": ids.requestID, "basispoints_resume_token": ids.resumeToken})
	frame := append([]byte(nil), trimmed[:len(trimmed)-1]...)
	frame = append(frame, ',')
	frame = append(frame, extra[1:]...)
	return cfg, frame, ids, nil
}

type wsHandshakeConn struct {
	net.Conn
	captured bytes.Buffer
	capture  bool
}

func (c *wsHandshakeConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if c.capture && c.captured.Len() < 65536 {
		limit := n
		if remain := 65536 - c.captured.Len(); limit > remain {
			limit = remain
		}
		_, _ = c.captured.Write(p[:limit])
	}
	return n, err
}

func dialBPSWebSocket(ctx context.Context, cfg *websocket.Config, transport http.RoundTripper) (*websocket.Conn, int, http.Header, error) {
	t, ok := transport.(*utlsRoundTripper)
	if !ok {
		return nil, 0, nil, errors.New("websocket browser transport unavailable")
	}
	dialCtx, cancel := context.WithTimeout(ctx, utlsDialTimeout)
	defer cancel()
	raw, err := t.createConnectionWithALPN(dialCtx, "bps.openai.com", "bps.openai.com:443", []string{"http/1.1"})
	if err != nil {
		return nil, 0, nil, errors.New("websocket TLS connection failed")
	}
	if d, ok := dialCtx.Deadline(); ok {
		_ = raw.SetDeadline(d)
	}
	conn := &wsHandshakeConn{Conn: raw, capture: true}
	stop := context.AfterFunc(dialCtx, func() { _ = raw.Close() })
	ws, err := websocket.NewClient(cfg, conn)
	conn.capture = false
	stop()
	var status int
	headers := http.Header{}
	if response, parseErr := http.ReadResponse(bufio.NewReader(bytes.NewReader(conn.captured.Bytes())), &http.Request{Method: http.MethodGet}); parseErr == nil {
		status = response.StatusCode
		headers = response.Header.Clone()
	}
	conn.captured.Reset()
	if err != nil {
		_ = raw.Close()
		return nil, status, headers, errors.New("websocket handshake failed")
	}
	if d, ok := ctx.Deadline(); ok {
		_ = raw.SetDeadline(d)
	} else {
		_ = raw.SetDeadline(time.Time{})
	}
	ws.MaxPayloadBytes = maxBody
	return ws, status, headers, nil
}

type wsBody struct {
	*io.PipeReader
	closeFn func()
	once    sync.Once
}

func (b *wsBody) Close() error { b.once.Do(b.closeFn); return b.PipeReader.Close() }

func (a *Adapter) websocketResponse(call *invocation, req *http.Request, raw []byte, streaming bool, trace identityLogRecord) (*http.Response, error) {
	bodyHash := sha256.Sum256(raw)
	call.wsMu.Lock()
	alreadySent := call.wsSent[bodyHash]
	call.wsMu.Unlock()
	if alreadySent {
		return nil, errors.New("websocket request outcome already committed or unknown; replay blocked")
	}
	cfg, frame, ids, err := prepareBPSWebSocket(req.Header, raw)
	if err != nil {
		return nil, err
	}
	trace.Transport = "wss"
	trace.Stream = true
	trace.UpstreamRequestID = ids.requestID
	a.logIdentity(trace, "bps.upstream.request", 0, 0)
	started := time.Now()
	dial := a.wsDial
	if dial == nil {
		dial = dialBPSWebSocket
	}
	ws, status, _, err := dial(call.ctx, cfg, call.client.Transport)
	if err != nil {
		if status >= 400 && status <= 599 {
			a.logIdentity(trace, "bps.upstream.response", status, time.Since(started).Milliseconds())
			body := fmt.Sprintf(`{"error":{"type":"bps_ws_handshake","message":"Basis Points WebSocket handshake rejected (HTTP %d)"}}`, status)
			return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(body))}, nil
		}
		a.logIdentity(trace, "bps.upstream.transport_error", 0, time.Since(started).Milliseconds())
		return nil, errors.New("selected-account websocket connection failed")
	}
	a.logIdentity(trace, "bps.upstream.response", 101, time.Since(started).Milliseconds())
	reader, writer := io.Pipe()
	ctx, cancel := context.WithCancel(call.ctx)
	stop := context.AfterFunc(ctx, func() {
		_ = ws.SetDeadline(time.Now())
		_ = ws.Close()
		_ = writer.CloseWithError(errors.New("websocket request cancelled"))
	})
	result := &wsBody{PipeReader: reader, closeFn: func() { cancel(); stop(); _ = ws.SetDeadline(time.Now()); _ = ws.Close(); _ = writer.Close() }}
	call.wsMu.Lock()
	if call.wsSent == nil {
		call.wsSent = make(map[[32]byte]bool)
	}
	if call.wsSent[bodyHash] {
		call.wsMu.Unlock()
		result.Close()
		return nil, errors.New("websocket request replay blocked")
	}
	// A partial write is an unknown outcome too. Never automatically resend it.
	call.wsSent[bodyHash] = true
	call.wsSendAttempted.Store(true)
	call.wsMu.Unlock()
	if err := websocket.Message.Send(ws, string(frame)); err != nil {
		result.Close()
		a.logIdentity(trace, "bps.upstream.transport_error", 0, time.Since(started).Milliseconds())
		return nil, errors.New("websocket request send failed; outcome unknown")
	}
	go func() {
		defer writer.Close()
		defer cancel()
		defer stop()
		defer func() { _ = ws.SetDeadline(time.Now()); _ = ws.Close() }()
		var total int
		for {
			var message []byte
			if err := websocket.Message.Receive(ws, &message); err != nil {
				a.logIdentity(trace, "bps.upstream.transport_error", 0, time.Since(started).Milliseconds())
				_ = writer.CloseWithError(errors.New("websocket ended before a terminal response"))
				return
			}
			total += len(message)
			if total > maxBody {
				_ = writer.CloseWithError(errors.New("websocket response exceeds limit"))
				return
			}
			var event struct {
				Type     string          `json:"type"`
				Response json.RawMessage `json:"response"`
			}
			if json.Unmarshal(message, &event) != nil {
				_ = writer.CloseWithError(errors.New("invalid websocket response frame"))
				return
			}
			if strings.HasPrefix(event.Type, "basispoints.response.") {
				switch event.Type {
				case "basispoints.response.resume_token", "basispoints.response.metadata", "basispoints.response.upstream_sent", "basispoints.response.heartbeat":
					continue
				case "basispoints.response.server_draining":
					_ = writer.CloseWithError(errors.New("websocket server draining; request not replayed"))
					return
				default:
					continue
				}
			}
			terminal := event.Type == "response.completed" || event.Type == "response.failed" || event.Type == "response.incomplete" || event.Type == "error"
			if streaming {
				if _, err := writer.Write(append(append([]byte("data: "), message...), []byte("\n\n")...)); err != nil {
					return
				}
				if terminal {
					_, _ = writer.Write([]byte("data: [DONE]\n\n"))
					return
				}
			} else if terminal {
				if len(event.Response) == 0 {
					_, _ = writer.Write([]byte(`{"error":{"type":"bps_ws_error","message":"Basis Points WebSocket request failed"}}`))
				} else {
					_, _ = writer.Write(event.Response)
				}
				return
			}
		}
	}()
	contentType := "application/json"
	if streaming {
		contentType = "text/event-stream"
	}
	return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{contentType}}, Body: result}, nil
}
