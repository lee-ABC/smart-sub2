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
	"strings"
	"testing"
)

func TestExcelAccountTestGroupResolution(t *testing.T) {
	cases := []struct {
		name      string
		groups    []int64
		requested int64
		mode      string
		wantGroup int64
		fail      bool
	}{
		{"unique excel", []int64{9}, 0, "excel", 9, false},
		{"unique native", []int64{4}, 0, "native", 4, false},
		{"ungrouped native", nil, 0, "native", 0, false},
		{"all native backward compatible", []int64{4, 12}, 0, "native", 0, false},
		{"ambiguous never guesses", []int64{4, 9}, 0, "", 0, true},
		{"explicit excel", []int64{4, 9}, 9, "excel", 9, false},
		{"explicit native", []int64{4, 9}, 4, "native", 4, false},
		{"foreign group rejected", []int64{9}, 12, "", 0, true},
		{"negative group rejected", []int64{9}, -1, "", 0, true},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			excelTestConfig(t)
			account := &Account{ID: 100, Platform: PlatformOpenAI, Type: AccountTypeOAuth, GroupIDs: tt.groups}
			ctx, route, err := prepareExcelAccountTestContext(context.Background(), account, tt.requested, "")
			if tt.fail {
				require.Error(t, err)
				require.False(t, isExcelRequest(ctx))
				return
			}
			require.NoError(t, err)
			require.Equal(t, tt.mode, route.Transport)
			require.Equal(t, tt.wantGroup, route.GroupID)
			require.Equal(t, tt.mode == "excel", isExcelRequest(ctx))
		})
	}
}

func TestExcelAccountTestNeverForgesUserKey(t *testing.T) {
	excelTestConfig(t)
	a := &Account{ID: 100, Platform: PlatformOpenAI, Type: AccountTypeOAuth, GroupIDs: []int64{9}}
	ctx, _, err := prepareExcelAccountTestContext(context.Background(), a, 9, "")
	require.NoError(t, err)
	route := ctx.Value(excelRequestRouteKey{}).(*excelRequestRoute)
	require.Zero(t, route.keyID)
	require.EqualValues(t, 9, route.groupID)
	require.Equal(t, "account-test", route.source)
}

func TestExcelIntelligentConfigDistinguishesGroups(t *testing.T) {
	a := IntelligentTestConfig{Model: "gpt-6-sol", GroupID: 9}
	b := a
	b.GroupID = 4
	require.False(t, SameIntelligentTestConfig(a, b))
}

func TestExcelAdminTestEntrypointsFollowSelectedGroup(t *testing.T) {
	for _, intelligent := range []bool{false, true} {
		for _, group := range []int64{4, 9} {
			t.Run(fmtTestRouteName(intelligent, group), func(t *testing.T) {
				excelTestConfig(t)
				fixture := newAstraOAuthSetup(t, false)
				fixture.account.GroupIDs = []int64{4, 9}
				excelSetResponse(fixture, "gpt-6-sol")
				repo := &intelligentRunnerRepo{account: fixture.account}
				svc := &AccountTestService{accountRepo: repo, httpUpstream: fixture.upstream, cfg: fixture.svc.cfg}
				if intelligent {
					record := &IntelligentTestRecord{AccountID: fixture.account.ID, Input: "Answer the exact regression question", ConfigSnapshot: &IntelligentTestConfig{GroupID: group, Model: "gpt-6-sol"}}
					require.NoError(t, svc.RunIntelligentTest(context.Background(), record))
					require.Equal(t, "OK", record.Result)
					require.Equal(t, group, record.ConfigSnapshot.Execution.TestGroupID)
					require.Equal(t, map[int64]string{4: "native", 9: "excel"}[group], record.ConfigSnapshot.Execution.OutboundTransport)
					require.Contains(t, string(fixture.upstream.lastBody), record.Input)
					if group == 9 {
						require.Equal(t, "excel_sidecar", record.ConfigSnapshot.Execution.EffectiveTLS)
					}
					require.NotContains(t, record.RawResponse, "oauth-token")
					require.Zero(t, repo.writes)
				} else {
					ctx, recorder := newTestContext()
					require.NoError(t, svc.TestAccountConnection(ctx, fixture.account.ID, "gpt-6-sol", "", "", AccountTestOptions{GroupID: group}))
					require.Contains(t, recorder.Header().Get("Content-Type"), "text/event-stream")
					require.Contains(t, recorder.Body.String(), "test_route")
					require.Contains(t, recorder.Body.String(), "OK")
					require.NotContains(t, recorder.Body.String(), "oauth-token")
				}
				require.Equal(t, "gpt-6-sol", gjson.GetBytes(fixture.upstream.lastBody, "model").String())
				if group == 9 {
					require.Equal(t, "/internal/v1/responses", fixture.upstream.lastReq.URL.Path)
					require.Equal(t, "oauth-token", fixture.upstream.lastReq.Header.Get("X-Sub2-Excel-Access-Token"))
					require.Equal(t, "chatgpt-acc", fixture.upstream.lastReq.Header.Get("X-Sub2-Excel-Account-Id"))
					require.Empty(t, fixture.upstream.lastProxyURL)
				} else {
					require.Equal(t, chatgptCodexAPIURL, fixture.upstream.lastReq.URL.String())
					require.Empty(t, fixture.upstream.lastReq.Header.Get("X-Sub2-Excel-Access-Token"))
				}
			})
		}
	}
}
func fmtTestRouteName(intelligent bool, group int64) string {
	name := "connectivity"
	if intelligent {
		name = "intelligence"
	}
	if group == 9 {
		return name + " excel"
	}
	return name + " native"
}

func TestExcelAdminAmbiguityAndUnavailableNeverSendNative(t *testing.T) {
	for _, reason := range []string{"ambiguous", "missing-key", "foreign-group", "compact"} {
		t.Run(reason, func(t *testing.T) {
			cfg := excelTestConfig(t)
			f := newAstraOAuthSetup(t, false)
			f.account.GroupIDs = []int64{4, 9}
			repo := &intelligentRunnerRepo{account: f.account}
			svc := &AccountTestService{accountRepo: repo, httpUpstream: f.upstream, cfg: f.svc.cfg}
			opts := AccountTestOptions{GroupID: 9}
			mode, model := "", "gpt-6-sol"
			switch reason {
			case "ambiguous":
				opts.GroupID = 0
			case "missing-key":
				require.NoError(t, os.Remove(cfg.TransportKeyFile))
			case "foreign-group":
				opts.GroupID = 12
			case "unsupported-apikey":
				f.account.Type = AccountTypeAPIKey
			case "compact":
				mode = AccountTestModeCompact
			case "image":
				model = "gpt-image-2"
			}
			c, _ := newTestContext()
			require.Error(t, svc.TestAccountConnection(c, f.account.ID, model, "", mode, opts))
			require.Nil(t, f.upstream.lastReq)
			require.Zero(t, repo.writes)
		})
	}
}

func TestExcelAdminPrivateFailureDoesNotPoisonAccount(t *testing.T) {
	for _, intelligent := range []bool{false, true} {
		t.Run(fmtTestRouteName(intelligent, 9), func(t *testing.T) {
			excelTestConfig(t)
			f := newAstraOAuthSetup(t, false)
			f.account.GroupIDs = []int64{9}
			f.upstream.resp = &http.Response{StatusCode: 401, Header: http.Header{"X-Sub2-Excel-Failure": []string{"transport"}}, Body: io.NopCloser(strings.NewReader("private endpoint denied"))}
			repo := &intelligentRunnerRepo{account: f.account}
			svc := &AccountTestService{accountRepo: repo, httpUpstream: f.upstream, cfg: f.svc.cfg}
			if intelligent {
				r := &IntelligentTestRecord{AccountID: f.account.ID, Input: "test", ConfigSnapshot: &IntelligentTestConfig{GroupID: 9, Model: "gpt-6-sol"}}
				require.Error(t, svc.RunIntelligentTest(context.Background(), r))
				require.Contains(t, r.ErrorMessage, "Excel private transport unavailable")
				require.NotContains(t, r.RawResponse, "private endpoint denied")
			} else {
				c, rec := newTestContext()
				require.Error(t, svc.TestAccountConnection(c, f.account.ID, "gpt-6-sol", "", ""))
				require.Contains(t, rec.Body.String(), "Excel private transport unavailable")
			}
			require.Zero(t, repo.writes)
			require.True(t, f.account.Schedulable)
			require.Equal(t, StatusActive, f.account.Status)
		})
	}
}

func TestExcelAccountTestCatalogAndDefaults(t *testing.T) {
	excelTestConfig(t)
	a := &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeOAuth, GroupIDs: []int64{4, 9}}
	models, excel, err := ExcelAccountTestModels(context.Background(), a, 9)
	require.NoError(t, err)
	require.True(t, excel)
	require.Contains(t, models, "gpt-6-sol")
	_, excel, err = ExcelAccountTestModels(context.Background(), a, 4)
	require.NoError(t, err)
	require.False(t, excel)
	_, _, err = ExcelAccountTestModels(context.Background(), a, 0)
	require.Error(t, err)
	ctx, _, err := prepareExcelAccountTestContext(context.Background(), a, 9, "")
	require.NoError(t, err)
	require.Equal(t, "gpt-6-sol", excelAccountTestModel(ctx, ""))
	require.Equal(t, "gpt-6-astra", excelAccountTestModel(ctx, "gpt-6-astra"))
	require.Equal(t, "", excelAccountTestModel(context.Background(), ""))
}

func TestExcelTestAndAPIRequestScopesStaySeparate(t *testing.T) {
	excelTestConfig(t)
	f := newAstraOAuthSetup(t, false)
	f.account.GroupIDs = []int64{9}
	ctx, _, err := prepareExcelAccountTestContext(context.Background(), f.account, 9, "")
	require.NoError(t, err)
	excelSetRequestGroup(f, 9)
	apiCtx, err := prepareExcelRequestContext(context.Background(), f.c, f.account, "gpt-6-sol")
	require.NoError(t, err)
	scopes := map[string]bool{}
	for _, requestCtx := range []context.Context{ctx, apiCtx} {
		excelSetResponse(f, "gpt-6-sol")
		req, err := http.NewRequestWithContext(requestCtx, "POST", chatgptCodexAPIURL, strings.NewReader(`{"model":"gpt-6-sol"}`))
		require.NoError(t, err)
		req.Header.Set("Authorization", "Bearer already-prepared-token")
		resp, handled, err := f.svc.forwardExcelHTTP(req, "http://selected-account-proxy:8080", f.account)
		require.NoError(t, err)
		require.True(t, handled)
		resp.Body.Close()
		require.Equal(t, "already-prepared-token", f.upstream.lastReq.Header.Get("X-Sub2-Excel-Access-Token"))
		scope := f.upstream.lastReq.Header.Get("X-Sub2-Excel-Scope")
		require.False(t, scopes[scope])
		scopes[scope] = true
		require.Equal(t, "http://selected-account-proxy:8080", f.upstream.lastReq.Header.Get("X-Sub2-Excel-Proxy"))
		require.Empty(t, f.upstream.lastProxyURL)
	}
}

func TestExcelDefaultModelUsedByIntelligenceEntrypoint(t *testing.T) {
	excelTestConfig(t)
	f := newAstraOAuthSetup(t, false)
	f.account.GroupIDs = []int64{9}
	excelSetResponse(f, "gpt-6-sol")
	repo := &intelligentRunnerRepo{account: f.account}
	svc := &AccountTestService{accountRepo: repo, httpUpstream: f.upstream, cfg: f.svc.cfg}
	r := &IntelligentTestRecord{AccountID: f.account.ID, Input: "test", ConfigSnapshot: &IntelligentTestConfig{}}
	require.NoError(t, svc.RunIntelligentTest(context.Background(), r))
	raw, err := json.Marshal(r.ConfigSnapshot.Execution)
	require.NoError(t, err)
	require.Contains(t, string(raw), "excel")
	require.Equal(t, "gpt-6-sol", gjson.GetBytes(f.upstream.lastBody, "model").String())
	require.Empty(t, r.ConfigSnapshot.Model)
	require.Zero(t, repo.writes)
}

func TestExcelAdminNativeModelPreserved(t *testing.T) {
	for _, intelligent := range []bool{false, true} {
		for _, group := range []int64{4, 9} {
			t.Run(fmtTestRouteName(intelligent, group), func(t *testing.T) {
				excelTestConfig(t)
				fixture := newAstraOAuthSetup(t, false)
				fixture.account.GroupIDs = []int64{4, 9}
				excelSetResponse(fixture, "gpt-5.5")
				repo := &intelligentRunnerRepo{account: fixture.account}
				svc := &AccountTestService{accountRepo: repo, httpUpstream: fixture.upstream, cfg: fixture.svc.cfg}
				if intelligent {
					record := &IntelligentTestRecord{AccountID: fixture.account.ID, Input: "Answer the exact regression question", ConfigSnapshot: &IntelligentTestConfig{GroupID: group, Model: "gpt-5.5"}}
					require.NoError(t, svc.RunIntelligentTest(context.Background(), record))
					require.Equal(t, "OK", record.Result)
					require.Equal(t, group, record.ConfigSnapshot.Execution.TestGroupID)
					require.Equal(t, "native", record.ConfigSnapshot.Execution.OutboundTransport)
					require.Contains(t, string(fixture.upstream.lastBody), record.Input)

					require.NotContains(t, record.RawResponse, "oauth-token")
					require.Zero(t, repo.writes)
				} else {
					ctx, recorder := newTestContext()
					require.NoError(t, svc.TestAccountConnection(ctx, fixture.account.ID, "gpt-5.5", "", "", AccountTestOptions{GroupID: group}))
					require.Contains(t, recorder.Header().Get("Content-Type"), "text/event-stream")
					require.Contains(t, recorder.Body.String(), "test_route")
					require.Contains(t, recorder.Body.String(), "OK")
					require.NotContains(t, recorder.Body.String(), "oauth-token")
				}
				require.Equal(t, "gpt-5.5", gjson.GetBytes(fixture.upstream.lastBody, "model").String())

				require.Equal(t, chatgptCodexAPIURL, fixture.upstream.lastReq.URL.String())
				require.Empty(t, fixture.upstream.lastReq.Header.Get("X-Sub2-Excel-Access-Token"))

			})
		}
	}
}
