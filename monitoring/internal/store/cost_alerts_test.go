package store

import (
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/DaisWW/Sub2ApiExt/monitoring/internal/config"
	"github.com/DaisWW/Sub2ApiExt/monitoring/internal/model"
)

func TestCostAlertQueryUsesRequestMetadataOnly(t *testing.T) {
	for _, fragment := range []string{
		"ul.user_id",
		"u.username",
		"u.email",
		"ul.api_key_id",
		"k.name",
		"ul.model",
		"ul.channel_id",
		"ul.actual_cost > 0",
		"ul.input_tokens",
		"ul.cache_read_tokens",
		"GROUP BY user_key, model, api_key_id",
	} {
		if !strings.Contains(costUsageQuery, fragment) {
			t.Fatalf("cost query missing %q", fragment)
		}
	}
	for _, forbidden := range []string{"prompt", "request_body", "response_body", "authorization", "current_max_multiplier", "current_high_multiplier_requests"} {
		if strings.Contains(strings.ToLower(costUsageQuery), forbidden) {
			t.Fatalf("cost query must not read %q", forbidden)
		}
	}
}

func TestCostDailyQueryDoesNotProjectAcrossPreviousDay(t *testing.T) {
	for _, fragment := range []string{
		"u.username",
		"u.email",
		"k.name",
		"LEFT JOIN users u ON u.id = ul.user_id",
		"LEFT JOIN api_keys k ON k.id = ul.api_key_id",
		"GREATEST(bounds.window_start, bounds.day_start)",
		"$3::timestamptz AS day_start",
		"SUM(usage.actual_cost)",
		"GROUP BY usage.user_key, usage.api_key_id",
	} {
		if !strings.Contains(costDailyQuery, fragment) {
			t.Fatalf("daily cost query missing %q", fragment)
		}
	}
}

func testCostAlertPolicy() config.CostAlertConfig {
	return config.CostAlertConfig{
		Enabled:      true,
		Window:       15 * time.Minute,
		Baseline:     7 * 24 * time.Hour,
		Cooldown:     30 * time.Minute,
		MinRequests:  3,
		MinTokens:    100_000,
		TotalMinCost: 5,
		BurnRatio:    1.5,
	}
}

func TestAggregateCostUsageGroupsByUserAndAPIKey(t *testing.T) {
	groups := []costUsageGroup{
		{
			userKey: "42", apiKeyID: 7, model: "gpt-4.1", channelID: 1,
			current:  costUsageMetrics{Requests: 2, InputTokens: 60_000, ActualCost: 3},
			baseline: costUsageMetrics{Requests: 10, InputTokens: 100_000, ActualCost: 4},
		},
		{
			userKey: "42", apiKeyID: 7, model: "claude", channelID: 2,
			current:  costUsageMetrics{Requests: 2, OutputTokens: 60_000, ActualCost: 3},
			baseline: costUsageMetrics{Requests: 8, OutputTokens: 100_000, ActualCost: 5},
		},
	}
	aggregated := aggregateCostUsageGroups(groups)
	if len(aggregated) != 1 {
		t.Fatalf("aggregated groups = %d, want 1", len(aggregated))
	}
	group := aggregated[0]
	if group.current.Requests != 4 || group.current.TotalTokens() != 120_000 || group.current.ActualCost != 6 {
		t.Fatalf("unexpected current aggregate: %+v", group.current)
	}
	if group.baseline.Requests != 18 || group.baseline.TotalTokens() != 200_000 || group.baseline.ActualCost != 9 {
		t.Fatalf("unexpected baseline aggregate: %+v", group.baseline)
	}
	if group.model != "" || group.channelID != 0 {
		t.Fatalf("aggregate retained a detail dimension: %+v", group)
	}
}

func TestBuildCostAlertCandidatesUsesTotalCostAcrossDetails(t *testing.T) {
	policy := testCostAlertPolicy()
	bounds := costAlertBounds{now: time.Unix(200, 0), currentStart: time.Unix(100, 0)}
	groups := []costUsageGroup{
		{
			userKey: "42", userName: "Owner", apiKeyID: 7,
			model: "gpt-4.1", channelID: 1,
			current: costUsageMetrics{Requests: 2, InputTokens: 60_000, ActualCost: 3},
		},
		{
			userKey: "42", userName: "Owner", apiKeyID: 7,
			model: "claude", channelID: 2,
			current: costUsageMetrics{Requests: 2, OutputTokens: 60_000, ActualCost: 3},
		},
	}
	events := buildCostAlertCandidates(groups, nil, policy, bounds)
	if len(events) != 1 || events[0].Kind != model.CostAlertTotalCost {
		t.Fatalf("unexpected total-cost events: %+v", events)
	}
	if events[0].TargetKey != "user:42|key:7" || events[0].CurrentCost != 6 {
		t.Fatalf("unexpected total-cost event: %+v", events[0])
	}
	if !strings.Contains(events[0].Message, "Owner") {
		t.Fatalf("total-cost message is missing resolved identity: %q", events[0].Message)
	}
}

func TestBuildCostAlertCandidatesIgnoresLowCostOrInsufficientEvidence(t *testing.T) {
	policy := testCostAlertPolicy()
	bounds := costAlertBounds{now: time.Unix(200, 0), currentStart: time.Unix(100, 0)}
	for name, current := range map[string]costUsageMetrics{
		"low cost":     {Requests: 3, InputTokens: 100_000, ActualCost: 4.99},
		"few requests": {Requests: 2, InputTokens: 100_000, ActualCost: 6},
		"few tokens":   {Requests: 3, InputTokens: 99_999, ActualCost: 6},
	} {
		t.Run(name, func(t *testing.T) {
			groups := []costUsageGroup{{userKey: "42", apiKeyID: 7, current: current}}
			if events := buildCostAlertCandidates(groups, nil, policy, bounds); len(events) != 0 {
				t.Fatalf("unexpected low-evidence event: %+v", events)
			}
		})
	}
}

func TestCostUsageMetricsCacheHitRateIncludesCacheCreationTokens(t *testing.T) {
	metrics := costUsageMetrics{InputTokens: 100, CacheCreationTokens: 300, CacheReadTokens: 100}
	if got := metrics.CacheHitRate(); got != 20 {
		t.Fatalf("cache hit rate = %v, want 20", got)
	}
}

func TestEvaluateBudgetBurnUsesProjectedDailyCost(t *testing.T) {
	policy := testCostAlertPolicy()
	policy.DailyBudget = 10
	now := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	usage := map[string]costDailyUsage{
		"42": {userKey: "42", dayStart: now.Add(-12 * time.Hour), windowRequests: 3, windowTokens: 100_000, windowCost: 1, dailyCost: 5},
	}
	events := evaluateBudgetBurn(usage, policy, now)
	if len(events) != 1 || events[0].Kind != model.CostAlertBudgetBurn {
		t.Fatalf("unexpected budget events: %+v", events)
	}
	if events[0].ProjectedCost <= events[0].DailyCost {
		t.Fatalf("projected cost = %v, daily = %v", events[0].ProjectedCost, events[0].DailyCost)
	}
}

func TestEvaluateBudgetBurnDisabledWithoutBudget(t *testing.T) {
	policy := testCostAlertPolicy()
	usage := map[string]costDailyUsage{"42": {userKey: "42", dayStart: time.Now().UTC(), windowCost: 100, dailyCost: 100}}
	if events := evaluateBudgetBurn(usage, policy, time.Now().UTC()); len(events) != 0 {
		t.Fatalf("disabled budget produced events: %+v", events)
	}
}

func TestCostDailyUsageKeyIncludesAPIKey(t *testing.T) {
	if got := costUserTargetKey("42", 7); got != "user:42|key:7" {
		t.Fatalf("cost daily usage key = %q", got)
	}
	if got := costUserTargetKey("42", 8); got == costUserTargetKey("42", 7) {
		t.Fatal("different API keys must not share a daily usage key")
	}
}

func TestCostAlertStateAllowedInAggregateModeRetiresLegacyKinds(t *testing.T) {
	if costAlertStateAllowedInAggregateMode(costAlertState{lastEvent: model.CostAlertEvent{Kind: "cache_degraded"}}) {
		t.Fatal("legacy detail state must not remain active")
	}
	for _, kind := range []string{model.CostAlertTotalCost, model.CostAlertBudgetBurn} {
		if !costAlertStateAllowedInAggregateMode(costAlertState{lastEvent: model.CostAlertEvent{Kind: kind}}) {
			t.Fatalf("aggregate state kind %q was retired", kind)
		}
	}
}

func TestEvaluateBudgetBurnKeepsMultipleAPIKeysSeparate(t *testing.T) {
	policy := testCostAlertPolicy()
	policy.DailyBudget = 10
	now := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	item := costDailyUsage{userKey: "42", apiKeyID: 7, dayStart: now.Add(-12 * time.Hour), windowCost: 1, dailyCost: 5}
	itemForSecondKey := item
	itemForSecondKey.apiKeyID = 8
	usage := map[string]costDailyUsage{costUserTargetKey("42", 7): item, costUserTargetKey("42", 8): itemForSecondKey}
	events := evaluateBudgetBurn(usage, policy, now)
	if len(events) != 2 {
		t.Fatalf("got %d budget events, want one per API key: %+v", len(events), events)
	}
	if events[0].TargetKey == events[1].TargetKey {
		t.Fatalf("API key budget events share target: %+v", events)
	}
}

func TestEvaluateBudgetBurnUsesRawUserKeyWhenUsageMapIsTargetKeyed(t *testing.T) {
	policy := testCostAlertPolicy()
	policy.DailyBudget = 10
	now := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	usage := map[string]costDailyUsage{
		costUserTargetKey("42", 7): {
			userKey: "42", userName: "Owner", apiKeyID: 7, apiKeyName: "Codex",
			dayStart: now.Add(-12 * time.Hour), windowCost: 1, dailyCost: 5,
		},
	}
	events := evaluateBudgetBurn(usage, policy, now)
	if len(events) != 1 {
		t.Fatalf("unexpected budget events: %+v", events)
	}
	if events[0].UserKey != "42" || events[0].TargetKey != "user:42|key:7" {
		t.Fatalf("budget event used map key as user identity: %+v", events[0])
	}
	if !strings.Contains(events[0].Message, "Owner #42") {
		t.Fatalf("budget message is missing resolved user identity: %q", events[0].Message)
	}
}

func TestEvaluateBudgetBurnReportsAlreadyExceededDailyBudgetWithoutRecentTraffic(t *testing.T) {
	policy := testCostAlertPolicy()
	policy.DailyBudget = 10
	now := time.Date(2026, 9, 22, 23, 0, 0, 0, time.UTC)
	usage := map[string]costDailyUsage{costUserTargetKey("42", 7): {
		userKey: "42", apiKeyID: 7, dayStart: now.Add(-23 * time.Hour), dailyCost: 11,
	}}
	events := evaluateBudgetBurn(usage, policy, now)
	if len(events) != 1 || events[0].Severity != "critical" {
		t.Fatalf("exceeded budget without recent traffic = %+v", events)
	}
}

func TestObserveCostAlertStoresIncidentStartAndAlertKey(t *testing.T) {
	now := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	candidate := model.CostAlertEvent{AlertKey: "total_cost_high|target", Severity: "warning"}
	state, event, notify := observeCostAlert(costAlertState{}, false, candidate, now, 30*time.Minute)
	if !notify || event.NotificationType != model.CostAlertNotificationStart {
		t.Fatalf("start notification = %+v, notify=%v", event, notify)
	}
	if state.alertKey != candidate.AlertKey || event.IncidentStartedAt != now || state.lastEvent.IncidentStartedAt != now {
		t.Fatalf("incident snapshot is inconsistent: state=%+v event=%+v", state, event)
	}
}

func TestPrepareCostAlertRecoveryHonorsCooldownAndConfiguredWindow(t *testing.T) {
	policy := testCostAlertPolicy()
	now := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	state := costAlertState{
		alertKey: "total_cost_high|target", active: true,
		firstSeenAt: timePointer(now.Add(-time.Hour)), normalSinceAt: timePointer(now.Add(-31 * time.Minute)),
		lastAlertedAt: sql.NullTime{Time: now.Add(-10 * time.Minute), Valid: true},
		lastEvent:     model.CostAlertEvent{AlertKey: "", UserKey: "42"},
	}
	unchanged, _, notify, changed := prepareCostAlertRecovery(state, now, policy)
	if notify || changed || unchanged.pendingRecovery {
		t.Fatalf("recovery ignored cooldown: state=%+v notify=%v changed=%v", unchanged, notify, changed)
	}

	recoveryState, recovery, notify, changed := prepareCostAlertRecovery(state, now.Add(20*time.Minute), policy)
	if !notify || !changed || !recoveryState.pendingRecovery {
		t.Fatalf("recovery was not scheduled after cooldown: state=%+v event=%+v", recoveryState, recovery)
	}
	if recovery.AlertKey != state.alertKey {
		t.Fatalf("recovery alert key = %q, want %q", recovery.AlertKey, state.alertKey)
	}
	if want := now.Add(20 * time.Minute).Add(-policy.Window); !recovery.WindowStart.Equal(want) {
		t.Fatalf("recovery window start = %v, want %v", recovery.WindowStart, want)
	}
}
