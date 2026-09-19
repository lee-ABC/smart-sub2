package repository

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

func TestGatewayCacheOpenAICurrentTurnStateLifecycle(t *testing.T) {
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	cache := &gatewayCache{rdb: client}
	ctx := context.Background()

	require.NoError(t, cache.SetOpenAICurrentTurnState(ctx, 41, "gpt-5.6-sol", "opaque-state", time.Hour))
	state, err := cache.GetOpenAICurrentTurnState(ctx, 41, "gpt-5.6-sol")
	require.NoError(t, err)
	require.Equal(t, "opaque-state", state)

	otherModel, err := cache.GetOpenAICurrentTurnState(ctx, 41, "gpt-5.6-terra")
	require.NoError(t, err)
	require.Empty(t, otherModel)

	otherAccount, err := cache.GetOpenAICurrentTurnState(ctx, 42, "gpt-5.6-sol")
	require.NoError(t, err)
	require.Empty(t, otherAccount)

	require.NoError(t, cache.DeleteOpenAICurrentTurnState(ctx, 41, "gpt-5.6-sol"))
	state, err = cache.GetOpenAICurrentTurnState(ctx, 41, "gpt-5.6-sol")
	require.NoError(t, err)
	require.Empty(t, state)
}
