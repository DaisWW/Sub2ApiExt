package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"
)

func progressiveRecoveryRunner(t *testing.T, status int) (*Runner, chan int) {
	t.Helper()
	updates := make(chan int, 64)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		var payload struct {
			Priority int `json:"priority"`
		}
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			t.Error(err)
		}
		updates <- payload.Priority
		w.WriteHeader(status)
		if status == http.StatusOK {
			_, _ = io.WriteString(w, `{"code":0}`)
		}
	}))
	t.Cleanup(server.Close)
	runner := NewRunner(testRunnerConfig(t, server.URL, false), &fakeMetricsSource{}, server.Client(), nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	runner.config.ExplorationEnabled = false
	runner.tableWriter = io.Discard
	return runner, updates
}

func TestProgressiveRecoveryRetainsNoTrafficProgressAcrossRestart(t *testing.T) {
	now := nowForTest()
	accounts := comparableRecoveryAccounts()
	runner, updates := progressiveRecoveryRunner(t, http.StatusOK)
	previous := priorityUnavailable
	for cycle := 0; cycle < 30 && previous > priorityBest; cycle++ {
		recommendations := scoreAccounts(accounts, now, 5)
		runner.prepareExploration(accounts, recommendations, now)
		candidate := testRecommendationByID(t, recommendations, 1)
		if !candidate.explorationStart || candidate.RecommendedPriority >= previous || candidate.RecommendedPriority < priorityBest {
			t.Fatalf("recovery did not advance gradually: previous=%d recommendation=%+v", previous, candidate)
		}
		runner.applyRecommendations(context.Background(), []Recommendation{candidate}, "secret", now)
		accounts[0].CurrentPriority = candidate.RecommendedPriority
		if len(updates) != cycle+1 {
			t.Fatalf("step did not write exactly once: %d", len(updates))
		}
		// Restart in the middle of a lease, including its written position.
		if err := saveState(runner.config.StateFile, runner.state); err != nil {
			t.Fatal(err)
		}
		loaded, err := loadState(runner.config.StateFile)
		if err != nil {
			t.Fatal(err)
		}
		runner.state = loaded
		now = now.Add(recoveryDuration)
		recommendations = scoreAccounts(accounts, now, 5)
		runner.prepareExploration(accounts, recommendations, now)
		candidate = testRecommendationByID(t, recommendations, 1)
		if !candidate.explorationEnd || candidate.recoveryOutcome != recoveryOutcomeNoResult || candidate.RecommendedPriority != accounts[0].CurrentPriority {
			t.Fatalf("no traffic reverted progress or counted as success: %+v", candidate)
		}
		runner.applyRecommendations(context.Background(), []Recommendation{candidate}, "secret", now)
		if runner.state.Exploration != nil || runner.state.Accounts[1].RecoveryFailures != 0 || len(updates) != cycle+1 {
			t.Fatalf("no traffic changed failure state or priority: %+v, writes=%d", runner.state, len(updates))
		}
		previous = accounts[0].CurrentPriority
		now = now.Add(recoveryDuration)
	}
	if previous != priorityBest || len(updates) < 4 || <-updates != 670 {
		t.Fatalf("no traffic could not eventually reach a flow-eligible position: priority=%d writes=%d", previous, len(updates))
	}
}

func TestProgressiveRecoveryUsesFreshTimestampsRatherThanRollingCounts(t *testing.T) {
	now := nowForTest()
	tests := []struct {
		name                       string
		lastFailure, lastSuccess   *time.Time
		wantPriority, wantFailures int
		wantOutcome                string
	}{
		{"old failure", timePtr(now.Add(-time.Minute)), nil, 670, 0, recoveryOutcomeNoResult},
		{"new failure", timePtr(now.Add(time.Minute)), nil, priorityUnavailable, 1, recoveryOutcomeFailure},
		{"old success", nil, timePtr(now.Add(-time.Minute)), 670, 0, recoveryOutcomeNoResult},
		{"new success", nil, timePtr(now.Add(time.Minute)), 340, 0, recoveryOutcomeSuccess},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			accounts := comparableRecoveryAccounts()
			accounts[0].CurrentPriority = 670
			accounts[0].LastTerminalFailureAt, accounts[0].LastPricedSuccessAt = test.lastFailure, test.lastSuccess
			runner, _ := progressiveRecoveryRunner(t, http.StatusOK)
			runner.state.Exploration = &explorationState{AccountID: 1, OriginalPriority: priorityUnavailable,
				Recovery: true, RecoveryProgressive: true, RecoveryPriority: 670, RecoveryTargetPriority: priorityBest, StartedAt: timePtr(now),
				// Old errors have left the window: even a lower count may contain a new failure.
				RecoveryBaselineFailures: 20}
			recommendations := scoreAccounts(accounts, now.Add(recoveryDuration), 5)
			runner.prepareExploration(accounts, recommendations, now.Add(recoveryDuration))
			candidate := testRecommendationByID(t, recommendations, 1)
			if candidate.RecommendedPriority != test.wantPriority || candidate.recoveryOutcome != test.wantOutcome {
				t.Fatalf("rolling evidence was consumed incorrectly: %+v", candidate)
			}
			runner.applyRecommendations(context.Background(), []Recommendation{candidate}, "secret", now.Add(recoveryDuration))
			if runner.state.Exploration != nil || runner.state.Accounts[1].RecoveryFailures != test.wantFailures {
				t.Fatalf("outcome state = %+v", runner.state)
			}
		})
	}
}

func TestRecoveryFailedRollbackKeepsLeaseAndDoesNotAdvanceOutcome(t *testing.T) {
	now := nowForTest()
	accounts := comparableRecoveryAccounts()
	accounts[0].CurrentPriority, accounts[0].LastTerminalFailureAt = 670, timePtr(now.Add(time.Minute))
	runner, _ := progressiveRecoveryRunner(t, http.StatusBadGateway)
	runner.state.Exploration = &explorationState{AccountID: 1, OriginalPriority: priorityUnavailable, Recovery: true,
		RecoveryProgressive: true, RecoveryPriority: 670, RecoveryTargetPriority: priorityBest, StartedAt: timePtr(now)}
	before := cloneSyncState(runner.state).Exploration
	recommendations := scoreAccounts(accounts, now.Add(recoveryDuration), 5)
	runner.prepareExploration(accounts, recommendations, now.Add(recoveryDuration))
	candidate := testRecommendationByID(t, recommendations, 1)
	runner.applyRecommendations(context.Background(), []Recommendation{candidate}, "secret", now.Add(recoveryDuration))
	if !reflect.DeepEqual(before, runner.state.Exploration) || runner.state.Accounts[1].RecoveryFailures != 0 || runner.state.Accounts[1].LastAppliedAt != nil {
		t.Fatalf("failed API write advanced recovery: %+v", runner.state)
	}
}

func TestProgressiveRecoveryStopsOnFailureBeforeObservationExpires(t *testing.T) {
	now := nowForTest()
	accounts := comparableRecoveryAccounts()
	accounts[0].CurrentPriority, accounts[0].LastTerminalFailureAt = 63, timePtr(now.Add(time.Minute))
	runner, _ := progressiveRecoveryRunner(t, http.StatusOK)
	runner.state.Exploration = &explorationState{AccountID: 1, OriginalPriority: 90, Recovery: true,
		RecoveryProgressive: true, RecoveryPriority: 63, RecoveryTargetPriority: priorityBest, StartedAt: timePtr(now)}
	recommendations := scoreAccounts(accounts, now.Add(2*time.Minute), 5)
	runner.prepareExploration(accounts, recommendations, now.Add(2*time.Minute))
	candidate := testRecommendationByID(t, recommendations, 1)
	if !candidate.explorationEnd || !candidate.applyImmediately || candidate.RecommendedPriority != 90 || candidate.recoveryOutcome != recoveryOutcomeFailure {
		t.Fatalf("new failure waited for the full observation lease: %+v", candidate)
	}
}

func TestRecoveryUsesNewCostTargetAndBackoffAlsoBlocksMaturePromotion(t *testing.T) {
	now := nowForTest()
	accounts := comparableRecoveryAccounts()
	accounts[0].Window2h = accounts[0].Pools[0].Window7d
	accounts[0].Pools[0].Window2h = accounts[0].Window2h
	accounts[0].Window2h.BaseCost, accounts[0].Window2h.AccountCost = 800, 80
	accounts[0].CurrentPriority, accounts[0].LastPricedSuccessAt = 63, timePtr(now.Add(time.Minute))
	runner, _ := progressiveRecoveryRunner(t, http.StatusOK)
	runner.state.Exploration = &explorationState{AccountID: 1, OriginalPriority: 90, Recovery: true,
		RecoveryProgressive: true, RecoveryPriority: 63, RecoveryTargetPriority: priorityBest, StartedAt: timePtr(now)}
	recommendations := scoreAccounts(accounts, now.Add(recoveryDuration), 5)
	runner.prepareExploration(accounts, recommendations, now.Add(recoveryDuration))
	if got := testRecommendationByID(t, recommendations, 1); got.RecommendedPriority != 83 || got.recoveryOutcome != recoveryOutcomeSuccess {
		t.Fatalf("recovery ignored the mature cost target: %+v", got)
	}
	runner.state.Exploration = nil
	runner.state.Accounts[1] = accountState{RecoveryRetryAt: timePtr(now.Add(time.Hour))}
	accounts[0].Window2h.BaseCost, accounts[0].Window2h.AccountCost = 20, 2
	recommendations = scoreAccounts(accounts, now, 5)
	runner.prepareExploration(accounts, recommendations, now)
	if got := testRecommendationByID(t, recommendations, 1); got.RecommendedPriority != 63 || got.explorationStart {
		t.Fatalf("mature evidence bypassed recovery backoff: %+v", got)
	}
}

func TestProgressiveRecoveryDryRunDoesNotConsumeFreshEvidence(t *testing.T) {
	now := nowForTest()
	accounts := comparableRecoveryAccounts()
	accounts[0].CurrentPriority, accounts[0].LastPricedSuccessAt = 670, timePtr(now.Add(time.Minute))
	runner, updates := progressiveRecoveryRunner(t, http.StatusOK)
	runner.config.DryRun = true
	runner.source = &fakeMetricsSource{accounts: accounts}
	runner.state.Exploration = &explorationState{AccountID: 1, OriginalPriority: priorityUnavailable, Recovery: true,
		RecoveryProgressive: true, RecoveryPriority: 670, RecoveryTargetPriority: priorityBest, StartedAt: timePtr(now),
		RecoveryLastFailureAt: timePtr(now.Add(-time.Minute))}
	before := cloneSyncState(runner.state)
	if err := runner.RunOnce(context.Background(), now.Add(recoveryDuration)); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, runner.state) || len(updates) != 0 {
		t.Fatalf("dry-run mutated recovery: %+v, writes=%d", runner.state, len(updates))
	}
}

func TestLowEvidenceRecoveryStillHandlesFailureAfterLeaseEnds(t *testing.T) {
	now := nowForTest()
	accounts := comparableRecoveryAccounts()
	accounts[0].CurrentPriority = 15
	runner, _ := progressiveRecoveryRunner(t, http.StatusOK)
	recommendations := scoreAccounts(accounts, now, 5)
	runner.prepareExploration(accounts, recommendations, now)
	candidate := testRecommendationByID(t, recommendations, 1)
	runner.applyRecommendations(context.Background(), []Recommendation{candidate}, "secret", now)
	accounts[0].CurrentPriority = candidate.RecommendedPriority
	now = now.Add(recoveryDuration)
	recommendations = scoreAccounts(accounts, now, 5)
	runner.prepareExploration(accounts, recommendations, now)
	candidate = testRecommendationByID(t, recommendations, 1)
	runner.applyRecommendations(context.Background(), []Recommendation{candidate}, "secret", now)
	if runner.state.Exploration != nil || accounts[0].CurrentPriority != priorityBest {
		t.Fatalf("test did not reach the low-evidence reference position: %+v", runner.state)
	}
	accounts[0].LastTerminalFailureAt = timePtr(now.Add(time.Minute))
	now = now.Add(2 * time.Minute)
	recommendations = scoreAccounts(accounts, now, 5)
	runner.prepareExploration(accounts, recommendations, now)
	candidate = testRecommendationByID(t, recommendations, 1)
	if candidate.RecommendedPriority != 15 || candidate.recoveryOutcome != recoveryOutcomeFailure || !candidate.applyImmediately {
		t.Fatalf("failure after the trial lease was ignored: %+v", candidate)
	}
	runner.applyRecommendations(context.Background(), []Recommendation{candidate}, "secret", now)
	accounts[0].CurrentPriority = 15
	state := runner.state.Accounts[1]
	if state.RecoveryFailures != 1 || state.RecoveryRetryAt == nil || !state.RecoveryRetryAt.Equal(now.Add(2*time.Hour)) {
		t.Fatalf("post-lease failure did not record backoff: %+v", state)
	}
	// The same error remains in the rolling window but must be consumed once.
	recommendations = scoreAccounts(accounts, now.Add(time.Minute), 5)
	runner.prepareExploration(accounts, recommendations, now.Add(time.Minute))
	candidate = testRecommendationByID(t, recommendations, 1)
	if candidate.recoveryOutcome != "" || candidate.explorationStart {
		t.Fatalf("historical error was consumed again: %+v", candidate)
	}
}

func TestPostLeaseRecoveryFailureKeepsHardExclusion(t *testing.T) {
	now := nowForTest()
	accounts := comparableRecoveryAccounts()
	accounts[0].CurrentPriority = priorityBest
	accounts[0].LastTerminalFailureAt = timePtr(now.Add(-time.Minute))
	accounts[0].RateLimitResetAt = timePtr(now.Add(time.Hour))
	runner, updates := progressiveRecoveryRunner(t, http.StatusOK)
	runner.state.Accounts[1] = accountState{RecoveryOriginalPriority: 15,
		LastAppliedAt: timePtr(now.Add(-recoveryDuration)), LastExploredAt: timePtr(now.Add(-recoveryDuration))}
	recommendations := scoreAccounts(accounts, now, 5)
	runner.prepareExploration(accounts, recommendations, now)
	candidate := testRecommendationByID(t, recommendations, 1)
	if !candidate.HardExcluded || candidate.RecommendedPriority != priorityUnavailable || !candidate.applyImmediately {
		t.Fatalf("post-lease rollback overrode hard exclusion: %+v", candidate)
	}
	runner.applyRecommendations(context.Background(), []Recommendation{candidate}, "secret", now)
	if len(updates) != 1 || <-updates != priorityUnavailable {
		t.Fatalf("hard exclusion did not write unavailable priority: %+v", runner.state)
	}
}

func TestPostLeaseRecoveryFailedWriteDoesNotConsumeFailure(t *testing.T) {
	now := nowForTest()
	for _, dryRun := range []bool{false, true} {
		t.Run(fmt.Sprintf("dry_run=%t", dryRun), func(t *testing.T) {
			accounts := comparableRecoveryAccounts()
			accounts[0].CurrentPriority = priorityBest
			accounts[0].LastTerminalFailureAt = timePtr(now.Add(-time.Minute))
			runner, updates := progressiveRecoveryRunner(t, http.StatusBadGateway)
			runner.config.DryRun = dryRun
			runner.source = &fakeMetricsSource{accounts: accounts}
			runner.state.Accounts[1] = accountState{RecoveryOriginalPriority: 15, LastApplied: priorityBest,
				LastAppliedAt: timePtr(now.Add(-recoveryDuration)), LastExploredAt: timePtr(now.Add(-recoveryDuration))}
			before := cloneSyncState(runner.state)
			if err := runner.RunOnce(context.Background(), now); err != nil {
				t.Fatal(err)
			}
			state := runner.state.Accounts[1]
			if state.RecoveryFailures != 0 || state.RecoveryRetryAt != nil || state.LastApplied != priorityBest ||
				!state.LastAppliedAt.Equal(*before.Accounts[1].LastAppliedAt) {
				t.Fatalf("failed rollback consumed the new failure: %+v", state)
			}
			if dryRun && (!reflect.DeepEqual(before, runner.state) || len(updates) != 0) {
				t.Fatalf("dry-run changed recovery state or wrote priority: %+v, writes=%d", runner.state, len(updates))
			}
			recommendations := scoreAccounts(accounts, now.Add(time.Minute), 5)
			runner.prepareExploration(accounts, recommendations, now.Add(time.Minute))
			candidate := testRecommendationByID(t, recommendations, 1)
			if candidate.RecommendedPriority != 15 || candidate.recoveryOutcome != recoveryOutcomeFailure {
				t.Fatalf("failed write lost the retry recommendation: %+v", candidate)
			}
		})
	}
}
