//go:build unit

package service

import (
	"context"
	"encoding/json"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

func bpsSchedulerRoutingConfig(t *testing.T, backend string, enabled bool) {
	t.Helper()
	cfg := excelTestConfig(t)
	cfg.TransportBackend, cfg.WebSocketEnabled = backend, enabled
	raw, err := json.Marshal(cfg)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(excelConfigPath(), raw, 0600))
}

func bpsSchedulerAccount(id int64) Account {
	return Account{ID: id, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive, Schedulable: true, Concurrency: 10, GroupIDs: []int64{9, 4}, Credentials: map[string]any{"plan_type": "team"}, Extra: map[string]any{"openai_oauth_responses_websockets_v2_enabled": true, "openai_oauth_responses_websockets_v2_mode": OpenAIWSIngressModePassthrough}}
}

func bpsSchedulerService(t *testing.T, advanced bool, cfg *config.Config, accounts []Account, cache *schedulerTestGatewayCache) *OpenAIGatewayService {
	t.Helper()
	resetOpenAIAdvancedSchedulerSettingCacheForTest()
	t.Cleanup(resetOpenAIAdvancedSchedulerSettingCacheForTest)
	if cfg == nil {
		cfg = &config.Config{}
	}
	cfg.Gateway.Scheduling.LoadBatchEnabled = false
	if cache == nil {
		cache = &schedulerTestGatewayCache{}
	}
	return &OpenAIGatewayService{accountRepo: schedulerGroupAwareOpenAIAccountRepo{schedulerTestOpenAIAccountRepo{accounts: accounts}}, cache: cache, cfg: cfg, rateLimitService: newOpenAIAdvancedSchedulerRateLimitService(strconv.FormatBool(advanced)), concurrencyService: NewConcurrencyService(schedulerTestConcurrencyCache{})}
}

func TestBPSSchedulerIngressAllowsNativeDisabledAccount(t *testing.T) {
	for _, advanced := range []bool{false, true} {
		t.Run(map[bool]string{false: "legacy", true: "advanced"}[advanced], func(t *testing.T) {
			bpsSchedulerRoutingConfig(t, "cpa", true)
			account := bpsSchedulerAccount(13)
			svc := bpsSchedulerService(t, advanced, nil, []Account{account}, nil)
			groupID := int64(9)
			require.False(t, svc.isOpenAIAccountTransportCompatible(&account, OpenAIUpstreamTransportResponsesWebsocketV2Ingress), "native global WS remains disabled")
			beforeAccount, err := json.Marshal(account)
			require.NoError(t, err)
			beforeConfig, err := os.ReadFile(excelConfigPath())
			require.NoError(t, err)
			selection, decision, err := svc.SelectAccountWithScheduler(context.Background(), &groupID, "", "", "gpt-5.6-sol", nil, OpenAIUpstreamTransportResponsesWebsocketV2Ingress, false)
			require.NoError(t, err)
			require.NotNil(t, selection)
			require.Equal(t, int64(13), selection.Account.ID)
			require.Equal(t, openAIAccountScheduleLayerLoadBalance, decision.Layer)
			if selection.ReleaseFunc != nil {
				selection.ReleaseFunc()
			}
			require.False(t, svc.isOpenAIAccountTransportCompatible(&account, OpenAIUpstreamTransportResponsesWebsocketV2Ingress))
			require.False(t, svc.cfg.Gateway.OpenAIWS.Enabled)
			afterAccount, err := json.Marshal(account)
			require.NoError(t, err)
			require.JSONEq(t, string(beforeAccount), string(afterAccount))
			afterConfig, err := os.ReadFile(excelConfigPath())
			require.NoError(t, err)
			require.Equal(t, beforeConfig, afterConfig)
		})
	}
}

func TestBPSSchedulerTransportEligibilityIsRequestScoped(t *testing.T) {
	for _, advanced := range []bool{false, true} {
		for _, tc := range []struct {
			name, backend, model, accountType string
			enabled, want                     bool
			group                             int64
			transport                         OpenAIUpstreamTransport
			mapping                           map[string]any
			passthrough                       bool
		}{
			{name: "supported", backend: "cpa", enabled: true, want: true, group: 9, model: "gpt-5.6-sol"},
			{name: "setup_token", backend: "cpa", enabled: true, want: true, group: 9, model: "gpt-5.6-luna", accountType: AccountTypeSetupToken},
			{name: "native_group", backend: "cpa", enabled: true, group: 4, model: "gpt-5.6-sol"},
			{name: "unknown_group", backend: "cpa", enabled: true, group: 17, model: "gpt-5.6-sol"},
			{name: "unsupported_model", backend: "cpa", enabled: true, group: 9, model: "gpt-5.1"},
			{name: "api_key", backend: "cpa", enabled: true, group: 9, model: "gpt-5.6-sol", accountType: AccountTypeAPIKey},
			{name: "legacy_adapter", backend: "legacy", enabled: true, group: 9, model: "gpt-5.6-sol"},
			{name: "ws_disabled", backend: "cpa", enabled: false, group: 9, model: "gpt-5.6-sol"},
			{name: "native_transport_only", backend: "cpa", enabled: true, group: 9, model: "gpt-5.6-sol", transport: OpenAIUpstreamTransportResponsesWebsocketV2},
			{name: "sse_unchanged", backend: "legacy", enabled: false, want: true, group: 9, model: "gpt-5.1", transport: OpenAIUpstreamTransportHTTPSSE},
			{name: "alias_mapped_to_bps", backend: "cpa", enabled: true, want: true, group: 9, model: "internal-alias", mapping: map[string]any{"internal-alias": "gpt-5.6-sol"}},
			{name: "bps_mapped_to_native", backend: "cpa", enabled: true, group: 9, model: "gpt-5.6-sol", mapping: map[string]any{"gpt-5.6-sol": "gpt-5.1"}},
			{name: "passthrough_does_not_use_alias_mapping", backend: "cpa", enabled: true, group: 9, model: "internal-alias", mapping: map[string]any{"internal-alias": "gpt-5.6-sol"}, passthrough: true},
		} {
			t.Run(map[bool]string{false: "legacy/", true: "advanced/"}[advanced]+tc.name, func(t *testing.T) {
				bpsSchedulerRoutingConfig(t, tc.backend, tc.enabled)
				account := bpsSchedulerAccount(13)
				if tc.accountType != "" {
					account.Type = tc.accountType
				}
				if tc.mapping != nil {
					account.Credentials["model_mapping"] = tc.mapping
				}
				if tc.passthrough {
					account.Extra["openai_passthrough"] = true
				}
				svc := bpsSchedulerService(t, advanced, nil, []Account{account}, nil)
				transport := tc.transport
				if transport == "" {
					transport = OpenAIUpstreamTransportResponsesWebsocketV2Ingress
				}
				selection, _, err := svc.SelectAccountWithScheduler(context.Background(), &tc.group, "", "", tc.model, nil, transport, false)
				if tc.want {
					require.NoError(t, err)
					require.NotNil(t, selection)
					require.Equal(t, account.ID, selection.Account.ID)
					if selection.ReleaseFunc != nil {
						selection.ReleaseFunc()
					}
				} else {
					require.Error(t, err)
					require.Nil(t, selection)
				}
			})
		}
	}
}

func TestBPSSchedulerKeepsStickyAndGuardianSelection(t *testing.T) {
	for _, advanced := range []bool{false, true} {
		for _, lane := range []string{"session", "guardian", "previous"} {
			if lane == "previous" && !advanced {
				continue
			}
			t.Run(strconv.FormatBool(advanced)+"/"+lane, func(t *testing.T) {
				bpsSchedulerRoutingConfig(t, "cpa", true)
				selected, alternative := bpsSchedulerAccount(13), bpsSchedulerAccount(14)
				selected.Priority = 10
				alternative.Priority = 0
				cache := &schedulerTestGatewayCache{sessionBindings: map[string]int64{"openai:parent-session": selected.ID, "openai:session": selected.ID}}
				var nativeConfig *config.Config
				if lane == "previous" {
					// Preserve the existing native continuation gate: BPS capability
					// does not invent resumability for old response IDs.
					nativeConfig = newSchedulerTestOpenAIWSV2Config()
				}
				svc := bpsSchedulerService(t, advanced, nativeConfig, []Account{selected, alternative}, cache)
				ctx := context.Background()
				groupID := int64(9)
				session := "session"
				previous := ""
				wantLayer := openAIAccountScheduleLayerSessionSticky
				if !advanced {
					// The legacy entrypoint reports load_balance even on a sticky hit.
					wantLayer = openAIAccountScheduleLayerLoadBalance
				}
				if lane == "guardian" {
					ctx = context.WithValue(ctx, openAIGuardianParentAffinityContextKey{}, openAIGuardianParentAffinity{currentSessionHash: "parent-session"})
					session = "child-session"
					wantLayer = openAIAccountScheduleLayerGuardianParent
				}
				if lane == "previous" {
					previous = "resp_bps_parent"
					session = ""
					wantLayer = openAIAccountScheduleLayerPreviousResponse
					require.NoError(t, svc.getOpenAIWSStateStore().BindResponseAccount(ctx, groupID, previous, selected.ID, time.Hour))
				}
				selection, decision, err := svc.SelectAccountWithScheduler(ctx, &groupID, previous, session, "gpt-5.6-sol", nil, OpenAIUpstreamTransportResponsesWebsocketV2Ingress, false)
				require.NoError(t, err)
				require.NotNil(t, selection)
				require.Equal(t, selected.ID, selection.Account.ID)
				require.Equal(t, wantLayer, decision.Layer)
				require.Empty(t, cache.deletedSessions)
				if selection.ReleaseFunc != nil {
					selection.ReleaseFunc()
				}
			})
		}
	}
}

func TestBPSSchedulerNativeSettingsAndPriorityRemainUnchanged(t *testing.T) {
	for _, advanced := range []bool{false, true} {
		t.Run(strconv.FormatBool(advanced), func(t *testing.T) {
			bpsSchedulerRoutingConfig(t, "cpa", true)
			first, second := bpsSchedulerAccount(13), bpsSchedulerAccount(14)
			first.Priority, second.Priority = 0, 10
			first.Extra["openai_oauth_responses_websockets_v2_mode"] = OpenAIWSIngressModeOff
			first.Extra["openai_oauth_responses_websockets_v2_enabled"] = false
			cfg := newSchedulerTestOpenAIWSV2Config()
			cfg.Gateway.OpenAIWS.ModeRouterV2Enabled = true
			cfg.Gateway.OpenAIWS.LBTopK = 1
			cfg.Gateway.OpenAIWS.SchedulerScoreWeights.Priority = 1
			svc := bpsSchedulerService(t, advanced, cfg, []Account{first, second}, nil)
			groupID := int64(9)
			require.False(t, svc.isOpenAIAccountTransportCompatible(&first, OpenAIUpstreamTransportResponsesWebsocketV2Ingress))
			for _, transport := range []OpenAIUpstreamTransport{OpenAIUpstreamTransportHTTPSSE, OpenAIUpstreamTransportResponsesWebsocketV2Ingress} {
				selection, _, err := svc.SelectAccountWithScheduler(context.Background(), &groupID, "", "", "gpt-5.6-sol", nil, transport, false)
				require.NoError(t, err)
				require.NotNil(t, selection)
				require.Equal(t, first.ID, selection.Account.ID, "BPS eligibility must not change priority ordering")
				if selection.ReleaseFunc != nil {
					selection.ReleaseFunc()
				}
			}
			require.False(t, svc.isOpenAIAccountTransportCompatible(&first, OpenAIUpstreamTransportResponsesWebsocketV2Ingress))
			nativeReq := OpenAIAccountScheduleRequest{GroupID: &groupID, RequestedModel: "gpt-5.1", RequiredTransport: OpenAIUpstreamTransportResponsesWebsocketV2Ingress}
			require.False(t, svc.isOpenAIAccountRequestTransportCompatible(&first, nativeReq))
			require.True(t, svc.isOpenAIAccountRequestTransportCompatible(&second, nativeReq), "existing native capability is not narrowed")
		})
	}
}
