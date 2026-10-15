package repository

import (
	"context"
	"encoding/json"
	"errors"
	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

func (r *accountRepository) UpdateBPSCredentialsIfUnchanged(ctx context.Context, id int64, expected *service.Account, profile map[string]any) (bool, error) {
	if r == nil || r.sql == nil || expected == nil || expected.ID != id {
		return false, errors.New("BPS credential repository is unavailable")
	}
	oldJSON, err := json.Marshal(expected.Credentials[service.BPSCredentialKey])
	if err != nil {
		return false, err
	}
	newJSON, err := json.Marshal(profile)
	if err != nil {
		return false, err
	}
	result, err := r.sql.ExecContext(ctx, `
 WITH changed AS (
  UPDATE accounts SET credentials=jsonb_set(credentials,'{bps_credentials}',$1::jsonb,true),updated_at=NOW()
  WHERE id=$2 AND deleted_at IS NULL AND platform='openai' AND type='oauth'
   AND COALESCE(credentials->'bps_credentials','null'::jsonb)=$3::jsonb
   AND COALESCE(credentials->>'chatgpt_account_id','')=$4
   AND COALESCE(credentials->>'chatgpt_user_id','')=$5
   AND proxy_id IS NOT DISTINCT FROM $6
   AND ($3::jsonb <> 'null'::jsonb OR COALESCE(credentials->>'access_token','')=$7)
  RETURNING id
 )
 INSERT INTO scheduler_outbox(event_type,account_id)
 SELECT 'account_changed',id FROM changed`, string(newJSON), id, string(oldJSON), expected.GetChatGPTAccountID(), expected.GetChatGPTUserID(), expected.ProxyID, expected.GetOpenAIAccessToken())
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	if err != nil || n == 0 {
		return false, err
	}
	r.syncSchedulerAccountSnapshotDetached(ctx, id)
	return true, nil
}

// Generic/native account writes cannot replace a concurrently refreshed BPS
// slot. The caller holds the row lock acquired by lockAndMergeAccountProbeExtra.
// Intentional BPS writes must use UpdateBPSCredentialsIfUnchanged instead.
func preserveLockedBPSCredentials(ctx context.Context, client *dbent.Client, account *service.Account) error {
	if account.Platform != service.PlatformOpenAI || account.Type != service.AccountTypeOAuth {
		return nil
	}
	rows, err := client.QueryContext(ctx, `SELECT credentials->'bps_credentials' FROM accounts WHERE id=$1 AND deleted_at IS NULL`, account.ID)
	if err != nil {
		return err
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return err
		}
		return service.ErrAccountNotFound
	}
	var raw []byte
	if err := rows.Scan(&raw); err != nil {
		return err
	}
	if err := rows.Err(); err != nil {
		return err
	}
	next := copyJSONMap(account.Credentials)
	delete(next, service.BPSCredentialKey)
	if len(raw) != 0 && string(raw) != "null" {
		var profile map[string]any
		if err := json.Unmarshal(raw, &profile); err != nil {
			return err
		}
		next[service.BPSCredentialKey] = profile
	}
	account.Credentials = next
	return nil
}
