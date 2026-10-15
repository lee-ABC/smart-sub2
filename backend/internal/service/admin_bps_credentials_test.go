//go:build unit

package service

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type adminBPSRepoStub struct {
	AccountRepository
	account  *Account
	conflict bool
	calls    int
}

func (r *adminBPSRepoStub) GetByID(context.Context, int64) (*Account, error) {
	return snapshotOAuthRefreshAccount(r.account), nil
}
func (r *adminBPSRepoStub) UpdateBPSCredentialsIfUnchanged(_ context.Context, id int64, expected *Account, next map[string]any) (bool, error) {
	r.calls++
	if r.conflict || id != r.account.ID || !reflect.DeepEqual(expected.Credentials[BPSCredentialKey], r.account.Credentials[BPSCredentialKey]) {
		return false, nil
	}
	r.account.Credentials[BPSCredentialKey] = next
	return true, nil
}
func adminBPSTestProfile(t *testing.T, workspace, user string) map[string]any {
	t.Helper()
	b, err := json.Marshal(map[string]any{"client_id": BPSOAuthClientID, "sub": "test-subject", "exp": time.Now().Add(time.Hour).Unix(), "https://api.openai.com/auth": map[string]any{"chatgpt_account_id": workspace, "chatgpt_user_id": user}})
	require.NoError(t, err)
	return map[string]any{"access_token": "e30." + base64.RawURLEncoding.EncodeToString(b) + ".test", "refresh_token": "synthetic-bps-rt", "client_id": BPSOAuthClientID}
}
func TestBPSAdminImportPreservesBusinessAndNative(t *testing.T) {
	expired := time.Now().Add(-time.Hour)
	a := &Account{ID: 11, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Name: "existing", Status: StatusError, Schedulable: false, ExpiresAt: &expired, Concurrency: 7, Priority: 82, GroupIDs: []int64{2}, Credentials: map[string]any{"access_token": "native-at", "refresh_token": "native-rt", "chatgpt_account_id": "workspace", "chatgpt_user_id": "user"}}
	repo := &adminBPSRepoStub{account: a}
	svc := &adminServiceImpl{accountRepo: repo}
	out, err := svc.UpdateAccount(context.Background(), a.ID, &UpdateAccountInput{BPSProfileOnly: true, Credentials: map[string]any{BPSCredentialKey: adminBPSTestProfile(t, "workspace", "user")}})
	require.NoError(t, err)
	require.Equal(t, 1, repo.calls)
	require.Equal(t, "native-at", out.GetOpenAIAccessToken())
	require.Equal(t, "native-rt", out.GetOpenAIRefreshToken())
	require.Equal(t, StatusError, out.Status)
	require.False(t, out.Schedulable)
	require.Equal(t, &expired, out.ExpiresAt)
	require.Equal(t, 7, out.Concurrency)
	require.Equal(t, 82, out.Priority)
	require.Equal(t, []int64{2}, out.GroupIDs)
	_, err = BPSCredentialsForAccount(out)
	require.NoError(t, err)
}
func TestBPSAdminImportRejectsMismatchAndCAS(t *testing.T) {
	for _, tc := range []struct {
		name, workspace, user string
		conflict              bool
	}{{"workspace", "other", "user", false}, {"user", "workspace", "other", false}, {"CAS", "workspace", "user", true}} {
		t.Run(tc.name, func(t *testing.T) {
			a := &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Credentials: map[string]any{"chatgpt_account_id": "workspace", "chatgpt_user_id": "user"}}
			repo := &adminBPSRepoStub{account: a, conflict: tc.conflict}
			svc := &adminServiceImpl{accountRepo: repo}
			_, err := svc.UpdateAccount(context.Background(), 1, &UpdateAccountInput{BPSProfileOnly: true, Credentials: map[string]any{BPSCredentialKey: adminBPSTestProfile(t, tc.workspace, tc.user)}})
			require.Error(t, err)
			require.NotContains(t, a.Credentials, BPSCredentialKey)
		})
	}
}
