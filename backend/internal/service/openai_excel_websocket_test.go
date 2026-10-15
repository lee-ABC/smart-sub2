//go:build unit

package service

import (
	"context"
	"encoding/json"
	"github.com/stretchr/testify/require"
	"os"
	"testing"
)

func TestExcelWebSocketTextEligibility(t *testing.T) {
	cases := []struct {
		name, body string
		want       bool
	}{
		{"string", `{"input":"hello"}`, true},
		{"message text", `{"input":[{"role":"user","content":[{"type":"input_text","text":"hello"}]}]}`, true},
		{"tools", `{"input":"hello","tools":[{"type":"function","name":"x"}]}`, false},
		{"image", `{"input":[{"role":"user","content":[{"type":"input_image","image_url":"x"}]}]}`, false},
		{"previous response", `{"input":"hello","previous_response_id":"resp_old"}`, false},
		{"compaction", `{"input":[{"type":"compaction","encrypted_content":"x"}]}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) { require.Equal(t, tc.want, excelWebSocketTextRequest([]byte(tc.body))) })
	}
}

func TestExcelWebSocketOptInIsScopedAndNativePreserved(t *testing.T) {
	cfg := excelTestConfig(t)
	cfg.WebSocketEnabled = true
	raw, err := json.Marshal(cfg)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(excelConfigPath(), raw, 0600))
	s := newAstraOAuthSetup(t, false)
	excelSetRequestGroup(s, 9)
	ctx, err := prepareExcelRequestContext(context.Background(), s.c, s.account, "gpt-6-sol")
	require.NoError(t, err)
	enableExcelWebSocket(ctx, []byte(`{"model":"gpt-6-sol","input":"hello"}`))
	require.True(t, isExcelWebSocket(ctx))
	url, err := s.svc.buildOpenAIResponsesWSURLForContext(ctx, s.account)
	require.NoError(t, err)
	require.Equal(t, excelResponsesWebSocketURL, url)

	nativeCtx, err := prepareExcelRequestContext(context.Background(), s.c, s.account, "gpt-5.5")
	require.NoError(t, err)
	enableExcelWebSocket(nativeCtx, []byte(`{"model":"gpt-5.5","input":"hello"}`))
	require.False(t, isExcelWebSocket(nativeCtx))
	nativeURL, err := s.svc.buildOpenAIResponsesWSURLForContext(nativeCtx, s.account)
	require.NoError(t, err)
	require.NotEqual(t, excelResponsesWebSocketURL, nativeURL)
}
