package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/DaisWW/Sub2ApiExt/monitoring/internal/model"
)

const dashboardWindow = 24 * time.Hour
const dashboardBucket = dashboardWindow / 24

const dashboardQuery = `
WITH bounds AS (
    SELECT NOW() - INTERVAL '24 hours' AS start_at,
           NOW() AS end_at,
           EXTRACT(EPOCH FROM INTERVAL '1 hour') AS bucket_seconds
), visible_targets AS MATERIALIZED (
    SELECT t.target_key, t.kind, t.source_updated_at
    FROM monitoring_targets t
    WHERE active = TRUE
      AND (
          t.kind <> 'account'
          OR EXISTS (
              SELECT 1
              FROM accounts current_account
              WHERE current_account.id = t.entity_id
                AND current_account.deleted_at IS NULL
                AND current_account.schedulable = TRUE
                AND LOWER(TRIM(current_account.status)) IN ('active', 'error')
          )
      )
      AND (
          LOWER(TRIM(t.source_status)) = 'active'
          OR (t.kind = 'account' AND LOWER(TRIM(t.source_status)) = 'error')
      )
), active_accounts AS MATERIALIZED (
	SELECT id
	FROM accounts
	WHERE deleted_at IS NULL
	  AND schedulable = TRUE
	  AND LOWER(TRIM(status)) IN ('active', 'error')
), active_groups AS MATERIALIZED (
	SELECT id
	FROM groups
	WHERE deleted_at IS NULL AND LOWER(TRIM(status)) = 'active'
), active_targets AS MATERIALIZED (
    SELECT target_key, source_updated_at
    FROM visible_targets
), period_usage AS MATERIALIZED (
	SELECT ul.id, ul.account_id, ul.group_id, ul.duration_ms, ul.first_token_ms, ul.created_at
	FROM usage_logs ul
	CROSS JOIN bounds
	WHERE ul.created_at >= bounds.start_at AND ul.created_at < bounds.end_at AND ul.actual_cost > 0
), eligible_usage AS MATERIALIZED (
	SELECT ul.id, ul.account_id, ul.group_id, ul.duration_ms, ul.first_token_ms, ul.created_at
	FROM period_usage ul
	JOIN active_accounts a ON a.id = ul.account_id
	JOIN active_groups g ON g.id = ul.group_id
), latest_account_usage AS MATERIALIZED (
	SELECT DISTINCT ON (usage.account_id)
	       targets.target_key, usage.duration_ms, usage.first_token_ms, usage.created_at
	FROM eligible_usage usage
	JOIN active_targets targets ON targets.target_key = 'account:' || usage.account_id::text
	WHERE targets.source_updated_at IS NULL OR usage.created_at >= targets.source_updated_at
	ORDER BY usage.account_id, usage.created_at DESC, usage.id DESC
), latest_group_usage AS MATERIALIZED (
	SELECT DISTINCT ON (usage.group_id)
	       targets.target_key, usage.duration_ms, usage.first_token_ms, usage.created_at
	FROM eligible_usage usage
	JOIN active_targets targets ON targets.target_key = 'group:' || usage.group_id::text
	WHERE targets.source_updated_at IS NULL OR usage.created_at >= targets.source_updated_at
	ORDER BY usage.group_id, usage.created_at DESC, usage.id DESC
), group_request_rows AS MATERIALIZED (
	SELECT 'group:' || oe.group_id::text AS target_key,
	       oe.group_id, oe.id, oe.created_at,
	       oe.status_code, oe.upstream_status_code,
	       COALESCE(NULLIF(BTRIM(oe.request_id), ''),
	                NULLIF(BTRIM(oe.client_request_id), ''),
	                'error:' || oe.id::text) AS request_key,
	       oe.is_business_limited,
	       LOWER(BTRIM(COALESCE(oe.error_type, ''))) AS error_type,
	       LOWER(BTRIM(COALESCE(oe.error_owner, ''))) AS error_owner,
	       LOWER(BTRIM(COALESCE(oe.error_phase, ''))) AS error_phase,
	       LOWER(BTRIM(COALESCE(oe.error_source, ''))) AS error_source
	FROM ops_error_logs oe
	JOIN active_targets targets ON targets.target_key = 'group:' || oe.group_id::text
	CROSS JOIN bounds
	WHERE oe.group_id IS NOT NULL
	  AND oe.created_at >= bounds.start_at AND oe.created_at < bounds.end_at
	  AND (targets.source_updated_at IS NULL OR oe.created_at >= targets.source_updated_at)
), group_request_ranked AS (
	SELECT group_request_rows.*,
	       ROW_NUMBER() OVER (
	           PARTITION BY group_id, request_key
	           ORDER BY created_at DESC, id DESC
	       ) AS position
	FROM group_request_rows
), group_error_candidates AS MATERIALIZED (
	SELECT target_key, group_id, id, created_at,
	       COALESCE(NULLIF(upstream_status_code, 0), NULLIF(status_code, 0)) AS status_code,
	       request_key
	FROM group_request_ranked
	WHERE position = 1
	  AND COALESCE(is_business_limited, FALSE) = FALSE
	  AND error_type NOT IN (
	      'cyber_policy', 'client_cancelled', 'rate_limit_error', 'invalid_request_error'
	  )
	  AND COALESCE(NULLIF(upstream_status_code, 0), NULLIF(status_code, 0), 0) <> 429
	  AND (
	      COALESCE(NULLIF(upstream_status_code, 0), NULLIF(status_code, 0), 0) >= 500
	      OR (
	          COALESCE(NULLIF(upstream_status_code, 0), NULLIF(status_code, 0), 0) >= 400
	          AND error_owner = 'provider'
	          AND (
	              error_phase IN ('account_auth', 'network', 'upstream')
	              OR error_source IN ('upstream_http', 'upstream_network')
	          )
	      )
	  )
), group_error_events AS (
	SELECT target_key, group_id, id, created_at, status_code,
	       CASE WHEN status_code IS NULL THEN '最近请求失败'
	            ELSE '最近请求失败：HTTP ' || status_code::text END AS message
	FROM group_error_candidates
), latest_group_error AS MATERIALIZED (
	SELECT DISTINCT ON (target_key) target_key, created_at, status_code, message
	FROM group_error_events
	ORDER BY target_key, created_at DESC, id DESC
), samples AS (
    SELECT mc.target_key, mc.status, mc.latency_ms, mc.first_byte_ms, mc.checked_at, mc.source
    FROM monitoring_checks mc
    JOIN active_targets targets ON targets.target_key = mc.target_key
    CROSS JOIN bounds
    WHERE mc.checked_at >= bounds.start_at AND mc.checked_at < bounds.end_at
      AND (targets.source_updated_at IS NULL OR mc.checked_at >= targets.source_updated_at)
      AND (targets.target_key NOT LIKE 'group:%' OR mc.source <> 'aggregate')
	UNION ALL
	SELECT target_key, 'failed', NULL::integer, NULL::integer, created_at, 'request_error'
	FROM group_error_events
	UNION ALL
	SELECT targets.target_key,
	       CASE WHEN usage.duration_ms >= 20000 THEN 'degraded' ELSE 'operational' END,
	       usage.duration_ms, usage.first_token_ms, usage.created_at, 'history'
	FROM eligible_usage usage
	JOIN active_targets targets ON targets.target_key = 'account:' || usage.account_id::text
	WHERE targets.source_updated_at IS NULL OR usage.created_at >= targets.source_updated_at
	UNION ALL
	SELECT targets.target_key,
	       CASE WHEN usage.duration_ms >= 20000 THEN 'degraded' ELSE 'operational' END,
	       usage.duration_ms, usage.first_token_ms, usage.created_at, 'history'
	FROM eligible_usage usage
	JOIN active_targets targets ON targets.target_key = 'group:' || usage.group_id::text
	WHERE targets.source_updated_at IS NULL OR usage.created_at >= targets.source_updated_at
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
	SELECT samples.target_key, samples.status, samples.latency_ms, samples.checked_at, samples.source,
	       LEAST(23, FLOOR(EXTRACT(EPOCH FROM (samples.checked_at - bounds.start_at)) / bounds.bucket_seconds)::int) AS bucket_index
	FROM samples
	CROSS JOIN bounds
	WHERE samples.status NOT IN ('unknown','disabled')
), recent_ranked AS (
	SELECT target_key, status, latency_ms, checked_at, source, bucket_index,
	       ROW_NUMBER() OVER (
	           PARTITION BY target_key, bucket_index
	           ORDER BY checked_at DESC,
	                    CASE WHEN source = 'request_error' THEN 0
	                         WHEN source = 'history' THEN 1
	                         WHEN source = 'probe' THEN 2
	                         ELSE 3 END
	       ) AS position
	FROM recent_bucketed
), recent AS (
	SELECT targets.target_key,
	       jsonb_agg(jsonb_build_object(
	           'status', COALESCE(recent_ranked.status, 'unknown'),
	           'latency_ms', recent_ranked.latency_ms,
	           'checked_at', COALESCE(
	               recent_ranked.checked_at,
	               bounds.start_at + ((bucket_positions.bucket_index + 1) * bounds.bucket_seconds) * INTERVAL '1 second'
	           ),
	           'source', COALESCE(recent_ranked.source, '')
	       ) ORDER BY bucket_positions.bucket_index) AS samples
	FROM visible_targets targets
	CROSS JOIN bounds
	CROSS JOIN bucket_positions
	LEFT JOIN recent_ranked
	       ON recent_ranked.target_key = targets.target_key
	      AND recent_ranked.bucket_index = bucket_positions.bucket_index
	      AND recent_ranked.position = 1
	GROUP BY targets.target_key
), latest_checks AS (
    SELECT targets.target_key, checks.status, checks.latency_ms, checks.first_byte_ms,
           checks.checked_at, checks.source, checks.message
    FROM monitoring_targets targets
    LEFT JOIN LATERAL (
        SELECT mc.status, mc.latency_ms, mc.first_byte_ms, mc.checked_at, mc.source, mc.message
        FROM monitoring_checks mc
        WHERE mc.target_key = targets.target_key
          AND (targets.source_updated_at IS NULL OR mc.checked_at >= targets.source_updated_at)
        ORDER BY mc.checked_at DESC, mc.id DESC
        LIMIT 1
    ) checks ON TRUE
), latest_evidence_inputs AS (
    SELECT targets.target_key, targets.kind, targets.last_activity_at, targets.source_updated_at,
           targets.last_channel_error_at, targets.last_channel_error_class,
           targets.last_channel_error_status_code, targets.last_channel_error_resolved_at,
           latest_checks.status, latest_checks.latency_ms, latest_checks.first_byte_ms,
           latest_checks.checked_at, latest_checks.source, latest_checks.message,
           latest_account_usage.created_at AS account_success_at,
           latest_account_usage.duration_ms AS account_success_latency_ms,
           latest_account_usage.first_token_ms AS account_success_first_byte_ms,
           latest_group_usage.created_at AS group_success_at,
           latest_group_usage.duration_ms AS group_success_latency_ms,
		   latest_group_usage.first_token_ms AS group_success_first_byte_ms,
		   latest_group_error.created_at AS group_error_at,
           latest_group_error.message AS group_error_message,
            CASE WHEN targets.kind = 'group'
                       AND latest_group_error.created_at IS NOT NULL
                       AND (latest_group_usage.created_at IS NULL
                            OR latest_group_error.created_at >= latest_group_usage.created_at)
                 THEN TRUE ELSE FALSE END AS group_error_wins,
            CASE WHEN targets.kind = 'group'
                       AND latest_group_usage.created_at IS NOT NULL
                       AND (latest_group_error.created_at IS NULL
                            OR latest_group_usage.created_at > latest_group_error.created_at)
                 THEN TRUE ELSE FALSE END AS group_success_wins,
            CASE WHEN targets.last_channel_error_at IS NOT NULL
                       AND (targets.source_updated_at IS NULL
                            OR targets.last_channel_error_at >= targets.source_updated_at)
                       AND (targets.last_channel_error_resolved_at IS NULL
                            OR targets.last_channel_error_at > targets.last_channel_error_resolved_at)
                       AND (latest_checks.checked_at IS NULL
                            OR targets.last_channel_error_at > latest_checks.checked_at)
                       AND (targets.last_activity_at IS NULL
                            OR targets.last_channel_error_at > targets.last_activity_at)
                 THEN TRUE ELSE FALSE END AS channel_error_wins,
            CASE WHEN targets.last_channel_error_at IS NOT NULL
                       AND (targets.source_updated_at IS NULL
                            OR targets.last_channel_error_at >= targets.source_updated_at)
                       AND (targets.last_channel_error_resolved_at IS NULL
                            OR targets.last_channel_error_at > targets.last_channel_error_resolved_at)
                       AND (targets.last_activity_at IS NULL
                            OR targets.last_activity_at < targets.last_channel_error_at)
                       AND (
                           latest_checks.checked_at IS NULL
                           OR latest_checks.checked_at < targets.last_channel_error_at
                           OR COALESCE(latest_checks.status, '') NOT IN ('operational', 'degraded')
                       )
                 THEN TRUE ELSE FALSE END AS recovery_active,
           CASE WHEN targets.last_activity_at IS NOT NULL
                        AND (targets.source_updated_at IS NULL
                             OR targets.last_activity_at >= targets.source_updated_at)
                       AND (latest_checks.checked_at IS NULL
                           OR targets.last_activity_at >= latest_checks.checked_at)
                THEN TRUE ELSE FALSE END AS history_wins
    FROM monitoring_targets targets
    CROSS JOIN bounds
    LEFT JOIN latest_checks ON latest_checks.target_key = targets.target_key
	LEFT JOIN latest_account_usage ON latest_account_usage.target_key = targets.target_key
	LEFT JOIN latest_group_usage ON latest_group_usage.target_key = targets.target_key
	LEFT JOIN latest_group_error ON latest_group_error.target_key = targets.target_key
), latest_evidence AS (
    SELECT target_key,
           CASE WHEN kind = 'group' AND group_error_wins THEN 'failed'
                WHEN kind = 'group' AND group_success_wins
                THEN CASE WHEN COALESCE(group_success_latency_ms, 0) >= 20000
                          THEN 'degraded' ELSE 'operational' END
                WHEN kind = 'group' THEN NULL
                WHEN channel_error_wins THEN 'failed'
                WHEN history_wins AND kind = 'account'
                THEN CASE WHEN COALESCE(account_success_latency_ms, 0) >= 20000
                          THEN 'degraded' ELSE 'operational' END
                WHEN history_wins THEN 'operational'
                ELSE status END AS status,
           CASE WHEN kind = 'group' AND group_success_wins THEN group_success_latency_ms
                WHEN kind = 'group' THEN NULL
                WHEN kind = 'account' AND history_wins THEN account_success_latency_ms
                WHEN channel_error_wins OR history_wins THEN NULL ELSE latency_ms END AS latency_ms,
           CASE WHEN kind = 'group' AND group_success_wins THEN group_success_first_byte_ms
                WHEN kind = 'group' THEN NULL
                WHEN kind = 'account' AND history_wins THEN account_success_first_byte_ms
                WHEN channel_error_wins OR history_wins THEN NULL ELSE first_byte_ms END AS first_byte_ms,
           CASE WHEN kind = 'group' AND group_error_wins THEN group_error_at
                WHEN kind = 'group' AND group_success_wins THEN group_success_at
                WHEN kind = 'group' THEN NULL
                WHEN channel_error_wins THEN last_channel_error_at
                WHEN history_wins AND kind = 'account' AND account_success_at IS NOT NULL THEN account_success_at
                WHEN history_wins THEN last_activity_at ELSE checked_at END AS checked_at,
           CASE WHEN recovery_active THEN last_channel_error_at ELSE NULL END AS recovery_trigger_at,
           CASE WHEN kind = 'group' AND group_error_wins THEN 'request_error'
                WHEN kind = 'group' AND group_success_wins THEN 'history'
                WHEN kind = 'group' THEN NULL
                WHEN channel_error_wins THEN 'request_error'
                WHEN history_wins THEN 'history'
                ELSE source END AS source,
           CASE WHEN kind = 'group' AND group_error_wins THEN group_error_message
                WHEN kind = 'group' AND group_success_wins THEN '最近成功请求'
                WHEN kind = 'group' THEN NULL
                WHEN channel_error_wins THEN '真实请求报错，等待恢复探测'
                WHEN history_wins THEN '近期真实请求'
                ELSE message END AS message
    FROM latest_evidence_inputs
)
SELECT t.target_key, t.kind, t.entity_id, t.name, t.platform, t.source_status, t.probe_enabled,
       e.recovery_trigger_at,
       CASE
           WHEN t.kind = 'group' THEN g.rate_multiplier::double precision
           WHEN t.kind = 'account' THEN a.rate_multiplier::double precision
       END,
       e.status, e.latency_ms, e.first_byte_ms, e.checked_at, e.source, e.message,
       COALESCE(s.samples,0), COALESCE(s.successful,0),
       s.first_fastest, s.first_median, s.first_p95,
       s.latency_fastest, s.latency_median, s.latency_p95,
       COALESCE(r.samples, '[]'::jsonb)
FROM monitoring_targets t
JOIN visible_targets visible
  ON visible.target_key = t.target_key
CROSS JOIN bounds
LEFT JOIN accounts a ON t.kind = 'account' AND a.id = t.entity_id AND a.deleted_at IS NULL
LEFT JOIN groups g ON t.kind = 'group' AND g.id = t.entity_id AND g.deleted_at IS NULL
LEFT JOIN latest_evidence e ON e.target_key = t.target_key
LEFT JOIN stats s ON s.target_key = t.target_key
LEFT JOIN recent r ON r.target_key = t.target_key
WHERE t.active = TRUE
  AND (
      LOWER(TRIM(t.source_status)) = 'active'
      OR (t.kind = 'account' AND LOWER(TRIM(t.source_status)) = 'error')
  )
ORDER BY CASE WHEN t.kind = 'group' THEN 0 ELSE 1 END, t.name, t.entity_id`

func (s *Store) Dashboard(ctx context.Context, staleAfter time.Duration, intervalSec int, _ ...int) (model.Dashboard, error) {
	rows, err := s.db.QueryContext(ctx, dashboardQuery)
	if err != nil {
		return model.Dashboard{}, fmt.Errorf("load dashboard: %w", err)
	}
	defer rows.Close()
	return buildDashboard(rows, staleAfter, intervalSec)
}

func buildDashboard(rows *sql.Rows, staleAfter time.Duration, intervalSec int) (model.Dashboard, error) {
	now := time.Now().UTC()
	dashboard := model.Dashboard{
		GeneratedAt: now, IntervalSec: intervalSec,
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
	var currentRate sql.NullFloat64
	var recoveryTrigger sql.NullTime
	if err := rows.Scan(
		&target.Key, &target.Kind, &target.EntityID, &target.Name, &target.Platform,
		&target.SourceStatus, &target.ProbeEnabled, &recoveryTrigger,
		&currentRate,
		&latestStatus, &latestLatency,
		&latestFirst, &latestAt, &latestSource, &latestMessage, &samples, &successful,
		&firstFastest, &firstMedian, &firstP95, &latencyFastest,
		&latencyMedian, &latencyP95, &recentJSON,
	); err != nil {
		return model.DashboardTarget{}, false, fmt.Errorf("scan dashboard: %w", err)
	}
	if currentRate.Valid {
		value := currentRate.Float64
		target.RateMultiplier = &value
	}
	if recoveryTrigger.Valid {
		value := recoveryTrigger.Time.UTC()
		target.RecoveryTriggerAt = &value
	}
	applyLatestTargetStateWithMessage(&target, latestStatus, latestSource, latestMessage, latestLatency, latestFirst, latestAt, now, staleAfter)
	target.Stats = targetStats(samples, successful, firstFastest, firstMedian, firstP95, latencyFastest, latencyMedian, latencyP95)
	if err := json.Unmarshal(recentJSON, &target.RecentSamples); err != nil {
		return model.DashboardTarget{}, false, fmt.Errorf("decode recent samples: %w", err)
	}
	carryForwardStatusSamples(target.RecentSamples)
	if target.LastCheckedAt != nil && isObservedStatus(target.Status) {
		// A target can have valid evidence older than the fixed 24-hour
		// display window. Keep the trajectory continuous by using that latest
		// status as a carried baseline for any buckets still empty after the
		// in-window carry-forward pass. The carried marker makes the age of the
		// evidence visible without pretending those buckets were freshly probed.
		carrySource := target.LatestSource
		if carrySource == "" {
			carrySource = "cache"
		}
		carryForwardTargetStatus(target.RecentSamples, target.Status, carrySource, *target.LastCheckedAt)
		if target.LatestSource == "request_error" &&
			!target.LastCheckedAt.Before(now.Add(-dashboardWindow)) {
			// Channel errors are stored as trigger metadata rather than samples.
			// Overlay the winning error on the current and later buckets so the
			// timeline cannot remain green merely because an older sample exists.
			overlayLatestTargetStatus(target.RecentSamples, target.Status, target.LatestSource, *target.LastCheckedAt, now.Add(-dashboardWindow))
		}
	}
	return target, targetContributesAvailability(target), nil
}

func carryForwardStatusSamples(samples []model.StatusSample) {
	// Carry a known state forward through gaps after the first observation.
	// Leading buckets stay unknown internally: an observation later in the
	// window cannot prove what happened before it. The web layer renders those
	// empty buckets with a neutral tone, so the user does not see an invented
	// historical failure or a falsely green baseline.
	var previous *model.StatusSample
	for i := range samples {
		sample := &samples[i]
		if isObservedStatus(sample.Status) {
			observed := *sample
			previous = &observed
			continue
		}
		if previous != nil {
			carryStatusSample(sample, *previous)
		}
	}
}

func carryForwardTargetStatus(samples []model.StatusSample, status, source string, checkedAt time.Time) {
	if !isObservedStatus(status) || checkedAt.IsZero() {
		return
	}
	baseline := model.StatusSample{Status: status, Source: source, CheckedAt: checkedAt}
	for index := range samples {
		// Preserve both real observations and gaps that were already carried
		// from an earlier observation. Only genuinely empty buckets at or after
		// the target-level evidence can use this baseline. In particular, do not
		// paint buckets before a first failure red.
		if isObservedStatus(samples[index].Status) || samples[index].CarriedFrom != nil {
			continue
		}
		if samples[index].CheckedAt.Before(checkedAt) {
			continue
		}
		carryStatusSample(&samples[index], baseline)
	}
}

func carryStatusSample(sample *model.StatusSample, previous model.StatusSample) {
	if sample == nil {
		return
	}
	sample.Status = previous.Status
	sample.Source = previous.Source
	if previous.LatencyMs == nil {
		sample.LatencyMs = nil
	} else {
		latency := *previous.LatencyMs
		sample.LatencyMs = &latency
	}
	carriedFrom := previous.CheckedAt
	sample.CarriedFrom = &carriedFrom
}

func overlayLatestTargetStatus(samples []model.StatusSample, status, source string, checkedAt, windowStart time.Time) {
	if checkedAt.Before(windowStart) {
		return
	}
	for index := range samples {
		sample := &samples[index]
		if sample.CheckedAt.Before(checkedAt) {
			// Empty buckets use their end time as CheckedAt. Include the bucket
			// containing the trigger, while preserving real observations before it.
			if sample.CarriedFrom == nil || !sample.CheckedAt.Add(dashboardBucket).After(checkedAt) {
				continue
			}
		}
		sample.Status = status
		sample.Source = source
		sample.CarriedFrom = nil
	}
}

func hasObservedStatusSample(samples []model.StatusSample) bool {
	for _, sample := range samples {
		if isObservedStatus(sample.Status) {
			return true
		}
	}
	return false
}

func isObservedStatus(status string) bool {
	switch status {
	case model.StatusOperational, model.StatusDegraded, model.StatusFailed, model.StatusError:
		return true
	default:
		return false
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
		if target.Kind == model.KindGroup && groupIsActive(target.SourceStatus) {
			target.Status = model.StatusFailed
			target.LatestMessage = "无启用渠道或可调度候选"
		} else {
			target.Status = model.StatusDisabled
		}
		return
	}
	if status.Valid {
		target.Status = status.String
		if target.Status == model.StatusUnknown {
			if target.Kind == model.KindGroup {
				target.LatestMessage = "暂无真实请求，等待下一次请求确认"
			} else if strings.EqualFold(strings.TrimSpace(target.SourceStatus), "error") {
				if target.RecoveryTriggerAt != nil {
					target.LatestMessage = "渠道报错，等待恢复探测"
				} else {
					target.LatestMessage = "账户处于错误状态；等待真实请求或新的渠道错误"
				}
			} else {
				target.LatestMessage = "暂无真实请求，等待渠道错误或下一次请求确认"
			}
		}
	} else {
		if target.Kind == model.KindGroup {
			target.LatestMessage = "暂无真实请求，等待下一次请求确认"
		} else if strings.EqualFold(strings.TrimSpace(target.SourceStatus), "error") {
			if target.RecoveryTriggerAt != nil {
				target.LatestMessage = "渠道报错，等待恢复探测"
			} else {
				target.LatestMessage = "账户处于错误状态；等待真实请求或新的渠道错误"
			}
		} else {
			target.LatestMessage = "暂无真实请求；仅在渠道报错后主动探测"
		}
	}
	if checkedAt.Valid {
		value := checkedAt.Time.UTC()
		target.LastCheckedAt = &value
		target.Stale = now.Sub(value) > staleAfter
	}
	if source.Valid {
		target.LatestSource = source.String
	}
	if message.Valid && (target.Kind == model.KindGroup || (source.Valid && source.String == "request_error")) {
		if sanitized := sanitizeUpstreamMessage(message.String); sanitized != "" {
			target.LatestMessage = sanitized
		}
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
	if targetContributesAvailability(target) {
		summary.Availability += targetAvailabilityValue(target)
	}
}

func targetContributesAvailability(target model.DashboardTarget) bool {
	if target.Kind == model.KindAccount && strings.EqualFold(strings.TrimSpace(target.SourceStatus), "error") {
		return false
	}
	if target.Kind == model.KindGroup && groupIsActive(target.SourceStatus) && !target.ProbeEnabled {
		return true
	}
	return target.ProbeEnabled && target.Stats.Samples > 0
}

func targetAvailabilityValue(target model.DashboardTarget) float64 {
	if target.Kind == model.KindGroup && groupIsActive(target.SourceStatus) && !target.ProbeEnabled {
		return 0
	}
	return target.Stats.Availability
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
