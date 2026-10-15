package service

import (
	"context"
	"strconv"
	"time"
)

// BPSCredentialRepository changes only the BPS slot, so a concurrent native
// token refresh cannot be overwritten by a BPS refresh (or vice versa).
type BPSCredentialRepository interface {
	UpdateBPSCredentialsIfUnchanged(context.Context, int64, *Account, map[string]any) (bool, error)
}

func (p *OpenAITokenProvider) GetBPSAccessToken(ctx context.Context, account *Account) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	profile, err := BPSCredentialsForAccount(account)
	if err != nil {
		return "", err
	}
	claims, err := ParseBPSCredentialClaims(bpsString(profile, "access_token"))
	if err != nil {
		return "", err
	}
	if _, pending := profile["workspace_refresh_pending"]; pending {
		return "", bpsCredentialFailure("workspace_refresh_pending")
	}
	if time.Until(time.Unix(claims.ExpiresAt, 0)) > openAITokenRefreshSkew {
		return bpsProfileToken(account)
	}
	if _, pending := profile["refresh_pending"]; pending {
		return bpsProfileToken(account)
	}
	if bpsString(profile, "refresh_token") == "" {
		return bpsProfileToken(account)
	}
	if p == nil || p.accountRepo == nil || p.openAIOAuthService == nil || p.tokenCache == nil {
		return "", bpsCredentialFailure("refresh_unavailable")
	}
	repo, ok := p.accountRepo.(BPSCredentialRepository)
	if !ok {
		return "", bpsCredentialFailure("refresh_storage_unavailable")
	}
	key := "openai:bps:account:" + strconv.FormatInt(account.ID, 10)
	actual, _ := p.bpsRefreshLocks.LoadOrStore(account.ID, newContextMutex())
	mu := actual.(*contextMutex)
	if err := mu.Lock(ctx); err != nil {
		return "", err
	}
	defer mu.Unlock()
	acquired, err := p.tokenCache.AcquireRefreshLock(ctx, key, 60*time.Second)
	if err != nil {
		return "", bpsCredentialFailure("refresh_lock_unavailable")
	}
	if !acquired {
		return p.readCurrentBPSToken(ctx, account.ID, claims)
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
		defer cancel()
		_ = p.tokenCache.ReleaseRefreshLock(cleanup, key)
	}()
	fresh, err := p.accountRepo.GetByID(ctx, account.ID)
	if err != nil || fresh == nil || !fresh.IsActive() {
		return "", bpsCredentialFailure("account_unavailable")
	}
	current, err := BPSCredentialsForAccount(fresh)
	if err != nil {
		return "", err
	}
	oldClaims, err := ParseBPSCredentialClaims(bpsString(current, "access_token"))
	if err != nil {
		return "", err
	}
	if oldClaims.Subject != claims.Subject || oldClaims.Auth.AccountID != claims.Auth.AccountID || oldClaims.Auth.UserID != claims.Auth.UserID {
		return "", bpsCredentialFailure("account_changed")
	}
	if _, pending := current["workspace_refresh_pending"]; pending {
		return "", bpsCredentialFailure("workspace_refresh_pending")
	}
	if _, pending := current["refresh_pending"]; pending {
		return bpsProfileToken(fresh)
	}
	if time.Until(time.Unix(oldClaims.ExpiresAt, 0)) > openAITokenRefreshSkew {
		return bpsProfileToken(fresh)
	}
	refreshToken := bpsString(current, "refresh_token")
	if refreshToken == "" {
		return bpsProfileToken(fresh)
	}

	// Persist the attempt before spending a rotating refresh token. Unknown
	// outcomes remain pending, rather than repeatedly consuming the same RT.
	pending := shallowCopyMap(current)
	pending["refresh_pending"] = map[string]any{"started_at": time.Now().UTC().Format(time.RFC3339), "reason": "in_progress"}
	applied, err := repo.UpdateBPSCredentialsIfUnchanged(ctx, fresh.ID, fresh, pending)
	if err != nil {
		return "", bpsCredentialFailure("refresh_storage_unavailable")
	}
	if !applied {
		return p.readCurrentBPSToken(ctx, account.ID, claims)
	}
	expected := snapshotOAuthRefreshAccount(fresh)
	expected.Credentials[BPSCredentialKey] = pending
	proxy := ""
	if fresh.Proxy != nil {
		proxy = fresh.Proxy.URL()
	}
	attempt, cancel := context.WithTimeout(ctx, 40*time.Second)
	grant, refreshErr := p.openAIOAuthService.RefreshTokenWithClientID(attempt, refreshToken, proxy, BPSOAuthClientID)
	cancel()
	cleanup, cleanupCancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cleanupCancel()
	if refreshErr != nil || grant == nil {
		pending = shallowCopyMap(pending)
		pending["refresh_pending"] = map[string]any{"reason": "outcome_unknown_or_failed", "at": time.Now().UTC().Format(time.RFC3339)}
		_, _ = repo.UpdateBPSCredentialsIfUnchanged(cleanup, fresh.ID, expected, pending)
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		// The still-valid AT is usable, but another refresh is not attempted until
		// a deliberate credential re-import clears the durable marker.
		return bpsProfileToken(fresh)
	}
	next := shallowCopyMap(current)
	if grant.RefreshToken != "" {
		next["refresh_token"] = grant.RefreshToken
	}
	next["client_id"] = BPSOAuthClientID
	next["last_refresh"] = time.Now().UTC().Format(time.RFC3339)
	next["_token_version"] = time.Now().UnixMilli()
	newClaims, claimErr := ParseBPSCredentialClaims(grant.AccessToken)
	if claimErr != nil || newClaims.Subject != oldClaims.Subject || newClaims.Auth.UserID != oldClaims.Auth.UserID || newClaims.ExpiresAt <= time.Now().Unix() {
		next["workspace_refresh_pending"] = map[string]any{"reason": "refreshed_identity_invalid"}
	} else if newClaims.Auth.AccountID != oldClaims.Auth.AccountID {
		next["workspace_refresh_pending"] = map[string]any{"reason": "workspace_changed", "expected_account_id": oldClaims.Auth.AccountID, "observed_account_id": newClaims.Auth.AccountID}
	} else {
		next["access_token"] = grant.AccessToken
		next["expires_at"] = time.Unix(newClaims.ExpiresAt, 0).UTC().Format(time.RFC3339)
		next["chatgpt_account_id"] = newClaims.Auth.AccountID
		next["chatgpt_user_id"] = newClaims.Auth.UserID
		if grant.IDToken != "" {
			next["id_token"] = grant.IDToken
		}
	}
	delete(next, "refresh_pending")
	applied, err = repo.UpdateBPSCredentialsIfUnchanged(cleanup, fresh.ID, expected, next)
	if err != nil {
		return "", bpsCredentialFailure("refresh_persistence_uncertain")
	}
	if !applied {
		return p.readCurrentBPSToken(cleanup, account.ID, claims)
	}
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	return p.readCurrentBPSToken(ctx, account.ID, claims)
}

func (p *OpenAITokenProvider) readCurrentBPSToken(ctx context.Context, id int64, expected *BPSCredentialClaims) (string, error) {
	account, err := p.accountRepo.GetByID(ctx, id)
	if err != nil || account == nil || !account.IsActive() {
		return "", bpsCredentialFailure("account_unavailable")
	}
	profile, err := BPSCredentialsForAccount(account)
	if err != nil {
		return "", err
	}
	claims, err := ParseBPSCredentialClaims(bpsString(profile, "access_token"))
	if err != nil {
		return "", err
	}
	if expected == nil || claims.Subject != expected.Subject || claims.Auth.AccountID != expected.Auth.AccountID || claims.Auth.UserID != expected.Auth.UserID {
		return "", bpsCredentialFailure("account_changed")
	}
	return bpsProfileToken(account)
}
