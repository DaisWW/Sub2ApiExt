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
	if account.LatestMessage != "暂无真实请求；仅在渠道报错后主动探测" {
		t.Fatalf("account missing-evidence message = %q", account.LatestMessage)
	}

	applyLatestTargetStateWithMessage(&account,
		sql.NullString{String: model.StatusFailed, Valid: true},
		sql.NullString{String: "request_error", Valid: true},
		sql.NullString{String: "真实请求报错，等待恢复探测", Valid: true},
		sql.NullInt64{}, sql.NullInt64{}, sql.NullTime{Time: now, Valid: true}, now, time.Minute)
	if account.LatestMessage != "真实请求报错，等待恢复探测" {
		t.Fatalf("request error message should be safe to expose: %q", account.LatestMessage)
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

func TestApplyLatestTargetStateExplainsMissingTrafficEvidence(t *testing.T) {
	now := time.Now().UTC()
	target := model.DashboardTarget{Target: model.Target{Kind: model.KindAccount, ProbeEnabled: true}}
	applyLatestTargetStateWithMessage(&target, sql.NullString{}, sql.NullString{}, sql.NullString{},
		sql.NullInt64{}, sql.NullInt64{}, sql.NullTime{}, now, time.Minute)
	if target.Status != model.StatusUnknown || target.LatestMessage != "暂无真实请求；仅在渠道报错后主动探测" {
		t.Fatalf("missing evidence state = %+v", target.Target)
	}
}

func TestApplyLatestTargetStateExplainsMissingGroupEvidence(t *testing.T) {
	now := time.Now().UTC()
	target := model.DashboardTarget{Target: model.Target{Kind: model.KindGroup, ProbeEnabled: true}}
	applyLatestTargetStateWithMessage(&target, sql.NullString{}, sql.NullString{}, sql.NullString{},
		sql.NullInt64{}, sql.NullInt64{}, sql.NullTime{}, now, time.Minute)
	if target.Status != model.StatusUnknown || target.LatestMessage != "暂无真实请求，等待下一次请求确认" {
		t.Fatalf("missing group evidence = %+v", target.Target)
	}
}

func TestApplyLatestTargetStateExplainsErrorAccountRecovery(t *testing.T) {
	now := time.Now().UTC()
	target := model.DashboardTarget{Target: model.Target{
		Kind: model.KindAccount, SourceStatus: "error", ProbeEnabled: true,
	}}
	applyLatestTargetStateWithMessage(&target, sql.NullString{}, sql.NullString{}, sql.NullString{},
		sql.NullInt64{}, sql.NullInt64{}, sql.NullTime{}, now, time.Minute)
	if target.Status != model.StatusUnknown || target.LatestMessage != "账户处于错误状态；等待真实请求或新的渠道错误" {
		t.Fatalf("error account without channel trigger = %+v, want waiting for new evidence", target.Target)
	}

	errorAt := now.Add(-time.Minute)
	target.RecoveryTriggerAt = &errorAt
	target.LatestMessage = ""
	applyLatestTargetStateWithMessage(&target, sql.NullString{}, sql.NullString{}, sql.NullString{},
		sql.NullInt64{}, sql.NullInt64{}, sql.NullTime{}, now, time.Minute)
	if target.LatestMessage != "渠道报错，等待恢复探测" {
		t.Fatalf("error account with channel trigger = %q, want recovery wait", target.LatestMessage)
	}

	probeTarget := model.DashboardTarget{Target: model.Target{
		Kind: model.KindAccount, SourceStatus: "error", ProbeEnabled: true,
	}}
	applyLatestTargetStateWithMessage(&probeTarget,
		sql.NullString{String: model.StatusFailed, Valid: true},
		sql.NullString{String: "probe", Valid: true},
		sql.NullString{String: "上游请求失败：HTTP 502: secret", Valid: true},
		sql.NullInt64{}, sql.NullInt64{}, sql.NullTime{Time: now, Valid: true}, now, time.Minute)
	if probeTarget.LatestMessage != "" {
		t.Fatalf("account probe response must not be exposed: %q", probeTarget.LatestMessage)
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

func TestErrorAccountDoesNotContributeAvailability(t *testing.T) {
	target := model.DashboardTarget{Target: model.Target{
		Kind: model.KindAccount, SourceStatus: "error", ProbeEnabled: true,
	}, Stats: model.TargetStats{Samples: 2, Successful: 2, Availability: 100}}
	if targetContributesAvailability(target) {
		t.Fatal("error account must not contribute to business availability")
	}
	var summary model.Summary
	addDashboardSummary(&summary, target)
	if summary.Availability != 0 || summary.Targets != 1 {
		t.Fatalf("error account summary = %+v, want no availability contribution", summary)
	}
}

func TestDashboardQueryUsesWindowBucketsAndSuccessfulLatencySamples(t *testing.T) {
	for _, fragment := range []string{
		"visible_targets AS MATERIALIZED",
		"t.kind <> 'account'",
		"current_account.schedulable = TRUE",
		"LOWER(TRIM(current_account.status)) IN ('active', 'error')",
		"OR (t.kind = 'account' AND LOWER(TRIM(t.source_status)) = 'error')",
		"FROM visible_targets targets",
		"JOIN visible_targets visible",
		"schedulable = TRUE",
		"LOWER(TRIM(status)) IN ('active', 'error')",
		"percentile_cont(0.95)",
		"status IN ('operational','degraded') AND first_byte_ms IS NOT NULL",
		"status IN ('operational','degraded') AND latency_ms IS NOT NULL",
		"CASE WHEN usage.duration_ms >= 20000 THEN 'degraded' ELSE 'operational' END",
		"period_usage AS MATERIALIZED",
		"eligible_usage AS MATERIALIZED",
		"latest_account_usage AS MATERIALIZED",
		"latest_group_usage AS MATERIALIZED",
		"group_request_rows AS MATERIALIZED",
		"group_request_ranked AS",
		"group_error_candidates AS MATERIALIZED",
		"account_error_events AS MATERIALIZED",
		"ops_error_logs",
		"COALESCE(NULLIF(upstream_status_code, 0), NULLIF(status_code, 0)) AS status_code",
		"COALESCE(NULLIF(upstream_status_code, 0), NULLIF(status_code, 0), 0) <> 429",
		"PARTITION BY group_id, request_key",
		"latest_group_error AS MATERIALIZED",
		"mc.source <> 'aggregate'",
		"recent_samples AS",
		"FROM recent_samples",
		"mc.source = 'aggregate'",
		"group_error_wins",
		"group_success_wins",
		"group_aggregate_wins",
		"targets.kind <> 'group' OR mc.source = 'aggregate'",
		"latest_group_error.created_at >= latest_checks.checked_at",
		"latest_group_usage.created_at >= latest_checks.checked_at",
		"latest_checks.checked_at > latest_group_error.created_at",
		"latest_checks.checked_at > latest_group_usage.created_at",
		"COALESCE(group_success_latency_ms, 0) >= 20000",
		"COALESCE(account_success_latency_ms, 0) >= 20000",
		"generate_series(0, 23)",
		"PARTITION BY target_key, bucket_index",
		"'latency_ms', recent_ranked.latency_ms",
		"ORDER BY checked_at DESC",
		"CASE WHEN source = 'request_error' THEN 0",
		"COALESCE(recent_ranked.status, 'unknown')",
		"LEFT JOIN LATERAL",
		"ORDER BY mc.checked_at DESC, mc.id DESC",
		"targets.last_activity_at >= latest_checks.checked_at",
		"targets.source_updated_at",
		"targets.last_channel_error_at",
		"targets.last_channel_error_resolved_at",
		"channel_error_wins",
		"recovery_active",
		"recovery_trigger_at",
		"e.recovery_trigger_at",
		"latest_checks.checked_at < targets.last_channel_error_at",
		"targets.last_channel_error_at > targets.last_channel_error_resolved_at",
		"COALESCE(latest_checks.status, '') NOT IN ('operational', 'degraded')",
		"THEN 'request_error'",
		"真实请求报错，等待恢复探测",
		"mc.checked_at >= targets.source_updated_at - INTERVAL '2 minutes'",
		"targets.last_activity_at >= targets.source_updated_at - INTERVAL '2 minutes'",
		"NOW() - INTERVAL '24 hours' AS start_at",
		"EXTRACT(EPOCH FROM INTERVAL '1 hour') AS bucket_seconds",
		"latest_evidence",
		"latest_evidence_inputs",
		"WHEN t.kind = 'group' THEN g.rate_multiplier::double precision",
		"WHEN t.kind = 'account' THEN a.rate_multiplier::double precision",
		"LEFT JOIN accounts a ON t.kind = 'account' AND a.id = t.entity_id AND a.deleted_at IS NULL",
		"LEFT JOIN groups g ON t.kind = 'group' AND g.id = t.entity_id AND g.deleted_at IS NULL",
		"WHEN kind = 'group' THEN NULL",
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
	samplesStart := strings.Index(dashboardQuery, "), samples AS (")
	statsStart := strings.Index(dashboardQuery, "), stats AS (")
	recentSamplesStart := strings.Index(dashboardQuery, "), recent_samples AS (")
	bucketsStart := strings.Index(dashboardQuery, "), bucket_positions AS (")
	if samplesStart < 0 || statsStart <= samplesStart || recentSamplesStart <= statsStart || bucketsStart <= recentSamplesStart {
		t.Fatalf("dashboard query must separate traffic samples from aggregate health samples: samples=%d stats=%d recent=%d buckets=%d", samplesStart, statsStart, recentSamplesStart, bucketsStart)
	}
	trafficSamples := dashboardQuery[samplesStart:statsStart]
	if !strings.Contains(trafficSamples, "mc.source <> 'aggregate'") || strings.Contains(trafficSamples, "mc.source = 'aggregate'") {
		t.Fatal("dashboard traffic statistics must exclude group aggregate observations")
	}
	statsQuery := dashboardQuery[statsStart:recentSamplesStart]
	if !strings.Contains(statsQuery, "FROM samples") {
		t.Fatal("dashboard traffic statistics must use only traffic samples")
	}
	recentSamples := dashboardQuery[recentSamplesStart:bucketsStart]
	if !strings.Contains(recentSamples, "mc.kind = 'group'") || !strings.Contains(recentSamples, "mc.source = 'aggregate'") {
		t.Fatal("dashboard health trajectory must include group aggregate observations")
	}
	if strings.Contains(dashboardQuery, "first_seen_at") {
		t.Fatal("dashboard query must not depend on the removed idle timestamp")
	}
	if strings.Contains(dashboardQuery, "CASE WHEN kind = 'group' AND source = 'aggregate' THEN 0 ELSE 1 END") {
		t.Fatal("an older aggregate observation must not outrank a newer real request")
	}
	if strings.Contains(dashboardQuery, "AND NOT (") {
		t.Fatal("a newer successful request must be allowed to prove that a group still has a working route")
	}
	if !strings.Contains(dashboardQuery, "WHERE active = TRUE\n      AND (") ||
		!strings.Contains(dashboardQuery, "WHERE t.active = TRUE\n  AND (") {
		t.Fatal("dashboard target visibility filter is missing")
	}
	rowsStart := strings.Index(dashboardQuery, "group_request_rows AS MATERIALIZED")
	rankedStart := strings.Index(dashboardQuery, "group_request_ranked AS")
	errorsStart := strings.Index(dashboardQuery, "group_error_candidates AS MATERIALIZED")
	if rowsStart < 0 || rankedStart <= rowsStart || errorsStart <= rankedStart {
		t.Fatalf("dashboard query must rank all request rows before filtering failures: rows=%d ranked=%d errors=%d", rowsStart, rankedStart, errorsStart)
	}
	selectStart := strings.Index(dashboardQuery, "SELECT t.target_key")
	if selectStart < 0 {
		t.Fatal("dashboard query select list is missing")
	}
	selectList := dashboardQuery[selectStart:]
	if strings.Contains(selectList, "t.last_channel_error_at") {
		t.Fatal("dashboard must not expose an old channel error as an active recovery trigger")
	}
	if !strings.Contains(selectList, "e.recovery_trigger_at") {
		t.Fatal("dashboard must expose only the currently winning recovery trigger")
	}
}

func TestDashboardQueryLetsLaterAccountSuccessClearChannelError(t *testing.T) {
	if count := strings.Count(dashboardQuery, "targets.last_channel_error_at >= latest_account_usage.created_at"); count != 2 {
		t.Fatalf("latest account success must clear both current error and recovery state, found %d comparisons", count)
	}
	if !strings.Contains(dashboardQuery, "latest_account_usage.created_at >= latest_checks.checked_at") {
		t.Fatal("latest account success must become current evidence when it is newer than the latest check")
	}
}

func TestDashboardQueryCountsUnresolvedAccountChannelErrors(t *testing.T) {
	if !strings.Contains(dashboardQuery, "account_error_events AS MATERIALIZED") {
		t.Fatal("dashboard must materialize account channel errors as window observations")
	}
	if !strings.Contains(dashboardQuery, "FROM account_error_events") {
		t.Fatal("dashboard must include unresolved account channel errors in samples")
	}
	if !strings.Contains(dashboardQuery, "SELECT target_key, 'failed'") {
		t.Fatal("account channel errors must count as failed observations")
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
		t.Fatalf("leading sample should remain unknown before the first observation: %+v", samples[0])
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

func TestCarryForwardTargetStatusUsesEvidenceOlderThanWindow(t *testing.T) {
	windowStart := time.Date(2026, 8, 27, 0, 0, 0, 0, time.UTC)
	samples := []model.StatusSample{
		{Status: model.StatusUnknown, CheckedAt: windowStart.Add(time.Hour)},
		{Status: model.StatusUnknown, CheckedAt: windowStart.Add(2 * time.Hour)},
	}
	checkedAt := windowStart.Add(-time.Minute)
	carryForwardTargetStatus(samples, model.StatusOperational, "probe", checkedAt)
	for index, sample := range samples {
		if sample.Status != model.StatusOperational || sample.Source != "probe" {
			t.Fatalf("sample %d should carry older evidence: %+v", index, sample)
		}
		if sample.CarriedFrom == nil || !sample.CarriedFrom.Equal(checkedAt) {
			t.Fatalf("sample %d has wrong carried origin: %+v", index, sample)
		}
	}
}

func TestCarryForwardTargetStatusUsesEvidenceInsideWindow(t *testing.T) {
	windowStart := time.Date(2026, 8, 27, 0, 0, 0, 0, time.UTC)
	checkedAt := windowStart.Add(90 * time.Minute)
	samples := []model.StatusSample{
		{Status: model.StatusUnknown, CheckedAt: windowStart.Add(time.Hour)},
		{Status: model.StatusUnknown, CheckedAt: windowStart.Add(2 * time.Hour)},
	}
	carryForwardTargetStatus(samples, model.StatusOperational, "probe", checkedAt)
	if samples[0].Status != model.StatusUnknown || samples[0].CarriedFrom != nil {
		t.Fatalf("bucket before in-window evidence should remain empty: %+v", samples[0])
	}
	if samples[1].Status != model.StatusOperational || samples[1].CarriedFrom == nil ||
		!samples[1].CarriedFrom.Equal(checkedAt) {
		t.Fatalf("bucket after in-window evidence was not carried: %+v", samples[1])
	}
}

func TestCarryForwardTargetStatusPreservesRealSamples(t *testing.T) {
	checkedAt := time.Date(2026, 8, 27, 2, 0, 0, 0, time.UTC)
	realAt := checkedAt.Add(-time.Hour)
	samples := []model.StatusSample{
		{Status: model.StatusFailed, CheckedAt: realAt, Source: "probe"},
		{Status: model.StatusUnknown, CheckedAt: checkedAt.Add(time.Hour)},
	}
	carryForwardTargetStatus(samples, model.StatusOperational, "history", checkedAt)
	if samples[0].Status != model.StatusFailed || samples[0].Source != "probe" || samples[0].CarriedFrom != nil {
		t.Fatalf("real sample was overwritten: %+v", samples[0])
	}
	if samples[1].Status != model.StatusOperational || samples[1].Source != "history" || samples[1].CarriedFrom == nil ||
		!samples[1].CarriedFrom.Equal(checkedAt) {
		t.Fatalf("empty sample was not carried: %+v", samples[1])
	}
}

func TestCarryForwardTargetStatusPreservesEarlierCarriedSamples(t *testing.T) {
	checkedAt := time.Date(2026, 8, 27, 2, 0, 0, 0, time.UTC)
	carriedAt := checkedAt.Add(-2 * time.Hour)
	samples := []model.StatusSample{
		{Status: model.StatusFailed, Source: "probe", CheckedAt: checkedAt.Add(-time.Hour), CarriedFrom: &carriedAt},
		{Status: model.StatusOperational, Source: "history", CheckedAt: checkedAt.Add(time.Hour)},
	}
	carryForwardTargetStatus(samples, model.StatusOperational, "history", checkedAt)
	if samples[0].Status != model.StatusFailed || samples[0].Source != "probe" || samples[0].CarriedFrom == nil ||
		!samples[0].CarriedFrom.Equal(carriedAt) {
		t.Fatalf("earlier carried sample was overwritten: %+v", samples[0])
	}
	if samples[1].Status != model.StatusOperational || samples[1].CarriedFrom != nil {
		t.Fatalf("real sample was unexpectedly marked carried: %+v", samples[1])
	}
}

func TestCarryForwardTargetStatusDoesNotPaintBeforeFailure(t *testing.T) {
	checkedAt := time.Date(2026, 8, 27, 10, 0, 0, 0, time.UTC)
	samples := []model.StatusSample{
		{Status: model.StatusUnknown, CheckedAt: checkedAt.Add(-2 * time.Hour)},
		{Status: model.StatusUnknown, CheckedAt: checkedAt.Add(-time.Hour)},
		{Status: model.StatusFailed, Source: "aggregate", CheckedAt: checkedAt},
		{Status: model.StatusUnknown, CheckedAt: checkedAt.Add(time.Hour)},
	}
	carryForwardStatusSamples(samples)
	carryForwardTargetStatus(samples, model.StatusFailed, "aggregate", checkedAt)
	if samples[0].Status != model.StatusUnknown || samples[1].Status != model.StatusUnknown {
		t.Fatalf("buckets before failure should not be painted failed: %+v", samples)
	}
	if samples[3].Status != model.StatusFailed || samples[3].CarriedFrom == nil ||
		!samples[3].CarriedFrom.Equal(checkedAt) {
		t.Fatalf("bucket after failure was not carried: %+v", samples[3])
	}
}

func TestOverlayLatestRequestErrorReplacesCarriedHealthyBuckets(t *testing.T) {
	windowStart := time.Date(2026, 8, 27, 0, 0, 0, 0, time.UTC)
	samples := []model.StatusSample{
		{Status: model.StatusOperational, CheckedAt: windowStart.Add(time.Hour), Source: "history"},
		{Status: model.StatusUnknown, CheckedAt: windowStart.Add(2 * time.Hour)},
		{Status: model.StatusUnknown, CheckedAt: windowStart.Add(3 * time.Hour)},
	}
	carryForwardStatusSamples(samples)
	overlayLatestTargetStatus(samples, model.StatusFailed, "request_error", windowStart.Add(150*time.Minute), windowStart)
	if samples[0].Status != model.StatusOperational {
		t.Fatalf("bucket before request error changed: %+v", samples[0])
	}
	for index := 1; index < len(samples); index++ {
		if samples[index].Status != model.StatusFailed || samples[index].Source != "request_error" || samples[index].CarriedFrom != nil {
			t.Fatalf("bucket %d did not show winning request error: %+v", index, samples[index])
		}
	}
}
