package service

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"
)

const BPSCredentialKey = "bps_credentials"
const BPSOAuthClientID = "app_fnr0pYvVwwFDocDumLG3H2Bp"

type bpsCredentialError struct{ code string }

func (e *bpsCredentialError) Error() string  { return "BPS credentials unavailable: " + e.code }
func bpsCredentialFailure(code string) error { return &bpsCredentialError{code: code} }

type BPSCredentialClaims struct {
	ClientID  string `json:"client_id"`
	Subject   string `json:"sub"`
	ExpiresAt int64  `json:"exp"`
	Auth      struct {
		AccountID string `json:"chatgpt_account_id"`
		UserID    string `json:"chatgpt_user_id"`
		User      string `json:"user_id"`
		Plan      string `json:"chatgpt_plan_type"`
	} `json:"https://api.openai.com/auth"`
}

// ParseBPSCredentialClaims performs structural and client/identity validation.
// Signature verification remains the upstream's responsibility; these claims
// are never used to authorize access to a different local account or user.
func ParseBPSCredentialClaims(token string) (*BPSCredentialClaims, error) {
	parts := strings.Split(strings.TrimSpace(token), ".")
	if len(parts) != 3 || len(parts[1]) > 1024*1024 {
		return nil, bpsCredentialFailure("invalid_token")
	}
	data, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, bpsCredentialFailure("invalid_token")
	}
	var claims BPSCredentialClaims
	if json.Unmarshal(data, &claims) != nil || claims.ClientID != BPSOAuthClientID || claims.ExpiresAt <= 0 {
		return nil, bpsCredentialFailure("wrong_client_or_token")
	}
	if claims.Auth.UserID == "" {
		claims.Auth.UserID = claims.Auth.User
	}
	if claims.Subject == "" || claims.Auth.AccountID == "" || claims.Auth.UserID == "" {
		return nil, bpsCredentialFailure("missing_identity")
	}
	return &claims, nil
}

func bpsString(profile map[string]any, key string) string {
	s, _ := profile[key].(string)
	return strings.TrimSpace(s)
}

// BPSCredentialsForAccount returns a shallow immutable profile projection.
// It deliberately permits an expired AT so refresh can validate its identity.
func BPSCredentialsForAccount(account *Account) (map[string]any, error) {
	if account == nil || account.Platform != PlatformOpenAI || !account.IsOpenAIOAuthLike() {
		return nil, bpsCredentialFailure("unsupported_account")
	}
	profile, ok := account.Credentials[BPSCredentialKey].(map[string]any)
	if !ok || len(profile) == 0 {
		return nil, bpsCredentialFailure("missing_profile")
	}
	claims, err := ParseBPSCredentialClaims(bpsString(profile, "access_token"))
	if err != nil {
		return nil, err
	}
	for key, want := range map[string]string{"client_id": BPSOAuthClientID, "chatgpt_account_id": claims.Auth.AccountID, "chatgpt_user_id": claims.Auth.UserID} {
		if got := bpsString(profile, key); got != "" && got != want {
			return nil, bpsCredentialFailure("profile_identity_mismatch")
		}
	}
	if expected := account.GetChatGPTAccountID(); expected != "" && expected != claims.Auth.AccountID {
		return nil, bpsCredentialFailure("workspace_mismatch")
	}
	if expected := account.GetChatGPTUserID(); expected != "" && expected != claims.Auth.UserID {
		return nil, bpsCredentialFailure("user_mismatch")
	}
	if native := account.GetOpenAIAccessToken(); native != "" {
		parts := strings.Split(native, ".")
		if len(parts) == 3 {
			if data, decodeErr := base64.RawURLEncoding.DecodeString(parts[1]); decodeErr == nil {
				var other BPSCredentialClaims
				if json.Unmarshal(data, &other) == nil {
					if other.Auth.UserID == "" {
						other.Auth.UserID = other.Auth.User
					}
					if other.Auth.UserID != "" && other.Auth.UserID != claims.Auth.UserID {
						return nil, bpsCredentialFailure("user_mismatch")
					}
					if other.Subject != "" && other.Subject != claims.Subject {
						return nil, bpsCredentialFailure("user_mismatch")
					}
					if other.Auth.AccountID != "" && other.Auth.AccountID != claims.Auth.AccountID {
						return nil, bpsCredentialFailure("workspace_mismatch")
					}
				}
			}
		}
	}
	copy := make(map[string]any, len(profile))
	for k, v := range profile {
		copy[k] = v
	}
	return copy, nil
}

func BPSCredentialIdentity(account *Account) (workspace, user string, err error) {
	profile, err := BPSCredentialsForAccount(account)
	if err != nil {
		return "", "", err
	}
	claims, err := ParseBPSCredentialClaims(bpsString(profile, "access_token"))
	if err != nil {
		return "", "", err
	}
	return claims.Auth.AccountID, claims.Auth.UserID, nil
}

// usesBPSOnlyQuotaCredentials selects BPS only for read-only quota queries.
// Never fall back after a native failure or mix credential namespaces.
func usesBPSOnlyQuotaCredentials(account *Account) bool {
	if account == nil || !account.IsOpenAIOAuth() || account.IsOpenAIAgentIdentity() {
		return false
	}
	if strings.TrimSpace(account.GetOpenAIAccessToken()) != "" || strings.TrimSpace(account.GetOpenAIRefreshToken()) != "" {
		return false
	}
	_, present := account.Credentials[BPSCredentialKey]
	return present
}

func NativeOAuthCredentialError(account *Account) error {
	if account == nil || account.Platform != PlatformOpenAI || !account.IsOpenAIOAuthLike() {
		return nil
	}
	token := account.GetOpenAIAccessToken()
	if token == "" {
		if _, hasBPS := account.Credentials[BPSCredentialKey]; hasBPS {
			return errors.New("native OAuth credentials are missing; import the native credential file")
		}
		return nil
	}
	if _, err := ParseBPSCredentialClaims(token); err == nil {
		return errors.New("BPS OAuth token cannot be used as native credentials; import the two profiles separately")
	}
	return nil
}

func bpsProfileToken(account *Account) (string, error) {
	profile, err := BPSCredentialsForAccount(account)
	if err != nil {
		return "", err
	}
	if _, pending := profile["workspace_refresh_pending"]; pending {
		return "", bpsCredentialFailure("workspace_refresh_pending")
	}
	claims, err := ParseBPSCredentialClaims(bpsString(profile, "access_token"))
	if err != nil {
		return "", err
	}
	if time.Now().Unix() >= claims.ExpiresAt {
		return "", bpsCredentialFailure("expired")
	}
	return bpsString(profile, "access_token"), nil
}

func outboundCredentialFailover(err error, bps bool) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	reason := GatewayFailureReason("native_credentials_unavailable")
	message := "Native OAuth credentials are missing or invalid; import the native credential file"
	if bps {
		reason = "bps_credentials_unavailable"
		message = "BPS OAuth credentials are missing, expired or inconsistent; import the BPS credential file"
	}
	return &UpstreamFailoverError{Stage: GatewayFailureStageAccountAuth, Scope: GatewayFailureScopeRequest, Reason: reason, NextAccountAction: NextAccountRetry, ClientStatusCode: http.StatusServiceUnavailable, ClientMessage: message, RequestScopedTransient: true}
}
