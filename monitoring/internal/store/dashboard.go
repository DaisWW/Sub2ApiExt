package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/DaisWW/Sub2ApiExt/monitoring/internal/model"
)

const dashboardQuery = `
WITH bounds AS (
    SELECT NOW() - ($1::int * INTERVAL '1 day') AS start_at,
           NOW() AS end_at,
           NOW() - ($2::bigint * INTERVAL '1 second') AS stale_at,
           EXTRACT(EPOCH FROM ($1::int * INTERVAL '1 day')) / 24 AS bucket_seconds
), active_accounts AS MATERIALIZED (
	SELECT id
	FROM accounts
	WHERE deleted_at IS NULL AND schedulable = TRUE AND LOWER(TRIM(status)) = 'active'
), active_groups AS MATERIALIZED (
	SELECT id
	FROM groups
	WHERE deleted_at IS NULL AND LOWER(TRIM(status)) = 'active'
), period_usage AS MATERIALIZED (
	SELECT ul.account_id, ul.group_id, ul.duration_ms, ul.first_token_ms, ul.created_at
	FROM usage_logs ul
	CROSS JOIN bounds
	WHERE ul.created_at >= bounds.start_at AND ul.created_at < bounds.end_at AND ul.actual_cost > 0
), eligible_usage AS MATERIALIZED (
	SELECT ul.account_id, ul.group_id, ul.duration_ms, ul.first_token_ms, ul.created_at
	FROM period_usage ul
	JOIN active_accounts a ON a.id = ul.account_id
	JOIN active_groups g ON g.id = ul.group_id
), samples AS (
    SELECT target_key, status, latency_ms, first_byte_ms, checked_at, source
    FROM monitoring_checks, bounds
    WHERE checked_at >= bounds.start_at AND checked_at < bounds.end_at
	UNION ALL
	SELECT 'account:' || account_id::text, 'operational', duration_ms, first_token_ms, created_at, 'history'
	FROM eligible_usage
	UNION ALL
	SELECT 'group:' || group_id::text, 'operational', duration_ms, first_token_ms, created_at, 'history'
	FROM eligible_usage
), stats AS (
    SELECT target_key,
           COUNT(*) FILTER (WHERE status NOT IN ('unknown','disabled')) AS samples,
           COUNT(*) FILTER (WHERE status IN ('operational','degraded')) AS successful,
	       MIN(first_byte_ms) FILTER (WHERE status IN ('operational','degraded') AND first_byte_ms IS NOT NULL) AS first_fastest,
	       percentile_cont(0.5) WITHIN GROUP (ORDER BY first_byte_ms) FILTER (WHERE status IN ('operational','degraded') AND first_byte_ms IS NOT NULL) AS first_median,
	       percentile_cont(0.95) WITHIN GROUP (ORDER BY first_byte_ms) FILTER (WHERE status IN ('operational','degraded') AND first_byte_ms IS NOT NULL) AS first_p95,
	       MIN(latency_ms) FILTER (WHERE status IN ('operational','degraded') AND latency_ms IS NOT NULL) AS latency_fastest,
	       percentile_cont(0.5) WITHIN GROUP (ORDER BY latency_ms) FILTER (WHERE status IN ('operational','degraded') AND latency_ms IS NOT NULL) AS latency_median,
	       percentile_cont(0.95) WITHIN GROUP (ORDER BY latency_ms) FILTER (WHERE status IN ('operational','degraded') AND latency_ms IS NOT NULL) AS latency_p95
    FROM samples
    GROUP BY target_key
), bucket_positions AS (
	SELECT generate_series(0, 23)::int AS bucket_index
), recent_bucketed AS (
	SELECT samples.target_key, samples.status, samples.checked_at, samples.source,
	       LEAST(23, FLOOR(EXTRACT(EPOCH FROM (samples.checked_at - bounds.start_at)) / bounds.bucket_seconds)::int) AS bucket_index
	FROM samples
	CROSS JOIN bounds
	WHERE samples.status NOT IN ('unknown','disabled')
), recent_ranked AS (
	SELECT target_key, status, checked_at, source, bucket_index,
	       ROW_NUMBER() OVER (
	           PARTITION BY target_key, bucket_index
	           ORDER BY CASE WHEN source = 'history' THEN 0 ELSE 1 END, checked_at DESC
	       ) AS position
	FROM recent_bucketed
), recent AS (
	SELECT targets.target_key,
	       jsonb_agg(jsonb_build_object(
	           'status', COALESCE(recent_ranked.status, 'unknown'),
	           'checked_at', COALESCE(
	               recent_ranked.checked_at,
	               bounds.start_at + ((bucket_positions.bucket_index + 1) * bounds.bucket_seconds) * INTERVAL '1 second'
	           ),
	           'source', COALESCE(recent_ranked.source, '')
	       ) ORDER BY bucket_positions.bucket_index) AS samples
	FROM monitoring_targets targets
	CROSS JOIN bounds
	CROSS JOIN bucket_positions
	LEFT JOIN recent_ranked
	       ON recent_ranked.target_key = targets.target_key
	      AND recent_ranked.bucket_index = bucket_positions.bucket_index
	      AND recent_ranked.position = 1
	WHERE targets.active = TRUE AND LOWER(TRIM(targets.source_status)) = 'active'
	GROUP BY targets.target_key
), latest_checks AS (
    SELECT targets.target_key, checks.status, checks.latency_ms, checks.first_byte_ms,
           checks.checked_at, checks.source, checks.message
    FROM monitoring_targets targets
    LEFT JOIN LATERAL (
        SELECT mc.status, mc.latency_ms, mc.first_byte_ms, mc.checked_at, mc.source, mc.message
        FROM monitoring_checks mc
        WHERE mc.target_key = targets.target_key
        ORDER BY mc.checked_at DESC, mc.id DESC
        LIMIT 1
    ) checks ON TRUE
), latest_evidence_inputs AS (
    SELECT targets.target_key, targets.kind, targets.last_activity_at,
           latest_checks.status, latest_checks.latency_ms, latest_checks.first_byte_ms,
           latest_checks.checked_at, latest_checks.source, latest_checks.message,
           CASE WHEN targets.last_activity_at IS NOT NULL
                      AND NOT (
                          targets.kind = 'group'
                          AND COALESCE(latest_checks.status, '') IN ('degraded', 'failed', 'error')
                          AND latest_checks.checked_at IS NOT NULL
                          AND latest_checks.checked_at >= bounds.stale_at
                      )
                      AND (latest_checks.checked_at IS NULL
                           OR targets.last_activity_at >= latest_checks.checked_at)
                THEN TRUE ELSE FALSE END AS history_wins
    FROM monitoring_targets targets
    CROSS JOIN bounds
    LEFT JOIN latest_checks ON latest_checks.target_key = targets.target_key
), latest_evidence AS (
    SELECT target_key,
           CASE WHEN history_wins THEN 'operational' ELSE status END AS status,
           CASE WHEN history_wins THEN NULL ELSE latency_ms END AS latency_ms,
           CASE WHEN history_wins THEN NULL ELSE first_byte_ms END AS first_byte_ms,
           CASE WHEN history_wins THEN last_activity_at ELSE checked_at END AS checked_at,
           CASE WHEN history_wins THEN 'history' ELSE source END AS source,
           CASE WHEN history_wins THEN '近期真实请求' ELSE message END AS message
    FROM latest_evidence_inputs
)
SELECT t.target_key, t.kind, t.entity_id, t.name, t.platform, t.source_status, t.probe_enabled,
       e.status, e.latency_ms, e.first_byte_ms, e.checked_at, e.source, e.message,
       COALESCE(s.samples,0), COALESCE(s.successful,0),
	       s.first_fastest, s.first_median, s.first_p95,
	       s.latency_fastest, s.latency_median, s.latency_p95,
	       COALESCE(r.samples, '[]'::jsonb)
FROM monitoring_targets t
LEFT JOIN latest_evidence e ON e.target_key = t.target_key
LEFT JOIN stats s ON s.target_key = t.target_key
LEFT JOIN recent r ON r.target_key = t.target_key
WHERE t.active = TRUE AND LOWER(TRIM(t.source_status)) = 'active'
ORDER BY CASE WHEN t.kind = 'group' THEN 0 ELSE 1 END, t.name, t.entity_id`

func (s *Store) Dashboard(ctx context.Context, windowDays int, staleAfter time.Duration, intervalSec int) (model.Dashboard, error) {
	rows, err := s.db.QueryContext(ctx, dashboardQuery, windowDays, dashboardStaleSeconds(staleAfter))
	if err != nil {
		return model.Dashboard{}, fmt.Errorf("load dashboard: %w", err)
	}
	defer rows.Close()
	return buildDashboard(rows, windowDays, staleAfter, intervalSec)
}

func dashboardStaleSeconds(staleAfter time.Duration) int64 {
	seconds := int64(staleAfter / time.Second)
	if seconds < 1 {
		return 1
	}
	return seconds
}

func buildDashboard(rows *sql.Rows, windowDays int, staleAfter time.Duration, intervalSec int) (model.Dashboard, error) {
	now := time.Now().UTC()
	dashboard := model.Dashboard{
		GeneratedAt: now, WindowDays: windowDays, IntervalSec: intervalSec,
		Targets: []model.DashboardTarget{},
	}
	availabilityTargets := 0
	for rows.Next() {
		target, hasAvailability, err := scanDashboardTarget(rows, now, staleAfter)
		if err != nil {
			return model.Dashboard{}, err
		}
		dashboard.Targets = append(dashboard.Targets, target)
		addDashboardSummary(&dashboard.Summary, target)
		if hasAvailability {
			availabilityTargets++
		}
	}
	if err := rows.Err(); err != nil {
		return model.Dashboard{}, err
	}
	if availabilityTargets > 0 {
		dashboard.Summary.Availability /= float64(availabilityTargets)
	}
	return dashboard, nil
}

func scanDashboardTarget(rows *sql.Rows, now time.Time, staleAfter time.Duration) (model.DashboardTarget, bool, error) {
	var target model.DashboardTarget
	var latestStatus, latestSource, latestMessage sql.NullString
	var latestLatency, latestFirst sql.NullInt64
	var latestAt sql.NullTime
	var samples, successful int
	var firstFastest, latencyFastest sql.NullInt64
	var firstMedian, firstP95, latencyMedian, latencyP95 sql.NullFloat64
	var recentJSON []byte
	if err := rows.Scan(
		&target.Key, &target.Kind, &target.EntityID, &target.Name, &target.Platform,
		&target.SourceStatus, &target.ProbeEnabled,
		&latestStatus, &latestLatency,
		&latestFirst, &latestAt, &latestSource, &latestMessage, &samples, &successful,
		&firstFastest, &firstMedian, &firstP95, &latencyFastest,
		&latencyMedian, &latencyP95, &recentJSON,
	); err != nil {
		return model.DashboardTarget{}, false, fmt.Errorf("scan dashboard: %w", err)
	}
	applyLatestTargetStateWithMessage(&target, latestStatus, latestSource, latestMessage, latestLatency, latestFirst, latestAt, now, staleAfter)
	target.Stats = targetStats(samples, successful, firstFastest, firstMedian, firstP95, latencyFastest, latencyMedian, latencyP95)
	if err := json.Unmarshal(recentJSON, &target.RecentSamples); err != nil {
		return model.DashboardTarget{}, false, fmt.Errorf("decode recent samples: %w", err)
	}
	carryForwardStatusSamples(target.RecentSamples)
	return target, target.ProbeEnabled && samples > 0, nil
}

func carryForwardStatusSamples(samples []model.StatusSample) {
	var previous *model.StatusSample
	for i := range samples {
		sample := &samples[i]
		switch sample.Status {
		case model.StatusOperational, model.StatusDegraded, model.StatusFailed, model.StatusError:
			observed := *sample
			previous = &observed
		default:
			if previous == nil {
				continue
			}
			carriedFrom := previous.CheckedAt
			sample.Status = previous.Status
			sample.Source = previous.Source
			sample.CarriedFrom = &carriedFrom
		}
	}
}

// applyLatestTargetState keeps the original helper signature for callers that
// do not need to surface a group-health explanation.
func applyLatestTargetState(
	target *model.DashboardTarget,
	status, source sql.NullString,
	latency, firstByte sql.NullInt64,
	checkedAt sql.NullTime,
	now time.Time,
	staleAfter time.Duration,
) {
	applyLatestTargetStateWithMessage(target, status, source, sql.NullString{}, latency, firstByte, checkedAt, now, staleAfter)
}

func applyLatestTargetStateWithMessage(
	target *model.DashboardTarget,
	status, source sql.NullString,
	message sql.NullString,
	latency, firstByte sql.NullInt64,
	checkedAt sql.NullTime,
	now time.Time,
	staleAfter time.Duration,
) {
	target.Status = model.StatusUnknown
	if !target.ProbeEnabled {
		target.Status = model.StatusDisabled
	} else if status.Valid {
		target.Status = status.String
	}
	if checkedAt.Valid {
		value := checkedAt.Time.UTC()
		target.LastCheckedAt = &value
		target.Stale = now.Sub(value) > staleAfter
	}
	if source.Valid {
		target.LatestSource = source.String
	}
	if message.Valid && target.Kind == model.KindGroup {
		target.LatestMessage = sanitizeUpstreamMessage(message.String)
	}
	if latency.Valid {
		value := int(latency.Int64)
		target.LatestLatencyMs = &value
	}
	if firstByte.Valid {
		value := int(firstByte.Int64)
		target.LatestFirstByteMs = &value
	}
}

func targetStats(
	samples, successful int,
	firstFastest sql.NullInt64,
	firstMedian sql.NullFloat64,
	firstP95 sql.NullFloat64,
	latencyFastest sql.NullInt64,
	latencyMedian, latencyP95 sql.NullFloat64,
) model.TargetStats {
	stats := model.TargetStats{Samples: samples, Successful: successful, Errors: samples - successful}
	if samples > 0 {
		stats.Availability = float64(successful) * 100 / float64(samples)
	}
	stats.FirstByte = metricStats(firstFastest, firstMedian, firstP95)
	stats.Latency = metricStats(latencyFastest, latencyMedian, latencyP95)
	return stats
}

func addDashboardSummary(summary *model.Summary, target model.DashboardTarget) {
	summary.Targets++
	switch target.Status {
	case model.StatusOperational:
		summary.Operational++
	case model.StatusDegraded:
		summary.Degraded++
	case model.StatusFailed, model.StatusError:
		summary.Failed++
	default:
		summary.Unknown++
	}
	if target.ProbeEnabled && target.Stats.Samples > 0 {
		summary.Availability += target.Stats.Availability
	}
}

func metricStats(fastest sql.NullInt64, median, p95 sql.NullFloat64) model.MetricStats {
	var stats model.MetricStats
	if fastest.Valid {
		value := int(fastest.Int64)
		stats.FastestMs = &value
	}
	if median.Valid {
		value := median.Float64
		stats.MedianMs = &value
	}
	if p95.Valid {
		value := p95.Float64
		stats.P95Ms = &value
	}
	return stats
}
