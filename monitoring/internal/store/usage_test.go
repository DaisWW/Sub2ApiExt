package store

import (
	"encoding/json"
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/DaisWW/Sub2ApiExt/monitoring/internal/model"
)

func TestCacheHitRate(t *testing.T) {
	if got := cacheHitRate(250_000, 750_000); math.Abs(got-75) > 1e-9 {
		t.Fatalf("cacheHitRate() = %v, want 75", got)
	}
	if got := cacheHitRate(0, 0); got != 0 {
		t.Fatalf("zero-token cache hit rate = %v, want 0", got)
	}
}

func TestParseUsagePeriod(t *testing.T) {
	cases := []struct {
		value  string
		bucket string
	}{
		{value: "1h", bucket: "minute"},
		{value: "24h", bucket: "hour"},
		{value: "today", bucket: "hour"},
		{value: "yesterday", bucket: "hour"},
		{value: "7d", bucket: "day"},
		{value: "15d", bucket: "day"},
		{value: "30d", bucket: "day"},
		{value: "24H ", bucket: "hour"},
	}
	for _, test := range cases {
		period, err := parseUsagePeriod(test.value)
		if err != nil || period.key == "" {
			t.Errorf("parseUsagePeriod(%q) 返回 period=%+v, err=%v", test.value, period, err)
			continue
		}
		if period.bucket != test.bucket {
			t.Errorf("parseUsagePeriod(%q) bucket = %q, want %q", test.value, period.bucket, test.bucket)
		}
	}
	if _, err := parseUsagePeriod("90d"); !errors.Is(err, ErrInvalidUsagePeriod) {
		t.Fatalf("非法周期返回 %v，期望 %v", err, ErrInvalidUsagePeriod)
	}
}

func TestParseUsagePeriodUsesDefault(t *testing.T) {
	period, err := parseUsagePeriod("")
	if err != nil {
		t.Fatal(err)
	}
	if period.key != "24h" {
		t.Fatalf("默认周期为 %q，期望 24h", period.key)
	}
}

func TestResolveUsageBoundsUsesPreviousCalendarDay(t *testing.T) {
	period, err := parseUsagePeriod("yesterday")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 3, 9, 12, 0, 0, 0, time.FixedZone("UTC-4", -4*60*60))
	todayStart := time.Date(2026, 3, 9, 0, 0, 0, 0, time.FixedZone("UTC-4", -4*60*60))
	yesterdayStart := time.Date(2026, 3, 8, 0, 0, 0, 0, time.FixedZone("UTC-5", -5*60*60))

	bounds := resolveUsageBounds(period, now, todayStart, yesterdayStart)
	if !bounds.start.Equal(yesterdayStart) || !bounds.end.Equal(todayStart) {
		t.Fatalf("昨天窗口为 [%v, %v)，期望 [%v, %v)", bounds.start, bounds.end, yesterdayStart, todayStart)
	}
	if got := bounds.end.Sub(bounds.start); got != 23*time.Hour {
		t.Fatalf("夏令时切换日窗口长度为 %v，期望 23h", got)
	}
}

func TestUsageRankingOmitsUnusedDetailDimensions(t *testing.T) {
	ranking := newUsageRanking(usageBounds{})
	encoded, err := json.Marshal(ranking)
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(encoded, &payload); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"users", "token_distribution", "channels"} {
		if _, exists := payload[key]; exists {
			t.Errorf("用量响应不应包含顶层字段 %q", key)
		}
	}
}

func TestUsageRankingExposesAccountCacheRates(t *testing.T) {
	ranking := newUsageRanking(usageBounds{})
	ranking.Accounts = append(ranking.Accounts, model.UsageRankItem{
		Kind: model.KindAccount, Name: "账户甲", InputTokens: 250_000,
		CacheRead: 750_000, CacheHitRate: 75,
	})
	encoded, err := json.Marshal(ranking)
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(encoded, &payload); err != nil {
		t.Fatal(err)
	}
	accounts, ok := payload["accounts"].([]any)
	if !ok || len(accounts) != 1 {
		t.Fatalf("unexpected account cache payload: %s", encoded)
	}
	account := accounts[0].(map[string]any)
	if account["cache_hit_rate"] != float64(75) || account["cache_read_tokens"] != float64(750_000) {
		t.Fatalf("unexpected cache fields: %s", encoded)
	}
}

func TestUsageSummaryExposesCostPerMillionTokens(t *testing.T) {
	encoded, err := json.Marshal(model.UsageSummary{CostPerMillionTokens: 12.5})
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(encoded, &payload); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"users", "quota"} {
		if _, exists := payload[key]; exists {
			t.Errorf("用量摘要不应包含字段 %q", key)
		}
	}
	if payload["cost_per_million_tokens"] != 12.5 {
		t.Fatalf("unexpected unit cost payload: %s", encoded)
	}
}

func TestCostPerMillionTokens(t *testing.T) {
	if got := costPerMillionTokens(1.25, 100_000); math.Abs(got-12.5) > 1e-9 {
		t.Fatalf("costPerMillionTokens() = %v, want 12.5", got)
	}
	if got := costPerMillionTokens(1.25, 0); got != 0 {
		t.Fatalf("zero-token unit cost = %v, want 0", got)
	}
}

func TestUsageCostBreakdown(t *testing.T) {
	tokenCost, nonTokenCost, multiplier := usageCostBreakdown(2, 2.4, 0.8, 0.6, 0.2, 0.1)
	if math.Abs(tokenCost-1.7) > 1e-9 {
		t.Fatalf("token cost = %v, want 1.7", tokenCost)
	}
	if math.Abs(nonTokenCost-0.3) > 1e-9 {
		t.Fatalf("non-token cost = %v, want 0.3", nonTokenCost)
	}
	if math.Abs(multiplier-1.2) > 1e-9 {
		t.Fatalf("effective multiplier = %v, want 1.2", multiplier)
	}
	if _, nonTokenCost, _ = usageCostBreakdown(1, 1, 0.8, 0.8, 0, 0); nonTokenCost != 0 {
		t.Fatalf("negative non-token cost = %v, want 0", nonTokenCost)
	}
}

func TestApplyUsageSharesIncludesCostShare(t *testing.T) {
	items := []model.UsageRankItem{
		{TotalTokens: 25, TotalCost: 2},
		{TotalTokens: 75, TotalCost: 8},
	}
	applyUsageShares(items, 100, 10)
	if math.Abs(items[0].SharePercent-25) > 1e-9 || math.Abs(items[0].CostSharePercent-20) > 1e-9 {
		t.Fatalf("first shares = (%v, %v), want (25, 20)", items[0].SharePercent, items[0].CostSharePercent)
	}
	if math.Abs(items[1].SharePercent-75) > 1e-9 || math.Abs(items[1].CostSharePercent-80) > 1e-9 {
		t.Fatalf("second shares = (%v, %v), want (75, 80)", items[1].SharePercent, items[1].CostSharePercent)
	}
}

func TestUsageSummaryExposesCostBreakdown(t *testing.T) {
	encoded, err := json.Marshal(model.UsageSummary{
		BaseCost: 2, TotalCost: 2.4, TokenCost: 1.7,
		NonTokenCost: 0.3, EffectiveRateMultiplier: 1.2,
	})
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(encoded, &payload); err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]float64{
		"base_cost":                 2,
		"total_cost":                2.4,
		"token_cost":                1.7,
		"non_token_cost":            0.3,
		"effective_rate_multiplier": 1.2,
	} {
		if payload[key] != want {
			t.Errorf("%s = %v, want %v", key, payload[key], want)
		}
	}
}

func TestModelUsageRankAggregatesByModel(t *testing.T) {
	for _, fragment := range []string{
		"'model:' || COALESCE(NULLIF(ul.model, ''), 'unknown') AS entity_key",
		"''::text AS context",
		"GROUP BY COALESCE(NULLIF(ul.model, ''), 'unknown')",
	} {
		if !strings.Contains(modelUsageRankQuery, fragment) {
			t.Fatalf("model usage rank query missing model aggregation %q", fragment)
		}
	}
	for _, fragment := range []string{
		":account:",
		":channel:",
		":group:",
		":account-rate:",
		":record-rate:",
		"LEFT JOIN channels c",
		"ul.account_id, ul.channel_id",
	} {
		if strings.Contains(modelUsageRankQuery, fragment) {
			t.Fatalf("model usage rank query still splits by %q", fragment)
		}
	}
}
