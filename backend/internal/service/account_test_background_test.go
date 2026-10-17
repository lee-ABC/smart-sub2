package service

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type backgroundPanicAccountRepo struct {
	AccountRepository
}

func (r *backgroundPanicAccountRepo) GetByID(ctx context.Context, id int64) (*Account, error) {
	if id == 82 {
		panic("private-upstream-detail-must-not-leak")
	}
	return r.AccountRepository.GetByID(ctx, id)
}

func TestRunTestBackgroundContainsPanicAndContinues(t *testing.T) {
	svc, repo := intelligentRunnerFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"hi\"}\n\ndata: {\"type\":\"response.completed\"}\n\n")
	})
	svc.accountRepo = &backgroundPanicAccountRepo{AccountRepository: repo}

	result, err := svc.RunTestBackground(context.Background(), 82, "gpt-5.2")
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, "failed", result.Status)
	require.Contains(t, result.ErrorMessage, "internal error")
	require.NotContains(t, result.ErrorMessage, "private-upstream-detail")
	require.Empty(t, result.ResponseText)
	require.False(t, result.StartedAt.IsZero())
	require.False(t, result.FinishedAt.Before(result.StartedAt))

	// The next plan still runs; a recovered panic never becomes test success.
	result, err = svc.RunTestBackground(context.Background(), 81, "gpt-5.2")
	require.NoError(t, err)
	require.Equal(t, "success", result.Status)
	require.Zero(t, repo.writes)
}

type backgroundTestPlanRepo struct {
	ScheduledTestPlanRepository
	updated []int64
}

func (r *backgroundTestPlanRepo) UpdateAfterRun(_ context.Context, id int64, lastRun, nextRun time.Time) error {
	r.updated = append(r.updated, id)
	if !nextRun.After(lastRun) {
		return fmt.Errorf("next run did not advance")
	}
	return nil
}

type backgroundTestResultRepo struct {
	ScheduledTestResultRepository
	results []*ScheduledTestResult
}

func (r *backgroundTestResultRepo) Create(_ context.Context, result *ScheduledTestResult) (*ScheduledTestResult, error) {
	r.results = append(r.results, result)
	return result, nil
}

func (r *backgroundTestResultRepo) PruneOldResults(context.Context, int64, int) error {
	return nil
}

func TestScheduledTestRecordsRecoveredFailureAndAdvancesPlan(t *testing.T) {
	svc, repo := intelligentRunnerFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"hi\"}\n\ndata: {\"type\":\"response.completed\"}\n\n")
	})
	svc.accountRepo = &backgroundPanicAccountRepo{AccountRepository: repo}
	plans := &backgroundTestPlanRepo{}
	results := &backgroundTestResultRepo{}
	runner := NewScheduledTestRunnerService(plans, NewScheduledTestService(plans, results), svc, nil, nil)
	for i, accountID := range []int64{82, 81} {
		runner.runOnePlan(context.Background(), &ScheduledTestPlan{
			ID: int64(i + 1), AccountID: accountID, ModelID: "gpt-5.2",
			CronExpression: "* * * * *", MaxResults: 10, AutoRecover: true,
		})
	}
	require.Equal(t, []int64{1, 2}, plans.updated)
	require.Len(t, results.results, 2)
	require.Equal(t, "failed", results.results[0].Status)
	require.Equal(t, int64(1), results.results[0].PlanID)
	require.Equal(t, "success", results.results[1].Status)
	require.Zero(t, repo.writes)
}
