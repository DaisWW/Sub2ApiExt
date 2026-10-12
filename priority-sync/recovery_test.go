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

func TestHistoricalReevaluationIsBoundedAndRecoveryCandidatesGetFairTurns(t *testing.T) {
	now := nowForTest()
	accounts := comparableRecoveryAccounts()
	accounts[0].CurrentPriority = priorityPoor
	runner, _ := progressiveRecoveryRunner(t, http.StatusOK)
	recommendations := scoreAccounts(accounts, now, 5)
	for index := range recommendations {
		if recommendations[index].ID == 1 {
			recommendations[index].recoveryReevaluation = true
		}
	}
	runner.state.Accounts[1] = accountState{LastExploredAt: timePtr(now.Add(-time.Hour))}
	if _, ok := runner.selectRecoveryCandidate(accounts, recommendations, now); ok {
		t.Fatal("historical re-evaluation retried within two hours")
	}
	if _, ok := runner.selectRecoveryCandidate(accounts, recommendations, now.Add(time.Hour)); !ok {
		t.Fatal("historical re-evaluation never became eligible again")
	}
	other := accounts[0]
	other.ID, other.RateMultiplier = 4, 0.15
	accounts = append(accounts, other)
	recommendations = scoreAccounts(accounts, now, 5)
	if got, ok := runner.selectRecoveryCandidate(accounts, recommendations, now); !ok || got.Account.ID != 4 {
		t.Fatalf("repeated cheap candidate starved an untried peer: %+v, %t", got, ok)
	}
}

func TestProgressiveRecoveryRetainsNoTrafficProgressAcrossRestart(t *testing.T) {
	now := nowForTest()
	accounts := comparableRecoveryAccounts()
	runner, updates := progressiveRecoveryRunner(t, http.StatusOK)
	for cycle := 0; cycle < 3; cycle++ {
		recommendations := scoreAccounts(accounts, now, 5)
		runner.prepareExploration(accounts, recommendations, now)
		candidate := testRecommendationByID(t, recommendations, 1)
		if !candidate.explorationStart || candidate.RecommendedPriority != priorityUnavailable-recoveryRampStep {
			t.Fatalf("no traffic advanced recovery beyond the first bounded step: %+v", candidate)
		}
		runner.applyRecommendations(context.Background(), []Recommendation{candidate}, "secret", now)
		accounts[0].CurrentPriority = candidate.RecommendedPriority
		if len(updates) != 1 {
			t.Fatalf("no-result retry wrote another priority step: %d", len(updates))
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
		if runner.state.Exploration != nil || runner.state.Accounts[1].RecoveryFailures != 0 || len(updates) != 1 {
			t.Fatalf("no traffic changed failure state or priority: %+v, writes=%d", runner.state, len(updates))
		}
		now = now.Add(recoveryDuration)
	}
	if <-updates != priorityUnavailable-recoveryRampStep {
		t.Fatal("first recovery step exceeded the five-point limit")
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
		{"isolated new failure", timePtr(now.Add(time.Minute)), nil, 670, 0, recoveryOutcomeNoResult},
		{"three new failures", timePtr(now.Add(time.Minute)), nil, priorityUnavailable, 1, recoveryOutcomeUnreliable},
		{"old success", nil, timePtr(now.Add(-time.Minute)), 670, 0, recoveryOutcomeNoResult},
		{"one new success", nil, timePtr(now.Add(time.Minute)), 670, 0, recoveryOutcomeNoResult},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			accounts := comparableRecoveryAccounts()
			accounts[0].CurrentPriority = 670
			accounts[0].LastTerminalFailureAt, accounts[0].LastPricedSuccessAt = test.lastFailure, test.lastSuccess
			if test.lastFailure != nil {
				accounts[0].Window30m = &MetricSnapshot{TerminalFailures: 1, LastTerminalFailureAt: test.lastFailure}
				if test.wantFailures > 0 {
					accounts[0].Window30m.TerminalFailures, accounts[0].Window30m.TrailingTerminalFailures = 3, 3
					accounts[0].Window30m.ThirdTerminalFailureAt = timePtr(now.Add(time.Second))
				}
			}
			accounts[0].RecoveryWindow = accounts[0].Window30m
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
	accounts[0].Window30m = &MetricSnapshot{TerminalFailures: 3, TrailingTerminalFailures: 3, LastTerminalFailureAt: accounts[0].LastTerminalFailureAt}
	accounts[0].RecoveryWindow = accounts[0].Window30m
	accounts[0].Window30m.ThirdTerminalFailureAt = timePtr(now.Add(time.Second))
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

func TestProgressiveRecoveryStopsOnThreeFailuresBeforeObservationExpires(t *testing.T) {
	now := nowForTest()
	accounts := comparableRecoveryAccounts()
	accounts[0].CurrentPriority, accounts[0].LastTerminalFailureAt = 63, timePtr(now.Add(time.Minute))
	accounts[0].Window30m = &MetricSnapshot{TerminalFailures: 3, TrailingTerminalFailures: 3, LastTerminalFailureAt: accounts[0].LastTerminalFailureAt}
	accounts[0].RecoveryWindow = accounts[0].Window30m
	accounts[0].Window30m.ThirdTerminalFailureAt = timePtr(now.Add(time.Second))
	runner, _ := progressiveRecoveryRunner(t, http.StatusOK)
	runner.state.Exploration = &explorationState{AccountID: 1, OriginalPriority: 90, Recovery: true,
		RecoveryProgressive: true, RecoveryPriority: 63, RecoveryTargetPriority: priorityBest, StartedAt: timePtr(now)}
	recommendations := scoreAccounts(accounts, now.Add(2*time.Minute), 5)
	runner.prepareExploration(accounts, recommendations, now.Add(2*time.Minute))
	candidate := testRecommendationByID(t, recommendations, 1)
	if !candidate.explorationEnd || !candidate.applyImmediately || candidate.RecommendedPriority != 90 || candidate.recoveryOutcome != recoveryOutcomeUnreliable {
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
	accounts[0].RecoveryWindow = &MetricSnapshot{SuccessfulRequests: 50, LastPricedSuccessAt: accounts[0].LastPricedSuccessAt}
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
	accounts[0].SuccessfulRequests = recoveryMinimumSamples
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

func TestProgressiveRecoveryRequiresFreshSamplesAndUsesFailureRateBoundaries(t *testing.T) {
	now := nowForTest()
	for _, test := range []struct {
		name                string
		successes, failures int64
		oldSuccess          bool
		wantPriority        int
		wantOutcome         string
	}{
		{"one new success", 1, 0, false, 60, recoveryOutcomeNoResult},
		{"forty-nine requests", 49, 0, false, 60, recoveryOutcomeNoResult},
		{"fifty healthy requests", 50, 0, false, 55, recoveryOutcomeSuccess},
		{"isolated error below two percent", 99, 1, false, 55, recoveryOutcomeSuccess},
		{"exactly two percent", 98, 2, false, 58, recoveryOutcomeProgress},
		{"exactly five percent", 95, 5, false, 58, recoveryOutcomeProgress},
		{"above five percent", 94, 6, false, 65, recoveryOutcomeUnreliable},
		{"fifty requests above five percent", 47, 3, false, 65, recoveryOutcomeUnreliable},
		{"no fresh priced success", 50, 0, true, 60, recoveryOutcomeNoResult},
	} {
		t.Run(test.name, func(t *testing.T) {
			accounts := comparableRecoveryAccounts()
			accounts[0].CurrentPriority = 60
			snapshot := &MetricSnapshot{SuccessfulRequests: test.successes, TerminalFailures: test.failures,
				LastPricedSuccessAt: timePtr(now.Add(29 * time.Minute))}
			if test.oldSuccess {
				snapshot.LastPricedSuccessAt = timePtr(now.Add(-time.Minute))
			}
			if test.failures > 0 {
				snapshot.LastTerminalFailureAt = timePtr(now.Add(time.Minute))
			}
			if test.failures >= 3 {
				snapshot.ThirdTerminalFailureAt = timePtr(now.Add(time.Second))
			}
			accounts[0].RecoveryWindow = snapshot
			runner, updates := progressiveRecoveryRunner(t, http.StatusOK)
			runner.state.Exploration = &explorationState{AccountID: 1, OriginalPriority: 65, Recovery: true,
				RecoveryProgressive: true, RecoveryPriority: 60, RecoveryTargetPriority: priorityBest, StartedAt: timePtr(now)}
			at := now.Add(recoveryDuration)
			recommendations := scoreAccounts(accounts, at, 5)
			runner.prepareExploration(accounts, recommendations, at)
			candidate := testRecommendationByID(t, recommendations, 1)
			if candidate.RecommendedPriority != test.wantPriority || candidate.recoveryOutcome != test.wantOutcome {
				t.Fatalf("fresh sample/failure boundary decision = %+v", candidate)
			}
			runner.applyRecommendations(context.Background(), []Recommendation{candidate}, "secret", at)
			state := runner.state.Accounts[1]
			if test.wantOutcome == recoveryOutcomeUnreliable {
				if state.RecoveryFailures != 1 || state.RecoveryRetryAt == nil || !state.RecoveryRetryAt.Equal(at.Add(30*time.Minute)) {
					t.Fatalf("failed trial did not roll back once with short observation: %+v", state)
				}
			} else if state.RecoveryFailures != 0 {
				t.Fatalf("tolerated errors became a failure: %+v", state)
			}
			if test.wantPriority == 60 && len(updates) != 0 {
				t.Fatal("insufficient evidence wrote a priority")
			}
		})
	}
}

func TestRecoveryPromotionAPIFailureDoesNotConsumeSamples(t *testing.T) {
	now := nowForTest()
	accounts := comparableRecoveryAccounts()
	accounts[0].CurrentPriority = 60
	accounts[0].RecoveryWindow = &MetricSnapshot{SuccessfulRequests: 50, LastPricedSuccessAt: timePtr(now.Add(time.Minute))}
	runner, updates := progressiveRecoveryRunner(t, http.StatusBadGateway)
	runner.state.Exploration = &explorationState{AccountID: 1, OriginalPriority: 65, Recovery: true,
		RecoveryProgressive: true, RecoveryPriority: 60, RecoveryTargetPriority: priorityBest, StartedAt: timePtr(now)}
	before := cloneSyncState(runner.state).Exploration
	for cycle := 0; cycle < 2; cycle++ {
		at := now.Add(recoveryDuration + time.Duration(cycle)*time.Minute)
		recommendations := scoreAccounts(accounts, at, 5)
		runner.prepareExploration(accounts, recommendations, at)
		candidate := testRecommendationByID(t, recommendations, 1)
		if candidate.RecommendedPriority != 55 || candidate.recoveryOutcome != recoveryOutcomeSuccess {
			t.Fatalf("failed promotion lost the same unconsumed evidence: %+v", candidate)
		}
		runner.applyRecommendations(context.Background(), []Recommendation{candidate}, "secret", at)
		if !reflect.DeepEqual(before, runner.state.Exploration) || runner.state.Accounts[1].LastAppliedAt != nil || runner.state.Accounts[1].RecoveryRetryAt != nil {
			t.Fatalf("failed promotion advanced recovery: %+v", runner.state)
		}
	}
	if len(updates) != 2 {
		t.Fatal("failed promotion was not retried")
	}
}

type recoveryObservationSource struct {
	fakeMetricsSource
	observation AccountMetrics
	window      time.Duration
	err         error
}

func (s *recoveryObservationSource) LoadAccountMetrics(_ context.Context, _ time.Time, window time.Duration) ([]AccountMetrics, error) {
	s.window = window
	return []AccountMetrics{s.observation}, s.err
}

func TestRunnerQueriesFreshRecoveryPeriodRatherThanRollingDeltas(t *testing.T) {
	now := nowForTest()
	for _, duration := range []time.Duration{recoveryDuration, 4 * time.Hour} {
		t.Run(duration.String(), func(t *testing.T) {
			accounts := comparableRecoveryAccounts()
			accounts[0].CurrentPriority = 60
			accounts[1].CurrentPriority = priorityNeutral
			// These old rolling-window errors must not replace a healthy trial.
			accounts[0].Window2h = &MetricSnapshot{SuccessfulRequests: 50, TerminalFailures: 100,
				TrailingTerminalFailures: 100, LastTerminalFailureAt: timePtr(now.Add(-5 * time.Hour))}
			source := &recoveryObservationSource{fakeMetricsSource: fakeMetricsSource{accounts: accounts},
				observation: AccountMetrics{ID: 1, SuccessfulRequests: 50, LastPricedSuccessAt: timePtr(now.Add(-time.Minute))}}
			runner, updates := progressiveRecoveryRunner(t, http.StatusOK)
			runner.source = source
			runner.state.Strategy = strategyVersion
			runner.state.Exploration = &explorationState{AccountID: 1, OriginalPriority: 65, Recovery: true,
				RecoveryProgressive: true, RecoveryPriority: 60, RecoveryTargetPriority: priorityBest, StartedAt: timePtr(now.Add(-duration))}
			if err := runner.RunOnce(context.Background(), now); err != nil {
				t.Fatal(err)
			}
			if source.window != min(duration, decisionWindow) || len(updates) != 1 || <-updates != 55 || runner.state.Exploration != nil {
				t.Fatalf("trial used rolling evidence or unbounded history: window=%s state=%+v", source.window, runner.state)
			}
			if runner.state.Accounts[1].RecoveryOriginalPriority != 60 {
				t.Fatal("next rollback position did not follow the accepted recovery step")
			}
		})
	}
}

func TestRecoveryObservationReadFailureKeepsLease(t *testing.T) {
	now := nowForTest()
	accounts := comparableRecoveryAccounts()
	accounts[0].CurrentPriority = 60
	runner, updates := progressiveRecoveryRunner(t, http.StatusOK)
	runner.source = &recoveryObservationSource{fakeMetricsSource: fakeMetricsSource{accounts: accounts}, err: fmt.Errorf("observation unavailable")}
	runner.state.Strategy = strategyVersion
	runner.state.Exploration = &explorationState{AccountID: 1, OriginalPriority: 65, Recovery: true,
		RecoveryProgressive: true, RecoveryPriority: 60, RecoveryTargetPriority: priorityBest, StartedAt: timePtr(now.Add(-recoveryDuration))}
	before := cloneSyncState(runner.state)
	if err := runner.RunOnce(context.Background(), now); err == nil {
		t.Fatal("unavailable fresh evidence was treated as healthy")
	}
	if !reflect.DeepEqual(before, runner.state) || len(updates) != 0 {
		t.Fatalf("failed observation advanced recovery: %+v", runner.state)
	}
}

func TestInsufficientRecoverySamplesAccumulateAtSamePriorityAcrossRestart(t *testing.T) {
	now := nowForTest()
	accounts := comparableRecoveryAccounts()
	accounts[0].CurrentPriority = 60
	accounts[1].CurrentPriority = priorityNeutral
	source := &recoveryObservationSource{fakeMetricsSource: fakeMetricsSource{accounts: accounts},
		observation: AccountMetrics{ID: 1, SuccessfulRequests: 25, LastPricedSuccessAt: timePtr(now.Add(29 * time.Minute))}}
	runner, updates := progressiveRecoveryRunner(t, http.StatusOK)
	runner.source = source
	runner.state.Strategy = strategyVersion
	runner.state.Accounts[1] = accountState{RecoveryOriginalPriority: 65, LastAppliedAt: timePtr(now), LastApplied: 60}
	runner.state.Exploration = &explorationState{AccountID: 1, OriginalPriority: 65, Recovery: true,
		RecoveryProgressive: true, RecoveryPriority: 60, RecoveryTargetPriority: priorityBest, StartedAt: timePtr(now)}
	if err := runner.RunOnce(context.Background(), now.Add(recoveryDuration)); err != nil {
		t.Fatal(err)
	}
	if runner.state.Exploration != nil || len(updates) != 0 {
		t.Fatal("twenty-five new samples advanced priority")
	}
	loaded, err := loadState(runner.config.StateFile)
	if err != nil {
		t.Fatal(err)
	}
	runner.state = loaded
	source.accounts[0].LastPricedSuccessAt = timePtr(now.Add(59 * time.Minute))
	source.observation.SuccessfulRequests = 50
	source.observation.LastPricedSuccessAt = source.accounts[0].LastPricedSuccessAt
	if err := runner.RunOnce(context.Background(), now.Add(2*recoveryDuration)); err != nil {
		t.Fatal(err)
	}
	if runner.state.Exploration == nil || !runner.state.Exploration.StartedAt.Equal(now) || len(updates) != 0 {
		t.Fatalf("no-result retry discarded the same-priority observation start: %+v", runner.state)
	}
	if err := runner.RunOnce(context.Background(), now.Add(2*recoveryDuration+defaultInterval)); err != nil {
		t.Fatal(err)
	}
	if source.window != 2*recoveryDuration+defaultInterval || len(updates) != 1 || <-updates != 55 || runner.state.Accounts[1].RecoveryOriginalPriority != 60 {
		t.Fatalf("unconsumed trial samples were lost or reused for multiple steps: window=%s state=%+v", source.window, runner.state)
	}
}

func TestProgressiveRecoveryKeepsStepLimitWhenOnlyDirectCostRemains(t *testing.T) {
	now := nowForTest()
	accounts := []AccountMetrics{{ID: 1, Status: "active", CurrentPriority: 60, RateMultiplier: 0.1,
		DecisionWindowsAvailable: true, Window2h: &MetricSnapshot{SuccessfulRequests: 50, PricedRequests: 50,
			PricedTokens: 1_000_000, TotalTokens: 1_000_000, AccountCost: 1},
		RecoveryWindow: &MetricSnapshot{SuccessfulRequests: 50, LastPricedSuccessAt: timePtr(now.Add(time.Minute))}}}
	runner, _ := progressiveRecoveryRunner(t, http.StatusOK)
	runner.state.Accounts[1] = accountState{RecoveryOriginalPriority: 65}
	runner.state.Exploration = &explorationState{AccountID: 1, OriginalPriority: 65, Recovery: true,
		RecoveryProgressive: true, RecoveryPriority: 60, RecoveryTargetPriority: priorityBest, StartedAt: timePtr(now)}
	recommendations := scoreAccounts(accounts, now.Add(recoveryDuration), 5)
	runner.prepareExploration(accounts, recommendations, now.Add(recoveryDuration))
	if got := recommendations[0]; got.CostEstimateSource != "" || got.RecommendedPriority != 55 || got.recoveryOutcome != recoveryOutcomeSuccess {
		t.Fatalf("direct cost bypassed or stopped the recovery step: %+v", got)
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
	accounts[0].Window30m = &MetricSnapshot{TerminalFailures: 3, TrailingTerminalFailures: 3, LastTerminalFailureAt: accounts[0].LastTerminalFailureAt}
	accounts[0].Window30m.ThirdTerminalFailureAt = accounts[0].LastTerminalFailureAt
	now = now.Add(2 * time.Minute)
	recommendations = scoreAccounts(accounts, now, 5)
	runner.prepareExploration(accounts, recommendations, now)
	candidate = testRecommendationByID(t, recommendations, 1)
	if candidate.RecommendedPriority != 15 || candidate.recoveryOutcome != recoveryOutcomeUnreliable || !candidate.applyImmediately {
		t.Fatalf("failure after the trial lease was ignored: %+v", candidate)
	}
	runner.applyRecommendations(context.Background(), []Recommendation{candidate}, "secret", now)
	accounts[0].CurrentPriority = 15
	state := runner.state.Accounts[1]
	if state.RecoveryFailures != 1 || state.RecoveryRetryAt == nil || !state.RecoveryRetryAt.Equal(now.Add(30*time.Minute)) {
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
			accounts[0].Window30m = &MetricSnapshot{TerminalFailures: 3, TrailingTerminalFailures: 3, LastTerminalFailureAt: accounts[0].LastTerminalFailureAt}
			accounts[0].Window30m.ThirdTerminalFailureAt = accounts[0].LastTerminalFailureAt
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
			if candidate.RecommendedPriority != 15 || candidate.recoveryOutcome != recoveryOutcomeUnreliable {
				t.Fatalf("failed write lost the retry recommendation: %+v", candidate)
			}
		})
	}
}
