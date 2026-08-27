package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSyncHealthAcceptsRecentCycle(t *testing.T) {
	stateFile := filepath.Join(t.TempDir(), "state.json")
	now := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	if err := writeSyncHealth(stateFile, now.Add(-2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := checkSyncHealth(stateFile, 5*time.Minute, now); err != nil {
		t.Fatalf("recent cycle should be healthy: %v", err)
	}
}

func TestSyncHealthAcceptsWaitingForAdminKey(t *testing.T) {
	stateFile := filepath.Join(t.TempDir(), "state.json")
	now := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	if err := writeSyncHealthState(stateFile, syncHealth{Phase: syncHealthWaiting, LastCycleAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := checkSyncHealth(stateFile, 5*time.Minute, now.Add(time.Minute)); err != nil {
		t.Fatalf("waiting state should keep the process ready: %v", err)
	}
}

func TestSyncHealthRejectsStaleWaitingCycle(t *testing.T) {
	stateFile := filepath.Join(t.TempDir(), "state.json")
	now := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	if err := writeSyncHealthState(stateFile, syncHealth{
		Phase:       syncHealthWaiting,
		LastCycleAt: now.Add(-16 * time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	if err := checkSyncHealth(stateFile, 5*time.Minute, now); err == nil {
		t.Fatal("stale waiting cycle should be unhealthy")
	}
}

func TestSyncHealthRejectsUnknownPhase(t *testing.T) {
	stateFile := filepath.Join(t.TempDir(), "state.json")
	if err := writeSyncHealthState(stateFile, syncHealth{Phase: "unknown"}); err != nil {
		t.Fatal(err)
	}
	if err := checkSyncHealth(stateFile, 5*time.Minute, time.Now()); err == nil {
		t.Fatal("unknown health phase should be rejected")
	}
}

func TestSyncHealthValidatesFailedCycleTimestamps(t *testing.T) {
	stateFile := filepath.Join(t.TempDir(), "state.json")
	now := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name   string
		health syncHealth
		wantOK bool
	}{
		{
			name: "recent previous success",
			health: syncHealth{
				Phase:         syncHealthFailed,
				LastCycleAt:   now.Add(-time.Minute),
				LastSuccessAt: now.Add(-2 * time.Minute),
			},
			wantOK: true,
		},
		{
			name: "missing failed cycle",
			health: syncHealth{
				Phase:         syncHealthFailed,
				LastSuccessAt: now.Add(-time.Minute),
			},
		},
		{
			name: "success after failed cycle",
			health: syncHealth{
				Phase:         syncHealthFailed,
				LastCycleAt:   now.Add(-2 * time.Minute),
				LastSuccessAt: now.Add(-time.Minute),
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := writeSyncHealthState(stateFile, test.health); err != nil {
				t.Fatal(err)
			}
			err := checkSyncHealth(stateFile, 5*time.Minute, now)
			if (err == nil) != test.wantOK {
				t.Fatalf("checkSyncHealth() error = %v, wantOK = %t", err, test.wantOK)
			}
		})
	}
}

func TestSyncHealthRejectsStaleCycle(t *testing.T) {
	stateFile := filepath.Join(t.TempDir(), "state.json")
	now := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	if err := writeSyncHealth(stateFile, now.Add(-16*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := checkSyncHealth(stateFile, 5*time.Minute, now); err == nil {
		t.Fatal("stale cycle should be unhealthy")
	}
}

func TestSyncHealthRejectsCycleAtAgeLimit(t *testing.T) {
	stateFile := filepath.Join(t.TempDir(), "state.json")
	now := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	if err := writeSyncHealth(stateFile, now.Add(-15*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := checkSyncHealth(stateFile, 5*time.Minute, now); err == nil {
		t.Fatal("cycle at the age limit should be unhealthy")
	}
}

func TestSyncHealthRejectsFarFutureCycle(t *testing.T) {
	stateFile := filepath.Join(t.TempDir(), "state.json")
	now := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	if err := writeSyncHealth(stateFile, now.Add(6*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := checkSyncHealth(stateFile, 5*time.Minute, now); err == nil {
		t.Fatal("far-future cycle should be unhealthy")
	}
}

func TestSyncHealthRejectsMalformedAndMissingState(t *testing.T) {
	stateFile := filepath.Join(t.TempDir(), "state.json")
	if err := checkSyncHealth(stateFile, 5*time.Minute, time.Now()); err == nil {
		t.Fatal("missing health state should be unhealthy")
	}
	path := syncHealthPath(stateFile)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("not-json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := checkSyncHealth(stateFile, 5*time.Minute, time.Now()); err == nil {
		t.Fatal("malformed health state should be unhealthy")
	}
}

func TestInvalidateSyncHealthRemovesPreviousMarker(t *testing.T) {
	stateFile := filepath.Join(t.TempDir(), "state.json")
	if err := writeSyncHealth(stateFile, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := invalidateSyncHealth(stateFile); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(syncHealthPath(stateFile)); !os.IsNotExist(err) {
		t.Fatalf("health marker still exists, stat error=%v", err)
	}
	if err := invalidateSyncHealth(stateFile); err != nil {
		t.Fatalf("invalidating missing marker should be idempotent: %v", err)
	}
}

func TestSyncReportHealthEvidence(t *testing.T) {
	if !newSyncReport("group", nil).healthy() {
		t.Fatal("successful empty discovery should keep an idle cycle healthy")
	}
	channel := testChannel("https://upstream.example", 0.1)
	for _, status := range []string{reportStatusChecked, reportStatusStable, reportStatusPreview, reportStatusUpdated} {
		report := newSyncReport("group", []Channel{channel})
		report.markGroup(channel.Group.ID, status)
		if !report.healthy() {
			t.Fatalf("successful status %q should keep a cycle healthy", status)
		}
	}
	report := newSyncReport("group", []Channel{channel})
	report.markGroup(channel.Group.ID, reportStatusSkipped)
	if report.healthy() {
		t.Fatal("all-skipped cycle health classification is incorrect")
	}
	report = newSyncReport("group", []Channel{channel})
	report.markGroup(channel.Group.ID, reportStatusFailed)
	if report.healthy() {
		t.Fatal("all-failed cycle should be unhealthy")
	}
}

func TestSyncHealthJSONIncludesUTCTimestamp(t *testing.T) {
	stateFile := filepath.Join(t.TempDir(), "state.json")
	value := time.Date(2026, 8, 27, 12, 0, 0, 0, time.FixedZone("CST", 8*60*60))
	if err := writeSyncHealth(stateFile, value); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(syncHealthPath(stateFile))
	if err != nil {
		t.Fatal(err)
	}
	var decoded syncHealth
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	if !decoded.LastSuccessAt.Equal(value.UTC()) {
		t.Fatalf("timestamp = %s, want %s", decoded.LastSuccessAt, value.UTC())
	}
}
