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

func TestApplyLatestTargetStateMarksUnroutableGroupFailed(t *testing.T) {
	now := time.Now().UTC()
	group := model.DashboardTarget{Target: model.Target{
		Kind: model.KindGroup, SourceStatus: "active", ProbeEnabled: false,
	}}
	applyLatestTargetStateWithMessage(&group,
		sql.NullString{String: model.StatusOperational, Valid: true},
		sql.NullString{String: "history", Valid: true}, sql.NullString{},
		sql.NullInt64{}, sql.NullInt64{}, sql.NullTime{Time: now.Add(-time.Minute), Valid: true},
		now, time.Hour)
	if group.Status != model.StatusFailed {
		t.Fatalf("unroutable active group status = %q, want failed", group.Status)
	}
	if group.LatestMessage != "无启用渠道或可调度候选" {
		t.Fatalf("unroutable group message = %q", group.LatestMessage)
	}
	if group.LastCheckedAt != nil || group.LatestSource != "" {
		t.Fatalf("old evidence must not override current routing state: %+v", group.Target)
	}

	account := model.DashboardTarget{Target: model.Target{Kind: model.KindAccount, ProbeEnabled: false}}
	applyLatestTargetStateWithMessage(&account, sql.NullString{}, sql.NullString{}, sql.NullString{},
		sql.NullInt64{}, sql.NullInt64{}, sql.NullTime{}, now, time.Hour)
	if account.Status != model.StatusDisabled {
		t.Fatalf("disabled account status = %q, want disabled", account.Status)
	}
}

func TestUnroutableActiveGroupContributesZeroAvailability(t *testing.T) {
	target := model.DashboardTarget{Target: model.Target{
		Kind: model.KindGroup, SourceStatus: "active", ProbeEnabled: false, Status: model.StatusFailed,
	}, Stats: model.TargetStats{Availability: 100}}
	if !targetContributesAvailability(target) {
		t.Fatal("unroutable active group must be included in availability denominator")
	}
	var summary model.Summary
	addDashboardSummary(&summary, target)
	if summary.Failed != 1 || summary.Availability != 0 {
		t.Fatalf("unroutable group summary = %+v, want failed with zero availability contribution", summary)
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
		"JOIN monitoring_targets targets ON targets.target_key = samples.target_key",
		"CASE WHEN kind = 'group' AND source = 'aggregate' THEN 0 ELSE 1 END",
		"CASE WHEN source = 'probe' THEN 0 WHEN source = 'history' THEN 1 ELSE 2 END",
		"COALESCE(recent_ranked.status, 'unknown')",
		"LEFT JOIN LATERAL",
		"ORDER BY mc.checked_at DESC, mc.id DESC",
		"targets.last_activity_at >= latest_checks.checked_at",
		"NOW() - INTERVAL '24 hours' AS start_at",
		"EXTRACT(EPOCH FROM INTERVAL '1 hour') AS bucket_seconds",
		"latest_evidence",
		"latest_evidence_inputs",
		"COALESCE(alert_states.failure_streak, 0) AS failure_streak",
		"LEFT JOIN monitoring_alert_states alert_states ON alert_states.target_key = targets.target_key",
		"e.failure_streak",
		"CASE WHEN t.kind = 'group' THEN g.rate_multiplier::double precision END",
		"LEFT JOIN groups g ON t.kind = 'group' AND g.id = t.entity_id AND g.deleted_at IS NULL",
		"targets.kind = 'group'",
		"latest_checks.source = 'aggregate'",
		"latest_checks.status, '') IN ('degraded', 'failed', 'error')",
	} {
		if !strings.Contains(dashboardQuery, fragment) {
			t.Fatalf("dashboard query missing %q", fragment)
		}
	}
	if strings.Contains(dashboardQuery, "MAX(latency_ms)") {
		t.Fatal("dashboard query must not expose a single latency outlier as P95")
	}
	if strings.Contains(dashboardQuery, "prior_samples AS MATERIALIZED") {
		t.Fatal("dashboard query must not scan checks before the fixed 24-hour window")
	}
	if strings.Contains(dashboardQuery, "INTERVAL '1 day'") || strings.Contains(dashboardQuery, "$") {
		t.Fatal("dashboard query must not accept a configurable health window")
	}
	if count := strings.Count(dashboardQuery, "FROM usage_logs ul"); count != 1 {
		t.Fatalf("dashboard query scans usage_logs %d times, want one filtered source", count)
	}
	if strings.Contains(dashboardQuery, "first_seen_at") {
		t.Fatal("dashboard query must not depend on the removed idle timestamp")
	}
	if strings.Contains(dashboardQuery, "CASE WHEN source = 'history' THEN 0 ELSE 1 END") {
		t.Fatal("history must not outrank a newer probe or aggregate observation")
	}
}

func TestEffectiveDashboardStatusRequiresConfirmedAggregateFailures(t *testing.T) {
	if got := effectiveDashboardStatus(model.KindGroup, model.StatusFailed, "aggregate", 1, 2); got != model.StatusDegraded {
		t.Fatalf("single aggregate failure = %q, want degraded", got)
	}
	if got := effectiveDashboardStatus(model.KindGroup, model.StatusError, "aggregate", 1, 2); got != model.StatusDegraded {
		t.Fatalf("single aggregate error = %q, want degraded", got)
	}
	if got := effectiveDashboardStatus(model.KindGroup, model.StatusFailed, "aggregate", 2, 2); got != model.StatusFailed {
		t.Fatalf("confirmed aggregate failure = %q, want failed", got)
	}
	if got := effectiveDashboardStatus(model.KindAccount, model.StatusFailed, "aggregate", 1, 2); got != model.StatusFailed {
		t.Fatalf("account failure must not be downgraded = %q", got)
	}
	if got := effectiveDashboardStatus(model.KindGroup, model.StatusFailed, "probe", 1, 2); got != model.StatusFailed {
		t.Fatalf("non-aggregate group failure must not be downgraded = %q", got)
	}
}

func TestPendingAggregateFailureMessageClarifiesUnconfirmedState(t *testing.T) {
	message := pendingAggregateFailureMessage(sql.NullString{String: "0/2 accounts healthy", Valid: true})
	if message != "最近一次分组检查失败：分组内健康账户 0/2；等待下一轮确认" {
		t.Fatalf("pending aggregate message = %q", message)
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

	if samples[0].Status != model.StatusUnknown || samples[0].Source != "" {
		t.Fatalf("leading sample without an in-window observation must stay unknown: %+v", samples[0])
	}
	if samples[0].CarriedFrom != nil {
		t.Fatalf("leading unknown sample must not be marked as carried: %+v", samples[0])
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

func TestCarryForwardStatusSamplesLeavesUnknownWithoutPriorState(t *testing.T) {
	samples := []model.StatusSample{{Status: model.StatusUnknown, CheckedAt: time.Now().UTC()}}
	carryForwardStatusSamples(samples)
	if samples[0].Status != model.StatusUnknown || samples[0].CarriedFrom != nil {
		t.Fatalf("sample without any prior state must stay unknown: %+v", samples[0])
	}
}
