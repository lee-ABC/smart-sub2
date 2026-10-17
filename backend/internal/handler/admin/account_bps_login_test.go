package admin

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

const bpsLoginFixtureID = "0123456789abcdef0123456789abcdef"
const bpsLoginFixtureSecret = "NEVER-RETURN-WORKER-SECRET"

func bpsLoginTestSetup(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	gin.SetMode(gin.TestMode)
	bpsLoginReceipts.Lock()
	bpsLoginReceipts.entries = map[string]*bpsLoginReceipt{}
	bpsLoginReceipts.Unlock()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	keyFile := filepath.Join(t.TempDir(), "worker-key")
	require.NoError(t, os.WriteFile(keyFile, []byte(strings.Repeat("k", 48)), 0600))
	t.Setenv("SUB2_BPS_LOGIN_URL", server.URL+"/internal/v1")
	t.Setenv("SUB2_BPS_LOGIN_KEY_FILE", keyFile)
	return server
}
func bpsLoginTestCall(t *testing.T, h func(*gin.Context), owner int64, method, id, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(method, "/api/v1/admin/accounts/bps-login", strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Request.Header.Set("X-Sub2-BPS-Owner", "999999")
	if owner > 0 {
		c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: owner})
	}
	c.Params = gin.Params{{Key: "jobID", Value: id}}
	h(c)
	return rec
}
func bpsLoginFixtureStatus() map[string]any {
	return map[string]any{"id": bpsLoginFixtureID, "status": "ready", "phase": "validate", "phase_elapsed_ms": 1234, "total_elapsed_ms": 5678, "code": "CREDENTIAL_VALIDATED", "expires_at": time.Now().Add(time.Minute).UTC().Format(time.RFC3339), "message": bpsLoginFixtureSecret, "access_token": bpsLoginFixtureSecret, "refresh_token": bpsLoginFixtureSecret, "preview": map[string]any{"email": "same@example.com", "workspace_id": "workspace", "plan_type": "team", "password": bpsLoginFixtureSecret}, "workspace_choices": []any{map[string]any{"id": "workspace", "name": "测试工作区", "kind": "unknown", "token": bpsLoginFixtureSecret}}}
}
func bpsLoginWriteJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
func bpsLoginStart(t *testing.T, h *AccountHandler, options string) {
	t.Helper()
	if options != "" {
		options = "," + options
	}
	rec := bpsLoginTestCall(t, h.StartBPSLogin, 7, http.MethodPost, "", `{"email":"same@example.com","password":"fake-password","totp_secret":"JBSWY3DPEHPK3PXP"`+options+`}`)
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
}

func TestBPSLoginStartUsesTrustedOwnerAndServerProxyAndRedacts(t *testing.T) {
	var calls atomic.Int32
	bpsLoginTestSetup(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		require.Equal(t, "Bearer "+strings.Repeat("k", 48), r.Header.Get("Authorization"))
		require.Equal(t, "7", r.Header.Get("X-Sub2-BPS-Owner"))
		require.Equal(t, "/internal/v1/jobs", r.URL.Path)
		var in map[string]any
		require.NoError(t, json.NewDecoder(r.Body).Decode(&in))
		require.Equal(t, "http://127.0.0.1:8080", in["proxy_url"])
		require.NotContains(t, in, "proxy_id")
		require.Equal(t, "fake-password", in["password"])
		bpsLoginWriteJSON(w, 202, bpsLoginFixtureStatus())
	})
	h := &AccountHandler{adminService: newStubAdminService()}
	rec := bpsLoginTestCall(t, h.StartBPSLogin, 7, "POST", "", `{"email":"same@example.com","password":"fake-password","proxy_id":4}`)
	require.Equal(t, 202, rec.Code)
	require.NotContains(t, rec.Body.String(), bpsLoginFixtureSecret)
	require.Contains(t, rec.Body.String(), "CREDENTIAL_VALIDATED")
	require.Contains(t, rec.Body.String(), `"phase":"validate"`)
	require.Contains(t, rec.Body.String(), `"phase_elapsed_ms":1234`)
	require.Contains(t, rec.Body.String(), `"total_elapsed_ms":5678`)
	require.Contains(t, rec.Body.String(), `"kind":"unknown"`)
	rec = bpsLoginTestCall(t, h.StartBPSLogin, 7, "POST", "", `{"email":"same@example.com","password":"fake-password","proxy_url":"http://attacker"}`)
	require.Equal(t, 400, rec.Code)
	require.EqualValues(t, 1, calls.Load())
	rec = bpsLoginTestCall(t, h.StartBPSLogin, 0, "POST", "", `{}`)
	require.Equal(t, 401, rec.Code)
}

func TestBPSLoginProxyFailsClosed(t *testing.T) {
	var calls atomic.Int32
	bpsLoginTestSetup(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		bpsLoginWriteJSON(w, 202, bpsLoginFixtureStatus())
	})
	for _, kind := range []string{"missing", "inactive", "expired", "bad-port", "bad-protocol"} {
		t.Run(kind, func(t *testing.T) {
			admin := newStubAdminService()
			id := int64(4)
			switch kind {
			case "missing":
				id = 999
			case "inactive":
				admin.proxies[0].Status = "inactive"
			case "expired":
				expiry := time.Now().Add(-time.Minute)
				admin.proxies[0].ExpiresAt = &expiry
			case "bad-port":
				admin.proxies[0].Port = 0
			case "bad-protocol":
				admin.proxies[0].Protocol = "file"
			}
			body, _ := json.Marshal(map[string]any{"email": "same@example.com", "password": "fake", "proxy_id": id})
			rec := bpsLoginTestCall(t, (&AccountHandler{adminService: admin}).StartBPSLogin, 7, "POST", "", string(body))
			require.Equal(t, 400, rec.Code)
		})
	}
	require.Zero(t, calls.Load())
}

func TestBPSLoginStatusInputAndOwnerIsolation(t *testing.T) {
	var inputCalls atomic.Int32
	bpsLoginTestSetup(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Sub2-BPS-Owner") != "7" {
			bpsLoginWriteJSON(w, 404, map[string]any{"error": bpsLoginFixtureSecret})
			return
		}
		if strings.HasSuffix(r.URL.Path, "/input") {
			inputCalls.Add(1)
			var in map[string]any
			require.NoError(t, json.NewDecoder(r.Body).Decode(&in))
			require.Equal(t, "email_otp", in["kind"])
			require.Equal(t, "123456", in["value"])
		}
		status := bpsLoginFixtureStatus()
		status["status"] = "awaiting_input"
		status["phase"] = "email_otp"
		status["challenge_kind"] = "email_otp"
		status["code"] = "EMAIL_CODE_REQUIRED"
		bpsLoginWriteJSON(w, 200, status)
	})
	h := &AccountHandler{adminService: newStubAdminService()}
	rec := bpsLoginTestCall(t, h.GetBPSLogin, 8, "GET", bpsLoginFixtureID, "")
	require.Equal(t, 404, rec.Code)
	require.NotContains(t, rec.Body.String(), bpsLoginFixtureSecret)
	rec = bpsLoginTestCall(t, h.GetBPSLogin, 7, "GET", bpsLoginFixtureID, "")
	require.Equal(t, 200, rec.Code)
	require.Contains(t, rec.Body.String(), "EMAIL_CODE_REQUIRED")
	require.NotContains(t, rec.Body.String(), bpsLoginFixtureSecret)
	rec = bpsLoginTestCall(t, h.SubmitBPSLoginInput, 7, "POST", bpsLoginFixtureID, `{"kind":"email_otp","value":"123456"}`)
	require.Equal(t, 200, rec.Code)
	rec = bpsLoginTestCall(t, h.SubmitBPSLoginInput, 7, "POST", bpsLoginFixtureID, `{"kind":"password","value":"injected"}`)
	require.Equal(t, 400, rec.Code)
	require.EqualValues(t, 1, inputCalls.Load())
	rec = bpsLoginTestCall(t, h.GetBPSLogin, 7, "GET", "../../secret", "")
	require.Equal(t, 400, rec.Code)
}

func TestBPSLoginConfirmImportsOnlyWorkerCredentialAndReplays(t *testing.T) {
	native := bpsImportFixture(t, false, "workspace", "user", "subject")
	item, err := normalizeCodexImportEntry(codexImportEntry{Index: 1, Value: native})
	require.NoError(t, err)
	expiry := time.Now().Add(-time.Hour)
	account := service.Account{ID: 51, Name: "unchanged", Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth, Credentials: item.Credentials, Status: service.StatusError, Priority: 9, Concurrency: 2, ExpiresAt: &expiry, Schedulable: false}
	admin := &bpsImportAdmin{newCodexImportMemoryAdminService([]service.Account{account})}
	h := &AccountHandler{adminService: admin}
	credential := bpsImportFixture(t, true, "workspace", "user", "subject")
	var resultCalls, deletes atomic.Int32
	bpsLoginTestSetup(t, func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "7", r.Header.Get("X-Sub2-BPS-Owner"))
		switch {
		case strings.HasSuffix(r.URL.Path, "/result"):
			resultCalls.Add(1)
			bpsLoginWriteJSON(w, 200, map[string]any{"credential": credential})
		case r.Method == "DELETE":
			deletes.Add(1)
			w.WriteHeader(204)
		default:
			bpsLoginWriteJSON(w, 200, bpsLoginFixtureStatus())
		}
	})
	bpsLoginStart(t, h, `"proxy_id":4,"workspace_id":"workspace"`)
	var wg sync.WaitGroup
	out := make(chan *httptest.ResponseRecorder, 2)
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out <- bpsLoginTestCall(t, h.ImportBPSLogin, 7, "POST", bpsLoginFixtureID, `{"priority":99,"concurrency":12}`)
		}()
	}
	wg.Wait()
	close(out)
	for rec := range out {
		require.Equal(t, 200, rec.Code, rec.Body.String())
		require.Contains(t, rec.Body.String(), `"updated":1`)
		require.NotContains(t, rec.Body.String(), credential["access_token"])
		require.NotContains(t, rec.Body.String(), credential["refresh_token"])
	}
	require.EqualValues(t, 1, resultCalls.Load())
	require.EqualValues(t, 1, deletes.Load())
	require.Len(t, admin.updatedAccounts, 1)
	saved := admin.accounts[0]
	require.Equal(t, native["access_token"], saved.Credentials["access_token"])
	require.Equal(t, native["refresh_token"], saved.Credentials["refresh_token"])
	require.Equal(t, 9, saved.Priority)
	require.Equal(t, 2, saved.Concurrency)
	require.Equal(t, service.StatusError, saved.Status)
	require.False(t, saved.Schedulable)
	require.Equal(t, &expiry, saved.ExpiresAt)
	rec := bpsLoginTestCall(t, h.ImportBPSLogin, 7, "POST", bpsLoginFixtureID, `{"priority":98}`)
	require.Equal(t, 409, rec.Code)
	rec = bpsLoginTestCall(t, h.CancelBPSLogin, 7, "DELETE", bpsLoginFixtureID, "")
	require.Equal(t, 200, rec.Code)
	require.EqualValues(t, 1, deletes.Load())
	rec = bpsLoginTestCall(t, h.ImportBPSLogin, 8, "POST", bpsLoginFixtureID, `{}`)
	require.Equal(t, 409, rec.Code)
	require.EqualValues(t, 1, resultCalls.Load())
}

func TestBPSLoginConfirmRejectsFrontendCredentialProxyAndWorkspaceMismatch(t *testing.T) {
	admin := &bpsImportAdmin{newCodexImportMemoryAdminService(nil)}
	h := &AccountHandler{adminService: admin}
	var resultCalls atomic.Int32
	bpsLoginTestSetup(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/result") {
			resultCalls.Add(1)
			bpsLoginWriteJSON(w, 200, map[string]any{"credential": bpsImportFixture(t, true, "different-workspace", "user", "subject")})
			return
		}
		bpsLoginWriteJSON(w, 200, bpsLoginFixtureStatus())
	})
	bpsLoginStart(t, h, `"workspace_id":"workspace","proxy_id":4`)
	for _, body := range []string{`{"content":"untrusted-token"}`, `{"contents":["untrusted-token"]}`, `{"proxy_id":5}`, `{"credentials":{"access_token":"injected"}}`} {
		rec := bpsLoginTestCall(t, h.ImportBPSLogin, 7, "POST", bpsLoginFixtureID, body)
		require.Equal(t, 400, rec.Code, body)
	}
	require.Zero(t, resultCalls.Load())
	rec := bpsLoginTestCall(t, h.ImportBPSLogin, 7, "POST", bpsLoginFixtureID, `{}`)
	require.Equal(t, 502, rec.Code)
	require.Empty(t, admin.createdAccounts)
}

func TestBPSLoginCancelPreventsImport(t *testing.T) {
	var resultCalls, deletes atomic.Int32
	bpsLoginTestSetup(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "DELETE" {
			deletes.Add(1)
			w.WriteHeader(204)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/result") {
			resultCalls.Add(1)
		}
		bpsLoginWriteJSON(w, 200, bpsLoginFixtureStatus())
	})
	h := &AccountHandler{adminService: newStubAdminService()}
	bpsLoginStart(t, h, "")
	rec := bpsLoginTestCall(t, h.CancelBPSLogin, 7, "DELETE", bpsLoginFixtureID, "")
	require.Equal(t, 200, rec.Code)
	rec = bpsLoginTestCall(t, h.ImportBPSLogin, 7, "POST", bpsLoginFixtureID, `{}`)
	require.Equal(t, 410, rec.Code)
	require.Zero(t, resultCalls.Load())
	require.EqualValues(t, 1, deletes.Load())
}

func TestBPSLoginWorkerFailuresBoundedAndRedacted(t *testing.T) {
	for _, kind := range []string{"error", "oversized", "redirect", "unknown-status"} {
		t.Run(kind, func(t *testing.T) {
			var redirected atomic.Int32
			bpsLoginTestSetup(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/leak" {
					redirected.Add(1)
					w.WriteHeader(200)
					return
				}
				switch kind {
				case "error":
					bpsLoginWriteJSON(w, 500, map[string]any{"error": bpsLoginFixtureSecret})
				case "oversized":
					_, _ = io.WriteString(w, strings.Repeat("x", bpsLoginMaxBody+1))
				case "redirect":
					http.Redirect(w, r, "/leak", 302)
				case "unknown-status":
					bpsLoginWriteJSON(w, 200, map[string]any{"id": bpsLoginFixtureID, "status": bpsLoginFixtureSecret})
				}
			})
			h := &AccountHandler{adminService: newStubAdminService()}
			rec := bpsLoginTestCall(t, h.GetBPSLogin, 7, "GET", bpsLoginFixtureID, "")
			require.Equal(t, 502, rec.Code)
			require.NotContains(t, rec.Body.String(), bpsLoginFixtureSecret)
			require.Zero(t, redirected.Load())
		})
	}
}

func TestBPSLoginDisabledMissingConfigAndCancelledContext(t *testing.T) {
	t.Setenv("SUB2_BPS_LOGIN_URL", "")
	h := &AccountHandler{adminService: newStubAdminService()}
	rec := bpsLoginTestCall(t, h.GetBPSLogin, 7, "GET", bpsLoginFixtureID, "")
	require.Equal(t, 503, rec.Code)
	bpsLoginTestSetup(t, func(w http.ResponseWriter, r *http.Request) { bpsLoginWriteJSON(w, 200, bpsLoginFixtureStatus()) })
	worker, err := newBPSLoginWorker()
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = worker.call(ctx, 7, "GET", "/jobs/"+bpsLoginFixtureID, nil, time.Second)
	require.Error(t, err)
	require.Equal(t, 12*time.Second, worker.client.Timeout)
}

func TestBPSLoginCanonicalCodesAllowlistedOnly(t *testing.T) {
	for _, code := range []string{"PURE_PASSWORD_INVALID", "PURE_MFA_CODE_INCORRECT", "CLOUDFLARE_BLOCK", "LOGIN_TIMEOUT", "TOKEN_EXCHANGE_STARTED", "CREDENTIAL_VALIDATED"} {
		require.Equal(t, code, bpsLoginSafeCode(code))
	}
	require.Empty(t, bpsLoginSafeCode("TOKEN_SECRET_"+bpsLoginFixtureSecret))
}
