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

func TestLoadMissingStateStartsEmpty(t *testing.T) {
	state, err := loadState(filepath.Join(t.TempDir(), "missing.json"))
	if err != nil || state == nil || len(state.Accounts) != 0 {
		t.Fatalf("state=%+v err=%v", state, err)
	}
}
