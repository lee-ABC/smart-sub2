//go:build unit

package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// This is a real conversion/forwarding matrix against a synthetic Responses
// upstream, not only a routing-helper test. No external accounts are invoked.
func TestBPSProtocolModelMatrix(t *testing.T) {
	models := append(ExcelModelIDs(), "gpt-5.5")
	for _, model := range models {
		for _, protocol := range []string{"responses", "chat", "messages"} {
			for _, stream := range []bool{false, true} {
				for _, mode := range []string{"native", "excel"} {
					for _, accountType := range []string{AccountTypeOAuth, AccountTypeAPIKey} {
						name := fmt.Sprintf("%s/%s/stream=%t/%s/%s", model, protocol, stream, mode, accountType)
						t.Run(name, func(t *testing.T) {
							cfg := excelTestConfig(t)
							cfg.TransportBackend = "cpa"
							cfg.WebSocketEnabled = true
							cfg.GroupModes["9"] = mode
							configJSON, err := json.Marshal(cfg)
							require.NoError(t, err)
							require.NoError(t, os.WriteFile(excelConfigPath(), configJSON, 0600))
							s := newAstraOAuthSetup(t, false)
							excelSetRequestGroup(s, 9)
							if accountType == AccountTypeAPIKey {
								s.account.Type = AccountTypeAPIKey
								s.account.Credentials = map[string]any{"api_key": "selected-native-api-key"}
								s.account.Extra = map[string]any{"openai_responses_supported": true}
							}
							excelSetResponse(s, model)
							// Native API-key non-streaming responses are already JSON; OAuth and
							// private CPA paths always use Responses SSE before ingress conversion.
							if accountType == AccountTypeAPIKey && !stream && protocol == "responses" {
								payload := `{"id":"resp_matrix","model":"` + model + `","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"OK"}]}],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`
								s.upstream.resp = &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(payload))}
							}
							body := astraRequestBody(model, stream, "", "high")
							path := "/v1/responses"
							if protocol != "responses" {
								body = []byte(fmt.Sprintf(`{"model":%q,"stream":%t,"max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`, model, stream))
								if protocol == "chat" {
									path = "/v1/chat/completions"
								} else {
									path = "/v1/messages"
								}
							}
							s.c.Request = httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
							s.c.Request.Header.Set("Content-Type", "application/json")
							for _, header := range []string{"X-Sub2-Excel-Access-Token", "X-Sub2-Excel-Account-Id", "X-Sub2-Excel-Account-User-Id", "X-Sub2-Excel-Credential-Id", "X-Sub2-Excel-Scope", "X-Sub2-Excel-Proxy"} {
								s.c.Request.Header.Set(header, "spoofed-client-value")
							}
							s.c.Request.Header.Set("Authorization", "Bearer spoofed-inbound-token")
							before, err := json.Marshal(s.account)
							require.NoError(t, err)
							switch protocol {
							case "responses":
								_, err = s.svc.Forward(context.Background(), s.c, s.account, body)
							case "chat":
								_, err = s.svc.ForwardAsChatCompletions(context.Background(), s.c, s.account, body, "", "")
							case "messages":
								_, err = s.svc.ForwardAsAnthropic(context.Background(), s.c, s.account, body, "", "")
							}
							require.NoError(t, err)
							req := s.upstream.lastReq
							require.NotNil(t, req)
							wantBPS := accountType == AccountTypeOAuth && mode == "excel" && model != "gpt-5.5"
							if wantBPS {
								require.Equal(t, "http://excel-sub2api:8000/internal/v1/responses", req.URL.String())
								require.Equal(t, "Bearer "+strings.Repeat("k", 48), req.Header.Get("Authorization"))
								require.Equal(t, excelBPSToken(), req.Header.Get("X-Sub2-Excel-Access-Token"))
								require.Equal(t, "chatgpt-acc", req.Header.Get("X-Sub2-Excel-Account-Id"))
								require.Equal(t, "test-user", req.Header.Get("X-Sub2-Excel-Account-User-Id"))
								require.Equal(t, "888", req.Header.Get("X-Sub2-Excel-Credential-Id"))
								require.Len(t, req.Header.Get("X-Sub2-Excel-Scope"), 64)
							} else {
								wantURL, wantBearer := astraProCodexResponsesURL, "Bearer oauth-token"
								if accountType == AccountTypeAPIKey {
									wantURL, wantBearer = "https://api.openai.com/v1/responses", "Bearer selected-native-api-key"
								}
								require.Equal(t, wantURL, req.URL.String())
								require.Equal(t, wantBearer, req.Header.Get("Authorization"))
								for header := range s.c.Request.Header {
									if strings.HasPrefix(strings.ToLower(header), "x-sub2-excel-") {
										require.Empty(t, req.Header.Get(header), "inbound private header leaked: %s", header)
									}
								}
							}
							require.Empty(t, req.Header.Get("X-Sub2-Excel-Proxy"))
							require.Equal(t, model, gjson.GetBytes(s.upstream.lastBody, "model").String())
							require.NotEmpty(t, s.rec.Body.String())
							if !stream {
								require.Contains(t, s.rec.Body.String(), "OK")
							}
							after, err := json.Marshal(s.account)
							require.NoError(t, err)
							require.JSONEq(t, string(before), string(after), "forwarding must not mutate scheduling/business credentials")
						})
					}
				}
			}
		}
	}
}
