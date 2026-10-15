//go:build unit

package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
	"github.com/stretchr/testify/require"
)

type bpsTestOAuthClient struct {
	OpenAIOAuthClient
	response   *openai.TokenResponse
	err        error
	calls      int
	rt, client string
}

func (o *bpsTestOAuthClient) RefreshTokenWithClientID(_ context.Context, rt, proxy, client string) (*openai.TokenResponse, error) {
	o.calls++
	o.rt = rt
	o.client = client
	return o.response, o.err
}

type bpsTestCache struct {
	*openAITokenCacheStub
	lockKey string
}

func (c *bpsTestCache) AcquireRefreshLock(ctx context.Context, key string, ttl time.Duration) (bool, error) {
	c.lockKey = key
	return c.openAITokenCacheStub.AcquireRefreshLock(ctx, key, ttl)
}
func bpsTestAccount() *Account {
	return &Account{ID: 13, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive, Credentials: map[string]any{"access_token": "native-at", "refresh_token": "native-rt", "chatgpt_account_id": "workspace", "chatgpt_user_id": "user", BPSCredentialKey: map[string]any{"access_token": syntheticBPSToken("sub", "workspace", "user", time.Now().Add(time.Minute).Unix()), "refresh_token": "bps-rt", "client_id": BPSOAuthClientID}}}
}
func TestBPSCredentialsRejectMissingAndConflictingIdentity(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*Account)
	}{
		{"missing", func(a *Account) { delete(a.Credentials, BPSCredentialKey) }},
		{"wrong-client", func(a *Account) {
			a.Credentials[BPSCredentialKey].(map[string]any)["access_token"] = "e30.eyJjbGllbnRfaWQiOiJuYXRpdmUifQ.test"
		}},
		{"workspace", func(a *Account) { a.Credentials["chatgpt_account_id"] = "other" }},
		{"user", func(a *Account) { a.Credentials["chatgpt_user_id"] = "other" }},
		{"profile-client", func(a *Account) { a.Credentials[BPSCredentialKey].(map[string]any)["client_id"] = "native-client" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := bpsTestAccount()
			tc.change(a)
			_, err := BPSCredentialsForAccount(a)
			require.Error(t, err)
		})
	}
	a := bpsTestAccount()
	profile, err := BPSCredentialsForAccount(a)
	require.NoError(t, err)
	profile["refresh_token"] = "changed"
	require.Equal(t, "bps-rt", a.Credentials[BPSCredentialKey].(map[string]any)["refresh_token"])
	a.Credentials["access_token"] = excelBPSToken()
	require.Error(t, NativeOAuthCredentialError(a))
	delete(a.Credentials, "access_token")
	require.Error(t, NativeOAuthCredentialError(a))
}
func TestBPSTokenRefreshIndependentClientRTCacheAndNative(t *testing.T) {
	a := bpsTestAccount()
	repo := &adminBPSRepoStub{account: a}
	cache := &bpsTestCache{openAITokenCacheStub: newOpenAITokenCacheStub()}
	cache.tokens[OpenAITokenCacheKey(a)] = "native-cache"
	newAT := syntheticBPSToken("sub", "workspace", "user", time.Now().Add(time.Hour).Unix())
	client := &bpsTestOAuthClient{response: &openai.TokenResponse{AccessToken: newAT, RefreshToken: "bps-rotated", ExpiresIn: 3600}}
	provider := NewOpenAITokenProvider(repo, cache, NewOpenAIOAuthService(nil, client))
	token, err := provider.GetBPSAccessToken(context.Background(), a)
	require.NoError(t, err)
	require.Equal(t, newAT, token)
	require.Equal(t, 1, client.calls)
	require.Equal(t, "bps-rt", client.rt)
	require.Equal(t, BPSOAuthClientID, client.client)
	require.Equal(t, "openai:bps:account:13", cache.lockKey)
	require.Zero(t, cache.getCalled)
	require.Zero(t, cache.setCalled)
	require.Equal(t, "native-cache", cache.tokens[OpenAITokenCacheKey(a)])
	require.Equal(t, "native-at", a.GetOpenAIAccessToken())
	require.Equal(t, "native-rt", a.GetOpenAIRefreshToken())
	require.Equal(t, "bps-rotated", a.Credentials[BPSCredentialKey].(map[string]any)["refresh_token"])
	require.Equal(t, 2, repo.calls)
}
func TestBPSTokenFailedRefreshPersistsPendingWithoutRepeating(t *testing.T) {
	a := bpsTestAccount()
	repo := &adminBPSRepoStub{account: a}
	cache := &bpsTestCache{openAITokenCacheStub: newOpenAITokenCacheStub()}
	client := &bpsTestOAuthClient{err: errors.New("synthetic timeout")}
	provider := NewOpenAITokenProvider(repo, cache, NewOpenAIOAuthService(nil, client))
	token, err := provider.GetBPSAccessToken(context.Background(), a)
	require.NoError(t, err)
	require.NotEmpty(t, token)
	profile := a.Credentials[BPSCredentialKey].(map[string]any)
	require.Equal(t, "outcome_unknown_or_failed", profile["refresh_pending"].(map[string]any)["reason"])
	_, err = provider.GetBPSAccessToken(context.Background(), a)
	require.NoError(t, err)
	require.Equal(t, 1, client.calls)
	require.Equal(t, 2, repo.calls)
}
func TestBPSTokenWorkspaceDriftQuarantinesButPreservesRotatedRT(t *testing.T) {
	a := bpsTestAccount()
	original := a.Credentials[BPSCredentialKey].(map[string]any)["access_token"]
	repo := &adminBPSRepoStub{account: a}
	cache := &bpsTestCache{openAITokenCacheStub: newOpenAITokenCacheStub()}
	client := &bpsTestOAuthClient{response: &openai.TokenResponse{AccessToken: syntheticBPSToken("sub", "personal", "user", time.Now().Add(time.Hour).Unix()), RefreshToken: "bps-rotated", ExpiresIn: 3600}}
	provider := NewOpenAITokenProvider(repo, cache, NewOpenAIOAuthService(nil, client))
	token, err := provider.GetBPSAccessToken(context.Background(), a)
	require.Error(t, err)
	require.Empty(t, token)
	profile := a.Credentials[BPSCredentialKey].(map[string]any)
	require.Equal(t, original, profile["access_token"])
	require.Equal(t, "bps-rotated", profile["refresh_token"])
	require.Contains(t, profile, "workspace_refresh_pending")
	require.Equal(t, "native-at", a.GetOpenAIAccessToken())
	require.Equal(t, "native-rt", a.GetOpenAIRefreshToken())
	_, err = provider.GetBPSAccessToken(context.Background(), a)
	require.Error(t, err)
	require.Equal(t, 1, client.calls)
}
func TestBPSTokenReadbackNeverSwitchesSelectedIdentity(t *testing.T) {
	a := bpsTestAccount()
	repo := &adminBPSRepoStub{account: a}
	p := NewOpenAITokenProvider(repo, nil, nil)
	expected, _ := ParseBPSCredentialClaims(syntheticBPSToken("sub", "other", "user", time.Now().Add(time.Hour).Unix()))
	_, err := p.readCurrentBPSToken(context.Background(), 13, expected)
	require.ErrorContains(t, err, "account_changed")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = p.GetBPSAccessToken(ctx, a)
	require.ErrorIs(t, err, context.Canceled)
	require.Zero(t, repo.calls)
}
