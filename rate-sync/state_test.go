package main

import (
	"encoding/json"
	"os"
	"path/filepath"
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

func TestStateStorePersistsDynamicGroupMemory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	state := newState()
	state.DynamicGroups[24] = &DynamicGroupState{
		Initialized: true,
		LastUsageID: 321,
		Fast: DynamicCostMemory{
			Denominator: 5,
			AccountBase: map[int64]float64{10: 3, 11: 2},
		},
		Slow: DynamicCostMemory{
			Denominator: 100,
			AccountBase: map[int64]float64{10: 60, 11: 40},
		},
		LastAccountRates: map[int64]float64{10: 0.1, 11: 0.2},
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
	if group == nil || group.LastUsageID != 321 || group.Fast.AccountBase[10] != 3 || group.Slow.Denominator != 100 || group.LastAccountRates[11] != 0.2 {
		encoded, _ := json.Marshal(loaded)
		t.Fatalf("dynamic state did not round-trip: %s", encoded)
	}
}
