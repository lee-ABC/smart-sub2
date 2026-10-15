//go:build unit

package service

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func syntheticBPSToken(subject, workspace, user string, expires int64) string {
	b, _ := json.Marshal(map[string]any{"client_id": BPSOAuthClientID, "sub": subject, "exp": expires, "https://api.openai.com/auth": map[string]any{"chatgpt_account_id": workspace, "chatgpt_user_id": user}})
	return "e30." + base64.RawURLEncoding.EncodeToString(b) + ".test"
}
func excelBPSToken() string {
	return syntheticBPSToken("test-subject", "chatgpt-acc", "test-user", 4102444800)
}
func excelAttachBPS(a *Account) {
	a.Credentials[BPSCredentialKey] = map[string]any{"access_token": excelBPSToken(), "client_id": BPSOAuthClientID, "chatgpt_account_id": "chatgpt-acc", "chatgpt_user_id": "test-user"}
}

func TestBPSRoutingMissingProfileNeverUsesNative(t *testing.T) {
	excelTestConfig(t)
	s := newAstraOAuthSetup(t, false)
	excelSetRequestGroup(s, 9)
	delete(s.account.Credentials, BPSCredentialKey)
	_, err := s.svc.Forward(context.Background(), s.c, s.account, astraRequestBody("gpt-6-sol", false, "", "high"))
	require.Error(t, err)
	require.Nil(t, s.upstream.lastReq)
}
func TestBPSRoutingFreezeGroupPerAttemptAndSwitchModel(t *testing.T) {
	cfg := excelTestConfig(t)
	cfg.TransportBackend = "cpa"
	cfg.WebSocketEnabled = true
	raw, _ := json.Marshal(cfg)
	require.NoError(t, os.WriteFile(excelConfigPath(), raw, 0600))
	s := newAstraOAuthSetup(t, false)
	excelSetRequestGroup(s, 9)
	ctx, err := prepareExcelRequestContext(context.Background(), s.c, s.account, "gpt-6-sol")
	require.NoError(t, err)
	require.True(t, isExcelRequest(ctx))
	enableExcelWebSocket(ctx, astraRequestBody("gpt-6-sol", true, "", "high"))
	require.False(t, isExcelWebSocket(ctx))
	require.NoError(t, SetExcelGroupMode(9, "native"))
	same, err := prepareExcelRequestContext(ctx, s.c, s.account, "gpt-6-sol")
	require.NoError(t, err)
	require.True(t, isExcelRequest(same))
	token, _, err := s.svc.GetAccessToken(same, s.account)
	require.NoError(t, err)
	require.Equal(t, excelBPSToken(), token)
	native, err := prepareExcelRequestContext(ctx, s.c, s.account, "gpt-5.5")
	require.NoError(t, err)
	require.False(t, isExcelRequest(native))
	token, _, err = s.svc.GetAccessToken(native, s.account)
	require.NoError(t, err)
	require.Equal(t, "oauth-token", token)
	fresh, err := prepareExcelRequestContext(context.Background(), s.c, s.account, "gpt-6-sol")
	require.NoError(t, err)
	require.False(t, isExcelRequest(fresh))
}
func TestBPSRoutingNativeCacheRejectsPriorMisplacedBPS(t *testing.T) {
	a := &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Credentials: map[string]any{"access_token": "native-good", "expires_at": time.Now().Add(time.Hour).Format(time.RFC3339)}}
	cache := newOpenAITokenCacheStub()
	cache.tokens[OpenAITokenCacheKey(a)] = excelBPSToken()
	p := NewOpenAITokenProvider(nil, cache, nil)
	token, err := p.GetAccessToken(context.Background(), a)
	require.NoError(t, err)
	require.Equal(t, "native-good", token)
}
