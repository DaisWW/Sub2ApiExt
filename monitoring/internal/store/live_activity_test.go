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
		"COUNT(*)::bigint AS requests",
		"JOIN accounts a",
		"a.deleted_at IS NULL",
		"LOWER(TRIM(a.status)) = 'active'",
		"a.schedulable = TRUE",
		"LEFT JOIN groups g",
		"g.deleted_at IS NULL",
		"LOWER(TRIM(g.status)) = 'active'",
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
			{TargetKey: "account:1", ActiveUsers: 2, Requests: 7, CurrentConcurrency: 4},
			{TargetKey: "group:2", ActiveUsers: 3, Requests: 11, CurrentConcurrency: 6},
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
	targets, ok := payload["targets"].([]any)
	if !ok || len(targets) != 2 {
		t.Fatalf("live activity JSON targets = %#v", payload["targets"])
	}
	for index, want := range []float64{7, 11} {
		target, ok := targets[index].(map[string]any)
		if !ok || target["requests"] != want {
			t.Fatalf("live activity JSON target %d requests = %#v, want %v", index, target["requests"], want)
		}
		if target["current_concurrency"] != []float64{4, 6}[index] {
			t.Fatalf("live activity JSON target %d current_concurrency = %#v", index, target["current_concurrency"])
		}
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

func TestTargetEntityID(t *testing.T) {
	if id, ok := targetEntityID("account:42", "account"); !ok || id != 42 {
		t.Fatalf("account target = %d, %v", id, ok)
	}
	for _, value := range []string{"group:42", "account:0", "account:-1", "account:x"} {
		if _, ok := targetEntityID(value, "account"); ok {
			t.Fatalf("targetEntityID(%q) unexpectedly succeeded", value)
		}
	}
}
