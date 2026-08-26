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
), latest AS (
    SELECT DISTINCT ON (target_key) target_key, status, latency_ms, first_byte_ms, checked_at, source
    FROM samples
    ORDER BY target_key, checked_at DESC, CASE WHEN source = 'history' THEN 0 ELSE 1 END
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
)
SELECT t.target_key, t.kind, t.entity_id, t.name, t.platform, t.source_status, t.probe_enabled,
       l.status, l.latency_ms, l.first_byte_ms, l.checked_at, l.source,
       COALESCE(s.samples,0), COALESCE(s.successful,0),
	       s.first_fastest, s.first_median, s.first_p95,
	       s.latency_fastest, s.latency_median, s.latency_p95,
	       COALESCE(r.samples, '[]'::jsonb)
FROM monitoring_targets t
LEFT JOIN latest l ON l.target_key = t.target_key
LEFT JOIN stats s ON s.target_key = t.target_key
LEFT JOIN recent r ON r.target_key = t.target_key
WHERE t.active = TRUE AND LOWER(TRIM(t.source_status)) = 'active'
ORDER BY CASE WHEN t.kind = 'group' THEN 0 ELSE 1 END, t.name, t.entity_id`

func (s *Store) Dashboard(ctx context.Context, windowDays int, staleAfter time.Duration, intervalSec int) (model.Dashboard, error) {
	rows, err := s.db.QueryContext(ctx, dashboardQuery, windowDays)
	if err != nil {
		return model.Dashboard{}, fmt.Errorf("load dashboard: %w", err)
	}
	defer rows.Close()
	return buildDashboard(rows, windowDays, staleAfter, intervalSec)
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
	var latestStatus, latestSource sql.NullString
	var latestLatency, latestFirst sql.NullInt64
	var latestAt sql.NullTime
	var samples, successful int
	var firstFastest, latencyFastest sql.NullInt64
	var firstMedian, firstP95, latencyMedian, latencyP95 sql.NullFloat64
	var recentJSON []byte
	if err := rows.Scan(
		&target.Key, &target.Kind, &target.EntityID, &target.Name, &target.Platform,
		&target.SourceStatus, &target.ProbeEnabled, &latestStatus, &latestLatency,
		&latestFirst, &latestAt, &latestSource, &samples, &successful,
		&firstFastest, &firstMedian, &firstP95, &latencyFastest,
		&latencyMedian, &latencyP95, &recentJSON,
	); err != nil {
		return model.DashboardTarget{}, false, fmt.Errorf("scan dashboard: %w", err)
	}
	applyLatestTargetState(&target, latestStatus, latestSource, latestLatency, latestFirst, latestAt, now, staleAfter)
	target.Stats = targetStats(samples, successful, firstFastest, firstMedian, firstP95, latencyFastest, latencyMedian, latencyP95)
	if err := json.Unmarshal(recentJSON, &target.RecentSamples); err != nil {
		return model.DashboardTarget{}, false, fmt.Errorf("decode recent samples: %w", err)
	}
	return target, target.ProbeEnabled && samples > 0, nil
}

func applyLatestTargetState(
	target *model.DashboardTarget,
	status, source sql.NullString,
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
