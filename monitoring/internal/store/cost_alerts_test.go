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
		"LEFT JOIN users u ON u.id = ul.user_id",
		"LEFT JOIN api_keys k ON k.id = ul.api_key_id",
		"u.username",
		"u.email",
		"k.name",
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
	for _, fragment := range []string{
		"LAG(account_id)",
		"previous_account_name",
		"transition_count",
		"PARTITION BY user_key, api_key_id, model, channel_id, group_id, session_key",
		"transition_count >= $3",
	} {
		if !strings.Contains(costAlertAccountSwitchQuery, fragment) {
			t.Fatalf("account switch query missing %q", fragment)
		}
	}
	for _, forbidden := range []string{"prompt", "request_body", "response_body", "authorization", "ul.request_id"} {
		if strings.Contains(strings.ToLower(costAlertAccountSwitchQuery), forbidden) {
			t.Fatalf("account switch query must not read %q", forbidden)
		}
	}
}

func TestCostAlertRequestQueryReturnsBoundedMetadataSamples(t *testing.T) {
	for _, fragment := range []string{
		"ul.id AS usage_log_id",
		"ul.created_at",
		"ul.duration_ms",
		"ul.first_token_ms",
		"ROW_NUMBER() OVER",
		"PARTITION BY user_key, model, api_key_id, channel_id, account_id",
		"PARTITION BY user_key, api_key_id",
		"WHERE group_rank <= $3 OR user_rank <= $3",
	} {
		if !strings.Contains(costAlertRequestQuery, fragment) {
			t.Fatalf("request sample query missing %q", fragment)
		}
	}
	for _, forbidden := range []string{"prompt", "request_body", "response_body", "authorization", "ul.request_id"} {
		if strings.Contains(strings.ToLower(costAlertRequestQuery), forbidden) {
			t.Fatalf("request sample query must not read %q", forbidden)
		}
	}
}

func TestCostAlertCacheMissQueryUsesPerRequestThresholdAndSessionGrouping(t *testing.T) {
	for _, fragment := range []string{
		"ul.session_id",
		"input_tokens >= $3",
		"cache_hit_rate < $4",
		"affected_sessions",
		"COUNT(DISTINCT raw.account_id)",
		"session_account_count",
		"COALESCE(ul.group_id, 0)::bigint AS group_id",
	} {
		if !strings.Contains(costAlertCacheMissQuery, fragment) {
			t.Fatalf("cache miss query missing %q", fragment)
		}
	}
	for _, forbidden := range []string{"prompt", "request_body", "response_body", "authorization", "ul.request_id"} {
		if strings.Contains(strings.ToLower(costAlertCacheMissQuery), forbidden) {
			t.Fatalf("cache miss query must not read %q", forbidden)
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
		Enabled:                     true,
		Window:                      15 * time.Minute,
		Baseline:                    7 * 24 * time.Hour,
		Cooldown:                    30 * time.Minute,
		MinRequests:                 3,
		MinTokens:                   100_000,
		CacheMissMinRequests:        3,
		CacheMissInputTokens:        100_000,
		CacheMissMinCost:            0.5,
		CacheMissMaxSpan:            5 * time.Minute,
		AccountSwitchMinTransitions: 2,
		MinCost:                     0.5,
		MinBaseCost:                 0.1,
		CacheBaselineMin:            0.60,
		CacheCurrentMax:             0.20,
		CacheCostRatio:              1.5,
		UnitCostRatio:               1.5,
		MultiplierRatio:             2,
		BurnRatio:                   1.5,
	}
}

func TestEvaluateCacheMissRequestsDetectsCrossAccountSession(t *testing.T) {
	policy := testCostAlertPolicy()
	start := time.Unix(100, 0)
	end := time.Unix(200, 0)
	items := []costAlertCacheMissRequest{
		{
			request: model.CostAlertRequest{
				UsageLogID: 101, CreatedAt: end.Add(-2 * time.Minute), Model: "gpt-5.6-sol",
				ChannelName: "渠道 A", AccountName: "账户 A", AccountID: 11,
				InputTokens: 180_000, CacheReadTokens: 2_000, TotalTokens: 182_000,
				BaseCost: 0.8, ActualCost: 0.2, CacheHitRate: 1.1,
			},
			userKey: "42", userName: "Owner", userEmail: "owner@example.com",
			apiKeyID: 7, apiKeyName: "Codex", channelID: 9, groupID: 53,
			sessionKey: "session-internal", sessionAccountCount: 2,
		},
		{
			request: model.CostAlertRequest{
				UsageLogID: 102, CreatedAt: end.Add(-time.Minute), Model: "gpt-5.6-sol",
				ChannelName: "渠道 A", AccountName: "账户 B", AccountID: 78,
				InputTokens: 175_000, CacheReadTokens: 4_000, TotalTokens: 179_000,
				BaseCost: 0.8, ActualCost: 0.19, CacheHitRate: 2.2,
			},
			userKey: "42", userName: "Owner", userEmail: "owner@example.com",
			apiKeyID: 7, apiKeyName: "Codex", channelID: 9, groupID: 53,
			sessionKey: "session-internal", sessionAccountCount: 2,
		},
		{
			request: model.CostAlertRequest{
				UsageLogID: 103, CreatedAt: end.Add(-30 * time.Second), Model: "gpt-5.6-sol",
				ChannelName: "渠道 A", AccountName: "账户 A", AccountID: 11,
				InputTokens: 175_000, CacheReadTokens: 4_000, TotalTokens: 179_000,
				BaseCost: 0.8, ActualCost: 0.20, CacheHitRate: 2.2,
			},
			userKey: "42", userName: "Owner", userEmail: "owner@example.com",
			apiKeyID: 7, apiKeyName: "Codex", channelID: 9, groupID: 53,
			sessionKey: "session-internal", sessionAccountCount: 2,
		},
	}
	events := evaluateCacheMissRequests(items, policy, start, end)
	if len(events) != 1 {
		t.Fatalf("got %d cache miss events: %+v", len(events), events)
	}
	event := events[0]
	if event.Kind != model.CostAlertCacheMiss || event.Requests != 3 || len(event.RequestSamples) != 3 {
		t.Fatalf("unexpected cache miss event: %+v", event)
	}
	if event.AccountID != 0 || event.AccountName != "多个账户" {
		t.Fatalf("cross-account identity = %q #%d", event.AccountName, event.AccountID)
	}
	if !strings.Contains(event.Message, "疑似账户切换导致缓存断档") {
		t.Fatalf("cross-account message = %q", event.Message)
	}
	seenLogs := map[int64]bool{}
	seenAccounts := map[int64]bool{}
	for _, sample := range event.RequestSamples {
		seenLogs[sample.UsageLogID] = true
		seenAccounts[sample.AccountID] = true
	}
	if !seenLogs[101] || !seenLogs[102] || !seenLogs[103] {
		t.Fatalf("request samples = %+v", event.RequestSamples)
	}
	if !seenAccounts[11] || !seenAccounts[78] {
		t.Fatalf("request samples do not contain both accounts: %+v", event.RequestSamples)
	}
}

func TestEvaluateCacheMissRequestsNeedsThreeConcentratedLargeLowHitRequests(t *testing.T) {
	policy := testCostAlertPolicy()
	item := costAlertCacheMissRequest{
		request: model.CostAlertRequest{
			UsageLogID: 101, InputTokens: 100_000, CacheReadTokens: 1,
			TotalTokens: 100_001, CacheHitRate: 0.001, ActualCost: 0.2,
		},
		userKey: "42", apiKeyID: 7, channelID: 9,
	}
	if events := evaluateCacheMissRequests([]costAlertCacheMissRequest{item}, policy, time.Unix(100, 0), time.Unix(200, 0)); len(events) != 0 {
		t.Fatalf("one low-hit request produced events: %+v", events)
	}
	if events := evaluateCacheMissRequests([]costAlertCacheMissRequest{item, item}, policy, time.Unix(100, 0), time.Unix(200, 0)); len(events) != 0 {
		t.Fatalf("two low-hit requests produced events: %+v", events)
	}
	if events := evaluateCacheMissRequests([]costAlertCacheMissRequest{item, item, item}, policy, time.Unix(100, 0), time.Unix(200, 0)); len(events) != 1 {
		t.Fatalf("three low-hit requests did not produce an event: %+v", events)
	}
	item.request.CacheHitRate = 30
	if events := evaluateCacheMissRequests([]costAlertCacheMissRequest{item, item, item}, policy, time.Unix(100, 0), time.Unix(200, 0)); len(events) != 0 {
		t.Fatalf("healthy requests produced events: %+v", events)
	}
}

func TestEvaluateCacheMissRequestsRequiresConcentratedRequests(t *testing.T) {
	policy := testCostAlertPolicy()
	base := costAlertCacheMissRequest{
		request: model.CostAlertRequest{
			InputTokens: 100_000, CacheReadTokens: 1, TotalTokens: 100_001,
			CacheHitRate: 0.001, ActualCost: 0.2,
		},
		userKey: "42", apiKeyID: 7, channelID: 9,
	}
	items := []costAlertCacheMissRequest{base, base, base}
	items[0].request.CreatedAt = time.Unix(100, 0)
	items[1].request.CreatedAt = time.Unix(100, 0).Add(6 * time.Minute)
	items[2].request.CreatedAt = time.Unix(100, 0).Add(7 * time.Minute)
	if events := evaluateCacheMissRequests(items, policy, time.Unix(100, 0), time.Unix(100, 0).Add(10*time.Minute)); len(events) != 0 {
		t.Fatalf("spread-out low-hit requests produced an event: %+v", events)
	}
}

func TestEvaluateAccountSwitchesRequiresRepeatedTransitions(t *testing.T) {
	policy := testCostAlertPolicy()
	makeItem := func(id int64, at time.Time, previousID, currentID int64, previousName, currentName string, transitions int64) costAlertAccountSwitch {
		return costAlertAccountSwitch{
			request: model.CostAlertRequest{
				UsageLogID: id, CreatedAt: at, Model: "gpt-5.6-sol", ChannelName: "渠道 A",
				AccountName: currentName, AccountID: currentID, PreviousAccountName: previousName, PreviousAccountID: previousID,
				InputTokens: 100_000, CacheReadTokens: 90_000, TotalTokens: 190_000, ActualCost: 0.2,
			},
			userKey: "42", userName: "Owner", userEmail: "owner@example.com", apiKeyID: 7, apiKeyName: "Codex",
			channelID: 9, groupID: 53, sessionKey: "secret-session-value", transitionCount: transitions,
		}
	}
	items := []costAlertAccountSwitch{
		makeItem(201, time.Unix(100, 0), 11, 78, "账户 A", "账户 B", 2),
		makeItem(202, time.Unix(101, 0), 78, 11, "账户 B", "账户 A", 2),
	}
	events := evaluateAccountSwitches(items, policy, time.Unix(90, 0), time.Unix(110, 0))
	if len(events) != 1 {
		t.Fatalf("got %d account switch events: %+v", len(events), events)
	}
	event := events[0]
	if event.Kind != model.CostAlertAccountSwitch || event.AccountSwitches != 2 || event.AccountSwitchSessions != 1 {
		t.Fatalf("unexpected account switch event: %+v", event)
	}
	if strings.Contains(event.AlertKey, "secret-session-value") || strings.Contains(event.Message, "secret-session-value") {
		t.Fatalf("session identifier leaked: %+v", event)
	}
	if !strings.Contains(event.Message, "账户 A #11") || !strings.Contains(event.Message, "账户 B #78") {
		t.Fatalf("account identities missing: %q", event.Message)
	}
	items[0].transitionCount = 1
	items[1].transitionCount = 1
	if events := evaluateAccountSwitches(items, policy, time.Unix(90, 0), time.Unix(110, 0)); len(events) != 0 {
		t.Fatalf("single transition produced an event: %+v", events)
	}
}

func TestEvaluateCostUsageGroupDetectsCacheAndUnitCostAnomalies(t *testing.T) {
	policy := testCostAlertPolicy()
	group := costUsageGroup{
		userKey: "42", userName: "Owner", userEmail: "owner@example.com", model: "gpt-4.1", apiKeyID: 7, apiKeyName: "Codex",
		channelID: 7, channelName: "渠道 A", accountID: 9, accountName: "owner@example.com",
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
		if event.AccountID != 9 || event.AccountName != "owner@example.com" || event.UserName != "Owner" ||
			event.UserEmail != "owner@example.com" || event.APIKeyName != "Codex" {
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
