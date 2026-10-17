package admin

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/mail"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

const bpsLoginMaxBody = 1 << 20

var bpsLoginJobID = regexp.MustCompile(`^[A-Za-z0-9_-]{32,80}$`)
var bpsLoginWorkspaceID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

// Credentials only transit the private worker/result -> importer path. Neither
// status nor successful confirmation serializes a credential back to the browser.
type bpsLoginStartRequest struct {
	Email               string `json:"email"`
	Password            string `json:"password"`
	TOTPSecret          string `json:"totp_secret"`
	WorkspaceID         string `json:"workspace_id,omitempty"`
	WorkspacePreference string `json:"workspace_preference,omitempty"`
	ProxyID             *int64 `json:"proxy_id,omitempty"`
}
type bpsLoginPublicStatus struct {
	ID               string              `json:"id"`
	Status           string              `json:"status"`
	Phase            string              `json:"phase"`
	PhaseElapsedMS   int64               `json:"phase_elapsed_ms,omitempty"`
	TotalElapsedMS   int64               `json:"total_elapsed_ms,omitempty"`
	Code             string              `json:"code"`
	Message          string              `json:"message"`
	ExpiresAt        any                 `json:"expires_at,omitempty"`
	ChallengeKind    string              `json:"challenge_kind,omitempty"`
	WorkspaceChoices []bpsLoginWorkspace `json:"workspace_choices,omitempty"`
	Preview          *bpsLoginPreview    `json:"preview,omitempty"`
}
type bpsLoginWorkspace struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Kind string `json:"kind"`
}
type bpsLoginPreview struct {
	Email       string `json:"email,omitempty"`
	WorkspaceID string `json:"workspace_id,omitempty"`
	PlanType    string `json:"plan_type,omitempty"`
	ExpiresAt   any    `json:"expires_at,omitempty"`
}
type bpsLoginWorker struct {
	baseURL, key string
	client       *http.Client
}
type bpsLoginWorkerFailure struct{ status int }

func (e *bpsLoginWorkerFailure) Error() string { return "BPS login worker request failed" }

func newBPSLoginWorker() (*bpsLoginWorker, error) {
	endpoint := strings.TrimRight(strings.TrimSpace(os.Getenv("SUB2_BPS_LOGIN_URL")), "/")
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "/internal/v1" {
		return nil, &bpsLoginWorkerFailure{http.StatusServiceUnavailable}
	}
	path := strings.TrimSpace(os.Getenv("SUB2_BPS_LOGIN_KEY_FILE"))
	if path == "" {
		path = "/app/data/bps-login-key"
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, &bpsLoginWorkerFailure{http.StatusServiceUnavailable}
	}
	defer f.Close()
	keyBytes, err := io.ReadAll(io.LimitReader(f, 4097))
	if err != nil || len(keyBytes) > 4096 {
		return nil, &bpsLoginWorkerFailure{http.StatusServiceUnavailable}
	}
	key := strings.TrimSpace(string(keyBytes))
	if len(key) < 32 || strings.ContainsAny(key, "\r\n") {
		return nil, &bpsLoginWorkerFailure{http.StatusServiceUnavailable}
	}
	return &bpsLoginWorker{endpoint, key, &http.Client{Timeout: 12 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }, Transport: &http.Transport{Proxy: nil, ResponseHeaderTimeout: 10 * time.Second, IdleConnTimeout: 15 * time.Second, MaxIdleConns: 2, MaxConnsPerHost: 8}}}, nil
}
func (w *bpsLoginWorker) call(ctx context.Context, owner int64, method, path string, payload any, timeout time.Duration) (map[string]any, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var body io.Reader
	if payload != nil {
		raw, err := json.Marshal(payload)
		if err != nil {
			return nil, &bpsLoginWorkerFailure{http.StatusBadRequest}
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, w.baseURL+path, body)
	if err != nil {
		return nil, &bpsLoginWorkerFailure{http.StatusBadGateway}
	}
	req.Header.Set("Authorization", "Bearer "+w.key)
	req.Header.Set("X-Sub2-BPS-Owner", strconv.FormatInt(owner, 10))
	req.Header.Set("Content-Type", "application/json")
	defer w.client.CloseIdleConnections()
	resp, err := w.client.Do(req)
	if err != nil {
		return nil, &bpsLoginWorkerFailure{http.StatusBadGateway}
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, bpsLoginMaxBody+1))
	if err != nil || len(raw) > bpsLoginMaxBody {
		return nil, &bpsLoginWorkerFailure{http.StatusBadGateway}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		status := http.StatusBadGateway
		switch resp.StatusCode {
		case 400, 404, 409, 410, 429:
			status = resp.StatusCode
		case 403:
			status = 404
		}
		return nil, &bpsLoginWorkerFailure{status}
	}
	if method == http.MethodDelete && len(bytes.TrimSpace(raw)) == 0 {
		return map[string]any{}, nil
	}
	var out map[string]any
	if json.Unmarshal(raw, &out) != nil || out == nil {
		return nil, &bpsLoginWorkerFailure{http.StatusBadGateway}
	}
	return out, nil
}
func bpsLoginError(c *gin.Context, err error) {
	status := http.StatusBadGateway
	var failure *bpsLoginWorkerFailure
	if errors.As(err, &failure) {
		status = failure.status
	}
	message := "BPS 登录服务暂时不可用"
	switch status {
	case 400:
		message = "BPS 登录请求无效"
	case 404:
		message = "BPS 登录任务不存在"
	case 409:
		message = "BPS 登录任务状态已变化"
	case 410:
		message = "BPS 登录任务已过期"
	case 429:
		message = "BPS 登录任务过多，请稍后重试"
	case 503:
		message = "BPS 登录服务未配置"
	}
	response.Error(c, status, message)
}
func bpsLoginOwner(c *gin.Context) (int64, bool) {
	subject, ok := middleware.GetAuthSubjectFromContext(c)
	if !ok || subject.UserID <= 0 {
		response.Unauthorized(c, "需要管理员登录")
		return 0, false
	}
	return subject.UserID, true
}
func bpsLoginDecode(c *gin.Context, dst any) bool {
	decoder := json.NewDecoder(io.LimitReader(c.Request.Body, 64*1024+1))
	decoder.DisallowUnknownFields()
	if decoder.Decode(dst) != nil {
		response.BadRequest(c, "BPS 登录请求格式无效")
		return false
	}
	var trailing any
	if decoder.Decode(&trailing) != io.EOF {
		response.BadRequest(c, "BPS 登录请求格式无效")
		return false
	}
	return true
}
func bpsLoginValidateJob(c *gin.Context) (string, bool) {
	id := c.Param("jobID")
	if !bpsLoginJobID.MatchString(id) {
		response.BadRequest(c, "BPS 登录任务编号无效")
		return "", false
	}
	return id, true
}
func (h *AccountHandler) bpsLoginProxy(ctx context.Context, id *int64) (string, error) {
	if id == nil {
		return "", nil
	}
	if *id <= 0 {
		return "", &bpsLoginWorkerFailure{http.StatusBadRequest}
	}
	proxy, err := h.adminService.GetProxy(ctx, *id)
	if err != nil || proxy == nil || !proxy.IsActive() || proxy.IsExpired(time.Now()) || proxy.Port < 1 || proxy.Port > 65535 || strings.TrimSpace(proxy.Host) == "" {
		return "", &bpsLoginWorkerFailure{http.StatusBadRequest}
	}
	switch proxy.Protocol {
	case "http", "https", "socks5", "socks5h":
	default:
		return "", &bpsLoginWorkerFailure{http.StatusBadRequest}
	}
	return proxy.URL(), nil
}
func bpsLoginSafeExpiry(v any) any {
	switch n := v.(type) {
	case float64:
		if n > 0 && n < 1e13 {
			return n
		}
	case string:
		if len(n) <= 40 {
			if _, err := time.Parse(time.RFC3339, n); err == nil {
				return n
			}
		}
	}
	return nil
}
func bpsLoginSafeMillis(v any) int64 {
	var n int64
	switch value := v.(type) {
	case float64:
		n = int64(value)
	case json.Number:
		n, _ = value.Int64()
	}
	if n < 0 || n > 600000 {
		return 0
	}
	return n
}

func bpsLoginChoice(s string, choices ...string) string {
	for _, v := range choices {
		if s == v {
			return s
		}
	}
	return ""
}

// Finite CPA canonical diagnostics; arbitrary worker text is not a public code.
func bpsLoginSafeCode(code string) string {
	switch code {
	case "ACCOUNT_MFA_REQUIRED",
		"ACCOUNT_UNUSABLE",
		"AUTHORIZATION_CLIENT_REQUIRED",
		"AUTHORIZATION_CODE_REQUIRED",
		"AUTHORIZATION_READY",
		"AUTHORIZATION_REDIRECT_FAILED",
		"AUTHORIZATION_REDIRECT_LIMIT",
		"AUTHORIZATION_REDIRECT_LOOP",
		"AUTHORIZATION_REDIRECT_NOT_ALLOWED",
		"AUTHORIZATION_REJECTED",
		"AUTHORIZATION_RETRY",
		"AUTHORIZATION_STARTED",
		"AUTHORIZATION_URL_REQUIRED",
		"AUTH_URL_REQUIRED",
		"BUSINESS_USAGE_UNVERIFIED",
		"BUSINESS_WORKSPACE_NOT_FOUND",
		"BUSINESS_WORKSPACE_SELECTED",
		"BUSINESS_WORKSPACE_UNVERIFIED",
		"CANCELLED",
		"CLOUDFLARE_BLOCK",
		"CREDENTIAL_VALIDATED",
		"EMAIL_ACCEPTED",
		"EMAIL_CODE_INVALID",
		"EMAIL_CODE_REQUIRED",
		"EMAIL_CODE_VERIFIED",
		"EMAIL_OTP_INCORRECT",
		"EMAIL_REQUIRED",
		"INVALID_HOP_LIMIT",
		"INVALID_LOGIN_CLIENT",
		"INVALID_LOGIN_RESPONSE",
		"LOGIN_DISABLED",
		"LOGIN_ENGINE_FAILED",
		"LOGIN_HOP_LIMIT",
		"LOGIN_RATE_LIMITED",
		"LOGIN_REJECTED",
		"LOGIN_REQUEST_REJECTED",
		"LOGIN_RETRY_SCHEDULED",
		"LOGIN_STARTED",
		"LOGIN_STEP_FAILED",
		"LOGIN_TEMPORARY_FAILURE",
		"LOGIN_TIMEOUT",
		"LOGIN_TIMEOUT_INVALID",
		"LOGIN_TRANSPORT_INCOMPLETE",
		"LOGIN_TRANSPORT_REQUIRED",
		"LOGIN_WORKER_UNAVAILABLE",
		"MFA_FACTOR_MISMATCH",
		"MFA_FACTOR_REQUIRED",
		"MFA_FACTOR_UNSUPPORTED",
		"MFA_REQUIRED",
		"MFA_STARTED",
		"MFA_VERIFIED",
		"OAUTH_STATE_MISMATCH",
		"OAUTH_STATE_UNVERIFIED",
		"PASSWORD_REQUIRED",
		"PASSWORD_RESET_REQUIRED",
		"PASSWORD_VERIFIED",
		"PHONE_STEP_UNSUPPORTED",
		"PURE_BOOTSTRAP_FAILED",
		"PURE_BOOTSTRAP_NOT_ADVANCED",
		"PURE_BOOTSTRAP_NO_SESSION",
		"PURE_EMAIL_OTP_FAILED",
		"PURE_MFA_CODE_INCORRECT",
		"PURE_MFA_ISSUE_FAILED",
		"PURE_MFA_LOCKED",
		"PURE_MFA_TOTP_CALC_FAILED",
		"PURE_MFA_VERIFY_FAILED",
		"PURE_PASSWORD_INVALID",
		"PURE_PASSWORD_VERIFY_FAILED",
		"PURE_SUBMIT_EMAIL_FAILED",
		"REPEATED_LOGIN_STATE",
		"RT_WORKSPACE_NOT_FOUND",
		"SDK_DIAGNOSTICS_REQUIRED",
		"SDK_FLOW_MISMATCH",
		"SDK_INTERNAL_ERROR_PAYLOAD",
		"SDK_RESULT_INCOMPLETE",
		"SDK_RESULT_INVALID_ENVELOPE",
		"SDK_RESULT_INVALID_JSON",
		"SDK_RESULT_INVALID_STRUCTURE",
		"SDK_RESULT_MISSING",
		"SDK_RESULT_TOO_LARGE",
		"SDK_VERSION_DRIFT",
		"SENTINEL_REJECTED",
		"SESSION_READY",
		"TOKEN_ACCOUNT_MISMATCH",
		"TOKEN_CLIENT_MISMATCH",
		"TOKEN_ENDPOINT_NOT_ALLOWED",
		"TOKEN_EXCHANGE_COMPLETED",
		"TOKEN_EXCHANGE_REJECTED",
		"TOKEN_EXCHANGE_STARTED",
		"TOKEN_EXCHANGE_TEMPORARY_FAILURE",
		"TOKEN_EXPIRED",
		"TOKEN_EXPIRY_INVALID",
		"TOKEN_INCOMPLETE",
		"TOKEN_RESPONSE_INVALID",
		"TOKEN_STRUCTURE_INVALID",
		"TOKEN_VALIDATION_STARTED",
		"TOKEN_WORKSPACE_MISMATCH",
		"TOKEN_WORKSPACE_MISSING",
		"TOTP_CODE_INVALID",
		"TOTP_PROVIDER_REQUIRED",
		"TOTP_SECRET_INVALID",
		"UNKNOWN_LOGIN_PAGE",
		"UPSTREAM_UNAVAILABLE",
		"WORKSPACE_CHECK_STARTED",
		"WORKSPACE_LOOKUP_INCOMPLETE",
		"WORKSPACE_LOOKUP_REQUIRED",
		"WORKSPACE_MISMATCH",
		"WORKSPACE_NOT_FOUND",
		"WORKSPACE_SELECTION_REQUIRED",
		"WORKSPACE_UNVERIFIED":
		return code
	}
	return bpsLoginChoice(code, "ok", "queued", "running", "ready", "completed", "email_otp_required", "workspace_required", "invalid_credentials", "login_failed", "timeout", "cancelled", "expired")
}

func bpsLoginStatus(raw map[string]any, expectedID string) (bpsLoginPublicStatus, error) {
	id, _ := raw["id"].(string)
	if !bpsLoginJobID.MatchString(id) || (expectedID != "" && id != expectedID) {
		return bpsLoginPublicStatus{}, &bpsLoginWorkerFailure{http.StatusBadGateway}
	}
	str := func(k string) string { s, _ := raw[k].(string); return s }
	state := bpsLoginChoice(str("status"), "queued", "running", "awaiting_input", "ready", "completed", "failed", "cancelled", "expired")
	if state == "" {
		return bpsLoginPublicStatus{}, &bpsLoginWorkerFailure{http.StatusBadGateway}
	}
	out := bpsLoginPublicStatus{ID: id, Status: state, Phase: bpsLoginChoice(str("phase"), "session", "email", "password", "mfa", "email_otp", "authorization", "exchange", "validate", "queued", "workspace", "success", "completed", "failed", "cancelled"), PhaseElapsedMS: bpsLoginSafeMillis(raw["phase_elapsed_ms"]), TotalElapsedMS: bpsLoginSafeMillis(raw["total_elapsed_ms"]), Code: bpsLoginSafeCode(str("code")), ExpiresAt: bpsLoginSafeExpiry(raw["expires_at"]), ChallengeKind: bpsLoginChoice(str("challenge_kind"), "email_otp", "workspace")}
	out.Message = map[string]string{"queued": "等待登录", "running": "正在登录", "awaiting_input": "需要补充验证信息", "ready": "登录完成，请确认导入", "completed": "登录完成", "failed": "登录失败，请检查账号或重新创建任务", "cancelled": "任务已取消", "expired": "任务已过期"}[state]
	if choices, ok := raw["workspace_choices"].([]any); ok {
		for _, choice := range choices {
			if len(out.WorkspaceChoices) >= 100 {
				break
			}
			m, ok := choice.(map[string]any)
			if !ok {
				continue
			}
			id, _ := m["id"].(string)
			name, _ := m["name"].(string)
			kind, _ := m["kind"].(string)
			kind = bpsLoginChoice(kind, "team", "personal", "business", "enterprise", "unknown")
			if bpsLoginWorkspaceID.MatchString(id) && len(name) <= 256 && !strings.ContainsAny(name, "\r\n") {
				out.WorkspaceChoices = append(out.WorkspaceChoices, bpsLoginWorkspace{id, name, kind})
			}
		}
	}
	if preview, ok := raw["preview"].(map[string]any); ok {
		p := &bpsLoginPreview{ExpiresAt: bpsLoginSafeExpiry(preview["expires_at"])}
		email, _ := preview["email"].(string)
		if parsed, err := mail.ParseAddress(email); err == nil && parsed.Address == email && len(email) <= 254 {
			p.Email = email
		}
		workspace, _ := preview["workspace_id"].(string)
		if bpsLoginWorkspaceID.MatchString(workspace) {
			p.WorkspaceID = workspace
		}
		plan, _ := preview["plan_type"].(string)
		if len(plan) <= 64 && regexp.MustCompile(`^[a-zA-Z0-9_-]*$`).MatchString(plan) {
			p.PlanType = plan
		}
		out.Preview = p
	}
	return out, nil
}

func (h *AccountHandler) StartBPSLogin(c *gin.Context) {
	owner, ok := bpsLoginOwner(c)
	if !ok {
		return
	}
	var in bpsLoginStartRequest
	if !bpsLoginDecode(c, &in) {
		return
	}
	email, err := mail.ParseAddress(in.Email)
	if err != nil || email.Address != in.Email || len(in.Email) > 254 || in.Password == "" || len(in.Password) > 4096 || len(in.TOTPSecret) > 512 || (in.WorkspaceID != "" && !bpsLoginWorkspaceID.MatchString(in.WorkspaceID)) || (in.WorkspacePreference != "" && bpsLoginChoice(in.WorkspacePreference, "team_first", "team", "personal", "first") == "") {
		response.BadRequest(c, "邮箱、密码或工作区参数无效")
		return
	}
	if in.WorkspacePreference == "" {
		in.WorkspacePreference = "team_first"
	}
	proxy, err := h.bpsLoginProxy(c.Request.Context(), in.ProxyID)
	if err != nil {
		bpsLoginError(c, err)
		return
	}
	worker, err := newBPSLoginWorker()
	if err != nil {
		bpsLoginError(c, err)
		return
	}
	raw, err := worker.call(c.Request.Context(), owner, http.MethodPost, "/jobs", map[string]any{"email": in.Email, "password": in.Password, "totp_secret": in.TOTPSecret, "workspace_id": in.WorkspaceID, "workspace_preference": in.WorkspacePreference, "proxy_url": proxy}, 12*time.Second)
	in.Password, in.TOTPSecret = "", ""
	if err != nil {
		bpsLoginError(c, err)
		return
	}
	status, err := bpsLoginStatus(raw, "")
	if err != nil {
		bpsLoginError(c, err)
		return
	}
	receipt, release, err := acquireBPSLoginReceipt(owner, status.ID)
	if err != nil {
		cleanup, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		_, _ = worker.call(cleanup, owner, http.MethodDelete, "/jobs/"+status.ID, nil, 8*time.Second)
		bpsLoginError(c, err)
		return
	}
	receipt.started = true
	receipt.workspaceID = in.WorkspaceID
	if in.ProxyID != nil {
		id := *in.ProxyID
		receipt.proxyID = &id
	}
	release()
	response.Accepted(c, status)
}
func (h *AccountHandler) GetBPSLogin(c *gin.Context)         { h.bpsLoginStatusAction(c, false) }
func (h *AccountHandler) SubmitBPSLoginInput(c *gin.Context) { h.bpsLoginStatusAction(c, true) }
func (h *AccountHandler) bpsLoginStatusAction(c *gin.Context, submit bool) {
	owner, ok := bpsLoginOwner(c)
	if !ok {
		return
	}
	id, ok := bpsLoginValidateJob(c)
	if !ok {
		return
	}
	method, path, payload := http.MethodGet, "/jobs/"+id, any(nil)
	selectedWorkspace := ""
	if submit {
		var in struct {
			Kind  string `json:"kind"`
			Value string `json:"value"`
		}
		if !bpsLoginDecode(c, &in) {
			return
		}
		if in.Kind == "email_otp" {
			if !regexp.MustCompile(`^[0-9]{4,10}$`).MatchString(in.Value) {
				response.BadRequest(c, "邮箱验证码格式无效")
				return
			}
		} else if in.Kind == "workspace" {
			selectedWorkspace = in.Value
			if !bpsLoginWorkspaceID.MatchString(in.Value) {
				response.BadRequest(c, "工作区编号无效")
				return
			}
		} else {
			response.BadRequest(c, "验证类型无效")
			return
		}
		method, path, payload = http.MethodPost, path+"/input", in
	}
	worker, err := newBPSLoginWorker()
	if err != nil {
		bpsLoginError(c, err)
		return
	}
	raw, err := worker.call(c.Request.Context(), owner, method, path, payload, 8*time.Second)
	if err != nil {
		bpsLoginError(c, err)
		return
	}
	status, err := bpsLoginStatus(raw, id)
	if err != nil {
		bpsLoginError(c, err)
		return
	}
	if selectedWorkspace != "" {
		receipt, release, err := acquireBPSLoginReceipt(owner, id)
		if err != nil {
			bpsLoginError(c, err)
			return
		}
		receipt.workspaceID = selectedWorkspace
		release()
	}
	response.Success(c, status)
}

// Bounded, owner-scoped receipts serialize confirmation/cancellation and retain
// unknown import outcomes rather than blindly creating another account.
type bpsLoginReceipt struct {
	mu                   sync.Mutex
	refs                 int
	expires              time.Time
	hash                 [32]byte
	attempted, cancelled bool
	started              bool
	proxyID              *int64
	workspaceID          string
	result               *CodexSessionImportResult
}

var bpsLoginReceipts = struct {
	sync.Mutex
	entries map[string]*bpsLoginReceipt
}{entries: map[string]*bpsLoginReceipt{}}

func acquireBPSLoginReceipt(owner int64, id string) (*bpsLoginReceipt, func(), error) {
	key := strconv.FormatInt(owner, 10) + ":" + id
	now := time.Now()
	bpsLoginReceipts.Lock()
	for k, v := range bpsLoginReceipts.entries {
		if v.refs == 0 && v.expires.Before(now) {
			delete(bpsLoginReceipts.entries, k)
		}
	}
	r := bpsLoginReceipts.entries[key]
	if r == nil {
		if len(bpsLoginReceipts.entries) >= 256 {
			bpsLoginReceipts.Unlock()
			return nil, nil, &bpsLoginWorkerFailure{http.StatusTooManyRequests}
		}
		r = &bpsLoginReceipt{expires: now.Add(2 * time.Hour)}
		bpsLoginReceipts.entries[key] = r
	}
	r.refs++
	bpsLoginReceipts.Unlock()
	r.mu.Lock()
	return r, func() {
		r.mu.Unlock()
		bpsLoginReceipts.Lock()
		r.refs--
		r.expires = time.Now().Add(2 * time.Hour)
		bpsLoginReceipts.Unlock()
	}, nil
}
func (h *AccountHandler) CancelBPSLogin(c *gin.Context) {
	owner, ok := bpsLoginOwner(c)
	if !ok {
		return
	}
	id, ok := bpsLoginValidateJob(c)
	if !ok {
		return
	}
	r, release, err := acquireBPSLoginReceipt(owner, id)
	if err != nil {
		bpsLoginError(c, err)
		return
	}
	defer release()
	if r.result != nil || r.cancelled {
		response.Success(c, map[string]any{"id": id, "status": "cancelled", "message": "任务已取消"})
		return
	}
	worker, err := newBPSLoginWorker()
	if err != nil {
		bpsLoginError(c, err)
		return
	}
	_, err = worker.call(c.Request.Context(), owner, http.MethodDelete, "/jobs/"+id, nil, 8*time.Second)
	if err != nil {
		bpsLoginError(c, err)
		return
	}
	r.cancelled = true
	response.Success(c, map[string]any{"id": id, "status": "cancelled", "message": "任务已取消"})
}
func (h *AccountHandler) ImportBPSLogin(c *gin.Context) {
	owner, ok := bpsLoginOwner(c)
	if !ok {
		return
	}
	id, ok := bpsLoginValidateJob(c)
	if !ok {
		return
	}
	var req CodexSessionImportRequest
	if !bpsLoginDecode(c, &req) {
		return
	}
	if req.Content != "" || len(req.Contents) > 0 {
		response.BadRequest(c, "确认导入仅接受服务器保存的 BPS 凭据")
		return
	}
	if (req.Concurrency != nil && *req.Concurrency < 0) || (req.Priority != nil && *req.Priority < 0) || (req.RateMultiplier != nil && *req.RateMultiplier < 0) || (req.LoadFactor != nil && *req.LoadFactor > 10000) || service.ValidateOpenAILongContextBillingExtra(service.PlatformOpenAI, req.Extra) != nil {
		response.BadRequest(c, "导入设置无效")
		return
	}
	if req.UpdateExisting != nil && !*req.UpdateExisting {
		response.BadRequest(c, "BPS 登录导入必须保持同身份账号关联")
		return
	}
	receipt, release, err := acquireBPSLoginReceipt(owner, id)
	if err != nil {
		bpsLoginError(c, err)
		return
	}
	defer release()
	if !receipt.started {
		response.Error(c, http.StatusConflict, "登录任务确认信息已失效，请重新创建登录任务")
		return
	}
	if req.ProxyID != nil && (receipt.proxyID == nil || *req.ProxyID != *receipt.proxyID) {
		response.BadRequest(c, "导入代理必须与本次登录选择保持一致")
		return
	}
	req.ProxyID = receipt.proxyID
	options, _ := json.Marshal(req)
	hash := sha256.Sum256(options)
	if receipt.attempted {
		if receipt.hash != hash {
			response.Error(c, http.StatusConflict, "该任务已按另一组设置确认导入")
			return
		}
		if receipt.result != nil {
			response.Success(c, receipt.result)
			return
		}
		response.Error(c, http.StatusConflict, "此前导入结果尚未确认，请先核对账号，勿重复提交")
		return
	}
	if receipt.cancelled {
		response.Error(c, http.StatusGone, "BPS 登录任务已取消")
		return
	}
	if _, err := h.bpsLoginProxy(c.Request.Context(), req.ProxyID); err != nil {
		bpsLoginError(c, err)
		return
	}
	worker, err := newBPSLoginWorker()
	if err != nil {
		bpsLoginError(c, err)
		return
	}
	raw, err := worker.call(c.Request.Context(), owner, http.MethodPost, "/jobs/"+id+"/result", map[string]any{}, 12*time.Second)
	if err != nil {
		bpsLoginError(c, err)
		return
	}
	credential, ok := raw["credential"].(map[string]any)
	if !ok {
		response.Error(c, http.StatusBadGateway, "BPS 登录凭据结果无效")
		return
	}
	item, err := normalizeCodexImportEntry(codexImportEntry{Index: 1, Value: credential})
	if err != nil || !item.IsBPS || (receipt.workspaceID != "" && item.AccountID != receipt.workspaceID) {
		response.Error(c, http.StatusBadGateway, "BPS 登录凭据身份校验失败")
		return
	}
	receipt.attempted, receipt.hash = true, hash
	result, err := h.importCodexSessions(c.Request.Context(), req, []codexImportEntry{{Index: 1, Value: credential}})
	if err != nil {
		response.Error(c, http.StatusBadGateway, "导入结果尚未确认，请核对账号后处理")
		return
	}
	// The importer can surface repository errors. Never echo those or worker
	// credential-derived names; preserve only fixed actions/counts/IDs.
	for n := range result.Items {
		result.Items[n].Name = ""
		if result.Items[n].Message != "" {
			result.Items[n].Message = "请核对账号身份、分组与导入设置"
		}
	}
	for n := range result.Errors {
		result.Errors[n].Name = ""
		result.Errors[n].Message = "导入失败，请核对账号身份、分组与导入设置"
	}
	for n := range result.Warnings {
		result.Warnings[n].Name = ""
		result.Warnings[n].Message = "BPS 凭据独立保存；原生凭据与业务配置保持不变"
	}
	receipt.result = &result
	// Erase the worker's retained credentials after a definitive local receipt.
	cleanup, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	_, _ = worker.call(cleanup, owner, http.MethodDelete, "/jobs/"+id, nil, 8*time.Second)
	response.Success(c, result)
}
