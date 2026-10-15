//go:build unit

package service

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestBPSWebSocketBridgePassesUserModelAndEffort(t *testing.T) {
	for _, model := range ExcelModelIDs() {
		for _, effort := range []string{"low", "medium", "high", "xhigh"} {
			t.Run(model+"/"+effort, func(t *testing.T) {
				cfg := excelTestConfig(t)
				cfg.TransportBackend = "cpa"
				cfg.WebSocketEnabled = true
				data, _ := json.Marshal(cfg)
				require.NoError(t, os.WriteFile(excelConfigPath(), data, 0600))
				s := newAstraOAuthSetup(t, false)
				excelSetRequestGroup(s, 9)
				excelSetResponse(s, model)
				payload := []byte(`{"type":"response.create","model":"` + model + `","input":"hi","reasoning":{"effort":"` + effort + `"}}`)
				_, err := s.svc.proxyOpenAIWSHTTPBridgeTurn(context.Background(), s.c, s.account, "oauth-token", payload, len(payload), model, "", "", "", "", 1, func([]byte) error { return nil })
				require.NoError(t, err)
				require.Equal(t, "websocket", s.upstream.lastReq.Header.Get("X-Sub2-Excel-Transport"))
				require.Equal(t, model, gjson.GetBytes(s.upstream.lastBody, "model").String())
				require.Equal(t, effort, gjson.GetBytes(s.upstream.lastBody, "reasoning.effort").String())
			})
		}
	}
}
func TestBPSWebSocketSelectionIsTrustedAndSSEStaysSSE(t *testing.T) {
	for _, tc := range []struct {
		name, backend    string
		enabled, ingress bool
		want             string
	}{
		{"cpa ws", "cpa", true, true, "websocket"}, {"cpa sse", "cpa", true, false, ""}, {"ws disabled", "cpa", false, true, ""}, {"legacy unchanged", "legacy", true, true, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := excelTestConfig(t)
			cfg.TransportBackend = tc.backend
			cfg.WebSocketEnabled = tc.enabled
			data, _ := json.Marshal(cfg)
			require.NoError(t, os.WriteFile(excelConfigPath(), data, 0600))
			s := newAstraOAuthSetup(t, false)
			excelSetRequestGroup(s, 9)
			excelSetResponse(s, "gpt-5.6-sol")
			ctx, err := prepareExcelRequestContext(context.Background(), s.c, s.account, "gpt-5.6-sol")
			require.NoError(t, err)
			if tc.ingress {
				ctx = context.WithValue(ctx, excelWSBridgeTransportKey{}, true)
			}
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, astraProCodexResponsesURL, strings.NewReader(`{"model":"gpt-5.6-sol","reasoning":{"effort":"high"}}`))
			require.NoError(t, err)
			req.Header.Set("Authorization", "Bearer "+excelBPSToken())
			req.Header.Set("X-Sub2-Excel-Transport", "websocket")
			_, handled, err := s.svc.forwardExcelHTTP(req, "", s.account)
			require.NoError(t, err)
			require.True(t, handled)
			require.Equal(t, tc.want, s.upstream.lastReq.Header.Get("X-Sub2-Excel-Transport"))
		})
	}
}

func TestBPSMarkedTransportFailureRetainsReplayBarrier(t *testing.T) {
	cfg := excelTestConfig(t)
	cfg.TransportBackend = "cpa"
	cfg.WebSocketEnabled = true
	data, _ := json.Marshal(cfg)
	require.NoError(t, os.WriteFile(excelConfigPath(), data, 0600))
	setup := newAstraOAuthSetup(t, false)
	excelSetRequestGroup(setup, 9)
	excelSetResponse(setup, "gpt-5.6-sol")
	setup.upstream.resp.StatusCode = 502
	setup.upstream.resp.Header.Set("X-Sub2-Excel-Failure", "transport")
	setup.upstream.resp.Header.Set("X-Sub2-Excel-Upstream-Started", "true")
	ctx, err := prepareExcelRequestContext(context.Background(), setup.c, setup.account, "gpt-5.6-sol")
	require.NoError(t, err)
	ctx = context.WithValue(ctx, excelWSBridgeTransportKey{}, true)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, astraProCodexResponsesURL, strings.NewReader(`{"model":"gpt-5.6-sol"}`))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+excelBPSToken())
	response, handled, err := setup.svc.forwardExcelHTTP(req, "", setup.account)
	require.NoError(t, err)
	require.True(t, handled)
	require.NotNil(t, response)
	require.Equal(t, "true", response.Header.Get("X-Sub2-Excel-Upstream-Started"))
	response.Body.Close()
	failure := &UpstreamFailoverError{Stage: GatewayFailureStageInference, Scope: GatewayFailureScopeRequest, Reason: "bps_ws_outcome_unknown", NextAccountAction: NextAccountStop, RequestScopedTransient: true}
	require.False(t, failure.ShouldReportAccountScheduleFailure())
	require.False(t, failure.ShouldRetryNextAccount())
	require.True(t, (&UpstreamFailoverError{Stage: GatewayFailureStageInference, Scope: GatewayFailureScopeRequest, Reason: "unrelated_legacy_failure"}).ShouldReportAccountScheduleFailure())
}
