package service

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

// updateBPSProfile deliberately bypasses general account edits. Adding or
// rotating one OAuth profile must not change status, expiry, routing or native RT.
func (s *adminServiceImpl) updateBPSProfile(ctx context.Context, account *Account, credentials map[string]any) (*Account, error) {
	if account == nil || account.Platform != PlatformOpenAI || account.Type != AccountTypeOAuth || account.IsCredentialShadow() {
		return nil, infraerrors.New(http.StatusBadRequest, "BPS_UNSUPPORTED_ACCOUNT", "BPS credentials require an OpenAI OAuth account")
	}
	profile, ok := credentials[BPSCredentialKey].(map[string]any)
	if !ok || len(credentials) != 1 {
		return nil, infraerrors.New(http.StatusBadRequest, "BPS_INVALID_PROFILE", "import a complete BPS credential profile")
	}
	candidate := snapshotOAuthRefreshAccount(account)
	candidate.Credentials[BPSCredentialKey] = profile
	if _, err := BPSCredentialsForAccount(candidate); err != nil {
		return nil, infraerrors.New(http.StatusBadRequest, "BPS_IDENTITY_MISMATCH", "BPS credential client, user and workspace must match the selected account")
	}
	repo, ok := s.accountRepo.(BPSCredentialRepository)
	if !ok {
		return nil, infraerrors.New(http.StatusServiceUnavailable, "BPS_STORAGE_UNAVAILABLE", "BPS profile storage is unavailable")
	}
	applied, err := repo.UpdateBPSCredentialsIfUnchanged(ctx, account.ID, account, profile)
	if err != nil {
		return nil, err
	}
	if !applied {
		return nil, infraerrors.New(http.StatusConflict, "BPS_PROFILE_CHANGED", "account credentials changed during import; reload and retry")
	}
	return s.accountRepo.GetByID(ctx, account.ID)
}

// ValidateStoredOpenAIProfiles is shared by ordinary and backup-file creation.
// A BPS-only account is valid, but its missing native slot is never invented.
func ValidateStoredOpenAIProfiles(account *Account) error {
	if account == nil || account.Platform != PlatformOpenAI || !account.IsOpenAIOAuthLike() {
		return nil
	}
	if account.GetOpenAIAccessToken() != "" {
		if _, err := ParseBPSCredentialClaims(account.GetOpenAIAccessToken()); err == nil {
			return infraerrors.New(http.StatusBadRequest, "BPS_NATIVE_SLOT_INVALID", "import BPS credentials using the native/BPS JSON import; do not replace the native token")
		}
	}
	if _, exists := account.Credentials[BPSCredentialKey]; exists {
		if native := account.GetOpenAIAccessToken(); native != "" {
			parts := strings.Split(native, ".")
			var claims BPSCredentialClaims
			valid := len(parts) == 3
			if valid {
				payload, err := base64.RawURLEncoding.DecodeString(parts[1])
				valid = err == nil && json.Unmarshal(payload, &claims) == nil
			}
			if claims.Auth.UserID == "" {
				claims.Auth.UserID = claims.Auth.User
			}
			if !valid || claims.Subject == "" || claims.Auth.AccountID == "" || claims.Auth.UserID == "" {
				return infraerrors.New(http.StatusBadRequest, "BPS_NATIVE_IDENTITY_UNPROVEN", "dual-profile files require a native JWT with verifiable user and workspace claims")
			}
		}
		if _, err := BPSCredentialsForAccount(account); err != nil {
			return infraerrors.New(http.StatusBadRequest, "BPS_IDENTITY_MISMATCH", "BPS profile must match the account client, user and workspace")
		}
	}
	return nil
}
