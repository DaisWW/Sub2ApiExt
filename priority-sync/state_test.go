package main

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestStateRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	now := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	want := &syncState{Accounts: map[int64]accountState{
		42: {CandidatePriority: 120, CandidateCount: 2, LastAppliedAt: &now, LastApplied: 120,
			LastReliabilityFailureAt: &now, LastReliabilityRollbackAt: &now, ReliabilityHighSince: &now},
	}}
	if err := saveState(path, want); err != nil {
		t.Fatal(err)
	}
	got, err := loadState(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.Accounts[42].CandidatePriority != 120 || got.Accounts[42].CandidateCount != 2 || got.Accounts[42].LastApplied != 120 {
		t.Fatalf("state = %+v", got)
	}
	account := got.Accounts[42]
	if account.LastReliabilityFailureAt == nil || account.LastReliabilityRollbackAt == nil || account.ReliabilityHighSince == nil ||
		!account.LastReliabilityFailureAt.Equal(now) || !account.LastReliabilityRollbackAt.Equal(now) || !account.ReliabilityHighSince.Equal(now) {
		t.Fatalf("reliability watermarks did not survive restart: %+v", account)
	}
}

func TestStateRoundTripPreservesExploration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	started := time.Date(2026, 9, 9, 0, 0, 0, 0, time.UTC)
	retryAt := started.Add(6 * time.Hour)
	want := &syncState{
		Accounts: map[int64]accountState{9: {RecoveryFailures: 2, LastRecoveryFailureAt: &started, RecoveryOriginalPriority: 90, RecoveryRetryAt: &retryAt}},
		Exploration: &explorationState{
			AccountID: 9, OriginalPriority: 90, StartedAt: &started,
			Recovery: true, RecoveryAnchorCostPerMillion: 0.75, RecoveryPeerCount: 3, RecoveryTargetPriority: 10, RecoveryBaselineFailures: 4,
			RecoveryProgressive: true, RecoveryPriority: 63, RecoveryLastSuccessAt: &started, RecoveryLastFailureAt: &started,
		},
		ExplorationCursor: 9,
	}
	if err := saveState(path, want); err != nil {
		t.Fatal(err)
	}
	got, err := loadState(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.Exploration == nil || got.Exploration.AccountID != 9 || got.Exploration.OriginalPriority != 90 || got.ExplorationCursor != 9 ||
		!got.Exploration.Recovery || got.Exploration.RecoveryAnchorCostPerMillion != 0.75 || got.Exploration.RecoveryPeerCount != 3 || got.Exploration.RecoveryTargetPriority != 10 || got.Exploration.RecoveryBaselineFailures != 4 {
		t.Fatalf("exploration state = %+v", got)
	}
	if !got.Exploration.RecoveryProgressive || got.Exploration.RecoveryPriority != 63 || got.Exploration.RecoveryLastSuccessAt == nil ||
		got.Exploration.RecoveryLastFailureAt == nil || !got.Exploration.RecoveryLastSuccessAt.Equal(started) || !got.Exploration.RecoveryLastFailureAt.Equal(started) {
		t.Fatalf("progressive recovery state was not preserved: %+v", got.Exploration)
	}
	if got.Accounts[9].RecoveryFailures != 2 || got.Accounts[9].RecoveryOriginalPriority != 90 || got.Accounts[9].RecoveryRetryAt == nil || !got.Accounts[9].RecoveryRetryAt.Equal(retryAt) {
		t.Fatalf("recovery retry state = %+v", got.Accounts[9])
	}
	if got.Accounts[9].LastRecoveryFailureAt == nil || !got.Accounts[9].LastRecoveryFailureAt.Equal(started) {
		t.Fatalf("failure episode time did not survive restart: %+v", got.Accounts[9])
	}
}

func TestStateRoundTripPreservesExplorationCooldown(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	last := time.Date(2026, 9, 9, 0, 0, 0, 0, time.UTC)
	want := &syncState{Accounts: map[int64]accountState{
		9: {LastExploredAt: &last},
	}}
	if err := saveState(path, want); err != nil {
		t.Fatal(err)
	}
	got, err := loadState(path)
	if err != nil || got.Accounts[9].LastExploredAt == nil || !got.Accounts[9].LastExploredAt.Equal(last) {
		t.Fatalf("exploration cooldown = %+v, err=%v", got, err)
	}
}

func TestLoadMissingStateStartsEmpty(t *testing.T) {
	state, err := loadState(filepath.Join(t.TempDir(), "missing.json"))
	if err != nil || state == nil || len(state.Accounts) != 0 {
		t.Fatalf("state=%+v err=%v", state, err)
	}
}

func TestLegacyRecoveryStateKeepsRestorationLeaseOnStrategyUpgrade(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, []byte(`{"strategy":"direct-cost-v2","accounts":{"9":{"candidate_priority":10,"candidate_count":2}},"exploration":{"account_id":9,"original_priority":90,"started_at":"2020-01-01T00:00:00Z","recovery":true,"recovery_target_priority":10}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	state, err := loadState(path)
	if err != nil {
		t.Fatal(err)
	}
	runner := &Runner{state: state}
	runner.prepareStrategyState(nowForTest())
	if state.Strategy != strategyVersion || state.Accounts[9].CandidateCount != 0 || state.Exploration == nil ||
		state.Exploration.RecoveryProgressive || state.Exploration.OriginalPriority != 90 {
		t.Fatalf("strategy upgrade lost the fixed-trial restoration lease: %+v", state)
	}
}

func TestStrategyUpgradeRestartsLegacyBackoffAtFirstTierAcrossRestart(t *testing.T) {
	now := nowForTest()
	for _, test := range []struct {
		name                     string
		strategy                 string
		rollback, applied, trial *time.Time
		retry, wantRetry         time.Time
	}{
		{"v6 24h backoff", "evidence-cost-v6", timePtr(now), nil, nil, now.Add(24 * time.Hour), now.Add(10 * time.Minute)},
		{"v7 5h backoff", "evidence-cost-v7", timePtr(now), nil, nil, now.Add(5 * time.Hour), now.Add(10 * time.Minute)},
		{"elapsed wait", "evidence-cost-v7", timePtr(now.Add(-6 * time.Hour)), nil, nil, now.Add(18 * time.Hour), now.Add(-6*time.Hour + 10*time.Minute)},
		{"short wait", "evidence-cost-v7", timePtr(now), nil, nil, now.Add(5 * time.Minute), now.Add(5 * time.Minute)},
		{"later priority write", "evidence-cost-v7", timePtr(now.Add(-8 * time.Hour)), timePtr(now.Add(-time.Hour)), nil, now.Add(16 * time.Hour), now.Add(-8*time.Hour + 10*time.Minute)},
		{"later trial", "evidence-cost-v7", timePtr(now.Add(-8 * time.Hour)), nil, timePtr(now), now.Add(30 * time.Minute), now.Add(-8*time.Hour + 10*time.Minute)},
		{"legacy write", "evidence-cost-v7", nil, timePtr(now.Add(-time.Hour)), nil, now.Add(23 * time.Hour), now.Add(-time.Hour + 10*time.Minute)},
		{"missing time records", "evidence-cost-v7", nil, nil, nil, now.Add(24 * time.Hour), now.Add(10 * time.Minute)},
	} {
		t.Run(test.name, func(t *testing.T) {
			account := accountState{CandidatePriority: 10, CandidateCount: 2, PromotionFrozenCycles: 2,
				LastReliabilityRollbackAt: test.rollback, LastAppliedAt: test.applied, LastExploredAt: test.trial,
				RecoveryFailures: 4, RecoveryOriginalPriority: 90, RecoveryRetryAt: timePtr(test.retry),
				LastReliabilityFailureAt: timePtr(now.Add(-time.Minute)), ReliabilityHighSince: timePtr(now.Add(-time.Hour))}
			state := &syncState{Strategy: test.strategy, Accounts: map[int64]accountState{9: account},
				Exploration: &explorationState{AccountID: 9, OriginalPriority: 90, Recovery: true, StartedAt: timePtr(now)}}
			lease := *state.Exploration
			runner := &Runner{state: state}
			runner.prepareStrategyState(now)
			want := account
			want.CandidatePriority, want.CandidateCount, want.PromotionFrozenCycles = 0, 0, 0
			want.RecoveryFailures = 1
			want.LastRecoveryFailureAt = test.rollback
			if want.LastRecoveryFailureAt == nil {
				want.LastRecoveryFailureAt = test.applied
			}
			if want.LastRecoveryFailureAt == nil {
				want.LastRecoveryFailureAt = timePtr(now)
			}
			want.RecoveryRetryAt = timePtr(test.wantRetry)
			if !reflect.DeepEqual(state.Accounts[9], want) || !reflect.DeepEqual(*state.Exploration, lease) || state.Strategy != strategyVersion {
				t.Fatalf("strategy upgrade changed recovery evidence or deadline: %+v", state)
			}
			path := filepath.Join(t.TempDir(), "state.json")
			if err := saveState(path, state); err != nil {
				t.Fatal(err)
			}
			loaded, err := loadState(path)
			if err != nil {
				t.Fatal(err)
			}
			runner.state = loaded
			runner.prepareStrategyState(now.Add(time.Hour))
			if !reflect.DeepEqual(loaded.Accounts[9], want) {
				t.Fatalf("restart renewed the recovery wait: %+v", loaded.Accounts[9])
			}
		})
	}
}

func TestStrategyUpgradePreservesTimedFailuresAndNoResultWait(t *testing.T) {
	now := nowForTest()
	for _, strategy := range []string{"evidence-cost-v8", "evidence-cost-v9"} {
		t.Run(strategy, func(t *testing.T) {
			state := &syncState{Strategy: strategy, Accounts: map[int64]accountState{
				1: {RecoveryRetryAt: timePtr(now.Add(recoveryDuration))},
				2: {RecoveryFailures: 5, LastRecoveryFailureAt: timePtr(now), RecoveryRetryAt: timePtr(now.Add(2 * time.Hour)),
					LastReliabilityRollbackAt: timePtr(now), ReliabilityHighSince: timePtr(now)},
			}, Exploration: &explorationState{AccountID: 1, OriginalPriority: 65, Recovery: true,
				RecoveryProgressive: true, RecoveryPriority: 60, RecoveryTargetPriority: 30, StartedAt: timePtr(now.Add(-15 * time.Minute))}}
			want := cloneSyncState(state)
			want.Strategy = strategyVersion
			runner := &Runner{state: state}
			runner.prepareStrategyState(now)
			if !reflect.DeepEqual(state, want) || !explorationExpired(state.Exploration, now) {
				t.Fatalf("upgrade lost recovery evidence or restarted the elapsed observation: %+v", state)
			}
		})
	}
}

func TestCloneSyncStateIsIndependent(t *testing.T) {
	started := nowForTest()
	state := &syncState{
		Accounts: map[int64]accountState{
			7: {
				CandidatePriority:        30,
				CandidateCount:           2,
				LastAppliedAt:            timePtr(started),
				LastExploredAt:           timePtr(started),
				RecoveryFailures:         2,
				LastRecoveryFailureAt:    timePtr(started),
				RecoveryOriginalPriority: 90,
				RecoveryRetryAt:          timePtr(started.Add(6 * time.Hour)),
				LastReliabilityFailureAt: timePtr(started), LastReliabilityRollbackAt: timePtr(started), ReliabilityHighSince: timePtr(started),
			},
		},
		Exploration: &explorationState{
			AccountID: 7, OriginalPriority: 90, StartedAt: timePtr(started), Recovery: true,
			RecoveryAnchorCostPerMillion: 0.5, RecoveryPeerCount: 2, RecoveryTargetPriority: 10, RecoveryBaselineFailures: 3,
			RecoveryProgressive: true, RecoveryPriority: 63, RecoveryLastSuccessAt: timePtr(started), RecoveryLastFailureAt: timePtr(started),
		},
		ExplorationCursor: 7,
	}
	clone := cloneSyncState(state)
	cloned := clone.Accounts[7]
	*cloned.LastRecoveryFailureAt = started.Add(time.Hour)
	*cloned.LastReliabilityFailureAt = started.Add(time.Hour)
	*cloned.LastReliabilityRollbackAt = started.Add(time.Hour)
	*cloned.ReliabilityHighSince = started.Add(time.Hour)
	if !state.Accounts[7].LastRecoveryFailureAt.Equal(started) || !state.Accounts[7].LastReliabilityFailureAt.Equal(started) || !state.Accounts[7].LastReliabilityRollbackAt.Equal(started) || !state.Accounts[7].ReliabilityHighSince.Equal(started) {
		t.Fatal("clone mutation changed source reliability watermarks")
	}
	if clone.Accounts[7].RecoveryOriginalPriority != 90 {
		t.Fatalf("clone lost recovery rollback priority: %+v", clone.Accounts[7])
	}
	clone.Accounts[7] = accountState{CandidatePriority: 10}
	clone.Exploration.StartedAt = timePtr(started.Add(time.Hour))
	*clone.Exploration.RecoveryLastSuccessAt = started.Add(time.Hour)
	*clone.Exploration.RecoveryLastFailureAt = started.Add(time.Hour)
	cloneAccount := clone.Accounts[7]
	cloneAccount.RecoveryRetryAt = timePtr(started.Add(24 * time.Hour))
	clone.Accounts[7] = cloneAccount
	clone.ExplorationCursor = 9
	if state.Accounts[7].CandidatePriority != 30 || state.ExplorationCursor != 7 {
		t.Fatalf("clone mutation changed source state: source=%+v clone=%+v", state, clone)
	}
	if state.Exploration.StartedAt == nil || !state.Exploration.StartedAt.Equal(started) {
		t.Fatalf("clone mutation changed source exploration time: %+v", state.Exploration)
	}
	if !state.Exploration.RecoveryLastSuccessAt.Equal(started) || !state.Exploration.RecoveryLastFailureAt.Equal(started) {
		t.Fatalf("clone mutation changed source evidence watermarks: %+v", state.Exploration)
	}
	if state.Accounts[7].RecoveryRetryAt == nil || !state.Accounts[7].RecoveryRetryAt.Equal(started.Add(6*time.Hour)) {
		t.Fatalf("clone mutation changed source recovery retry: %+v", state.Accounts[7])
	}
}
