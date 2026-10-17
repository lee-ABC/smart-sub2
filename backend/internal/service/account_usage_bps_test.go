//go:build unit

package service

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestBPSUsageCardReadOnlyRefreshAndCache(t *testing.T) {
	a := bpsQuotaTestAccount()
	a.Status = StatusError
	a.ErrorMessage = "temporary upstream error"
	a.Schedulable = false
	a.Extra = map[string]any{"codex_5h_used_percent": float64(90), "codex_7d_used_percent": float64(80), "codex_usage_updated_at": time.Now().Add(-time.Hour).UTC().Format(time.RFC3339), "auto_reset_credit_enabled": true}
	before, _ := json.Marshal(a.Credentials)
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Method != http.MethodGet {
			t.Error("quota refresh must not generate or consume")
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/backend-api/wham/usage":
			_, _ = w.Write([]byte(`{"rate_limit":{"allowed":true,"primary_window":{"used_percent":11,"limit_window_seconds":604800,"reset_after_seconds":600000},"secondary_window":null}}`))
		case "/backend-api/wham/rate-limit-reset-credits":
			_, _ = w.Write([]byte(`{"available_count":0,"credits":[]}`))
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(500)
		}
	}))
	defer srv.Close()
	repo := &stubQuotaAccountRepo{accounts: map[int64]*Account{a.ID: a}}
	quota := NewOpenAIQuotaService(repo, nil, NewOpenAITokenProvider(repo, nil, nil), newQuotaRedirectingFactory(srv))
	svc := &AccountUsageService{accountRepo: repo, usageLogRepo: &usageBatchLogRepoStub{}, openAIQuotaService: quota, cache: NewUsageCache()}
	got, err := svc.GetUsageForAccount(context.Background(), a, true)
	require.NoError(t, err)
	require.Nil(t, got.FiveHour, "a null quota window is not zero utilization")
	require.NotNil(t, got.SevenDay)
	require.Equal(t, float64(11), got.SevenDay.Utilization)
	require.Equal(t, 2, calls)
	require.Equal(t, 1, repo.extraUpdateCalls)
	require.Nil(t, repo.extraUpdates[a.ID]["codex_5h_used_percent"])
	require.Equal(t, StatusError, a.Status)
	require.False(t, a.Schedulable)
	require.Equal(t, "temporary upstream error", a.ErrorMessage)
	_, err = svc.GetUsageForAccount(context.Background(), a)
	require.NoError(t, err)
	require.Equal(t, 2, calls, "normal refresh honors TTL")
	_, err = svc.GetUsageForAccount(context.Background(), a, true)
	require.NoError(t, err)
	require.Equal(t, 4, calls, "explicit refresh bypasses TTL")
	after, _ := json.Marshal(a.Credentials)
	require.Equal(t, before, after)
}

func TestBPSUsageCardErrorsDoNotProduceEmptySuccess(t *testing.T) {
	for _, name := range []string{"upstream401", "missing_rate_limit", "empty_windows", "storage_failure", "service_missing"} {
		t.Run(name, func(t *testing.T) {
			a := bpsQuotaTestAccount()
			calls := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				w.Header().Set("Content-Type", "application/json")
				switch name {
				case "upstream401":
					w.WriteHeader(401)
					_, _ = w.Write([]byte(`{"error":"unauthorized"}`))
				case "missing_rate_limit":
					_, _ = w.Write([]byte(`{}`))
				case "empty_windows":
					_, _ = w.Write([]byte(`{"rate_limit":{"primary_window":null,"secondary_window":null}}`))
				default:
					_, _ = w.Write([]byte(`{"rate_limit":{"primary_window":{"used_percent":11,"limit_window_seconds":604800,"reset_after_seconds":600000}}}`))
				}
			}))
			defer srv.Close()
			repo := &stubQuotaAccountRepo{accounts: map[int64]*Account{a.ID: a}}
			if name == "storage_failure" {
				repo.extraUpdateErr = errors.New("storage unavailable")
			}
			quota := NewOpenAIQuotaService(repo, nil, NewOpenAITokenProvider(repo, nil, nil), newQuotaRedirectingFactory(srv))
			svc := &AccountUsageService{accountRepo: repo, openAIQuotaService: quota, cache: NewUsageCache()}
			if name == "service_missing" {
				svc.openAIQuotaService = nil
			}
			got, err := svc.getOpenAIUsage(context.Background(), a, true)
			require.Error(t, err)
			require.Nil(t, got)
			require.Empty(t, a.Extra)
			require.Zero(t, repo.extraUpdateCalls)
			previousCalls := calls
			got, err = svc.getOpenAIUsage(context.Background(), a, false)
			require.Error(t, err)
			require.Nil(t, got)
			require.Equal(t, previousCalls, calls, "failed refresh remains bounded")
		})
	}
}

func TestBPSUsageSnapshotExpiresWithoutWebSocket(t *testing.T) {
	a := bpsQuotaTestAccount()
	now := time.Now()
	a.Extra = map[string]any{"openai_oauth_responses_websockets_v2_enabled": false, "openai_oauth_responses_websockets_v2_mode": "off", "codex_usage_updated_at": now.Add(-openAIProbeCacheTTL - time.Second).UTC().Format(time.RFC3339)}
	require.True(t, isOpenAICodexSnapshotStale(a, now))
	a.Extra["codex_usage_updated_at"] = now.UTC().Format(time.RFC3339)
	require.False(t, isOpenAICodexSnapshotStale(a, now))
}
