package store

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/DaisWW/Sub2ApiExt/monitoring/internal/model"
)

func TestLiveActivityQueryUsesRecentPerTargetCounts(t *testing.T) {
	for _, fragment := range []string{
		"$1::bigint * INTERVAL '1 second'",
		"ul.user_id",
		"ul.account_id",
		"ul.group_id",
		"COUNT(DISTINCT activity.user_id)",
		"JOIN accounts a",
		"LEFT JOIN groups g",
		"ul.actual_cost > 0",
		"active_targets",
		"'account:'",
		"'group:'",
		"monitoring_targets",
	} {
		if !strings.Contains(liveActivityQuery, fragment) {
			t.Fatalf("live activity query missing %q", fragment)
		}
	}
	for _, fragment := range []string{
		"channel_id",
		"api_key",
		"session_id",
		"email",
	} {
		if strings.Contains(strings.ToLower(liveActivityQuery), fragment) {
			t.Fatalf("live activity query must not read detail %q", fragment)
		}
	}
}

func TestLiveActivityJSONContainsTargetCountsOnly(t *testing.T) {
	encoded, err := json.Marshal(model.LiveActivity{
		WindowSeconds: 300,
		Targets: []model.LiveActivityTarget{
			{TargetKey: "account:1", ActiveUsers: 2},
			{TargetKey: "group:2", ActiveUsers: 3},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(encoded, &payload); err != nil {
		t.Fatal(err)
	}
	if _, ok := payload["targets"]; !ok {
		t.Fatalf("live activity JSON missing targets: %s", encoded)
	}
	for _, key := range []string{"summary", "channels", "accounts", "routes"} {
		if _, exists := payload[key]; exists {
			t.Fatalf("live activity JSON should not expose %q: %s", key, encoded)
		}
	}
	for _, key := range []string{"user_id", "api_key_id", "session_id", "email"} {
		if strings.Contains(strings.ToLower(string(encoded)), key) {
			t.Fatalf("live activity JSON exposes %q: %s", key, encoded)
		}
	}
}
