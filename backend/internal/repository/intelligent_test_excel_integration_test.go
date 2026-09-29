package repository

import (
	"context"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestExcelIntelligentQueueGroupIsolation(t *testing.T) {
	db := intelligentTestDB(t)
	repo := &intelligentTestRepository{db: db}
	ctx := context.Background()
	_, err := db.Exec(`INSERT INTO account_groups VALUES(10,101)`)
	require.NoError(t, err)
	req := service.IntelligentTestEnqueue{AccountIDs: []int64{10}, TestTypes: []string{"pelican"}, GroupID: 100, IdempotencyKey: uuid.NewString()}
	first, err := repo.Enqueue(ctx, 1, req)
	require.NoError(t, err)
	require.Len(t, first.Records, 1)
	stored, err := repo.Get(ctx, first.Records[0].ID)
	require.NoError(t, err)
	require.EqualValues(t, 100, stored.ConfigSnapshot.GroupID)
	again, err := repo.Enqueue(ctx, 1, req)
	require.NoError(t, err)
	require.True(t, again.Reused)
	req.GroupID = 101
	_, err = repo.Enqueue(ctx, 1, req)
	require.ErrorIs(t, err, service.ErrIntelligentTestConflict)
	req.IdempotencyKey = uuid.NewString()
	_, err = repo.Enqueue(ctx, 1, req)
	require.Error(t, err, "different group must not reuse active work")
	req.GroupID = 999
	req.IdempotencyKey = uuid.NewString()
	_, err = repo.Enqueue(ctx, 1, req)
	require.Error(t, err, "foreign group rejected")
	var count int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM account_tests`).Scan(&count))
	require.Equal(t, 1, count)
}
