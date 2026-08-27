package store

import (
	"encoding/json"
	"errors"
	"math"
	"testing"

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
	for _, value := range []string{"1h", "24h", "today", "7d", "15d", "30d", "24H "} {
		period, err := parseUsagePeriod(value)
		if err != nil || period.key == "" {
			t.Errorf("parseUsagePeriod(%q) 返回 period=%+v, err=%v", value, period, err)
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
	for _, key := range []string{"users", "token_distribution", "cost_per_million_tokens", "channels"} {
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

func TestUsageSummaryOmitsUserAndQuotaDetails(t *testing.T) {
	encoded, err := json.Marshal(model.UsageSummary{})
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(encoded, &payload); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"users", "quota", "cost_per_million_tokens"} {
		if _, exists := payload[key]; exists {
			t.Errorf("用量摘要不应包含字段 %q", key)
		}
	}
}
