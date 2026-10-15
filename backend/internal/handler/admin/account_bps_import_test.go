package admin

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

func bpsImportFixture(t *testing.T, bps bool, workspace, user, subject string) map[string]any {
	t.Helper()
	client, kind := openai.ClientID, "codex"
	if bps {
		client, kind = service.BPSOAuthClientID, "oai-basispoints"
	}
	token := buildCodexImportTestJWT(t, time.Now().Add(time.Hour), map[string]any{"client_id": client, "sub": subject, "https://api.openai.com/auth": map[string]any{"chatgpt_account_id": workspace, "chatgpt_user_id": user}})
	return map[string]any{"type": kind, "access_token": token, "refresh_token": kind + "-refresh", "email": "same@example.com"}
}

type bpsImportAdmin struct{ *codexImportMemoryAdminService }

func (s *bpsImportAdmin) UpdateAccount(ctx context.Context, id int64, in *service.UpdateAccountInput) (*service.Account, error) {
	if !in.BPSProfileOnly {
		return s.codexImportMemoryAdminService.UpdateAccount(ctx, id, in)
	}
	s.updatedAccounts = append(s.updatedAccounts, struct {
		id    int64
		input *service.UpdateAccountInput
	}{id, in})
	for n := range s.accounts {
		if s.accounts[n].ID == id {
			candidate := s.accounts[n]
			candidate.Credentials = cloneCodexImportTestMap(candidate.Credentials)
			candidate.Credentials[service.BPSCredentialKey] = in.Credentials[service.BPSCredentialKey]
			if _, err := service.BPSCredentialsForAccount(&candidate); err != nil {
				return nil, err
			}
			s.accounts[n] = candidate
			return &s.accounts[n], nil
		}
	}
	return nil, service.ErrAccountNotFound
}

func runBPSImport(t *testing.T, accounts []service.Account, values ...map[string]any) (CodexSessionImportResult, *bpsImportAdmin) {
	t.Helper()
	admin := &bpsImportAdmin{newCodexImportMemoryAdminService(accounts)}
	handler := &AccountHandler{adminService: admin}
	req := CodexSessionImportRequest{}
	for _, value := range values {
		data, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		req.Contents = append(req.Contents, string(data))
	}
	entries, err := parseCodexSessionImportEntries(req)
	if err != nil {
		t.Fatal(err)
	}
	result, err := handler.importCodexSessions(context.Background(), req, entries)
	if err != nil {
		t.Fatal(err)
	}
	return result, admin
}

func TestBPSImportBothOrdersKeepSeparateProfiles(t *testing.T) {
	for _, bpsFirst := range []bool{false, true} {
		t.Run(map[bool]string{true: "bps-first", false: "native-first"}[bpsFirst], func(t *testing.T) {
			native, bps := bpsImportFixture(t, false, "workspace", "user", "subject"), bpsImportFixture(t, true, "workspace", "user", "subject")
			values := []map[string]any{native, bps}
			if bpsFirst {
				values[0], values[1] = values[1], values[0]
			}
			result, admin := runBPSImport(t, nil, values...)
			if result.Created != 1 || result.Updated != 1 || result.Failed != 0 || result.Skipped != 0 || len(admin.accounts) != 1 {
				t.Fatalf("unexpected result: %+v accounts=%d", result, len(admin.accounts))
			}
			creds := admin.accounts[0].Credentials
			if creds["access_token"] != native["access_token"] || creds["refresh_token"] != native["refresh_token"] || creds["client_id"] != openai.ClientID {
				t.Fatal("native credential set overwritten")
			}
			profile, err := service.BPSCredentialsForAccount(&admin.accounts[0])
			if err != nil {
				t.Fatal(err)
			}
			if profile["access_token"] != bps["access_token"] || profile["refresh_token"] != bps["refresh_token"] || profile["client_id"] != service.BPSOAuthClientID {
				t.Fatal("BPS profile mismatch")
			}
			update := admin.updatedAccounts[0].input
			if update.Extra != nil || update.Priority != nil || update.Concurrency != nil || update.GroupIDs != nil || update.ExpiresAt != nil || update.AutoPauseOnExpired != nil || update.ProxyID != nil || update.RateMultiplier != nil || update.LoadFactor != nil {
				t.Fatal("cross-profile import edited business settings")
			}
			if update.BPSProfileOnly == bpsFirst {
				t.Fatal("wrong update path")
			}
		})
	}
}

func TestBPSImportDedupesPerProfile(t *testing.T) {
	native, bps := bpsImportFixture(t, false, "workspace", "user", "subject"), bpsImportFixture(t, true, "workspace", "user", "subject")
	result, admin := runBPSImport(t, nil, native, bps, native, bps)
	if result.Created != 1 || result.Updated != 1 || result.Skipped != 2 || result.Failed != 0 || len(admin.accounts) != 1 {
		t.Fatalf("result: %+v", result)
	}
}

func TestBPSImportJWTClientDetectionAndNoNativeBorrow(t *testing.T) {
	raw := bpsImportFixture(t, true, "workspace", "user", "subject")
	delete(raw, "type")
	delete(raw, "refresh_token")
	item, err := normalizeCodexImportEntry(codexImportEntry{Index: 1, Value: raw})
	if err != nil {
		t.Fatal(err)
	}
	if !item.IsBPS {
		t.Fatal("BPS JWT not detected")
	}
	for _, key := range []string{"access_token", "refresh_token"} {
		if _, ok := item.Credentials[key]; ok {
			t.Fatalf("BPS leaked into native %s", key)
		}
	}
	profile := item.Credentials[service.BPSCredentialKey].(map[string]any)
	if _, ok := profile["refresh_token"]; ok {
		t.Fatal("BPS RT fabricated")
	}
	if !strings.Contains(strings.Join(item.WarningTexts, " "), "原生") {
		t.Fatal("missing native profile not explained")
	}
}

func TestBPSImportRejectsWrongClientAndIdentity(t *testing.T) {
	for _, name := range []string{"wrong-token-client", "wrong-declared-client", "workspace", "user", "missing-identity", "missing-token", "id-token", "agent-identity"} {
		t.Run(name, func(t *testing.T) {
			raw := bpsImportFixture(t, true, "workspace", "user", "subject")
			switch name {
			case "wrong-token-client":
				raw["access_token"] = bpsImportFixture(t, false, "workspace", "user", "subject")["access_token"]
			case "wrong-declared-client":
				raw["client_id"] = openai.ClientID
			case "workspace":
				raw["account_id"] = "other-workspace"
			case "user":
				raw["user_id"] = "other-user"
			case "missing-identity":
				raw = bpsImportFixture(t, true, "workspace", "", "subject")
			case "missing-token":
				delete(raw, "access_token")
			case "id-token":
				raw["id_token"] = bpsImportFixture(t, true, "workspace", "user", "other-subject")["access_token"]
			case "agent-identity":
				raw["agent_identity"] = map[string]any{}
			}
			if _, err := normalizeCodexImportEntry(codexImportEntry{Index: 1, Value: raw}); err == nil {
				t.Fatal("invalid BPS profile accepted")
			}
		})
	}
}

func TestBPSImportDoesNotMergeByEmailWorkspaceOrUserAlone(t *testing.T) {
	for _, name := range []string{"different-user", "different-workspace", "different-subject"} {
		t.Run(name, func(t *testing.T) {
			workspace, user, subject := "workspace", "user", "subject"
			switch name {
			case "different-user":
				user = "other-user"
			case "different-workspace":
				workspace = "other-workspace"
			case "different-subject":
				subject = "other-subject"
			}
			for _, bpsFirst := range []bool{false, true} {
				native, bps := bpsImportFixture(t, false, "workspace", "user", "subject"), bpsImportFixture(t, true, workspace, user, subject)
				values := []map[string]any{native, bps}
				if bpsFirst {
					values[0], values[1] = values[1], values[0]
				}
				result, admin := runBPSImport(t, nil, values...)
				if result.Created != 2 || result.Updated != 0 || result.Failed != 0 || len(admin.accounts) != 2 {
					t.Fatalf("unsafe cross-profile match bpsFirst=%v: %+v", bpsFirst, result)
				}
			}
		})
	}
}

func TestBPSImportRejectsAmbiguousExactIdentity(t *testing.T) {
	item, err := normalizeCodexImportEntry(codexImportEntry{Index: 1, Value: bpsImportFixture(t, false, "workspace", "user", "subject")})
	if err != nil {
		t.Fatal(err)
	}
	accounts := []service.Account{{ID: 1, Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth, Credentials: cloneCodexImportTestMap(item.Credentials)}, {ID: 2, Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth, Credentials: cloneCodexImportTestMap(item.Credentials)}}
	result, admin := runBPSImport(t, accounts, bpsImportFixture(t, true, "workspace", "user", "subject"))
	if result.Failed != 1 || result.Created != 0 || result.Updated != 0 || len(admin.updatedAccounts) != 0 {
		t.Fatalf("ambiguous accepted: %+v", result)
	}
}

func TestBPSImportAttachmentPreservesNativeAndBusinessSettings(t *testing.T) {
	item, err := normalizeCodexImportEntry(codexImportEntry{Index: 1, Value: bpsImportFixture(t, false, "workspace", "user", "subject")})
	if err != nil {
		t.Fatal(err)
	}
	expiry := time.Now().Add(-time.Hour)
	existing := service.Account{ID: 1, Name: "unchanged", Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth, Status: service.StatusError, Credentials: item.Credentials, Extra: map[string]any{"sticky": "keep"}, Priority: 7, Concurrency: 2, ExpiresAt: &expiry, Schedulable: false}
	before := cloneCodexImportTestMap(existing.Credentials)
	result, admin := runBPSImport(t, []service.Account{existing}, bpsImportFixture(t, true, "workspace", "user", "subject"))
	if result.Updated != 1 || result.Failed != 0 {
		t.Fatalf("result=%+v", result)
	}
	got := admin.accounts[0]
	for key, want := range before {
		if !reflect.DeepEqual(got.Credentials[key], want) {
			t.Fatalf("native field changed: %s", key)
		}
	}
	if got.Name != existing.Name || got.Status != existing.Status || got.Priority != existing.Priority || got.Concurrency != existing.Concurrency || got.ExpiresAt != existing.ExpiresAt || got.Schedulable || !reflect.DeepEqual(got.Extra, existing.Extra) {
		t.Fatal("business settings changed")
	}
}

func TestBPSImportCredentialExtrasCannotInjectProfile(t *testing.T) {
	extras := sanitizeCodexImportCredentialExtras(map[string]any{service.BPSCredentialKey: map[string]any{"access_token": "injected"}, "model_mapping": "keep"})
	if _, ok := extras[service.BPSCredentialKey]; ok {
		t.Fatal("profile injection allowed")
	}
	if extras["model_mapping"] != "keep" {
		t.Fatal("ordinary extras lost")
	}
}

func TestBPSImportATOnlyAttachmentDoesNotBorrowEitherRefreshToken(t *testing.T) {
	native, bps := bpsImportFixture(t, false, "workspace", "user", "subject"), bpsImportFixture(t, true, "workspace", "user", "subject")
	_, admin := runBPSImport(t, nil, native, bps)
	delete(bps, "refresh_token")
	result, next := runBPSImport(t, admin.accounts, bps)
	if result.Updated != 1 || result.Failed != 0 {
		t.Fatalf("result=%+v", result)
	}
	profile := next.accounts[0].Credentials[service.BPSCredentialKey].(map[string]any)
	if _, ok := profile["refresh_token"]; ok {
		t.Fatal("AT-only BPS import silently borrowed an old RT")
	}
	if next.accounts[0].Credentials["refresh_token"] != native["refresh_token"] {
		t.Fatal("native RT changed")
	}
}
