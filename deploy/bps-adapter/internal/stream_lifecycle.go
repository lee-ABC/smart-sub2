package adapter

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// relayClientStream owns the downstream terminal. CPA remains responsible for
// translating tools/history. In particular, no submitted request is replayed.
func (a *Adapter) relayClientStream(w http.ResponseWriter, r *http.Request, call *invocation, callbackID, token string) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	controller := http.NewResponseController(w)
	if err := controller.Flush(); err != nil {
		return
	}
	terminal, responseID, outcome := "", "resp_"+callbackID, "closed"
	var downstreamBytes int64
	defer func() { a.logStreamEnd(call, callbackID, outcome, terminal, downstreamBytes) }()
	observe := func(raw []byte) {
		// CPA emits complete SSE frames; never log their content or error strings.
		for _, line := range bytes.Split(raw, []byte("\n")) {
			if !bytes.HasPrefix(line, []byte("data:")) {
				continue
			}
			var event struct {
				Type     string `json:"type"`
				Response struct {
					ID string `json:"id"`
				} `json:"response"`
			}
			if json.Unmarshal(bytes.TrimSpace(line[5:]), &event) != nil {
				continue
			}
			if event.Response.ID != "" {
				responseID = event.Response.ID
			}
			switch event.Type {
			case "response.completed", "response.done", "response.failed", "response.incomplete", "response.cancelled", "response.canceled", "error":
				terminal = event.Type
			}
		}
	}
	fail := func(code, message string) {
		if terminal != "" || r.Context().Err() != nil {
			return
		}
		// The upstream budget has elapsed, not the client socket's write budget.
		// Bound this last write and send a real failed terminal, never completed.
		_ = controller.SetWriteDeadline(time.Now().Add(5 * time.Second))
		data, _ := json.Marshal(map[string]any{"type": "response.failed", "response": map[string]any{
			"id": responseID, "object": "response", "created_at": time.Now().Unix(), "model": call.requestedModel,
			"status": "failed", "output": []any{}, "error": map[string]string{"code": code, "type": "bps_stream_error", "message": strings.ReplaceAll(message, token, "[REDACTED]")},
		}})
		n, err := fmt.Fprintf(w, "event: response.failed\ndata: %s\n\ndata: [DONE]\n\n", data)
		downstreamBytes += int64(n)
		if err == nil {
			_ = controller.Flush()
			terminal = "response.failed"
		}
	}
	handle := func(event message) bool {
		if len(event.body) > 0 {
			n, err := w.Write(event.body)
			downstreamBytes += int64(n)
			if err != nil {
				outcome = "downstream_write_failed"
				return true
			}
			observe(event.body)
			if err = controller.Flush(); err != nil {
				outcome = "downstream_write_failed"
				return true
			}
		}
		if event.closed {
			if terminal == "" {
				outcome = "stream_failed"
				detail := event.err
				if detail == "" {
					detail = "Basis Points stream closed before a terminal response"
				}
				fail("bps_stream_error", detail)
			} else {
				outcome = "terminal"
			}
			return true
		}
		return false
	}
	for {
		select {
		case event := <-call.events:
			if handle(event) {
				return
			}
		case <-call.ctx.Done():
			if r.Context().Err() != nil {
				outcome = "client_cancelled"
				return
			}
			// A terminal may already be queued when the deadline fires. Drain it before
			// synthesizing failure, so a completed response can never become a failure.
			for {
				select {
				case event := <-call.events:
					if handle(event) {
						return
					}
				default:
					if terminal != "" {
						outcome = "terminal"
						return
					}
					if errors.Is(call.ctx.Err(), context.DeadlineExceeded) {
						outcome = "deadline"
						fail("upstream_timeout", "Basis Points upstream did not finish before the request deadline")
					} else {
						outcome = "cancelled"
						fail("stream_cancelled", "Basis Points stream was interrupted")
					}
					return
				}
			}
		}
	}
}

// Fixed projections only: never log payloads, headers, tokens or tool arguments.
func (a *Adapter) logStreamEnd(call *invocation, id, outcome, terminal string, downstreamBytes int64) {
	if a.identityLogger == nil {
		return
	}
	record := struct {
		Event           string `json:"event"`
		Time            string `json:"time"`
		RequestID       string `json:"request_id"`
		AccountID       int64  `json:"account_id"`
		Model           string `json:"requested_model"`
		Outcome         string `json:"outcome"`
		Terminal        string `json:"terminal,omitempty"`
		ElapsedMS       int64  `json:"elapsed_ms"`
		UpstreamBytes   int64  `json:"upstream_bytes"`
		DownstreamBytes int64  `json:"downstream_bytes"`
		FirstReadMS     int64  `json:"first_upstream_read_ms"`
		LastReadAgoMS   int64  `json:"last_upstream_read_ago_ms"`
	}{Event: "bps.stream.end", Time: time.Now().UTC().Format(time.RFC3339Nano), RequestID: id, AccountID: call.accountID, Model: call.requestedModel,
		Outcome: outcome, Terminal: terminal, ElapsedMS: time.Since(call.started).Milliseconds(), UpstreamBytes: call.upstreamBytes.Load(), DownstreamBytes: downstreamBytes,
		FirstReadMS: call.firstReadMS.Load(), LastReadAgoMS: -1}
	if last := call.lastReadAt.Load(); last > 0 {
		record.LastReadAgoMS = time.Since(time.Unix(0, last)).Milliseconds()
	}
	encoded, err := json.Marshal(record)
	if err == nil {
		a.identityLogger.Print(string(encoded))
	}
}
