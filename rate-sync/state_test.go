package main

import (
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
