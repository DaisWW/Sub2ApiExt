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

func TestRecoveryBackoffUpgradeResumesElapsedWaitAndKeepsDryRunReadOnly(t *testing.T) {
	now := nowForTest()
	for _, dryRun := range []bool{false, true} {
		t.Run(fmt.Sprintf("dry_run=%t", dryRun), func(t *testing.T) {
			accounts := comparableRecoveryAccounts()
			runner, updates := progressiveRecoveryRunner(t, http.StatusOK)
			runner.config.DryRun = dryRun
			runner.config.Confirmations = 2
			runner.source = &fakeMetricsSource{accounts: accounts}
			runner.state.Strategy = "evidence-cost-v6"
			runner.state.Accounts[1] = accountState{RecoveryFailures: 4,
				LastAppliedAt: timePtr(now.Add(-6 * time.Hour)), LastReliabilityRollbackAt: timePtr(now.Add(-6 * time.Hour)),
				RecoveryRetryAt: timePtr(now.Add(18 * time.Hour))}
			before := cloneSyncState(runner.state)
			if err := runner.RunOnce(context.Background(), now); err != nil {
				t.Fatal(err)
			}
			if dryRun {
				if len(updates) != 0 || !reflect.DeepEqual(runner.state, before) {
					t.Fatal("dry-run migrated live backoff or wrote a priority")
				}
				return
			}
			if len(updates) != 1 || <-updates != priorityUnavailable-recoveryRampStep || runner.state.Exploration == nil ||
				runner.state.Exploration.AccountID != 1 || runner.state.Accounts[1].RecoveryFailures != 1 {
				t.Fatalf("elapsed legacy backoff still blocked gradual recovery: %+v", runner.state)
			}
			loaded, err := loadState(runner.config.StateFile)
			if err != nil || loaded.Accounts[1].RecoveryRetryAt == nil || !loaded.Accounts[1].RecoveryRetryAt.Equal(now.Add(-6*time.Hour+10*time.Minute)) {
				t.Fatalf("shortened backoff was not persisted: state=%+v err=%v", loaded, err)
			}
		})
	}
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

func TestProgressiveRecoveryReachesAnchorWithoutTrafficAcrossRestart(t *testing.T) {
	now := nowForTest()
	accounts := comparableRecoveryAccounts()
	accounts[0].CurrentPriority = 39
	accounts[1].CurrentPriority = priorityNeutral
	runner, updates := progressiveRecoveryRunner(t, http.StatusOK)
	runner.source = &recoveryObservationSource{fakeMetricsSource: fakeMetricsSource{accounts: accounts}, observation: AccountMetrics{ID: 1}}
	failureAt := now.Add(-3 * time.Hour)
	runner.state.Strategy = strategyVersion
	runner.state.Accounts[1] = accountState{RecoveryFailures: 2, LastRecoveryFailureAt: timePtr(failureAt),
		LastReliabilityRollbackAt: timePtr(failureAt), RecoveryRetryAt: timePtr(now.Add(-time.Hour))}
	lastWrite := now.Add(-recoveryDuration)
	for cycle := 0; cycle <= 21; cycle++ {
		at := now.Add(time.Duration(cycle) * defaultInterval)
		previous := accounts[0].CurrentPriority
		if err := runner.RunOnce(context.Background(), at); err != nil {
			t.Fatal(err)
		}
		if len(updates) > 0 {
			priority := <-updates
			step := recoverySlowRampStep
			if cycle == 0 {
				step = recoveryRampStep
			}
			if len(updates) != 0 || priority != max(priorityBest, previous-step) || at.Sub(lastWrite) < recoveryDuration {
				t.Fatalf("no-traffic recovery skipped an observation or overshot its anchor: cycle=%d %d->%d", cycle, previous, priority)
			}
			accounts[0].CurrentPriority = priority
			lastWrite = at
		}
		state := runner.state.Accounts[1]
		if state.RecoveryFailures != 2 || state.LastRecoveryFailureAt == nil || !state.LastRecoveryFailureAt.Equal(failureAt) {
			t.Fatalf("no traffic was treated as a new failure or proven health: %+v", state)
		}
		loaded, err := loadState(runner.config.StateFile)
		if err != nil {
			t.Fatal(err)
		}
		runner.state = loaded
	}
	if accounts[0].CurrentPriority != priorityBest || runner.state.Exploration != nil {
		t.Fatalf("no traffic stranded recovery before its anchor: %+v", runner.state)
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
		{"old failure", timePtr(now.Add(-time.Minute)), nil, 666, 0, recoveryOutcomeProgress},
		{"isolated new failure", timePtr(now.Add(time.Minute)), nil, 670, 0, recoveryOutcomeNoResult},
		{"five new failures", timePtr(now.Add(time.Minute)), nil, priorityUnavailable, 1, recoveryOutcomeUnreliable},
		{"old success", nil, timePtr(now.Add(-time.Minute)), 666, 0, recoveryOutcomeProgress},
		{"one new success", nil, timePtr(now.Add(time.Minute)), 666, 0, recoveryOutcomeProgress},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			accounts := comparableRecoveryAccounts()
			accounts[0].CurrentPriority = 670
			accounts[0].LastTerminalFailureAt, accounts[0].LastPricedSuccessAt = test.lastFailure, test.lastSuccess
			if test.lastFailure != nil {
				accounts[0].Window30m = &MetricSnapshot{TerminalFailures: 1, LastTerminalFailureAt: test.lastFailure}
				if test.wantFailures > 0 {
					accounts[0].Window30m.TerminalFailures, accounts[0].Window30m.TrailingTerminalFailures = 5, 5
					accounts[0].Window30m.FifthTerminalFailureAt = timePtr(now.Add(time.Second))
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
	accounts[0].Window30m = &MetricSnapshot{TerminalFailures: 5, TrailingTerminalFailures: 5, LastTerminalFailureAt: accounts[0].LastTerminalFailureAt}
	accounts[0].RecoveryWindow = accounts[0].Window30m
	accounts[0].Window30m.FifthTerminalFailureAt = timePtr(now.Add(time.Second))
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

func TestProgressiveRecoveryStopsOnFiveFailuresBeforeObservationExpires(t *testing.T) {
	now := nowForTest()
	accounts := comparableRecoveryAccounts()
	accounts[0].CurrentPriority = 63
	accounts[0].Window30m = &MetricSnapshot{}
	accounts[0].RecoveryWindow = accounts[0].Window30m
	runner, updates := progressiveRecoveryRunner(t, http.StatusOK)
	runner.state.Exploration = &explorationState{AccountID: 1, OriginalPriority: 90, Recovery: true,
		RecoveryProgressive: true, RecoveryPriority: 63, RecoveryTargetPriority: priorityBest, StartedAt: timePtr(now)}
	for failures := int64(1); failures <= 5; failures++ {
		at := now.Add(time.Duration(failures) * time.Minute)
		snapshot := accounts[0].RecoveryWindow
		snapshot.TerminalFailures, snapshot.TrailingTerminalFailures = failures, failures
		snapshot.LastTerminalFailureAt = timePtr(at.Add(-time.Second))
		if failures == 5 {
			snapshot.FifthTerminalFailureAt = timePtr(now.Add(time.Second))
		}
		recommendations := scoreAccounts(accounts, at, 5)
		runner.prepareExploration(accounts, recommendations, at)
		candidate := testRecommendationByID(t, recommendations, 1)
		if failures < 5 && (candidate.explorationEnd || candidate.reliabilityRollback || candidate.RecommendedPriority != 63) {
			t.Fatalf("%d failures rolled back before the five-failure threshold: %+v", failures, candidate)
		}
		if failures == 5 && (!candidate.explorationEnd || !candidate.applyImmediately || candidate.RecommendedPriority != 90 || candidate.recoveryOutcome != recoveryOutcomeUnreliable) {
			t.Fatalf("five new failures waited for the full observation lease: %+v", candidate)
		}
		runner.applyRecommendations(context.Background(), []Recommendation{candidate}, "secret", at)
		if failures < 5 && (runner.state.Exploration == nil || runner.state.Accounts[1].RecoveryFailures != 0 || len(updates) != 0) {
			t.Fatalf("tolerated failures consumed recovery progress: %+v", runner.state)
		}
	}
	if len(updates) != 1 || <-updates != 90 || runner.state.Exploration != nil || runner.state.Accounts[1].RecoveryFailures != 1 {
		t.Fatalf("five failures did not roll back exactly once: %+v", runner.state)
	}
}

func TestProgressiveRecoveryHighFailureRateWaitsAcrossRestart(t *testing.T) {
	now := nowForTest()
	accounts := comparableRecoveryAccounts()
	accounts[0].CurrentPriority = 60
	accounts[0].RecoveryWindow = &MetricSnapshot{SuccessfulRequests: 91, TerminalFailures: 9,
		FifthTerminalFailureAt: timePtr(now.Add(time.Second)), LastTerminalFailureAt: timePtr(now.Add(time.Minute)),
		LastPricedSuccessAt: timePtr(now.Add(29 * time.Minute))}
	runner, updates := progressiveRecoveryRunner(t, http.StatusOK)
	runner.state.Accounts[1] = accountState{RecoveryOriginalPriority: 65, LastAppliedAt: timePtr(now), LastApplied: 60}
	runner.state.Exploration = &explorationState{AccountID: 1, OriginalPriority: 65, Recovery: true,
		RecoveryProgressive: true, RecoveryPriority: 60, RecoveryTargetPriority: priorityBest, StartedAt: timePtr(now)}
	for cycle := 0; cycle < 3; cycle++ {
		at := now.Add(recoveryDuration + time.Duration(cycle)*defaultInterval)
		recommendations := scoreAccounts(accounts, at, 5)
		runner.prepareExploration(accounts, recommendations, at)
		candidate := testRecommendationByID(t, recommendations, 1)
		if cycle < 2 && (candidate.RecommendedPriority != 60 || candidate.explorationEnd || candidate.ReliabilityAction != "observe") {
			t.Fatalf("expired recovery lease bypassed high-failure observation: %+v", candidate)
		}
		if cycle == 2 && (candidate.RecommendedPriority != 65 || !candidate.explorationEnd || candidate.recoveryOutcome != recoveryOutcomeUnreliable) {
			t.Fatalf("persistent recovery errors did not roll back after observation: %+v", candidate)
		}
		runner.applyRecommendations(context.Background(), []Recommendation{candidate}, "secret", at)
		if cycle < 2 && (len(updates) != 0 || runner.state.Exploration == nil || runner.state.Accounts[1].RecoveryFailures != 0) {
			t.Fatalf("observed errors consumed a recovery outcome: %+v", runner.state)
		}
		if err := saveState(runner.config.StateFile, runner.state); err != nil {
			t.Fatal(err)
		}
		loaded, err := loadState(runner.config.StateFile)
		if err != nil {
			t.Fatal(err)
		}
		runner.state = loaded
	}
	state := runner.state.Accounts[1]
	if len(updates) != 1 || <-updates != 65 || runner.state.Exploration != nil || state.RecoveryFailures != 1 ||
		state.RecoveryRetryAt == nil || !state.RecoveryRetryAt.Equal(now.Add(recoveryDuration+2*defaultInterval+10*time.Minute)) {
		t.Fatalf("confirmed recovery rollback did not preserve its outcome: %+v", runner.state)
	}
}

func TestProgressiveRecoveryHighFailureRateCanRecoverDuringObservation(t *testing.T) {
	now := nowForTest()
	accounts := comparableRecoveryAccounts()
	accounts[0].CurrentPriority = 60
	accounts[0].RecoveryWindow = &MetricSnapshot{SuccessfulRequests: 91, TerminalFailures: 9,
		LastTerminalFailureAt: timePtr(now.Add(time.Minute)), LastPricedSuccessAt: timePtr(now.Add(29 * time.Minute))}
	runner, updates := progressiveRecoveryRunner(t, http.StatusOK)
	runner.state.Exploration = &explorationState{AccountID: 1, OriginalPriority: 65, Recovery: true,
		RecoveryProgressive: true, RecoveryPriority: 60, RecoveryTargetPriority: priorityBest, StartedAt: timePtr(now)}
	at := now.Add(recoveryDuration)
	recommendations := scoreAccounts(accounts, at, 5)
	runner.prepareExploration(accounts, recommendations, at)
	runner.applyRecommendations(context.Background(), []Recommendation{testRecommendationByID(t, recommendations, 1)}, "secret", at)
	accounts[0].RecoveryWindow.SuccessfulRequests = 391
	accounts[0].RecoveryWindow.LastPricedSuccessAt = timePtr(at.Add(defaultInterval - time.Minute))
	at = at.Add(defaultInterval)
	recommendations = scoreAccounts(accounts, at, 5)
	runner.prepareExploration(accounts, recommendations, at)
	candidate := testRecommendationByID(t, recommendations, 1)
	if candidate.RecommendedPriority != 52 || candidate.recoveryOutcome != recoveryOutcomeSuccess || candidate.reliabilityRollback {
		t.Fatalf("healthier new traffic could not resume recovery during observation: %+v", candidate)
	}
	runner.applyRecommendations(context.Background(), []Recommendation{candidate}, "secret", at)
	state := runner.state.Accounts[1]
	if len(updates) != 1 || <-updates != 52 || state.RecoveryFailures != 0 || state.ReliabilityHighSince != nil || state.RecoveryRetryAt != nil {
		t.Fatalf("transient failure rate left a penalty after healthy recovery: %+v", state)
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
	if got := testRecommendationByID(t, recommendations, 1); got.RecommendedPriority != priorityPoor ||
		got.recoveryOutcome != recoveryOutcomeSuccess || !got.recoveryCompleted || got.applyImmediately {
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

func TestProgressiveRecoveryUsesSlowStepsForSparseSamplesAndFailureRateBoundaries(t *testing.T) {
	now := nowForTest()
	for _, test := range []struct {
		name                string
		successes, failures int64
		oldSuccess          bool
		wantPriority        int
		wantOutcome         string
	}{
		{"no requests", 0, 0, false, 56, recoveryOutcomeProgress},
		{"one new success", 1, 0, false, 56, recoveryOutcomeProgress},
		{"forty-nine requests", 49, 0, false, 56, recoveryOutcomeProgress},
		{"fresh error in short sample pauses", 1, 1, false, 60, recoveryOutcomeNoResult},
		{"fifty healthy requests", 50, 0, false, 52, recoveryOutcomeSuccess},
		{"isolated error below three percent", 99, 1, false, 52, recoveryOutcomeSuccess},
		{"exactly three percent", 97, 3, false, 56, recoveryOutcomeProgress},
		{"exactly eight percent", 92, 8, false, 56, recoveryOutcomeProgress},
		{"above eight percent waits for confirmation", 91, 9, false, 60, ""},
		{"fifty requests above eight percent waits for confirmation", 45, 5, false, 60, ""},
		{"no fresh priced success", 50, 0, true, 56, recoveryOutcomeProgress},
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
			if test.failures >= 5 {
				snapshot.FifthTerminalFailureAt = timePtr(now.Add(time.Second))
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
				if state.RecoveryFailures != 1 || state.RecoveryRetryAt == nil || !state.RecoveryRetryAt.Equal(at.Add(10*time.Minute)) {
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
		if candidate.RecommendedPriority != 52 || candidate.recoveryOutcome != recoveryOutcomeSuccess {
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

func TestSparseRecoveryFailedWriteAndDryRunKeepProgress(t *testing.T) {
	now := nowForTest()
	for _, dryRun := range []bool{false, true} {
		t.Run(fmt.Sprintf("dry_run=%t", dryRun), func(t *testing.T) {
			accounts := comparableRecoveryAccounts()
			accounts[0].CurrentPriority = 60
			accounts[1].CurrentPriority = priorityNeutral
			runner, updates := progressiveRecoveryRunner(t, http.StatusBadGateway)
			runner.config.DryRun = dryRun
			runner.source = &recoveryObservationSource{fakeMetricsSource: fakeMetricsSource{accounts: accounts}, observation: AccountMetrics{ID: 1}}
			runner.state.Strategy = strategyVersion
			failureAt := now.Add(-3 * time.Hour)
			runner.state.Accounts[1] = accountState{LastSeenAt: timePtr(now), RecoveryOriginalPriority: 65,
				LastAppliedAt: timePtr(now.Add(-recoveryDuration)), LastApplied: 60,
				RecoveryFailures: 2, LastRecoveryFailureAt: timePtr(failureAt), LastReliabilityRollbackAt: timePtr(failureAt)}
			runner.state.Exploration = &explorationState{AccountID: 1, OriginalPriority: 65, Recovery: true,
				RecoveryProgressive: true, RecoveryPriority: 60, RecoveryTargetPriority: priorityBest, StartedAt: timePtr(now.Add(-recoveryDuration))}
			before := cloneSyncState(runner.state)
			if err := runner.RunOnce(context.Background(), now); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(runner.state.Accounts[1], before.Accounts[1]) || !reflect.DeepEqual(runner.state.Exploration, before.Exploration) {
				t.Fatalf("failed sparse recovery consumed progress or reset its timer: %+v", runner.state)
			}
			if dryRun {
				if len(updates) != 0 || !reflect.DeepEqual(runner.state, before) {
					t.Fatal("sparse dry-run changed live state or wrote a priority")
				}
			} else if len(updates) != 1 || <-updates != 56 {
				t.Fatal("sparse recovery did not attempt the bounded step")
			}
		})
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
			if source.window != min(duration, decisionWindow) || len(updates) != 1 || <-updates != 52 || runner.state.Exploration != nil {
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

func TestInsufficientRecoverySamplesRestartObservationAfterSlowStep(t *testing.T) {
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
	if runner.state.Exploration != nil || len(updates) != 1 || <-updates != 56 {
		t.Fatal("twenty-five new samples did not allow a slow recovery step")
	}
	loaded, err := loadState(runner.config.StateFile)
	if err != nil {
		t.Fatal(err)
	}
	runner.state = loaded
	source.accounts[0].CurrentPriority = 56
	source.accounts[0].LastPricedSuccessAt = timePtr(now.Add(39 * time.Minute))
	source.observation.SuccessfulRequests = 50
	source.observation.LastPricedSuccessAt = source.accounts[0].LastPricedSuccessAt
	if err := runner.RunOnce(context.Background(), now.Add(recoveryDuration+defaultInterval)); err != nil {
		t.Fatal(err)
	}
	if runner.state.Exploration == nil || !runner.state.Exploration.StartedAt.Equal(now.Add(recoveryDuration)) || len(updates) != 0 {
		t.Fatalf("restarted lease skipped the new position's observation period: %+v", runner.state)
	}
	if err := runner.RunOnce(context.Background(), now.Add(2*recoveryDuration-defaultInterval)); err != nil {
		t.Fatal(err)
	}
	if len(updates) != 0 {
		t.Fatal("healthy samples skipped the new position's thirty-minute observation")
	}
	source.accounts[0].LastPricedSuccessAt = timePtr(now.Add(59 * time.Minute))
	source.observation.LastPricedSuccessAt = source.accounts[0].LastPricedSuccessAt
	if err := runner.RunOnce(context.Background(), now.Add(2*recoveryDuration)); err != nil {
		t.Fatal(err)
	}
	if source.window != recoveryDuration || len(updates) != 1 || <-updates != 48 || runner.state.Accounts[1].RecoveryOriginalPriority != 56 {
		t.Fatalf("slow step reused evidence from the prior position or failed to resume healthy recovery: window=%s state=%+v", source.window, runner.state)
	}
}

func TestProgressiveRecoveryKeepsStepLimitBeforeCostMatures(t *testing.T) {
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
	if got := recommendations[0]; got.CostEstimateSource != "" || got.RecommendedPriority != 52 ||
		got.recoveryOutcome != recoveryOutcomeSuccess || got.recoveryCompleted {
		t.Fatalf("short cost evidence bypassed or stopped the recovery step: %+v", got)
	}
}

func TestMatureRecoveryGraduatesAfterConfirmationAcrossRestart(t *testing.T) {
	now := nowForTest()
	for _, activeLease := range []bool{false, true} {
		t.Run(fmt.Sprintf("active_lease=%t", activeLease), func(t *testing.T) {
			account := comparableCostAccount(1, 0.1, 0.9, 100, 5_000_000, 90)
			account.CurrentPriority = 60
			account.Window2h.LastPricedSuccessAt = timePtr(now.Add(-time.Minute))
			source := &recoveryObservationSource{fakeMetricsSource: fakeMetricsSource{accounts: []AccountMetrics{account}},
				observation: AccountMetrics{ID: 1, SuccessfulRequests: 50, LastPricedSuccessAt: account.Window2h.LastPricedSuccessAt}}
			runner, updates := progressiveRecoveryRunner(t, http.StatusOK)
			runner.source, runner.config.Confirmations = source, 2
			runner.state.Strategy = strategyVersion
			runner.state.Accounts[1] = accountState{RecoveryOriginalPriority: 65, LastAppliedAt: timePtr(now.Add(-3 * time.Hour)), LastApplied: 60}
			if activeLease {
				runner.state.Exploration = &explorationState{AccountID: 1, OriginalPriority: 65, Recovery: true,
					RecoveryProgressive: true, RecoveryPriority: 60, RecoveryTargetPriority: priorityBest, StartedAt: timePtr(now.Add(-recoveryDuration))}
			}
			if err := runner.RunOnce(context.Background(), now); err != nil {
				t.Fatal(err)
			}
			if len(updates) != 0 || runner.state.Accounts[1].RecoveryOriginalPriority != 65 {
				t.Fatalf("graduation bypassed confirmation or consumed state: %+v", runner.state)
			}
			loaded, err := loadState(runner.config.StateFile)
			if err != nil {
				t.Fatal(err)
			}
			runner.state = loaded
			if err := runner.RunOnce(context.Background(), now.Add(defaultInterval)); err != nil {
				t.Fatal(err)
			}
			state := runner.state.Accounts[1]
			if len(updates) != 1 || <-updates != 40 || state.RecoveryOriginalPriority != 0 ||
				state.RecoveryFailures != 0 || state.RecoveryRetryAt != nil || runner.state.Exploration != nil {
				t.Fatalf("mature healthy recovery did not return to normal cost adjustment: %+v", runner.state)
			}
		})
	}
}

func TestMatureRecoveryNeedsFiftyFreshRequestsToGraduate(t *testing.T) {
	now := nowForTest()
	account := comparableCostAccount(1, 0.1, 0.9, 100, 5_000_000, 90)
	account.CurrentPriority = 60
	account.RecoveryWindow = &MetricSnapshot{SuccessfulRequests: 49, LastPricedSuccessAt: timePtr(now.Add(-time.Minute))}
	runner, _ := progressiveRecoveryRunner(t, http.StatusOK)
	runner.state.Accounts[1] = accountState{RecoveryOriginalPriority: 65}
	runner.state.Exploration = &explorationState{AccountID: 1, OriginalPriority: 65, Recovery: true,
		RecoveryProgressive: true, RecoveryPriority: 60, RecoveryTargetPriority: priorityBest, StartedAt: timePtr(now.Add(-recoveryDuration))}
	recommendations := scoreAccounts([]AccountMetrics{account}, now, 5)
	runner.prepareExploration([]AccountMetrics{account}, recommendations, now)
	if got := recommendations[0]; got.CostEvidenceWeight != 1 || got.RecommendedPriority != 56 ||
		got.recoveryCompleted || got.recoveryOutcome != recoveryOutcomeProgress {
		t.Fatalf("mature rolling costs bypassed the sparse-sample step limit: %+v", got)
	}
}

func TestRecoveryGraduationFailedWriteAndDryRunKeepProgress(t *testing.T) {
	now := nowForTest()
	for _, dryRun := range []bool{false, true} {
		t.Run(fmt.Sprintf("dry_run=%t", dryRun), func(t *testing.T) {
			account := comparableCostAccount(1, 0.1, 0.9, 100, 5_000_000, 90)
			account.CurrentPriority = 60
			account.Window2h.LastPricedSuccessAt = timePtr(now.Add(-time.Minute))
			runner, updates := progressiveRecoveryRunner(t, http.StatusBadGateway)
			runner.config.DryRun = dryRun
			runner.source = &recoveryObservationSource{fakeMetricsSource: fakeMetricsSource{accounts: []AccountMetrics{account}},
				observation: AccountMetrics{ID: 1, SuccessfulRequests: 50, LastPricedSuccessAt: account.Window2h.LastPricedSuccessAt}}
			runner.state.Strategy = strategyVersion
			runner.state.Accounts[1] = accountState{RecoveryOriginalPriority: 65, LastAppliedAt: timePtr(now.Add(-recoveryDuration)), LastApplied: 60}
			runner.state.Exploration = &explorationState{AccountID: 1, OriginalPriority: 65, Recovery: true,
				RecoveryProgressive: true, RecoveryPriority: 60, RecoveryTargetPriority: priorityBest, StartedAt: timePtr(now.Add(-recoveryDuration))}
			before := cloneSyncState(runner.state)
			if err := runner.RunOnce(context.Background(), now); err != nil {
				t.Fatal(err)
			}
			state := runner.state.Accounts[1]
			if state.RecoveryOriginalPriority != 65 || state.LastApplied != 60 ||
				!state.LastAppliedAt.Equal(*before.Accounts[1].LastAppliedAt) || !reflect.DeepEqual(runner.state.Exploration, before.Exploration) {
				t.Fatalf("failed graduation consumed recovery progress: %+v", runner.state)
			}
			if dryRun && (!reflect.DeepEqual(runner.state, before) || len(updates) != 0) {
				t.Fatal("dry-run mutated recovery state or wrote priorities")
			}
		})
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
	accounts[0].Window30m = &MetricSnapshot{TerminalFailures: 5, TrailingTerminalFailures: 5, LastTerminalFailureAt: accounts[0].LastTerminalFailureAt}
	accounts[0].Window30m.FifthTerminalFailureAt = accounts[0].LastTerminalFailureAt
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
	if state.RecoveryFailures != 1 || state.RecoveryRetryAt == nil || !state.RecoveryRetryAt.Equal(now.Add(10*time.Minute)) {
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
			accounts[0].Window30m = &MetricSnapshot{TerminalFailures: 5, TrailingTerminalFailures: 5, LastTerminalFailureAt: accounts[0].LastTerminalFailureAt}
			accounts[0].Window30m.FifthTerminalFailureAt = accounts[0].LastTerminalFailureAt
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

func TestMatureRecoveryWithModerateErrorsRollsBackToPreviousStepAfterLeaseEnds(t *testing.T) {
	now := nowForTest()
	account := AccountMetrics{ID: 1, Status: "active", CurrentPriority: 60, RateMultiplier: 0.1,
		DecisionWindowsAvailable: true, Window2h: &MetricSnapshot{SuccessfulRequests: 50, PricedRequests: 50,
			PricedTokens: 2_000_000, TotalTokens: 2_000_000, AccountCost: 2, ContinuousPricedRequests: 49},
		RecoveryWindow: &MetricSnapshot{SuccessfulRequests: 48, TerminalFailures: 2, LastPricedSuccessAt: timePtr(now.Add(time.Minute))}}
	runner, updates := progressiveRecoveryRunner(t, http.StatusOK)
	runner.state.Accounts[1] = accountState{RecoveryOriginalPriority: 65, LastAppliedAt: timePtr(now), LastApplied: 60}
	runner.state.Exploration = &explorationState{AccountID: 1, OriginalPriority: 65, Recovery: true,
		RecoveryProgressive: true, RecoveryPriority: 60, RecoveryTargetPriority: priorityBest, StartedAt: timePtr(now)}
	at := now.Add(recoveryDuration)
	recommendations := scoreAccounts([]AccountMetrics{account}, at, 5)
	runner.prepareExploration([]AccountMetrics{account}, recommendations, at)
	runner.applyRecommendations(context.Background(), recommendations, "secret", at)
	if runner.state.Exploration != nil || len(updates) != 1 || <-updates != 56 {
		t.Fatalf("test did not complete a slow recovery step: %+v", runner.state)
	}
	account.CurrentPriority = 56
	if err := saveState(runner.config.StateFile, runner.state); err != nil {
		t.Fatal(err)
	}
	loaded, err := loadState(runner.config.StateFile)
	if err != nil {
		t.Fatal(err)
	}
	runner.state = loaded
	account.Window2h.TerminalFailures, account.Window2h.TrailingTerminalFailures = 5, 5
	account.Window2h.FifthTerminalFailureAt = timePtr(at.Add(time.Second))
	account.Window2h.LastTerminalFailureAt = timePtr(at.Add(time.Minute))
	at = at.Add(2 * time.Minute)
	recommendations = scoreAccounts([]AccountMetrics{account}, at, 5)
	runner.prepareExploration([]AccountMetrics{account}, recommendations, at)
	if got := recommendations[0]; !hasReliableCost(&got) || !got.reliabilityRollback || got.RecommendedPriority != 60 {
		t.Fatalf("mature cost discarded the last recovery position after lease/restart: %+v", got)
	}
	runner.applyRecommendations(context.Background(), recommendations, "secret", at)
	state := runner.state.Accounts[1]
	if len(updates) != 1 || <-updates != 60 || state.RecoveryFailures != 1 || state.RecoveryRetryAt == nil ||
		!state.RecoveryRetryAt.Equal(at.Add(10*time.Minute)) {
		t.Fatalf("post-lease rollback did not restore one step with short backoff: %+v", state)
	}
}
