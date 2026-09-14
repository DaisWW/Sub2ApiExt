package main

import (
	"path/filepath"
	"testing"
	"time"
)

func TestStateRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	now := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	want := &syncState{Accounts: map[int64]accountState{
		42: {CandidatePriority: 120, CandidateCount: 2, LastAppliedAt: &now, LastApplied: 120},
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
}

func TestStateRoundTripPreservesExploration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	started := time.Date(2026, 9, 9, 0, 0, 0, 0, time.UTC)
	retryAt := started.Add(6 * time.Hour)
	want := &syncState{
		Accounts: map[int64]accountState{9: {RecoveryFailures: 2, RecoveryRetryAt: &retryAt}},
		Exploration: &explorationState{
			AccountID: 9, OriginalPriority: 90, StartedAt: &started,
			Recovery: true, RecoveryAnchorCostPerMillion: 0.75, RecoveryPeerCount: 3, RecoveryTargetPriority: 10, RecoveryBaselineFailures: 4,
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
	if got.Accounts[9].RecoveryFailures != 2 || got.Accounts[9].RecoveryRetryAt == nil || !got.Accounts[9].RecoveryRetryAt.Equal(retryAt) {
		t.Fatalf("recovery retry state = %+v", got.Accounts[9])
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

func TestCloneSyncStateIsIndependent(t *testing.T) {
	started := nowForTest()
	state := &syncState{
		Accounts: map[int64]accountState{
			7: {
				CandidatePriority: 30,
				CandidateCount:    2,
				LastAppliedAt:     timePtr(started),
				LastExploredAt:    timePtr(started),
				RecoveryFailures:  2,
				RecoveryRetryAt:   timePtr(started.Add(6 * time.Hour)),
			},
		},
		Exploration: &explorationState{
			AccountID: 7, OriginalPriority: 90, StartedAt: timePtr(started), Recovery: true,
			RecoveryAnchorCostPerMillion: 0.5, RecoveryPeerCount: 2, RecoveryTargetPriority: 10, RecoveryBaselineFailures: 3,
		},
		ExplorationCursor: 7,
	}
	clone := cloneSyncState(state)
	clone.Accounts[7] = accountState{CandidatePriority: 10}
	clone.Exploration.StartedAt = timePtr(started.Add(time.Hour))
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
	if state.Accounts[7].RecoveryRetryAt == nil || !state.Accounts[7].RecoveryRetryAt.Equal(started.Add(6*time.Hour)) {
		t.Fatalf("clone mutation changed source recovery retry: %+v", state.Accounts[7])
	}
}
