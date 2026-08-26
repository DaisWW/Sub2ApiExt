package store

import (
	"database/sql"
	"strings"
	"testing"
)

func TestMetricStatsUsesP95(t *testing.T) {
	stats := metricStats(
		sql.NullInt64{Int64: 120, Valid: true},
		sql.NullFloat64{Float64: 240.5, Valid: true},
		sql.NullFloat64{Float64: 890.25, Valid: true},
	)
	if stats.FastestMs == nil || *stats.FastestMs != 120 || stats.MedianMs == nil || *stats.MedianMs != 240.5 {
		t.Fatalf("unexpected base stats: %+v", stats)
	}
	if stats.P95Ms == nil || *stats.P95Ms != 890.25 {
		t.Fatalf("unexpected P95: %+v", stats)
	}
}

func TestDashboardQueryUsesWindowBucketsAndSuccessfulLatencySamples(t *testing.T) {
	for _, fragment := range []string{
		"percentile_cont(0.95)",
		"status IN ('operational','degraded') AND first_byte_ms IS NOT NULL",
		"status IN ('operational','degraded') AND latency_ms IS NOT NULL",
		"period_usage AS MATERIALIZED",
		"eligible_usage AS MATERIALIZED",
		"generate_series(0, 23)",
		"PARTITION BY target_key, bucket_index",
		"CASE WHEN source = 'history' THEN 0 ELSE 1 END",
		"COALESCE(recent_ranked.status, 'unknown')",
	} {
		if !strings.Contains(dashboardQuery, fragment) {
			t.Fatalf("dashboard query missing %q", fragment)
		}
	}
	if strings.Contains(dashboardQuery, "MAX(latency_ms)") {
		t.Fatal("dashboard query must not expose a single latency outlier as P95")
	}
	if count := strings.Count(dashboardQuery, "FROM usage_logs ul"); count != 1 {
		t.Fatalf("dashboard query scans usage_logs %d times, want one filtered source", count)
	}
}
