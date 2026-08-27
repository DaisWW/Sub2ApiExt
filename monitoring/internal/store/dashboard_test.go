package store

import (
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/DaisWW/Sub2ApiExt/monitoring/internal/model"
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

func TestApplyLatestTargetStateSanitizesGroupMessage(t *testing.T) {
	now := time.Now().UTC()
	target := model.DashboardTarget{Target: model.Target{Kind: model.KindGroup, ProbeEnabled: true}}
	applyLatestTargetStateWithMessage(&target,
		sql.NullString{String: model.StatusDegraded, Valid: true},
		sql.NullString{String: "aggregate", Valid: true},
		sql.NullString{String: "分组不可用：HTTP 502: secret", Valid: true},
		sql.NullInt64{}, sql.NullInt64{}, sql.NullTime{Time: now, Valid: true}, now, time.Minute)
	if target.LatestMessage != "分组不可用：HTTP 502" {
		t.Fatalf("group message was not sanitized: %q", target.LatestMessage)
	}

	account := model.DashboardTarget{Target: model.Target{Kind: model.KindAccount, ProbeEnabled: true}}
	applyLatestTargetStateWithMessage(&account, sql.NullString{}, sql.NullString{},
		sql.NullString{String: "secret", Valid: true}, sql.NullInt64{}, sql.NullInt64{}, sql.NullTime{}, now, time.Minute)
	if account.LatestMessage != "" {
		t.Fatalf("account message should not be exposed: %q", account.LatestMessage)
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
		"LEFT JOIN LATERAL",
		"ORDER BY mc.checked_at DESC, mc.id DESC",
		"targets.last_activity_at >= latest_checks.checked_at",
		"($2::bigint * INTERVAL '1 second')",
		"latest_checks.checked_at >= bounds.stale_at",
		"latest_evidence",
		"latest_evidence_inputs",
		"targets.kind = 'group'",
		"latest_checks.status, '') IN ('degraded', 'failed', 'error')",
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
	if strings.Contains(dashboardQuery, "first_seen_at") {
		t.Fatal("dashboard query must not depend on the removed idle timestamp")
	}
}

func TestDashboardStaleSecondsHasAUsableMinimum(t *testing.T) {
	if got := dashboardStaleSeconds(0); got != 1 {
		t.Fatalf("zero stale duration = %d seconds, want minimum 1", got)
	}
	if got := dashboardStaleSeconds(1500 * time.Millisecond); got != 1 {
		t.Fatalf("sub-second-rounded stale duration = %d seconds, want 1", got)
	}
	if got := dashboardStaleSeconds(2 * time.Minute); got != 120 {
		t.Fatalf("two-minute stale duration = %d seconds, want 120", got)
	}
}

func TestCarryForwardStatusSamples(t *testing.T) {
	start := time.Date(2026, 8, 27, 0, 0, 0, 0, time.UTC)
	samples := []model.StatusSample{
		{Status: model.StatusUnknown, CheckedAt: start},
		{Status: model.StatusOperational, CheckedAt: start.Add(time.Hour), Source: "history"},
		{Status: model.StatusUnknown, CheckedAt: start.Add(2 * time.Hour)},
		{Status: model.StatusUnknown, CheckedAt: start.Add(3 * time.Hour)},
		{Status: model.StatusFailed, CheckedAt: start.Add(4 * time.Hour), Source: "probe"},
		{Status: model.StatusUnknown, CheckedAt: start.Add(5 * time.Hour)},
	}

	carryForwardStatusSamples(samples)

	if samples[0].Status != model.StatusUnknown || samples[0].CarriedFrom != nil {
		t.Fatalf("leading unknown sample must stay empty: %+v", samples[0])
	}
	if samples[1].CarriedFrom != nil {
		t.Fatalf("observed sample must not be marked as carried: %+v", samples[1])
	}
	for _, index := range []int{2, 3} {
		if samples[index].Status != model.StatusOperational || samples[index].Source != "history" {
			t.Fatalf("sample %d did not carry the last successful state: %+v", index, samples[index])
		}
		if samples[index].CarriedFrom == nil || !samples[index].CarriedFrom.Equal(samples[1].CheckedAt) {
			t.Fatalf("sample %d has wrong carried origin: %+v", index, samples[index])
		}
	}
	if samples[5].Status != model.StatusFailed || samples[5].Source != "probe" {
		t.Fatalf("failed state was not carried: %+v", samples[5])
	}
	if samples[5].CarriedFrom == nil || !samples[5].CarriedFrom.Equal(samples[4].CheckedAt) {
		t.Fatalf("failed state has wrong carried origin: %+v", samples[5])
	}
}
