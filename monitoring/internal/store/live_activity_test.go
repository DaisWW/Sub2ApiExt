package store

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/DaisWW/Sub2ApiExt/monitoring/internal/model"
)

func TestLiveActivityQueryUsesRecentSuccessfulUsageAndRoutingDimensions(t *testing.T) {
	for _, fragment := range []string{
		"$1::bigint * INTERVAL '1 second'",
		"$2",
		"ul.user_id",
		"ul.channel_id",
		"ul.account_id",
		"COUNT(DISTINCT user_id)",
		"JOIN accounts a",
		"LEFT JOIN groups g",
		"LEFT JOIN channels c",
		"ul.actual_cost > 0",
		"ul.group_id IS NULL OR g.id IS NOT NULL",
		"'summary'",
		"'channel'",
		"'account'",
		"'route'",
		"ROW_NUMBER() OVER",
		"PARTITION BY activity_rows.kind",
		"detail_rank <= $2",
	} {
		if !strings.Contains(liveActivityQuery, fragment) {
			t.Fatalf("live activity query missing %q", fragment)
		}
	}
	for _, fragment := range []string{"email", "api_key", "session_id"} {
		if strings.Contains(strings.ToLower(liveActivityQuery), fragment) {
			t.Fatalf("live activity query must not read user detail %q", fragment)
		}
	}
}

func TestSortLiveActivityRanksAndLimitsDetails(t *testing.T) {
	activity := model.LiveActivity{
		Channels: make([]model.LiveActivityChannel, liveActivityDetailLimit+1),
		Accounts: make([]model.LiveActivityAccount, liveActivityDetailLimit+1),
		Routes:   make([]model.LiveActivityRoute, liveActivityDetailLimit+1),
	}
	for index := range activity.Channels {
		activity.Channels[index] = model.LiveActivityChannel{
			Name:        string(rune('a' + index%26)),
			ActiveUsers: int64(index),
			Requests:    int64(index),
		}
		activity.Accounts[index] = model.LiveActivityAccount{
			Name:        string(rune('a' + index%26)),
			ActiveUsers: int64(index),
			Requests:    int64(index),
		}
		activity.Routes[index] = model.LiveActivityRoute{
			ChannelName: string(rune('a' + index%26)),
			AccountName: string(rune('a' + index%26)),
			ActiveUsers: int64(index),
			Requests:    int64(index),
		}
	}

	sortLiveActivity(&activity)
	if len(activity.Channels) != liveActivityDetailLimit || len(activity.Accounts) != liveActivityDetailLimit || len(activity.Routes) != liveActivityDetailLimit {
		t.Fatalf("activity detail lengths = %d/%d/%d, want %d", len(activity.Channels), len(activity.Accounts), len(activity.Routes), liveActivityDetailLimit)
	}
	if activity.Channels[0].ActiveUsers != liveActivityDetailLimit || activity.Accounts[0].ActiveUsers != liveActivityDetailLimit || activity.Routes[0].ActiveUsers != liveActivityDetailLimit {
		t.Fatalf("activity details were not ranked by active users: %+v", activity)
	}
}

func TestLiveActivityJSONDoesNotExposeUserDetails(t *testing.T) {
	encoded, err := json.Marshal(model.LiveActivity{
		Summary:  model.LiveActivitySummary{ActiveUsers: 2, Requests: 3},
		Channels: []model.LiveActivityChannel{{Name: "渠道甲", ActiveUsers: 2}},
		Accounts: []model.LiveActivityAccount{{Name: "账户甲", ActiveUsers: 2}},
		Routes:   []model.LiveActivityRoute{{ChannelName: "渠道甲", AccountName: "账户甲", ActiveUsers: 2}},
	})
	if err != nil {
		t.Fatal(err)
	}
	payload := strings.ToLower(string(encoded))
	for _, key := range []string{"user_id", "api_key_id", "session_id", "email"} {
		if strings.Contains(payload, key) {
			t.Fatalf("live activity JSON exposes %q: %s", key, encoded)
		}
	}
}
