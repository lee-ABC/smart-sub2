package service

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"github.com/gin-gonic/gin"
	"io"
	"net/http"
	"strings"
)

// Trusted per-attempt context, never populated from inbound identity headers.
type excelRequestRoute struct {
	config         *ExcelRoutingConfig
	keyID, groupID int64
	source         string
}
type excelRequestRouteKey struct{}

func prepareExcelRequestContext(ctx context.Context, c *gin.Context, account *Account, model string) (context.Context, error) {
	ctx = context.WithValue(ctx, excelRequestRouteKey{}, (*excelRequestRoute)(nil))
	if account == nil || !account.IsOpenAIOAuthLike() || !IsExcelSupportedModel(model) || isOpenAIResponsesCompactPath(c) {
		return ctx, nil
	}
	key := getAPIKeyFromContext(c)
	if key == nil || key.GroupID == nil || account == nil || account.Platform != PlatformOpenAI {
		return ctx, nil
	}
	cfg, err := ReadExcelRoutingConfig()
	if err != nil {
		return ctx, err
	}
	return prepareExcelGroupContext(ctx, cfg, account, *key.GroupID, key.ID, "api")
}

func prepareExcelGroupContext(ctx context.Context, cfg *ExcelRoutingConfig, account *Account, groupID, keyID int64, source string) (context.Context, error) {
	if groupID <= 0 || cfg.Mode(groupID) != "excel" {
		return ctx, nil
	}
	if !account.IsOpenAIOAuthLike() {
		return ctx, &UpstreamFailoverError{StatusCode: 400, ClientStatusCode: 400, ClientMessage: "Excel mode requires a ChatGPT OAuth account"}
	}
	if _, err := cfg.TransportKey(); err != nil {
		return ctx, err
	}
	return context.WithValue(ctx, excelRequestRouteKey{}, &excelRequestRoute{config: cfg, keyID: keyID, groupID: groupID, source: source}), nil
}

func isExcelRequest(ctx context.Context) bool {
	route, ok := ctx.Value(excelRequestRouteKey{}).(*excelRequestRoute)
	return ok && route != nil
}

// The real account stays in the native response/billing/rate-limit/failover path.
// The sidecar never selects another account and never calls Sub2 again.
func (s *OpenAIGatewayService) forwardExcelHTTP(request *http.Request, proxyURL string, account *Account) (*http.Response, bool, error) {
	route, ok := request.Context().Value(excelRequestRouteKey{}).(*excelRequestRoute)
	if !ok || route == nil {
		return nil, false, nil
	}
	if !strings.HasSuffix(request.URL.Path, "/responses") {
		body := `{"error":{"type":"invalid_request_error","message":"This endpoint is not supported in Excel mode"}}`
		return &http.Response{StatusCode: 400, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(body)), Request: request}, true, nil
	}
	serviceKey, err := route.config.TransportKey()
	if err != nil {
		return nil, true, err
	}
	// Reuse the caller's resolved authentication. Do not refresh twice or
	// bypass the intelligence runner's read-only credential repository.
	authorization := strings.Fields(request.Header.Get("Authorization"))
	if len(authorization) != 2 || !strings.EqualFold(authorization[0], "Bearer") {
		return nil, true, fmt.Errorf("selected account has no prepared OAuth bearer token")
	}
	token := authorization[1]
	identity, err := resolveCredentialAccount(request.Context(), s.accountRepo, account)
	if err != nil {
		return nil, true, err
	}
	workspace := identity.GetChatGPTAccountID()
	if workspace == "" {
		return nil, true, fmt.Errorf("selected account has no ChatGPT workspace identity")
	}
	headers := http.Header{}
	headers.Set("Authorization", "Bearer "+serviceKey)
	headers.Set("Content-Type", "application/json")
	headers.Set("Accept", request.Header.Get("Accept"))
	headers.Set("X-Sub2-Excel-Access-Token", token)
	headers.Set("X-Sub2-Excel-Account-Id", workspace)
	// Preserve existing public-request session scopes across this upgrade.
	scope := fmt.Sprintf("%d:%d:%d:%s", route.keyID, route.groupID, account.ID, workspace)
	if route.source == "account-test" {
		scope = "account-test:" + scope
	}
	headers.Set("X-Sub2-Excel-Scope", fmt.Sprintf("%x", sha256.Sum256([]byte(scope))))
	if user := identity.GetChatGPTUserID(); user != "" {
		headers.Set("X-Sub2-Excel-Account-User-Id", user)
	}
	if proxyURL != "" {
		headers.Set("X-Sub2-Excel-Proxy", proxyURL)
	}
	req, err := http.NewRequestWithContext(request.Context(), http.MethodPost, strings.TrimRight(route.config.GatewayURL, "/")+"/responses", request.Body)
	if err != nil {
		return nil, true, err
	}
	req.Header, req.ContentLength, req.GetBody = headers, request.ContentLength, request.GetBody
	// Delegate the account proxy upstream, not to the internal Docker hop.
	resp, err := s.httpUpstream.Do(WithAccountTrafficRequest(req, account), "", account.ID, account.Mode1EffectiveConcurrency())
	if err != nil && !errors.Is(err, context.Canceled) {
		return nil, true, &excelTransportError{}
	}
	if resp != nil && resp.Header.Get("X-Sub2-Excel-Failure") == "transport" {
		if resp.Body != nil {
			resp.Body.Close()
		}
		return nil, true, &excelTransportError{}
	}
	return resp, true, err
}

type excelTransportError struct{}

func (*excelTransportError) Error() string { return "Excel private transport unavailable" }
func excelTransportFailover() *UpstreamFailoverError {
	return &UpstreamFailoverError{StatusCode: 503, ClientStatusCode: 503, Stage: GatewayFailureStageAccountAuth, Scope: GatewayFailureScopeProvider, Reason: "excel_transport_unavailable", NextAccountAction: NextAccountStop, RequestScopedTransient: true, ClientMessage: "Excel private transport unavailable; retry later"}
}
