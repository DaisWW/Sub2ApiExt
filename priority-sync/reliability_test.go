package main

import (
	"context"
	"net/http"
	"reflect"
	"testing"
	"time"
)

func reliabilityAccount(successes, failures, trailing int64, last time.Time) AccountMetrics {
	account := comparableCostAccount(1, 0.1, 0.9, 100, 5_000_000, 90)
	account.CurrentPriority = priorityNeutral
	account.Window2h.SuccessfulRequests = successes
	account.Window2h.TerminalFailures = failures
	account.Window2h.TrailingTerminalFailures = trailing
	account.Window2h.LastTerminalFailureAt = timePtr(last)
	return account
}

func TestReliabilityToleratesIsolatedFailureAndSlowsModerateFailureRate(t *testing.T) {
	now := nowForTest()
	for _, test := range []struct {
		name                string
		successes, failures int64
		wantFirst, wantNext int
	}{
		{"short sample", 2, 1, 50, 30},
		{"below two percent", 100, 1, 50, 30},
		{"two to five percent", 97, 3, 45, 45},
	} {
		t.Run(test.name, func(t *testing.T) {
			account := reliabilityAccount(test.successes, test.failures, 0, now.Add(-time.Minute))
			runner, updates := progressiveRecoveryRunner(t, http.StatusOK)
			for cycle, want := range []int{test.wantFirst, test.wantNext} {
				at := now.Add(time.Duration(cycle) * defaultInterval)
				recommendations := scoreAccounts([]AccountMetrics{account}, at, 5)
				runner.prepareExploration([]AccountMetrics{account}, recommendations, at)
				runner.applyRecommendations(context.Background(), recommendations, "secret", at)
				if recommendations[0].NextPriority != want || runner.state.Accounts[1].RecoveryFailures != 0 {
					t.Fatalf("soft failures punished or promotion speed incorrect: %+v state=%+v", recommendations[0], runner.state.Accounts[1])
				}
			}
			if len(updates) == 0 {
				t.Fatal("soft failures permanently froze promotion")
			}
		})
	}
}

func TestMaturePersistentFailureRollsBackOnceWithShortObservation(t *testing.T) {
	now := nowForTest()
	account := reliabilityAccount(90, 10, 0, now.Add(-time.Minute))
	account.CurrentPriority = priorityBest
	runner, updates := progressiveRecoveryRunner(t, http.StatusOK)
	for cycle := 0; cycle < 3; cycle++ {
		at := now.Add(time.Duration(cycle) * defaultInterval)
		recommendations := scoreAccounts([]AccountMetrics{account}, at, 5)
		runner.prepareExploration([]AccountMetrics{account}, recommendations, at)
		runner.applyRecommendations(context.Background(), recommendations, "secret", at)
		if cycle == 1 {
			if recommendations[0].RecommendedPriority != priorityGood || !recommendations[0].applyImmediately {
				t.Fatalf("persistent mature failures bypassed guard: %+v", recommendations[0])
			}
			account.CurrentPriority = priorityGood
		}
	}
	state := runner.state.Accounts[1]
	if len(updates) != 1 || state.RecoveryFailures != 1 || state.RecoveryRetryAt == nil || !state.RecoveryRetryAt.Equal(now.Add(defaultInterval+30*time.Minute)) {
		t.Fatalf("same window errors repeatedly punished or first backoff too long: writes=%d state=%+v", len(updates), state)
	}
}

func TestReliabilityRollbackAPIFailureAndDryRunDoNotConsumeEvidence(t *testing.T) {
	now := nowForTest()
	for _, dryRun := range []bool{false, true} {
		account := reliabilityAccount(10, 3, 3, now.Add(-time.Minute))
		account.CurrentPriority = priorityBest
		runner, updates := progressiveRecoveryRunner(t, http.StatusBadGateway)
		runner.config.DryRun = dryRun
		runner.state.Strategy = strategyVersion
		runner.source = &fakeMetricsSource{accounts: []AccountMetrics{account}}
		before := cloneSyncState(runner.state)
		if err := runner.RunOnce(context.Background(), now); err != nil {
			t.Fatal(err)
		}
		state := runner.state.Accounts[1]
		if state.RecoveryFailures != 0 || state.RecoveryRetryAt != nil || state.LastReliabilityRollbackAt != nil || state.LastReliabilityFailureAt != nil {
			t.Fatalf("failed write consumed reliability outcome: %+v", state)
		}
		if dryRun && (!reflect.DeepEqual(before, runner.state) || len(updates) != 0) {
			t.Fatal("dry-run changed live reliability state")
		}
		recommendations := scoreAccounts([]AccountMetrics{account}, now.Add(time.Minute), 5)
		runner.prepareExploration([]AccountMetrics{account}, recommendations, now.Add(time.Minute))
		if recommendations[0].RecommendedPriority != priorityGood || !recommendations[0].reliabilityRollback {
			t.Fatalf("failed rollback lost its retry: %+v", recommendations[0])
		}
	}
}

func TestReliabilityRepeatedFreshFailuresEscalateBackoff(t *testing.T) {
	now := nowForTest()
	account := reliabilityAccount(1, 3, 3, now.Add(-time.Minute))
	account.CurrentPriority = priorityBest
	runner, _ := progressiveRecoveryRunner(t, http.StatusOK)
	for _, backoff := range []time.Duration{30 * time.Minute, 2 * time.Hour, 6 * time.Hour, 24 * time.Hour} {
		account.Window2h.LastTerminalFailureAt = timePtr(now.Add(-time.Second))
		recommendations := scoreAccounts([]AccountMetrics{account}, now, 5)
		runner.prepareExploration([]AccountMetrics{account}, recommendations, now)
		runner.applyRecommendations(context.Background(), recommendations, "secret", now)
		state := runner.state.Accounts[1]
		if state.RecoveryRetryAt == nil || !state.RecoveryRetryAt.Equal(now.Add(backoff)) {
			t.Fatalf("fresh failure did not escalate correctly: want=%s state=%+v", backoff, state)
		}
		now = now.Add(backoff + time.Minute)
	}
}

func TestRecoveringAccountRetainsProgressAfterOneSoftFailure(t *testing.T) {
	now := nowForTest()
	accounts := comparableRecoveryAccounts()
	accounts[0].CurrentPriority = 63
	accounts[0].Window30m = &MetricSnapshot{TerminalFailures: 1, LastTerminalFailureAt: timePtr(now.Add(time.Minute))}
	runner, updates := progressiveRecoveryRunner(t, http.StatusOK)
	runner.state.Exploration = &explorationState{AccountID: 1, OriginalPriority: 90, Recovery: true,
		RecoveryProgressive: true, RecoveryPriority: 63, RecoveryTargetPriority: priorityBest, StartedAt: timePtr(now)}
	recommendations := scoreAccounts(accounts, now.Add(2*time.Minute), 5)
	runner.prepareExploration(accounts, recommendations, now.Add(2*time.Minute))
	candidate := testRecommendationByID(t, recommendations, 1)
	runner.applyRecommendations(context.Background(), []Recommendation{candidate}, "secret", now.Add(2*time.Minute))
	if candidate.RecommendedPriority != 63 || runner.state.Exploration == nil || len(updates) != 0 || runner.state.Accounts[1].RecoveryFailures != 0 {
		t.Fatalf("one soft error rolled back recovery or imposed backoff: %+v state=%+v", candidate, runner.state)
	}
	accounts[0].LastPricedSuccessAt = timePtr(now.Add(3 * time.Minute))
	recommendations = scoreAccounts(accounts, now.Add(recoveryDuration), 5)
	runner.prepareExploration(accounts, recommendations, now.Add(recoveryDuration))
	candidate = testRecommendationByID(t, recommendations, 1)
	runner.applyRecommendations(context.Background(), []Recommendation{candidate}, "secret", now.Add(recoveryDuration))
	if candidate.RecommendedPriority >= 63 || len(updates) != 1 || runner.state.Accounts[1].RecoveryFailures != 0 {
		t.Fatalf("old error blocked recovery after fresh success: %+v state=%+v", candidate, runner.state)
	}
}
