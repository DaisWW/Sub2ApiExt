package store

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/DaisWW/Sub2ApiExt/monitoring/internal/model"
)

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
	for _, key := range []string{"users", "accounts", "token_distribution"} {
		if _, exists := payload[key]; exists {
			t.Errorf("用量响应不应包含顶层字段 %q", key)
		}
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
	for _, key := range []string{"users", "quota"} {
		if _, exists := payload[key]; exists {
			t.Errorf("用量摘要不应包含字段 %q", key)
		}
	}
}
