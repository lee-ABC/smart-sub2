//go:build unit

package service

import (
	"context"
	"encoding/json"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func excelTestConfig(t *testing.T) *ExcelRoutingConfig {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "routing.json")
	keyfile := filepath.Join(dir, "transport-key")
	t.Setenv("SUB2_EXCEL_CONFIG_FILE", path)
	require.NoError(t, os.WriteFile(keyfile, []byte(strings.Repeat("k", 48)), 0600))
	cfg := &ExcelRoutingConfig{GatewayURL: "http://excel-sub2api:8000/internal/v1", TransportKeyFile: keyfile, GroupModes: map[string]string{"9": "excel"}}
	raw, err := json.Marshal(cfg)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, raw, 0600))
	return cfg
}
func TestExcelConfigIsolationAndAtomicSave(t *testing.T) {
	excelTestConfig(t)
	require.NoError(t, SetExcelGroupMode(12, "excel"))
	require.NoError(t, SetExcelGroupMode(9, "native"))
	cfg, err := ReadExcelRoutingConfig()
	require.NoError(t, err)
	require.Equal(t, "native", cfg.Mode(9))
	require.Equal(t, "excel", cfg.Mode(12))
	require.Equal(t, "native", cfg.Mode(4))
	require.Error(t, SetExcelGroupMode(12, "anything"))
	cfg, err = ReadExcelRoutingConfig()
	require.NoError(t, err)
	require.Equal(t, "excel", cfg.Mode(12))
	require.NoError(t, os.WriteFile(excelConfigPath(), []byte("invalid"), 0600))
	_, err = ReadExcelRoutingConfig()
	require.Error(t, err)
}
func TestExcelConfigMissingDefaultsNative(t *testing.T) {
	t.Setenv("SUB2_EXCEL_CONFIG_FILE", filepath.Join(t.TempDir(), "absent.json"))
	cfg, err := ReadExcelRoutingConfig()
	require.NoError(t, err)
	require.Equal(t, "native", cfg.Mode(9))
	require.Error(t, SetExcelGroupMode(9, "excel"))
}
func TestExcelConfigRejectsWrongTransportPath(t *testing.T) {
	cfg := excelTestConfig(t)
	cfg.GatewayURL = "http://sub2api:8080/v1"
	_, err := cfg.TransportKey()
	require.Error(t, err)
}
func excelSetRequestGroup(s *astraForwardSetup, group int64) {
	s.c.Set("api_key", &APIKey{ID: 71, GroupID: &group})
}
func excelSetResponse(s *astraForwardSetup, model string) {
	inner := `{"id":"resp_excel","model":"` + model + `","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"OK"}]}],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`
	s.upstream.resp = &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(codexCompletedSSE(inner)))}
}
func TestExcelSelectedAccountResponses(t *testing.T) {
	for _, passthrough := range []bool{false, true} {
		for _, stream := range []bool{false, true} {
			t.Run(map[bool]string{true: "passthrough", false: "transformed"}[passthrough]+map[bool]string{true: "stream", false: "json"}[stream], func(t *testing.T) {
				excelTestConfig(t)
				s := newAstraOAuthSetup(t, passthrough)
				excelSetRequestGroup(s, 9)
				excelSetResponse(s, "gpt-6-sol")
				s.c.Request.Header.Set("X-Sub2-Excel-Access-Token", "spoofed")
				s.c.Request.Header.Set("X-Sub2-Excel-Account-Id", "wrong")
				before, err := json.Marshal(s.account)
				require.NoError(t, err)
				result, err := s.svc.Forward(context.Background(), s.c, s.account, astraRequestBody("gpt-6-sol", stream, "", "high"))
				require.NoError(t, err)
				require.NotNil(t, result)
				req := s.upstream.lastReq
				require.Equal(t, "http://excel-sub2api:8000/internal/v1/responses", req.URL.String())
				require.Equal(t, "Bearer "+strings.Repeat("k", 48), req.Header.Get("Authorization"))
				require.Equal(t, "oauth-token", req.Header.Get("X-Sub2-Excel-Access-Token"))
				require.Equal(t, "chatgpt-acc", req.Header.Get("X-Sub2-Excel-Account-Id"))
				require.Len(t, req.Header.Get("X-Sub2-Excel-Scope"), 64)
				require.Equal(t, "", s.upstream.lastProxyURL)
				require.Equal(t, "gpt-6-sol", gjson.GetBytes(s.upstream.lastBody, "model").String())
				after, err := json.Marshal(s.account)
				require.NoError(t, err)
				require.JSONEq(t, string(before), string(after))
			})
		}
	}
}
func TestExcelNativeGroupUnchanged(t *testing.T) {
	excelTestConfig(t)
	s := newAstraOAuthSetup(t, false)
	excelSetRequestGroup(s, 4)
	excelSetResponse(s, "gpt-5.6-sol")
	_, err := s.svc.Forward(context.Background(), s.c, s.account, astraRequestBody("gpt-5.6-sol", false, "", "high"))
	require.NoError(t, err)
	require.Equal(t, astraProCodexResponsesURL, s.upstream.lastReq.URL.String())
	require.Equal(t, "Bearer oauth-token", s.upstream.lastReq.Header.Get("Authorization"))
	require.Empty(t, s.upstream.lastReq.Header.Get("X-Sub2-Excel-Access-Token"))
}
func TestExcelScopeDiffersAcrossAccountsAndGroups(t *testing.T) {
	cfg := excelTestConfig(t)
	cfg.GroupModes["12"] = "excel"
	raw, _ := json.Marshal(cfg)
	require.NoError(t, os.WriteFile(excelConfigPath(), raw, 0600))
	scopes := map[string]bool{}
	for _, group := range []int64{9, 12} {
		for _, accountID := range []int64{100, 101} {
			s := newAstraOAuthSetup(t, false)
			s.account.ID = accountID
			excelSetRequestGroup(s, group)
			excelSetResponse(s, "gpt-6-sol")
			ctx, err := prepareExcelRequestContext(context.Background(), s.c, s.account, "gpt-6-sol")
			require.NoError(t, err)
			req, err := http.NewRequestWithContext(ctx, "POST", astraProCodexResponsesURL, strings.NewReader(`{"model":"gpt-6-sol"}`))
			require.NoError(t, err)
			req.Header.Set("Authorization", "Bearer oauth-token")
			_, handled, err := s.svc.forwardExcelHTTP(req, "http://account-proxy:8888", s.account)
			require.NoError(t, err)
			require.True(t, handled)
			require.Empty(t, s.upstream.lastProxyURL)
			require.Equal(t, "http://account-proxy:8888", s.upstream.lastReq.Header.Get("X-Sub2-Excel-Proxy"))
			scope := s.upstream.lastReq.Header.Get("X-Sub2-Excel-Scope")
			require.False(t, scopes[scope])
			scopes[scope] = true
		}
	}
}
func TestExcelPrivateHeadersRedacted(t *testing.T) {
	for _, name := range []string{"X-Sub2-Excel-Access-Token", "X-Sub2-Excel-Proxy", "X-Sub2-Excel-Account-Id"} {
		require.Equal(t, "[redacted]", safeHeaderValueForLog(name, "secret"))
	}
}
func TestExcelChatAndMessagesProtocols(t *testing.T) {
	for _, protocol := range []string{"chat", "messages"} {
		t.Run(protocol, func(t *testing.T) {
			excelTestConfig(t)
			s := newAstraOAuthSetup(t, false)
			excelSetRequestGroup(s, 9)
			excelSetResponse(s, "gpt-6-sol")
			body := []byte(`{"model":"gpt-6-sol","max_tokens":32,"messages":[{"role":"user","content":"hi"}],"stream":false}`)
			var err error
			if protocol == "chat" {
				_, err = s.svc.ForwardAsChatCompletions(context.Background(), s.c, s.account, body, "", "")
			} else {
				_, err = s.svc.ForwardAsAnthropic(context.Background(), s.c, s.account, body, "", "")
			}
			require.NoError(t, err)
			require.Equal(t, "http://excel-sub2api:8000/internal/v1/responses", s.upstream.lastReq.URL.String())
			require.Contains(t, s.rec.Body.String(), "OK")
		})
	}
}

func TestExcelWebSocketHTTPBridgeUsesSelectedAccount(t *testing.T) {
	excelTestConfig(t)
	s := newAstraOAuthSetup(t, false)
	excelSetRequestGroup(s, 9)
	excelSetResponse(s, "gpt-6-sol")
	ctx, err := prepareExcelRequestContext(context.Background(), s.c, s.account, "gpt-6-sol")
	require.NoError(t, err)
	payload := []byte(`{"type":"response.create","model":"gpt-6-sol","stream":true,"input":"hi"}`)
	var events []string
	result, err := s.svc.proxyOpenAIWSHTTPBridgeTurn(ctx, s.c, s.account, "oauth-token", payload, len(payload), "gpt-6-sol", "", "", "", "", 1, func(event []byte) error { events = append(events, string(event)); return nil })
	require.NoError(t, err)
	require.NotNil(t, result)
	require.NotEmpty(t, events)
	require.Equal(t, "http://excel-sub2api:8000/internal/v1/responses", s.upstream.lastReq.URL.String())
}
func TestExcelTransportFailureDoesNotBlameAccount(t *testing.T) {
	s := newAstraOAuthSetup(t, false)
	err := s.svc.handleOpenAIUpstreamTransportError(context.Background(), s.c, s.account, &excelTransportError{}, false)
	var failure *UpstreamFailoverError
	require.ErrorAs(t, err, &failure)
	require.False(t, failure.ShouldReportAccountScheduleFailure())
	require.False(t, failure.ShouldRetryNextAccount())
	require.Equal(t, 503, failure.ClientStatusCode)
}

func TestExcelGroupNativeChatAndMessagesPreserved(t *testing.T) {
	for _, protocol := range []string{"chat", "messages"} {
		t.Run(protocol, func(t *testing.T) {
			excelTestConfig(t)
			s := newAstraOAuthSetup(t, false)
			excelSetRequestGroup(s, 9)
			excelSetResponse(s, "gpt-5.5")
			body := []byte(`{"model":"gpt-5.5","max_tokens":32,"messages":[{"role":"user","content":"hi"}],"stream":false}`)
			var err error
			if protocol == "chat" {
				_, err = s.svc.ForwardAsChatCompletions(context.Background(), s.c, s.account, body, "", "")
			} else {
				_, err = s.svc.ForwardAsAnthropic(context.Background(), s.c, s.account, body, "", "")
			}
			require.NoError(t, err)
			require.NotEqual(t, "excel-sub2api:8000", s.upstream.lastReq.URL.Host)
			require.Contains(t, s.rec.Body.String(), "OK")
		})
	}
}

func TestExcelGroupNativeResponsesIgnoreSidecarFailure(t *testing.T) {
	for _, passthrough := range []bool{false, true} {
		for _, stream := range []bool{false, true} {
			t.Run(fmtTestRouteName(passthrough, 9)+fmtTestRouteName(stream, 4), func(t *testing.T) {
				cfg := excelTestConfig(t)
				require.NoError(t, os.Remove(cfg.TransportKeyFile))
				f := newAstraOAuthSetup(t, passthrough)
				excelSetRequestGroup(f, 9)
				excelSetResponse(f, "gpt-5.5")
				_, err := f.svc.Forward(context.Background(), f.c, f.account, astraRequestBody("gpt-5.5", stream, "", "high"))
				require.NoError(t, err)
				require.NotEqual(t, "excel-sub2api:8000", f.upstream.lastReq.URL.Host)
				require.Equal(t, "gpt-5.5", gjson.GetBytes(f.upstream.lastBody, "model").String())
				require.Empty(t, f.upstream.lastReq.Header.Get("X-Sub2-Excel-Access-Token"))
			})
		}
	}
}
func TestExcelModelBoundary(t *testing.T) {
	cfg := excelTestConfig(t)
	f := newAstraOAuthSetup(t, false)
	excelSetRequestGroup(f, 9)
	ctx, err := prepareExcelRequestContext(context.Background(), f.c, f.account, "gpt-6-sol")
	require.NoError(t, err)
	require.True(t, isExcelRequest(ctx))
	require.NoError(t, os.Remove(cfg.TransportKeyFile))
	ctx, err = prepareExcelRequestContext(ctx, f.c, f.account, "gpt-5.5")
	require.NoError(t, err)
	require.False(t, isExcelRequest(ctx))
	for _, model := range []string{"gpt-5.5", "gpt-5.4", "gpt-image-2", "future-native-model"} {
		require.False(t, IsExcelSupportedModel(model))
	}
}
