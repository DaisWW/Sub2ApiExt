package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStateStoreDropsLegacyInferredPriceKeys(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	legacy := `{
  "version": 2,
  "rules": {
    "account:2/group:3": {
      "identity": "legacy",
      "template": "newapi_pricing",
      "price_key": "cc-max",
      "candidate_upstream_rate": 1,
      "candidate_count": 2
    }
  }
}`
	if err := os.WriteFile(path, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}

	state, err := (StateStore{Path: path}).Load()
	if err != nil {
		t.Fatal(err)
	}
	rule := state.Rules["account:2/group:3"]
	if state.Version != currentStateVersion || rule == nil {
		t.Fatalf("legacy state was not reset: %+v", state)
	}
	if rule.PriceKey != "" || rule.CandidateUpstreamRate != 0 || rule.CandidateCount != 0 {
		t.Fatalf("unsafe inferred values were preserved: %+v", rule)
	}
}

func TestStateStoreMigratesVersion3WithoutDroppingRules(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	legacy := `{
  "version": 3,
  "rules": {
    "account:2": {
      "identity": "keep-me",
      "template": "sub2api_usage",
      "candidate_upstream_rate": 0.1,
      "candidate_count": 1
    }
  }
}`
	if err := os.WriteFile(path, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}

	state, err := (StateStore{Path: path}).Load()
	if err != nil {
		t.Fatal(err)
	}
	rule := state.Rules["account:2"]
	if state.Version != currentStateVersion || rule == nil || rule.Identity != "keep-me" || rule.CandidateCount != 1 {
		t.Fatalf("version 3 state was not migrated safely: %+v", state)
	}
	if state.DynamicGroups == nil {
		t.Fatal("dynamic group map was not initialized")
	}
}

func TestStateStoreMigratesVersion4AndRebuildsDynamicMemory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	legacy := `{
  "version": 4,
  "rules": {"account:2": {"identity": "keep-me"}},
  "dynamic_groups": {"24": {"initialized": true, "last_usage_id": 321}}
}`
	if err := os.WriteFile(path, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}

	state, err := (StateStore{Path: path}).Load()
	if err != nil {
		t.Fatal(err)
	}
	if state.Version != currentStateVersion || state.Rules["account:2"] == nil {
		t.Fatalf("version 4 state did not preserve account rules: %+v", state)
	}
	if len(state.DynamicGroups) != 0 {
		t.Fatalf("legacy dynamic memory should be rebuilt: %+v", state.DynamicGroups)
	}
}

func TestStateStoreRejectsUnsupportedVersion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, []byte(`{"version":999,"rules":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := (StateStore{Path: path}).Load(); err == nil {
		t.Fatal("unsupported state version should stop startup")
	}
}

func TestStateStoreRejectsNullEntries(t *testing.T) {
	tests := []struct {
		name string
		data string
		want string
	}{
		{
			name: "rule",
			data: `{"version":2,"rules":{"account:2":null}}`,
			want: "rules",
		},
		{
			name: "dynamic group",
			data: `{"version":5,"rules":{},"dynamic_groups":{"24":null}}`,
			want: "dynamic_groups",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "state.json")
			if err := os.WriteFile(path, []byte(tt.data), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := (StateStore{Path: path}).Load()
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Load() error = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestStateStorePersistsDynamicGroupMemory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	state := newState()
	state.DynamicGroups[24] = &DynamicGroupState{
		Initialized: true,
		LastUsageID: 321,
		Fast: DynamicCostMemory{
			Denominator: 5,
			AccountBase: map[int64]float64{10: 3, 11: 2},
			AccountCost: 0.7,
		},
		Slow: DynamicCostMemory{
			Denominator: 100,
			AccountBase: map[int64]float64{10: 60, 11: 40},
			AccountCost: 14,
		},
		LastAccountRates: map[int64]float64{10: 0.1, 11: 0.2},
		PendingTarget:    0.1234,
		HasPendingTarget: true,
	}
	store := StateStore{Path: path}
	if err := store.Save(state); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	group := loaded.DynamicGroups[24]
	if group == nil || group.LastUsageID != 321 || group.Fast.AccountBase[10] != 3 || group.Fast.AccountCost != 0.7 || group.Slow.Denominator != 100 || group.LastAccountRates[11] != 0.2 ||
		group.PendingTarget != 0.1234 || !group.HasPendingTarget {
		encoded, _ := json.Marshal(loaded)
		t.Fatalf("dynamic state did not round-trip: %s", encoded)
	}
}
