//go:build unit

package service

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

const bpsWSNoReplayMarker = "X-Sub2-Excel-Upstream-Started"
const bpsWSNoReplayHintedError = "data: {\"type\":\"error\",\"error\":{\"type\":\"bps_stream_error\",\"message\":\"Basis Points stream interrupted; temporary relay failure, please retry\"}}\n\n"

func bpsWSNoReplaySetup(t *testing.T, backend string, enabled bool, group int64) *astraForwardSetup {
	t.Helper()
	cfg := excelTestConfig(t)
	cfg.TransportBackend, cfg.WebSocketEnabled = backend, enabled
	raw, err := json.Marshal(cfg)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(excelConfigPath(), raw, 0600))
	s := newAstraOAuthSetup(t, false)
	excelSetRequestGroup(s, group)
	return s
}

func bpsWSNoReplayResponse(status int, marker, body string) *http.Response {
	headers := http.Header{"Content-Type": []string{"text/event-stream"}}
	if marker != "" {
		headers.Set(bpsWSNoReplayMarker, marker)
	}
	return &http.Response{StatusCode: status, Header: headers, Body: io.NopCloser(strings.NewReader(body))}
}

func TestBPSWebSocketSentFailureNeverReplays(t *testing.T) {
	for _, turn := range []int{1, 2} {
		for _, tc := range []struct {
			name         string
			status       int
			body         string
			readErr      bool
			transportErr bool
		}{
			{name: "hinted_disconnect", status: 200, body: bpsWSNoReplayHintedError},
			{name: "early_eof", status: 200},
			{name: "done_without_terminal", status: 200, body: "data: [DONE]\n\n"},
			{name: "read_error", status: 200, readErr: true},
			{name: "failed_unauthorized", status: 200, body: "data: {\"type\":\"response.failed\",\"response\":{\"status\":\"failed\",\"error\":{\"code\":\"invalid_api_key\",\"message\":\"unauthorized\"}}}\n\n"},
			{name: "failed_rate_limit", status: 200, body: "data: {\"type\":\"response.failed\",\"response\":{\"status\":\"failed\",\"error\":{\"code\":\"rate_limit_exceeded\",\"message\":\"limited\"}}}\n\n"},
			{name: "partial_write_transport_error", status: 502, transportErr: true, body: `{"error":{"type":"upstream_transport","message":"selected-account upstream connection failed"}}`},
			{name: "post_send_503", status: 503, body: `{"error":{"type":"server_error","message":"temporary upstream failure"}}`},
			{name: "post_send_429", status: 429, body: `{"error":{"code":"rate_limit_exceeded","message":"limited"}}`},
			{name: "post_send_401", status: 401, body: `{"error":{"code":"invalid_api_key","message":"unauthorized"}}`},
			{name: "post_send_rejected_field", status: 400, body: `{"error":{"type":"invalid_request_error","code":"invalid_parameter","param":"truncation","message":"Unsupported parameter: truncation"}}`},
			{name: "post_send_redirect", status: 307},
		} {
			t.Run(tc.name+map[int]string{1: "/first", 2: "/later"}[turn], func(t *testing.T) {
				s := bpsWSNoReplaySetup(t, "cpa", true, 9)
				response := bpsWSNoReplayResponse(tc.status, "true", tc.body)
				if tc.transportErr {
					response.Header.Set("X-Sub2-Excel-Failure", "transport")
				}
				if tc.readErr {
					response.Body = passthroughErrReadCloser{err: io.ErrUnexpectedEOF}
				}
				s.upstream.responses = []*http.Response{response, bpsWSNoReplayResponse(200, "true", "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n")}
				payload := []byte(`{"type":"response.create","model":"gpt-5.6-sol","input":"hi","truncation":"auto"}`)
				before, err := json.Marshal(s.account)
				require.NoError(t, err)
				writes := 0
				_, err = s.svc.proxyOpenAIWSHTTPBridgeTurn(context.Background(), s.c, s.account, "oauth-token", payload, len(payload), "gpt-5.6-sol", "", "", "", "", turn, func([]byte) error { writes++; return nil })
				var stopped *UpstreamFailoverError
				require.ErrorAs(t, err, &stopped)
				require.Equal(t, GatewayFailureReason("bps_ws_outcome_unknown"), stopped.Reason)
				require.Equal(t, GatewayFailureStageInference, stopped.Stage)
				require.Equal(t, GatewayFailureScopeRequest, stopped.Scope)
				require.False(t, stopped.ShouldRetryNextAccount(), "outer WS handler must not submit another account")
				require.False(t, stopped.RetryableOnSameAccount)
				require.True(t, stopped.SameAccountRetryDeadline.IsZero())
				require.True(t, stopped.RequestScopedTransient)
				require.False(t, stopped.IsCredentialFailure())
				require.False(t, stopped.ShouldReportAccountScheduleFailure())
				_, _, healthFailure := classifyOpenAIAPIKeyHealthFailure(stopped)
				require.False(t, healthFailure)
				require.Len(t, s.upstream.requests, 1, "field-normalization retry must also be blocked")
				require.Zero(t, writes)
				after, marshalErr := json.Marshal(s.account)
				require.NoError(t, marshalErr)
				require.JSONEq(t, string(before), string(after), "unknown outcome is not a credential fault")
			})
		}
	}
}

func TestBPSWebSocketStartedMarkerRequiresTrustedSelectedWSTransport(t *testing.T) {
	for _, tc := range []struct {
		name, backend, marker string
		enabled               bool
		group                 int64
	}{
		{name: "missing_marker", backend: "cpa", enabled: true, group: 9},
		{name: "false_marker", backend: "cpa", enabled: true, group: 9, marker: "false"},
		{name: "invalid_marker", backend: "cpa", enabled: true, group: 9, marker: "1"},
		{name: "legacy_adapter", backend: "legacy", enabled: true, group: 9, marker: "true"},
		{name: "ordinary_sse_transport", backend: "cpa", enabled: false, group: 9, marker: "true"},
		{name: "native_group", backend: "cpa", enabled: true, group: 4, marker: "true"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := bpsWSNoReplaySetup(t, tc.backend, tc.enabled, tc.group)
			s.c.Request.Header.Set(bpsWSNoReplayMarker, "true") // Public spoofing is not upstream-send evidence.
			s.c.Request.Header.Set("X-Sub2-Excel-Transport", "websocket")
			s.upstream.resp = bpsWSNoReplayResponse(200, tc.marker, bpsWSNoReplayHintedError)
			payload := []byte(`{"type":"response.create","model":"gpt-5.6-sol","input":"hi"}`)
			_, err := s.svc.proxyOpenAIWSHTTPBridgeTurn(context.Background(), s.c, s.account, "oauth-token", payload, len(payload), "gpt-5.6-sol", "", "", "", "", 1, func([]byte) error { return nil })
			var failover *UpstreamFailoverError
			require.ErrorAs(t, err, &failover)
			require.True(t, failover.ShouldRetryNextAccount(), "unmarked/non-WS failover must stay unchanged")
			require.NotEqual(t, GatewayFailureReason("bps_ws_outcome_unknown"), failover.Reason)
			require.Len(t, s.upstream.requests, 1)
		})
	}
}

func TestBPSWebSocketUnsentRejectionKeepsExistingRetry(t *testing.T) {
	for _, marked := range []bool{false, true} {
		t.Run(map[bool]string{false: "unmarked", true: "marked"}[marked], func(t *testing.T) {
			s := bpsWSNoReplaySetup(t, "cpa", true, 9)
			marker := ""
			if marked {
				marker = "true"
			}
			s.upstream.responses = []*http.Response{
				bpsWSNoReplayResponse(400, marker, `{"error":{"type":"invalid_request_error","code":"invalid_parameter","param":"truncation","message":"Unsupported parameter: truncation"}}`),
				bpsWSNoReplayResponse(200, "true", "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_retry\",\"model\":\"gpt-5.6-sol\",\"status\":\"completed\",\"output\":[]}}\n\n"),
			}
			payload := []byte(`{"type":"response.create","model":"gpt-5.6-sol","input":"hi","truncation":"auto"}`)
			var writes [][]byte
			result, err := s.svc.proxyOpenAIWSHTTPBridgeTurn(context.Background(), s.c, s.account, "oauth-token", payload, len(payload), "gpt-5.6-sol", "", "", "", "", 1, func(message []byte) error { writes = append(writes, append([]byte(nil), message...)); return nil })
			if marked {
				var stopped *UpstreamFailoverError
				require.ErrorAs(t, err, &stopped)
				require.False(t, stopped.ShouldRetryNextAccount())
				require.Len(t, s.upstream.requests, 1)
				require.Empty(t, writes)
			} else {
				require.NoError(t, err)
				require.NotNil(t, result)
				require.Len(t, s.upstream.requests, 2)
				require.False(t, gjson.GetBytes(s.upstream.bodies[1], "truncation").Exists())
				require.Len(t, writes, 1)
			}
		})
	}
}

func TestBPSWebSocketStartedMarkerDoesNotChangeSuccessfulCompletion(t *testing.T) {
	s := bpsWSNoReplaySetup(t, "cpa", true, 9)
	excelSetResponse(s, "gpt-5.6-sol")
	s.upstream.resp.Header.Set(bpsWSNoReplayMarker, "true")
	payload := []byte(`{"type":"response.create","model":"gpt-5.6-sol","input":"hi"}`)
	var writes [][]byte
	result, err := s.svc.proxyOpenAIWSHTTPBridgeTurn(context.Background(), s.c, s.account, "oauth-token", payload, len(payload), "gpt-5.6-sol", "", "", "", "", 1, func(message []byte) error { writes = append(writes, append([]byte(nil), message...)); return nil })
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, "response.completed", result.UpstreamTerminalEvent)
	require.Len(t, s.upstream.requests, 1)
	require.Len(t, writes, 1)
	require.Equal(t, "response.completed", gjson.GetBytes(writes[0], "type").String())
}

func TestBPSWebSocketNoReplayMarkerCannotBeForgedWithoutBridgeContext(t *testing.T) {
	cfg := &ExcelRoutingConfig{TransportBackend: "cpa", WebSocketEnabled: true}
	ctx := context.WithValue(context.Background(), excelRequestRouteKey{}, &excelRequestRoute{config: cfg})
	response := bpsWSNoReplayResponse(http.StatusOK, "true", "")
	require.False(t, bpsWSUpstreamStarted(ctx, response))
	ctx = context.WithValue(ctx, excelWSBridgeTransportKey{}, false)
	require.False(t, bpsWSUpstreamStarted(ctx, response))
	ctx = context.WithValue(ctx, excelWSBridgeTransportKey{}, true)
	require.True(t, bpsWSUpstreamStarted(ctx, response))
	require.False(t, bpsWSUpstreamStarted(ctx, nil))
	require.False(t, bpsWSUpstreamStarted(nil, response))
}

func TestBPSWebSocketSentPartialOutputFailureNeverReplays(t *testing.T) {
	for _, ending := range []string{"", bpsWSNoReplayHintedError} {
		t.Run(map[bool]string{true: "error_event", false: "eof"}[ending != ""], func(t *testing.T) {
			s := bpsWSNoReplaySetup(t, "cpa", true, 9)
			s.upstream.resp = bpsWSNoReplayResponse(200, "true", "data: {\"type\":\"response.output_text.delta\",\"delta\":\"partial\"}\n\n"+ending)
			payload := []byte(`{"type":"response.create","model":"gpt-5.6-sol","input":"hi"}`)
			writes := 0
			result, err := s.svc.proxyOpenAIWSHTTPBridgeTurn(context.Background(), s.c, s.account, "oauth-token", payload, len(payload), "gpt-5.6-sol", "", "", "", "", 1, func([]byte) error { writes++; return nil })
			var stopped *UpstreamFailoverError
			require.ErrorAs(t, err, &stopped)
			require.NotNil(t, result)
			require.False(t, stopped.ShouldRetryNextAccount())
			require.False(t, stopped.RetryableOnSameAccount)
			require.False(t, stopped.ShouldReportAccountScheduleFailure())
			require.Equal(t, 1, writes)
			require.Len(t, s.upstream.requests, 1)
		})
	}
}

func TestBPSWebSocketSentFirstOutputStagingLimitNeverReplays(t *testing.T) {
	s := bpsWSNoReplaySetup(t, "cpa", true, 9)
	event := "data: {\"type\":\"response.created\",\"padding\":\"" + strings.Repeat("x", openAIFirstOutputStageMaxBytes) + "\"}\n\n"
	s.upstream.resp = bpsWSNoReplayResponse(200, "true", event)
	payload := []byte(`{"type":"response.create","model":"gpt-5.6-sol","input":"hi"}`)
	writes := 0
	_, err := s.svc.proxyOpenAIWSHTTPBridgeTurn(context.Background(), s.c, s.account, "oauth-token", payload, len(payload), "gpt-5.6-sol", "", "", "", "", 1, func([]byte) error { writes++; return nil })
	var stopped *UpstreamFailoverError
	require.ErrorAs(t, err, &stopped)
	require.Equal(t, GatewayFailureReason("bps_ws_outcome_unknown"), stopped.Reason)
	require.False(t, stopped.ShouldRetryNextAccount())
	require.False(t, stopped.RetryableOnSameAccount)
	require.Len(t, s.upstream.requests, 1)
	require.Zero(t, writes)
}

func TestBPSWebSocketUnsentHandshakeRejectionRetainsFailover(t *testing.T) {
	for _, status := range []int{http.StatusForbidden, http.StatusServiceUnavailable} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			s := bpsWSNoReplaySetup(t, "cpa", true, 9)
			s.upstream.resp = bpsWSNoReplayResponse(status, "", `{"error":{"type":"bps_ws_handshake","message":"Basis Points WebSocket handshake rejected"}}`)
			payload := []byte(`{"type":"response.create","model":"gpt-5.6-sol","input":"hi"}`)
			_, err := s.svc.proxyOpenAIWSHTTPBridgeTurn(context.Background(), s.c, s.account, "oauth-token", payload, len(payload), "gpt-5.6-sol", "", "", "", "", 1, func([]byte) error { return nil })
			var failover *UpstreamFailoverError
			require.ErrorAs(t, err, &failover)
			require.True(t, failover.ShouldRetryNextAccount())
			require.NotEqual(t, GatewayFailureReason("bps_ws_outcome_unknown"), failover.Reason)
			require.Len(t, s.upstream.requests, 1)
		})
	}
}
