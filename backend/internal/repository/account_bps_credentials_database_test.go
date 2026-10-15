//go:build protectionintegration

package repository

import (
	"context"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestBPSCredentialDatabaseSlotIsolationAndCAS(t *testing.T) {
	ctx := context.Background()
	client, db := protectionTestDatabase(t)
	repo := newAccountRepositoryWithSQL(client, db, nil)
	expiry := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	a := &service.Account{Name: "bps-test", Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth,
		Status: service.StatusError, ErrorMessage: "original-error", Schedulable: false, ExpiresAt: &expiry,
		Priority: 73, Concurrency: 4, Credentials: map[string]any{"access_token": "native-at", "refresh_token": "native-rt", "chatgpt_account_id": "workspace", "chatgpt_user_id": "user"}}
	require.NoError(t, repo.Create(ctx, a))
	original, err := repo.GetByID(ctx, a.ID)
	require.NoError(t, err)
	bps1 := map[string]any{"access_token": "bps-at", "refresh_token": "bps-rt"}
	applied, err := repo.UpdateBPSCredentialsIfUnchanged(ctx, a.ID, original, bps1)
	require.NoError(t, err)
	require.True(t, applied)
	// A duplicate import/refresh using the old missing slot cannot clobber it.
	applied, err = repo.UpdateBPSCredentialsIfUnchanged(ctx, a.ID, original, map[string]any{"access_token": "stale"})
	require.NoError(t, err)
	require.False(t, applied)
	current, err := repo.GetByID(ctx, a.ID)
	require.NoError(t, err)
	require.Equal(t, "native-at", current.Credentials["access_token"])
	require.Equal(t, "native-rt", current.Credentials["refresh_token"])
	require.Equal(t, bps1, current.Credentials[service.BPSCredentialKey])
	require.Equal(t, original.Status, current.Status)
	require.Equal(t, original.ErrorMessage, current.ErrorMessage)
	require.Equal(t, original.Schedulable, current.Schedulable)
	require.Equal(t, original.ExpiresAt, current.ExpiresAt)
	require.Equal(t, original.Concurrency, current.Concurrency)
	require.Equal(t, original.Priority, current.Priority)
	// A stale native refresh must keep the freshly persisted BPS profile.
	require.NoError(t, repo.UpdateCredentials(ctx, a.ID, map[string]any{"access_token": "native-new", "refresh_token": "native-rt-new", "chatgpt_account_id": "workspace", "chatgpt_user_id": "user"}))
	current, err = repo.GetByID(ctx, a.ID)
	require.NoError(t, err)
	require.Equal(t, bps1, current.Credentials[service.BPSCredentialKey])
	require.Equal(t, "native-new", current.Credentials["access_token"])
	staleAdmin, err := repo.GetByID(ctx, a.ID)
	require.NoError(t, err)
	bps2 := map[string]any{"access_token": "bps-new", "refresh_token": "bps-rotated"}
	applied, err = repo.UpdateBPSCredentialsIfUnchanged(ctx, a.ID, current, bps2)
	require.NoError(t, err)
	require.True(t, applied)
	// Ordinary settings/native imports rehydrate the BPS slot under the DB lock.
	staleAdmin.Name = "edited-name"
	require.NoError(t, repo.Update(ctx, staleAdmin))
	after, err := repo.GetByID(ctx, a.ID)
	require.NoError(t, err)
	require.Equal(t, bps2, after.Credentials[service.BPSCredentialKey])
	require.Equal(t, "native-new", after.Credentials["access_token"])
	require.Equal(t, "edited-name", after.Name)
	require.Equal(t, original.ExpiresAt, after.ExpiresAt)
	// Workspace changes make an old BPS write fail closed without changing state.
	old := *after
	old.Credentials = map[string]any{}
	for k, v := range after.Credentials {
		old.Credentials[k] = v
	}
	old.Credentials["chatgpt_account_id"] = "another-workspace"
	applied, err = repo.UpdateBPSCredentialsIfUnchanged(ctx, a.ID, &old, bps1)
	require.NoError(t, err)
	require.False(t, applied)
	var outboxCount int
	require.NoError(t, db.QueryRowContext(ctx, "SELECT count(*) FROM scheduler_outbox WHERE account_id=$1", a.ID).Scan(&outboxCount))
	require.Greater(t, outboxCount, 0)
}
