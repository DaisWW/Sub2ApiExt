package store

import (
	"strings"
	"testing"
	"time"

	"github.com/DaisWW/Sub2ApiExt/monitoring/internal/config"
	"github.com/DaisWW/Sub2ApiExt/monitoring/internal/model"
)

func TestCostAlertQueryUsesRequestMetadataOnly(t *testing.T) {
	for _, fragment := range []string{
		"ul.user_id",
		"ul.api_key_id",
		"ul.model",
		"ul.channel_id",
		"ul.actual_cost > 0",
		"ul.input_tokens",
		"ul.cache_read_tokens",
		"base_cost >= $5",
		"GROUP BY user_key, model, api_key_id",
	} {
		if !strings.Contains(costUsageQuery, fragment) {
			t.Fatalf("cost query missing %q", fragment)
		}
	}
	for _, forbidden := range []string{"prompt", "request_body", "response_body", "authorization"} {
		if strings.Contains(strings.ToLower(costUsageQuery), forbidden) {
			t.Fatalf("cost query must not read request payload %q", forbidden)
		}
	}
}

func TestCostDailyQueryDoesNotProjectAcrossPreviousDay(t *testing.T) {
	for _, fragment := range []string{
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
		Enabled:          true,
		Window:           15 * time.Minute,
		Baseline:         7 * 24 * time.Hour,
		Cooldown:         30 * time.Minute,
		MinRequests:      3,
		MinTokens:        100_000,
		MinCost:          0.5,
		MinBaseCost:      0.1,
		CacheBaselineMin: 0.60,
		CacheCurrentMax:  0.20,
		CacheCostRatio:   1.5,
		UnitCostRatio:    1.5,
		MultiplierRatio:  2,
		BurnRatio:        1.5,
	}
}

func TestEvaluateCostUsageGroupDetectsCacheAndUnitCostAnomalies(t *testing.T) {
	policy := testCostAlertPolicy()
	group := costUsageGroup{
		userKey: "42", model: "gpt-4.1", channelID: 7, channelName: "渠道 A", accountID: 9, accountName: "owner@example.com",
		current: costUsageMetrics{
			Requests: 3, InputTokens: 300_000, CacheReadTokens: 20_000,
			ActualCost: 4.5,
		},
		baseline: costUsageMetrics{
			Requests: 20, InputTokens: 100_000, CacheReadTokens: 300_000,
			ActualCost: 2,
		},
	}
	events := evaluateCostUsageGroup(group, policy, time.Unix(100, 0), time.Unix(200, 0))
	if len(events) != 2 {
		t.Fatalf("got %d events: %+v", len(events), events)
	}
	seen := map[string]bool{}
	for _, event := range events {
		seen[event.Kind] = true
		if event.TargetKey == "" || event.AlertKey == "" {
			t.Fatalf("event keys are empty: %+v", event)
		}
		if event.AccountID != 9 || event.AccountName != "owner@example.com" {
			t.Fatalf("event account = %q #%d", event.AccountName, event.AccountID)
		}
	}
	if !seen[model.CostAlertCacheDegraded] || !seen[model.CostAlertUnitCost] {
		t.Fatalf("unexpected event kinds: %+v", seen)
	}
}

func TestCostUsageMetricsCacheHitRateIncludesCacheCreationTokens(t *testing.T) {
	metrics := costUsageMetrics{
		InputTokens:         100,
		CacheCreationTokens: 300,
		CacheReadTokens:     100,
	}
	if got := metrics.CacheHitRate(); got != 20 {
		t.Fatalf("cache hit rate = %v, want 20", got)
	}
}

func TestEvaluateCostUsageGroupDetectsMultiplierSpike(t *testing.T) {
	policy := testCostAlertPolicy()
	group := costUsageGroup{
		userKey: "42", model: "gpt-4.1", channelID: 7, accountID: 9,
		current:                costUsageMetrics{Requests: 3, InputTokens: 150_000, ActualCost: 4, BaseCost: 2},
		baseline:               costUsageMetrics{Requests: 10, InputTokens: 150_000, ActualCost: 4, BaseCost: 4},
		maxMultiplier:          2,
		highMultiplierRequests: 2,
	}
	events := evaluateCostUsageGroup(group, policy, time.Unix(100, 0), time.Unix(200, 0))
	if len(events) != 1 || events[0].Kind != model.CostAlertMultiplier {
		t.Fatalf("unexpected multiplier events: %+v", events)
	}
	if events[0].Severity != "critical" {
		t.Fatalf("multiplier severity = %q", events[0].Severity)
	}
}

func TestEvaluateCostUsageGroupCanDetectMultiplierWithoutHistory(t *testing.T) {
	policy := testCostAlertPolicy()
	group := costUsageGroup{
		userKey: "42", model: "gpt-4.1", channelID: 7, accountID: 9,
		current:       costUsageMetrics{Requests: 3, InputTokens: 150_000, ActualCost: 4, BaseCost: 2},
		maxMultiplier: 2, highMultiplierRequests: 3,
	}
	events := evaluateCostUsageGroup(group, policy, time.Unix(100, 0), time.Unix(200, 0))
	if len(events) != 1 || events[0].Kind != model.CostAlertMultiplier {
		t.Fatalf("unexpected no-history multiplier events: %+v", events)
	}
}

func TestEvaluateCostUsageGroupDetectsSingleMultiplierSpikeBeforeWindowMinimum(t *testing.T) {
	policy := testCostAlertPolicy()
	group := costUsageGroup{
		userKey: "42", model: "gpt-4.1", channelID: 7, accountID: 9,
		current: costUsageMetrics{Requests: 1, InputTokens: 1_000, ActualCost: 0.4, BaseCost: 0.2},
	}
	events := evaluateCostUsageGroup(group, policy, time.Unix(100, 0), time.Unix(200, 0))
	if len(events) != 1 || events[0].Kind != model.CostAlertMultiplier {
		t.Fatalf("unexpected single multiplier events: %+v", events)
	}
}

func TestEvaluateCostUsageGroupNeedsMinimumEvidence(t *testing.T) {
	policy := testCostAlertPolicy()
	group := costUsageGroup{
		userKey: "42", model: "gpt-4.1",
		current:  costUsageMetrics{Requests: 1, InputTokens: 10_000, ActualCost: 10},
		baseline: costUsageMetrics{Requests: 20, InputTokens: 300_000, CacheReadTokens: 300_000, ActualCost: 2},
	}
	if events := evaluateCostUsageGroup(group, policy, time.Unix(100, 0), time.Unix(200, 0)); len(events) != 0 {
		t.Fatalf("low evidence produced events: %+v", events)
	}
}

func TestEvaluateCostUsageGroupDetectsSingleExpensiveRequest(t *testing.T) {
	policy := testCostAlertPolicy()
	policy.SingleRequestCost = 5
	group := costUsageGroup{
		userKey: "42", model: "gpt-4.1", channelID: 7, accountID: 9,
		current:        costUsageMetrics{Requests: 1, InputTokens: 10_000, ActualCost: 6},
		maxRequestCost: 6,
	}
	events := evaluateCostUsageGroup(group, policy, time.Unix(100, 0), time.Unix(200, 0))
	if len(events) != 1 || events[0].Kind != model.CostAlertSingleRequest {
		t.Fatalf("unexpected single request events: %+v", events)
	}
}

func TestEvaluateBudgetBurnUsesProjectedDailyCost(t *testing.T) {
	policy := testCostAlertPolicy()
	policy.DailyBudget = 10
	now := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	usage := map[string]costDailyUsage{
		"42": {
			userKey: "42", dayStart: now.Add(-12 * time.Hour), windowRequests: 3,
			windowTokens: 100_000, windowCost: 1, dailyCost: 5,
		},
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
	usage := map[string]costDailyUsage{
		"42": {userKey: "42", dayStart: time.Now().UTC(), windowCost: 100, dailyCost: 100},
	}
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

func TestEvaluateBudgetBurnKeepsMultipleAPIKeysSeparate(t *testing.T) {
	policy := testCostAlertPolicy()
	policy.DailyBudget = 10
	now := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	item := costDailyUsage{userKey: "42", apiKeyID: 7, dayStart: now.Add(-12 * time.Hour), windowCost: 1, dailyCost: 5}
	itemForSecondKey := item
	itemForSecondKey.apiKeyID = 8
	usage := map[string]costDailyUsage{
		costUserTargetKey("42", 7): item,
		costUserTargetKey("42", 8): itemForSecondKey,
	}
	events := evaluateBudgetBurn(usage, policy, now)
	if len(events) != 2 {
		t.Fatalf("got %d budget events, want one per API key: %+v", len(events), events)
	}
	if events[0].TargetKey == events[1].TargetKey {
		t.Fatalf("API key budget events share target: %+v", events)
	}
}

func TestEvaluateBudgetBurnReportsAlreadyExceededDailyBudgetWithoutRecentTraffic(t *testing.T) {
	policy := testCostAlertPolicy()
	policy.DailyBudget = 10
	now := time.Date(2026, 9, 22, 23, 0, 0, 0, time.UTC)
	usage := map[string]costDailyUsage{
		costUserTargetKey("42", 7): {
			userKey: "42", apiKeyID: 7, dayStart: now.Add(-23 * time.Hour), dailyCost: 11,
		},
	}
	events := evaluateBudgetBurn(usage, policy, now)
	if len(events) != 1 || events[0].Severity != "critical" {
		t.Fatalf("exceeded budget without recent traffic = %+v", events)
	}
}
