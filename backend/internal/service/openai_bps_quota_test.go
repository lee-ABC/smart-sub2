//go:build unit

package service

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
	"github.com/stretchr/testify/require"
)

func bpsQuotaTestAccount() *Account {
	a := bpsTestAccount()
	delete(a.Credentials, "access_token")
	delete(a.Credentials, "refresh_token")
	a.Credentials[BPSCredentialKey].(map[string]any)["access_token"] = syntheticBPSToken("sub", "workspace", "user", time.Now().Add(time.Hour).Unix())
	return a
}

func TestBPSQuotaReadUsesIsolatedToken(t *testing.T) {
	a := bpsQuotaTestAccount()
	before, err := json.Marshal(a.Credentials)
	require.NoError(t, err)
	token := a.Credentials[BPSCredentialKey].(map[string]any)["access_token"].(string)
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("unexpected method: %s", r.Method)
		}
		if r.Header.Get("Authorization") != "Bearer "+token {
			t.Error("wrong credential namespace")
		}
		if r.Header.Get("ChatGPT-Account-ID") != "workspace" {
			t.Error("wrong workspace")
		}
		paths = append(paths, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/backend-api/wham/usage" {
			_, _ = w.Write([]byte(`{"plan_type":"team","rate_limit":{"allowed":true,"primary_window":{"used_percent":11,"limit_window_seconds":604800,"reset_after_seconds":600000},"secondary_window":null},"rate_limit_reset_credits":{"available_count":0}}`))
		} else {
			_, _ = w.Write([]byte(`{"available_count":0,"credits":[]}`))
		}
	}))
	defer srv.Close()
	repo := &stubQuotaAccountRepo{accounts: map[int64]*Account{a.ID: a}}
	cache := newOpenAITokenCacheStub()
	svc := NewOpenAIQuotaService(repo, nil, NewOpenAITokenProvider(repo, cache, nil), newQuotaRedirectingFactory(srv))
	got, err := svc.QueryUsage(context.Background(), a.ID)
	require.NoError(t, err)
	require.NotNil(t, got.RateLimit)
	require.Equal(t, float64(11), got.RateLimit.PrimaryWindow.UsedPercent)
	require.Nil(t, got.RateLimit.SecondaryWindow)
	require.NotNil(t, got.RateLimitResetCredits)
	require.Zero(t, got.RateLimitResetCredits.AvailableCount)
	require.Equal(t, []string{"/backend-api/wham/usage", "/backend-api/wham/rate-limit-reset-credits"}, paths)
	require.Zero(t, cache.getCalled)
	require.Zero(t, cache.setCalled)
	after, err := json.Marshal(a.Credentials)
	require.NoError(t, err)
	require.Equal(t, before, after)
}

func TestBPSQuotaReadDoesNotEnableCreditConsumption(t *testing.T) {
	a := bpsQuotaTestAccount()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected upstream call: %s %s", r.Method, r.URL.Path)
		w.WriteHeader(500)
	}))
	defer srv.Close()
	repo := &stubQuotaAccountRepo{accounts: map[int64]*Account{a.ID: a}}
	svc := NewOpenAIQuotaService(repo, nil, NewOpenAITokenProvider(repo, nil, nil), newQuotaRedirectingFactory(srv))
	_, err := svc.ResetCredit(context.Background(), a.ID)
	require.ErrorContains(t, err, "native OAuth credentials are missing")
	_, err = svc.ResetCreditTargeted(context.Background(), a.ID, "credit", "request")
	require.ErrorContains(t, err, "native OAuth credentials are missing")
}

func TestBPSQuotaCredentialSelection(t *testing.T) {
	cases := []struct {
		name    string
		change  func(*Account)
		wantErr string
		native  bool
	}{
		{name: "profile workspace without root identity", change: func(a *Account) {
			delete(a.Credentials, "chatgpt_account_id")
			delete(a.Credentials, "chatgpt_user_id")
		}},
		{name: "workspace mismatch", change: func(a *Account) { a.Credentials["chatgpt_account_id"] = "other" }, wantErr: "workspace_mismatch"},
		{name: "user mismatch", change: func(a *Account) { a.Credentials["chatgpt_user_id"] = "other" }, wantErr: "user_mismatch"},
		{name: "malformed profile", change: func(a *Account) { a.Credentials[BPSCredentialKey] = "bad" }, wantErr: "missing_profile"},
		{name: "workspace refresh quarantined", change: func(a *Account) { a.Credentials[BPSCredentialKey].(map[string]any)["workspace_refresh_pending"] = true }, wantErr: "workspace_refresh_pending"},
		{name: "expired without RT", change: func(a *Account) {
			p := a.Credentials[BPSCredentialKey].(map[string]any)
			p["access_token"] = syntheticBPSToken("sub", "workspace", "user", time.Now().Add(-time.Minute).Unix())
			delete(p, "refresh_token")
		}, wantErr: "expired"},
		{name: "dual prefers native", change: func(a *Account) {
			a.Credentials["access_token"] = "native-at"
			a.Credentials["refresh_token"] = "native-rt"
		}, native: true},
		{name: "partial native does not fall back", change: func(a *Account) { a.Credentials["refresh_token"] = "native-rt" }, wantErr: "native OAuth credentials are missing"},
		{name: "API key unchanged", change: func(a *Account) { a.Type = AccountTypeAPIKey }, wantErr: "not an OAuth account"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := bpsQuotaTestAccount()
			tc.change(a)
			before, err := json.Marshal(a.Credentials)
			require.NoError(t, err)
			repo := &stubQuotaAccountRepo{accounts: map[int64]*Account{a.ID: a}}
			cache := newOpenAITokenCacheStub()
			cache.tokens[OpenAITokenCacheKey(a)] = "native-cached-at"
			svc := NewOpenAIQuotaService(repo, nil, NewOpenAITokenProvider(repo, cache, nil), newQuotaRedirectingFactory(&httptest.Server{URL: "http://127.0.0.1:1"}))
			at, workspace, _, _, err := svc.prepareQuotaUpstreamCall(context.Background(), a.ID, true)
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
				require.Empty(t, at)
			} else {
				require.NoError(t, err)
				require.Equal(t, "workspace", workspace)
				if tc.native {
					require.Equal(t, "native-cached-at", at)
					require.EqualValues(t, 1, cache.getCalled)
				} else {
					require.Equal(t, a.Credentials[BPSCredentialKey].(map[string]any)["access_token"], at)
					require.Zero(t, cache.getCalled)
				}
			}
			after, err := json.Marshal(a.Credentials)
			require.NoError(t, err)
			require.Equal(t, before, after)
		})
	}
}

func TestBPSQuotaUpstreamFailureDoesNotRetryOrMutateAccount(t *testing.T) {
	for _, status := range []int{401, 403, 429, 503} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			a := bpsQuotaTestAccount()
			calls := 0
			before, _ := json.Marshal(a.Credentials)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.Method != http.MethodGet {
					t.Error("must remain read-only")
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(status)
				_, _ = w.Write([]byte(`{"error":"unavailable"}`))
			}))
			defer srv.Close()
			repo := &stubQuotaAccountRepo{accounts: map[int64]*Account{a.ID: a}}
			cache := newOpenAITokenCacheStub()
			svc := NewOpenAIQuotaService(repo, nil, NewOpenAITokenProvider(repo, cache, nil), newQuotaRedirectingFactory(srv))
			_, err := svc.QueryUsage(context.Background(), a.ID)
			require.Error(t, err)
			require.Equal(t, 1, calls)
			require.Zero(t, cache.getCalled)
			require.Zero(t, repo.extraUpdateCalls)
			require.Equal(t, StatusActive, a.Status)
			after, _ := json.Marshal(a.Credentials)
			require.Equal(t, before, after)
		})
	}
}

func TestBPSQuotaReadRefreshesOnlyBPSCredentialSlot(t *testing.T) {
	a := bpsQuotaTestAccount()
	a.Credentials[BPSCredentialKey].(map[string]any)["access_token"] = syntheticBPSToken("sub", "workspace", "user", time.Now().Add(time.Minute).Unix())
	repo := &adminBPSRepoStub{account: a}
	cache := &bpsTestCache{openAITokenCacheStub: newOpenAITokenCacheStub()}
	cache.tokens[OpenAITokenCacheKey(a)] = "native-cache-sentinel"
	fresh := syntheticBPSToken("sub", "workspace", "user", time.Now().Add(time.Hour).Unix())
	oauth := &bpsTestOAuthClient{response: &openai.TokenResponse{AccessToken: fresh, RefreshToken: "rotated-bps-rt", ExpiresIn: 3600}}
	provider := NewOpenAITokenProvider(repo, cache, NewOpenAIOAuthService(nil, oauth))
	svc := NewOpenAIQuotaService(repo, nil, provider, newQuotaRedirectingFactory(&httptest.Server{URL: "http://127.0.0.1:1"}))
	got, workspace, _, _, err := svc.prepareQuotaUpstreamCall(context.Background(), a.ID, true)
	require.NoError(t, err)
	require.Equal(t, fresh, got)
	require.Equal(t, "workspace", workspace)
	require.Equal(t, 1, oauth.calls)
	require.Equal(t, BPSOAuthClientID, oauth.client)
	require.Equal(t, "bps-rt", oauth.rt)
	require.Equal(t, "openai:bps:account:13", cache.lockKey)
	require.Zero(t, cache.getCalled)
	require.Zero(t, cache.setCalled)
	require.Equal(t, "native-cache-sentinel", cache.tokens[OpenAITokenCacheKey(a)])
	require.Empty(t, a.GetOpenAIAccessToken())
	require.Empty(t, a.GetOpenAIRefreshToken())
	require.Equal(t, "rotated-bps-rt", a.Credentials[BPSCredentialKey].(map[string]any)["refresh_token"])
}
