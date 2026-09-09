package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/DaisWW/Sub2ApiExt/monitoring/internal/model"
	"github.com/DaisWW/Sub2ApiExt/monitoring/internal/stats"
)

func (s *Store) Dashboard(ctx context.Context, staleAfter time.Duration, intervalSec int) (model.Dashboard, error) {
	rows, err := s.db.QueryContext(ctx, dashboardQuery)
	if err != nil {
		return model.Dashboard{}, fmt.Errorf("load dashboard: %w", err)
	}
	defer rows.Close()
	dashboard, err := buildDashboard(rows, staleAfter, intervalSec)
	if err != nil {
		return model.Dashboard{}, err
	}
	return dashboard, nil
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

const dashboardWindow = 24 * time.Hour
const dashboardBucket = dashboardWindow / 24

const dashboardQuery = dashboardQuerySources + dashboardQueryCurrentHealth + dashboardQueryHistory + dashboardQueryEvidence

const dashboardQuerySources = `
WITH bounds AS (
    SELECT NOW() - INTERVAL '24 hours' AS start_at,
           NOW() AS end_at,
           EXTRACT(EPOCH FROM INTERVAL '1 hour') AS bucket_seconds
), visible_targets AS MATERIALIZED (
    SELECT t.target_key, t.kind, t.entity_id, t.source_status, t.last_activity_at,
           t.last_channel_error_at, t.last_channel_error_resolved_at,
           t.source_updated_at
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
	SELECT id, LOWER(TRIM(status)) AS status
	FROM accounts
	WHERE deleted_at IS NULL
	  AND schedulable = TRUE
	  AND LOWER(TRIM(status)) IN ('active', 'error')
), active_groups AS MATERIALIZED (
	SELECT id
	FROM groups
	WHERE deleted_at IS NULL AND LOWER(TRIM(status)) = 'active'
), active_targets AS MATERIALIZED (
    SELECT target_key, kind, entity_id, source_status, last_activity_at,
           last_channel_error_at, last_channel_error_resolved_at,
           source_updated_at
    FROM visible_targets
), group_route_state AS MATERIALIZED (
    SELECT groups.id,
           EXISTS (
               SELECT 1
               FROM channel_groups cg
               JOIN channels c ON c.id = cg.channel_id
               WHERE cg.group_id = groups.id
                 AND LOWER(TRIM(c.status)) = 'active'
           ) AS has_active_channel,
           EXISTS (
               SELECT 1
               FROM account_groups ag
               JOIN accounts a ON a.id = ag.account_id
               WHERE ag.group_id = groups.id
                 AND a.deleted_at IS NULL
                 AND a.schedulable = TRUE
                 AND LOWER(TRIM(a.status)) = 'active'
           ) AS has_schedulable_account
    FROM active_groups groups
), route_state AS MATERIALIZED (
    SELECT targets.target_key,
           CASE
               WHEN targets.kind = 'account' THEN LOWER(TRIM(targets.source_status)) = 'active'
               WHEN targets.kind = 'group' THEN COALESCE(routes.has_active_channel, FALSE)
                    AND COALESCE(routes.has_schedulable_account, FALSE)
               ELSE FALSE
           END AS route_configured,
           CASE
               WHEN targets.kind = 'account' AND LOWER(TRIM(targets.source_status)) = 'active'
                   THEN '账户可调度'
               WHEN targets.kind = 'account' THEN '账户处于错误状态'
               WHEN COALESCE(routes.has_active_channel, FALSE) = FALSE
                   THEN '无启用渠道'
               WHEN COALESCE(routes.has_schedulable_account, FALSE) = FALSE
                   THEN '无可调度账户'
               ELSE '已配置可用路由'
           END AS route_message
    FROM active_targets targets
    LEFT JOIN group_route_state routes
           ON routes.id = targets.entity_id AND targets.kind = 'group'
), period_usage AS MATERIALIZED (
	SELECT ul.id, ul.account_id, ul.group_id, ul.duration_ms, ul.first_token_ms, ul.created_at,
	       ul.request_id
	FROM usage_logs ul
	CROSS JOIN bounds
	WHERE ul.created_at >= bounds.start_at AND ul.created_at < bounds.end_at AND ul.actual_cost > 0

), account_usage AS MATERIALIZED (
	SELECT ul.id, ul.account_id, ul.group_id, ul.duration_ms, ul.first_token_ms, ul.created_at,
	       ul.request_id,
	       CASE
	           WHEN NULLIF(BTRIM(ul.request_id), '') IS NULL THEN 'usage:' || ul.id::text
	           ELSE 'request:' || LOWER(REGEXP_REPLACE(BTRIM(ul.request_id), '^client:', '', 'i'))
	       END AS request_key
	FROM period_usage ul
	JOIN active_accounts a ON a.id = ul.account_id
), latest_account_usage AS MATERIALIZED (
	SELECT DISTINCT ON (candidate.target_key)
	       candidate.target_key, candidate.duration_ms, candidate.first_token_ms, candidate.created_at
	FROM (
		SELECT targets.target_key, usage.id, usage.duration_ms, usage.first_token_ms, usage.created_at
		FROM account_usage usage
		JOIN active_targets targets ON targets.target_key = 'account:' || usage.account_id::text
		WHERE targets.kind = 'account'
		  AND (targets.source_updated_at IS NULL
		       OR usage.created_at >= targets.source_updated_at - INTERVAL '2 minutes')
		UNION ALL
		SELECT targets.target_key, history_usage.id, history_usage.duration_ms,
		       history_usage.first_token_ms, history_usage.created_at
		FROM active_targets targets
		CROSS JOIN bounds
		JOIN usage_logs history_usage
		  ON history_usage.account_id = targets.entity_id
		 AND history_usage.created_at = targets.last_activity_at
		 AND history_usage.created_at < bounds.end_at
		 AND history_usage.actual_cost > 0
		WHERE targets.kind = 'account'
		  AND targets.last_activity_at IS NOT NULL
		  AND (targets.source_updated_at IS NULL
		       OR history_usage.created_at >= targets.source_updated_at - INTERVAL '2 minutes')
	) candidate
	ORDER BY candidate.target_key, candidate.created_at DESC, candidate.id DESC
), account_error_events AS MATERIALIZED (
	SELECT targets.target_key, targets.last_channel_error_at AS created_at
	FROM monitoring_targets targets
	JOIN visible_targets visible ON visible.target_key = targets.target_key
	CROSS JOIN bounds
	WHERE targets.kind = 'account'
	  AND targets.last_channel_error_at IS NOT NULL
	  AND targets.last_channel_error_at >= bounds.start_at
	  AND targets.last_channel_error_at < bounds.end_at
	  AND (targets.source_updated_at IS NULL
	       OR targets.last_channel_error_at >= targets.source_updated_at - INTERVAL '2 minutes')
	  AND (targets.last_channel_error_resolved_at IS NULL
	       OR targets.last_channel_error_at > targets.last_channel_error_resolved_at)
`

const dashboardQueryCurrentHealth = `
), current_window AS MATERIALIZED (
	SELECT NOW() - INTERVAL '5 minutes' AS start_at,
	       NOW() AS end_at,
	       300::integer AS window_seconds
), current_success_requests AS MATERIALIZED (
	SELECT DISTINCT ON (usage.account_id, usage.request_key)
	       usage.id, usage.account_id, usage.request_key, usage.duration_ms,
	       usage.first_token_ms, usage.created_at
	FROM account_usage usage
	JOIN active_targets targets ON targets.target_key = 'account:' || usage.account_id::text
	CROSS JOIN current_window cw
	WHERE usage.created_at >= cw.start_at
	  AND usage.created_at < cw.end_at
	  AND (targets.source_updated_at IS NULL
	       OR usage.created_at >= targets.source_updated_at - INTERVAL '2 minutes')
	ORDER BY usage.account_id, usage.request_key, usage.created_at DESC, usage.id DESC
), current_error_rows AS MATERIALIZED (
	SELECT errors.id, errors.account_id,
	       CASE
	           WHEN COALESCE(
	                    NULLIF(BTRIM(errors.client_request_id), ''),
	                    NULLIF(BTRIM(errors.request_id), '')
	                ) IS NULL THEN 'error:' || errors.id::text
	           ELSE 'request:' || LOWER(REGEXP_REPLACE(
	                    COALESCE(
	                        NULLIF(BTRIM(errors.client_request_id), ''),
	                        NULLIF(BTRIM(errors.request_id), '')
	                    ),
	                    '^client:', '', 'i'
	                ))
	       END AS request_key,
	       errors.created_at,
	       CASE WHEN errors.upstream_status_code = 429
	               OR errors.status_code = 429
                  OR LOWER(BTRIM(COALESCE(errors.error_type, ''))) IN ('rate_limit_error', 'rate_limited', 'rate_limit')
            THEN TRUE ELSE FALSE END AS rate_limited
	FROM ops_error_logs errors
	JOIN active_accounts accounts ON accounts.id = errors.account_id
	JOIN active_targets targets ON targets.target_key = 'account:' || errors.account_id::text
	CROSS JOIN current_window cw
	WHERE errors.created_at >= cw.start_at
	  AND errors.created_at < cw.end_at
	  AND (targets.source_updated_at IS NULL
	       OR errors.created_at >= targets.source_updated_at - INTERVAL '2 minutes')
	  AND COALESCE(errors.is_business_limited, FALSE) = FALSE
	  AND LOWER(BTRIM(COALESCE(errors.error_type, ''))) NOT IN (
	      'cyber_policy', 'client_cancelled', 'invalid_request_error'
	  )
	  AND (
	      LOWER(BTRIM(COALESCE(errors.error_owner, ''))) = 'provider'
	      OR LOWER(BTRIM(COALESCE(errors.error_phase, ''))) IN ('account_auth', 'network', 'upstream')
	      OR LOWER(BTRIM(COALESCE(errors.error_source, ''))) IN ('upstream_http', 'upstream_network')
	  )
), current_error_requests AS MATERIALIZED (
	SELECT errors.account_id, errors.request_key, MAX(errors.created_at) AS error_at,
	       COUNT(*)::integer AS attempts,
	       COUNT(*) FILTER (WHERE errors.rate_limited)::integer AS rate_limited_attempts,
	       BOOL_OR(NOT errors.rate_limited) AS hard_failure
	FROM current_error_rows errors
	GROUP BY errors.account_id, errors.request_key
), current_error_health AS MATERIALIZED (
	SELECT errors.account_id,
	       COUNT(*) FILTER (WHERE success.request_key IS NULL)::integer AS final_failures,
	       COUNT(*) FILTER (WHERE success.request_key IS NULL AND errors.hard_failure)::integer AS hard_failures,
	       COALESCE(SUM(errors.attempts), 0)::integer AS attempts,
	       COALESCE(SUM(errors.rate_limited_attempts), 0)::integer AS rate_limited,
	       MAX(errors.error_at) AS latest_at
	FROM current_error_requests errors
	LEFT JOIN current_success_requests success
	       ON success.account_id = errors.account_id
	      AND success.request_key = errors.request_key
	      AND success.created_at >= errors.error_at
	GROUP BY errors.account_id
), current_account_success_health AS MATERIALIZED (
	SELECT success.account_id,
	       COUNT(*)::integer AS successful,
	       MAX(success.created_at) AS latest_at,
	       MIN(success.duration_ms) FILTER (WHERE success.duration_ms IS NOT NULL) AS latency_fastest,
	       percentile_cont(0.5) WITHIN GROUP (ORDER BY success.duration_ms)
	           FILTER (WHERE success.duration_ms IS NOT NULL) AS latency_median,
	       percentile_cont(0.95) WITHIN GROUP (ORDER BY success.duration_ms)
	           FILTER (WHERE success.duration_ms IS NOT NULL) AS latency_p95
	FROM current_success_requests success
	GROUP BY success.account_id
), current_account_health AS MATERIALIZED (
	SELECT COALESCE(success.account_id, errors.account_id) AS account_id,
	       COALESCE(success.successful, 0) + COALESCE(errors.final_failures, 0) AS samples,
	       COALESCE(success.successful, 0) AS successful,
	       COALESCE(errors.rate_limited, 0) AS rate_limited,
	       COALESCE(errors.hard_failures, 0) AS hard_failures,
	       COALESCE(success.successful, 0) + COALESCE(errors.attempts, 0) AS attempts,
	       CASE
	           WHEN success.latest_at IS NULL THEN errors.latest_at
	           WHEN errors.latest_at IS NULL THEN success.latest_at
	           WHEN success.latest_at >= errors.latest_at THEN success.latest_at
	           ELSE errors.latest_at
	       END AS latest_at,
	       success.latency_fastest, success.latency_median, success.latency_p95
	FROM current_account_success_health success
	FULL OUTER JOIN current_error_health errors ON errors.account_id = success.account_id
), active_group_members AS MATERIALIZED (
	SELECT DISTINCT ag.group_id, ag.account_id
	FROM account_groups ag
	JOIN active_groups groups ON groups.id = ag.group_id
	JOIN active_accounts accounts
	  ON accounts.id = ag.account_id
	 AND LOWER(TRIM(accounts.status)) = 'active'
), current_group_counts AS MATERIALIZED (
	SELECT members.group_id,
	       COALESCE(SUM(health.samples), 0)::integer AS samples,
	       COALESCE(SUM(health.successful), 0)::integer AS successful,
	       COALESCE(SUM(health.rate_limited), 0)::integer AS rate_limited,
	       COALESCE(SUM(health.hard_failures), 0)::integer AS hard_failures,
	       COALESCE(SUM(health.attempts), 0)::integer AS attempts,
	       MAX(health.latest_at) AS latest_at,
	       COUNT(DISTINCT members.account_id) FILTER (WHERE health.rate_limited > 0)::integer AS affected_accounts,
	       COUNT(DISTINCT members.account_id) FILTER (WHERE health.samples > 0)::integer AS observed_accounts,
	       COUNT(DISTINCT members.account_id)::integer AS member_accounts
	FROM active_group_members members
	LEFT JOIN current_account_health health ON health.account_id = members.account_id
	GROUP BY members.group_id
), current_group_latency AS MATERIALIZED (
	SELECT members.group_id,
	       MIN(success.duration_ms) FILTER (WHERE success.duration_ms IS NOT NULL) AS latency_fastest,
	       percentile_cont(0.5) WITHIN GROUP (ORDER BY success.duration_ms)
	           FILTER (WHERE success.duration_ms IS NOT NULL) AS latency_median,
	       percentile_cont(0.95) WITHIN GROUP (ORDER BY success.duration_ms)
	           FILTER (WHERE success.duration_ms IS NOT NULL) AS latency_p95
	FROM active_group_members members
	JOIN current_success_requests success ON success.account_id = members.account_id
	GROUP BY members.group_id
), current_group_health AS MATERIALIZED (
	SELECT counts.group_id, counts.samples, counts.successful, counts.rate_limited,
	       counts.hard_failures, counts.attempts, counts.latest_at,
	       latency.latency_fastest, latency.latency_median, latency.latency_p95,
	       counts.affected_accounts, counts.observed_accounts, counts.member_accounts
	FROM current_group_counts counts
	LEFT JOIN current_group_latency latency ON latency.group_id = counts.group_id
), current_health AS MATERIALIZED (
	SELECT 'account:' || health.account_id::text AS target_key,
	       300::integer AS window_seconds, health.samples, health.successful,
	       health.rate_limited, health.hard_failures, health.attempts, health.latest_at,
	       health.latency_fastest, health.latency_median, health.latency_p95,
	       0::integer AS affected_accounts, 1::integer AS observed_accounts, 1::integer AS member_accounts
	FROM current_account_health health
	UNION ALL
	SELECT 'group:' || health.group_id::text AS target_key,
	       300::integer AS window_seconds, health.samples, health.successful,
	       health.rate_limited, health.hard_failures, health.attempts, health.latest_at,
	       health.latency_fastest, health.latency_median, health.latency_p95,
	       health.affected_accounts, health.observed_accounts, health.member_accounts
	FROM current_group_health health
`

const dashboardQueryHistory = `
), samples AS (
    SELECT mc.target_key, mc.status, mc.health_reason, mc.latency_ms, mc.first_byte_ms, mc.checked_at, mc.source
    FROM monitoring_checks mc
    JOIN active_targets targets ON targets.target_key = mc.target_key
    CROSS JOIN bounds
    WHERE mc.checked_at >= bounds.start_at AND mc.checked_at < bounds.end_at
      AND ((targets.kind = 'account' AND mc.source IN ('probe', 'request_error'))
           OR (targets.kind = 'group' AND mc.source = 'aggregate'))
	UNION ALL
	SELECT target_key, 'failed', 'upstream_error', NULL::integer, NULL::integer, created_at, 'request_error'
	FROM account_error_events errors
	WHERE NOT EXISTS (
		SELECT 1
		FROM monitoring_checks consumed
		WHERE consumed.target_key = errors.target_key
		  AND consumed.kind = 'account'
		  AND consumed.source = 'request_error'
		  AND consumed.checked_at = errors.created_at
	)
	UNION ALL
	SELECT targets.target_key,
	       CASE WHEN usage.duration_ms >= 20000 THEN 'degraded' ELSE 'operational' END,
	       CASE WHEN usage.duration_ms >= 20000 THEN 'slow' ELSE '' END,
	       usage.duration_ms, usage.first_token_ms, usage.created_at, 'history'
	FROM account_usage usage
	JOIN active_targets targets ON targets.target_key = 'account:' || usage.account_id::text
), stats AS (
    SELECT samples.target_key,
           COUNT(*) FILTER (WHERE samples.status NOT IN ('unknown','disabled')) AS samples,
           COUNT(*) FILTER (WHERE samples.status IN ('operational','degraded')) AS successful,
	       COUNT(*) FILTER (WHERE samples.health_reason = 'rate_limited') AS rate_limited,
	       COUNT(*) FILTER (WHERE samples.status IN ('failed','error')
	                        AND COALESCE(samples.health_reason, '') <> 'rate_limited') AS hard_failures,
	       MIN(samples.first_byte_ms) FILTER (WHERE samples.status IN ('operational','degraded') AND samples.first_byte_ms IS NOT NULL) AS first_fastest,
	       percentile_cont(0.5) WITHIN GROUP (ORDER BY samples.first_byte_ms) FILTER (WHERE samples.status IN ('operational','degraded') AND samples.first_byte_ms IS NOT NULL) AS first_median,
	       percentile_cont(0.95) WITHIN GROUP (ORDER BY samples.first_byte_ms) FILTER (WHERE samples.status IN ('operational','degraded') AND samples.first_byte_ms IS NOT NULL) AS first_p95,
	       MIN(samples.latency_ms) FILTER (WHERE samples.status IN ('operational','degraded') AND samples.latency_ms IS NOT NULL) AS latency_fastest,
	       percentile_cont(0.5) WITHIN GROUP (ORDER BY samples.latency_ms) FILTER (WHERE samples.status IN ('operational','degraded') AND samples.latency_ms IS NOT NULL) AS latency_median,
	       percentile_cont(0.95) WITHIN GROUP (ORDER BY samples.latency_ms) FILTER (WHERE samples.status IN ('operational','degraded') AND samples.latency_ms IS NOT NULL) AS latency_p95
    FROM samples
	JOIN active_targets targets ON targets.target_key = samples.target_key
    CROSS JOIN bounds
    WHERE samples.checked_at >= bounds.end_at - bounds.bucket_seconds * INTERVAL '1 second'
	  AND (targets.source_updated_at IS NULL
	       OR samples.checked_at >= targets.source_updated_at - INTERVAL '2 minutes')
    GROUP BY samples.target_key
), baseline_checks AS MATERIALIZED (
	SELECT DISTINCT ON (mc.target_key)
	       mc.target_key, mc.status, mc.health_reason, mc.latency_ms, mc.checked_at, mc.source
	FROM monitoring_checks mc
	JOIN active_targets targets ON targets.target_key = mc.target_key
	CROSS JOIN bounds
	WHERE mc.checked_at < bounds.start_at
	  AND (targets.source_updated_at IS NULL
	       OR targets.source_updated_at > bounds.start_at
	       OR mc.checked_at >= targets.source_updated_at - INTERVAL '2 minutes')
	  AND ((targets.kind = 'account' AND mc.source IN ('probe', 'history', 'request_error'))
	       OR (targets.kind = 'group' AND mc.source = 'aggregate'))
	  AND mc.status NOT IN ('unknown', 'disabled')
	ORDER BY mc.target_key, mc.checked_at DESC,
	         CASE WHEN mc.source = 'request_error' THEN 0
	              WHEN mc.source = 'history' THEN 1
	              WHEN mc.source = 'probe' THEN 2
	              ELSE 3 END,
	         mc.id DESC
), baseline_account_error AS (
	SELECT targets.target_key, 'failed' AS status, 'upstream_error' AS health_reason, NULL::integer AS latency_ms,
	       targets.last_channel_error_at AS checked_at, 'request_error' AS source
	FROM active_targets targets
	CROSS JOIN bounds
	WHERE targets.kind = 'account'
	  AND targets.last_channel_error_at IS NOT NULL
	  AND targets.last_channel_error_at < bounds.start_at
	  AND (targets.source_updated_at IS NULL
	       OR targets.source_updated_at > bounds.start_at
	       OR targets.last_channel_error_at >= targets.source_updated_at - INTERVAL '2 minutes')
	  AND (targets.last_channel_error_resolved_at IS NULL
	       OR targets.last_channel_error_at > targets.last_channel_error_resolved_at)
), baseline_candidates AS (
	SELECT target_key, status, health_reason, latency_ms, checked_at, source FROM baseline_checks
	UNION ALL
	SELECT target_key, status, health_reason, latency_ms, checked_at, source FROM baseline_account_error
), baseline_ranked AS (
	SELECT target_key, status, health_reason, latency_ms, checked_at, source,
	       ROW_NUMBER() OVER (
	           PARTITION BY target_key
	           ORDER BY checked_at DESC,
	                    CASE WHEN source = 'request_error' THEN 0
	                         WHEN source = 'history' THEN 1
	                         WHEN source = 'probe' THEN 2
	                         ELSE 3 END
	       ) AS position
	FROM baseline_candidates
), baseline_samples AS (
	SELECT baseline_ranked.target_key, baseline_ranked.status, baseline_ranked.health_reason, baseline_ranked.latency_ms,
	       bounds.start_at AS checked_at, baseline_ranked.source,
	       baseline_ranked.checked_at AS carried_from
	FROM baseline_ranked
	CROSS JOIN bounds
	WHERE baseline_ranked.position = 1
), recent_samples AS (
	SELECT target_key, status, health_reason, latency_ms, checked_at, source,
	       NULL::timestamptz AS carried_from
	FROM samples
	UNION ALL
	SELECT target_key, status, health_reason, latency_ms, checked_at, source, carried_from
	FROM baseline_samples
	UNION ALL
	SELECT targets.target_key, 'unknown', '', NULL::integer, targets.source_updated_at,
	       'source_change', NULL::timestamptz
	FROM active_targets targets
	CROSS JOIN bounds
	WHERE targets.source_updated_at >= bounds.start_at
	  AND targets.source_updated_at < bounds.end_at
), bucket_positions AS (
	SELECT generate_series(0, 23)::int AS bucket_index
), recent_bucketed AS (
	SELECT recent_samples.target_key, recent_samples.status, recent_samples.health_reason, recent_samples.latency_ms,
	       recent_samples.checked_at, recent_samples.source, recent_samples.carried_from,
	       LEAST(23, FLOOR(EXTRACT(EPOCH FROM (recent_samples.checked_at - bounds.start_at)) / bounds.bucket_seconds)::int) AS bucket_index
	FROM recent_samples
	CROSS JOIN bounds
	WHERE recent_samples.status NOT IN ('unknown','disabled')
	   OR recent_samples.source = 'source_change'
), recent_ranked AS (
	SELECT target_key, status, health_reason, latency_ms, checked_at, source, carried_from, bucket_index,
	       ROW_NUMBER() OVER (
	           PARTITION BY target_key, bucket_index
	           ORDER BY checked_at DESC,
	                    CASE WHEN source = 'request_error' THEN 0
	                         WHEN source = 'history' THEN 1
	                         WHEN source = 'probe' THEN 2
	                         WHEN source = 'aggregate' THEN 3
	                         ELSE 4 END
	       ) AS position
	FROM recent_bucketed
), recent AS (
	SELECT targets.target_key,
	       jsonb_agg(jsonb_build_object(
	           'status', COALESCE(recent_ranked.status, 'unknown'),
	           'health_reason', COALESCE(recent_ranked.health_reason, ''),
	           'latency_ms', recent_ranked.latency_ms,
	           'checked_at', COALESCE(
	               recent_ranked.checked_at,
	               bounds.start_at + ((bucket_positions.bucket_index + 1) * bounds.bucket_seconds) * INTERVAL '1 second'
	           ),
	           'source', COALESCE(recent_ranked.source, ''),
	           'carried_from', recent_ranked.carried_from
	       ) ORDER BY bucket_positions.bucket_index) AS samples
	FROM visible_targets targets
	CROSS JOIN bounds
	CROSS JOIN bucket_positions
	LEFT JOIN recent_ranked
	       ON recent_ranked.target_key = targets.target_key
	      AND recent_ranked.bucket_index = bucket_positions.bucket_index
	      AND recent_ranked.position = 1
	GROUP BY targets.target_key
`

const dashboardQueryEvidence = `
), latest_checks AS (
    SELECT targets.target_key, checks.status, checks.health_reason, checks.latency_ms, checks.first_byte_ms,
           checks.checked_at, checks.source, checks.message
    FROM monitoring_targets targets
    LEFT JOIN LATERAL (
        SELECT mc.status, mc.health_reason, mc.latency_ms, mc.first_byte_ms, mc.checked_at, mc.source, mc.message
        FROM monitoring_checks mc
        WHERE mc.target_key = targets.target_key
          AND (targets.source_updated_at IS NULL OR mc.checked_at >= targets.source_updated_at - INTERVAL '2 minutes')
          AND ((targets.kind = 'account' AND mc.source = 'probe')
		       OR (targets.kind = 'group' AND mc.source = 'aggregate'))
        ORDER BY mc.checked_at DESC, mc.id DESC
        LIMIT 1
    ) checks ON TRUE
), latest_evidence_inputs AS (
    SELECT targets.target_key, targets.kind, targets.last_activity_at, targets.source_updated_at,
           targets.last_channel_error_at, targets.last_channel_error_class,
           targets.last_channel_error_status_code, targets.last_channel_error_resolved_at,
           latest_checks.status, latest_checks.health_reason, latest_checks.latency_ms, latest_checks.first_byte_ms,
           latest_checks.checked_at, latest_checks.source, latest_checks.message,
           latest_account_usage.created_at AS account_success_at,
           latest_account_usage.duration_ms AS account_success_latency_ms,
           latest_account_usage.first_token_ms AS account_success_first_byte_ms,
            CASE WHEN targets.last_channel_error_at IS NOT NULL
                       AND (targets.source_updated_at IS NULL
                            OR targets.last_channel_error_at >= targets.source_updated_at - INTERVAL '2 minutes')
                       AND (targets.last_channel_error_resolved_at IS NULL
                            OR targets.last_channel_error_at > targets.last_channel_error_resolved_at)
                       AND (latest_checks.checked_at IS NULL
                            OR targets.last_channel_error_at >= latest_checks.checked_at)
                       AND (targets.last_activity_at IS NULL
                            OR targets.last_channel_error_at >= targets.last_activity_at)
                       AND (latest_account_usage.created_at IS NULL
                            OR targets.last_channel_error_at >= latest_account_usage.created_at)
                  THEN TRUE ELSE FALSE END AS channel_error_wins,
            CASE WHEN targets.last_channel_error_at IS NOT NULL
                       AND (targets.source_updated_at IS NULL
                            OR targets.last_channel_error_at >= targets.source_updated_at - INTERVAL '2 minutes')
                       AND (targets.last_channel_error_resolved_at IS NULL
                            OR targets.last_channel_error_at > targets.last_channel_error_resolved_at)
                       AND (targets.last_activity_at IS NULL
                            OR targets.last_activity_at < targets.last_channel_error_at)
                       AND (latest_account_usage.created_at IS NULL
                            OR targets.last_channel_error_at >= latest_account_usage.created_at)
                       AND (
                           latest_checks.checked_at IS NULL
                           OR latest_checks.checked_at < targets.last_channel_error_at
                           OR COALESCE(latest_checks.status, '') NOT IN ('operational', 'degraded')
                       )
                  THEN TRUE ELSE FALSE END AS recovery_active,
           CASE WHEN (targets.last_activity_at IS NOT NULL
                       AND (targets.source_updated_at IS NULL
                            OR targets.last_activity_at >= targets.source_updated_at - INTERVAL '2 minutes')
                      AND (latest_checks.checked_at IS NULL
                           OR targets.last_activity_at >= latest_checks.checked_at))
                      OR (targets.kind = 'account'
                          AND latest_account_usage.created_at IS NOT NULL
                          AND (latest_checks.checked_at IS NULL
                               OR latest_account_usage.created_at >= latest_checks.checked_at))
                THEN TRUE ELSE FALSE END AS history_wins
    FROM monitoring_targets targets
    CROSS JOIN bounds
    LEFT JOIN latest_checks ON latest_checks.target_key = targets.target_key
	LEFT JOIN latest_account_usage ON latest_account_usage.target_key = targets.target_key
), latest_evidence AS (
    SELECT target_key,
           CASE WHEN kind = 'group' THEN status
                WHEN channel_error_wins THEN 'failed'
                WHEN history_wins AND kind = 'account'
                THEN CASE WHEN COALESCE(account_success_latency_ms, 0) >= 20000
                          THEN 'degraded' ELSE 'operational' END
                WHEN history_wins THEN 'operational'
                ELSE status END AS status,
           CASE WHEN kind = 'group' THEN health_reason
                WHEN channel_error_wins THEN 'upstream_error'
                WHEN history_wins AND kind = 'account' AND COALESCE(account_success_latency_ms, 0) >= 20000 THEN 'slow'
                ELSE health_reason END AS health_reason,
           CASE WHEN kind = 'group' THEN latency_ms
                WHEN kind = 'account' AND history_wins THEN account_success_latency_ms
                WHEN channel_error_wins OR history_wins THEN NULL ELSE latency_ms END AS latency_ms,
           CASE WHEN kind = 'group' THEN first_byte_ms
                WHEN kind = 'account' AND history_wins THEN account_success_first_byte_ms
                WHEN channel_error_wins OR history_wins THEN NULL ELSE first_byte_ms END AS first_byte_ms,
           CASE WHEN kind = 'group' THEN checked_at
                WHEN channel_error_wins THEN last_channel_error_at
                WHEN history_wins AND kind = 'account' AND account_success_at IS NOT NULL THEN account_success_at
                WHEN history_wins THEN last_activity_at ELSE checked_at END AS checked_at,
           CASE WHEN recovery_active THEN last_channel_error_at ELSE NULL END AS recovery_trigger_at,
           CASE WHEN kind = 'group' THEN source
                WHEN channel_error_wins THEN 'request_error'
                WHEN history_wins THEN 'history'
                ELSE source END AS source,
           CASE WHEN kind = 'group' THEN message
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
       CASE WHEN t.kind = 'account' THEN COALESCE(a.priority, 50) END,
       COALESCE(route.route_configured, FALSE), COALESCE(route.route_message, ''),
       e.status, e.health_reason, e.latency_ms, e.first_byte_ms, e.checked_at, e.source, e.message,
       COALESCE(s.samples,0), COALESCE(s.successful,0),
       COALESCE(s.rate_limited,0), COALESCE(s.hard_failures,0),
       s.first_fastest, s.first_median, s.first_p95,
       s.latency_fastest, s.latency_median, s.latency_p95,
       COALESCE(current_health.window_seconds, 300),
       COALESCE(current_health.samples, 0), COALESCE(current_health.successful, 0),
       COALESCE(current_health.rate_limited, 0), COALESCE(current_health.hard_failures, 0),
       COALESCE(current_health.attempts, 0), current_health.latest_at,
       current_health.latency_fastest, current_health.latency_median, current_health.latency_p95,
	       COALESCE(current_health.affected_accounts, 0), COALESCE(current_health.observed_accounts, 0),
	       COALESCE(current_health.member_accounts, 0),
       COALESCE(r.samples, '[]'::jsonb)
FROM monitoring_targets t
JOIN visible_targets visible
  ON visible.target_key = t.target_key
CROSS JOIN bounds
LEFT JOIN accounts a ON t.kind = 'account' AND a.id = t.entity_id AND a.deleted_at IS NULL
LEFT JOIN groups g ON t.kind = 'group' AND g.id = t.entity_id AND g.deleted_at IS NULL
LEFT JOIN route_state route ON route.target_key = t.target_key
LEFT JOIN latest_evidence e ON e.target_key = t.target_key
LEFT JOIN stats s ON s.target_key = t.target_key
LEFT JOIN current_health ON current_health.target_key = t.target_key
LEFT JOIN recent r ON r.target_key = t.target_key
WHERE t.active = TRUE
  AND (
      LOWER(TRIM(t.source_status)) = 'active'
      OR (t.kind = 'account' AND LOWER(TRIM(t.source_status)) = 'error')
  )
ORDER BY CASE WHEN t.kind = 'group' THEN 0 ELSE 1 END,
         CASE WHEN t.kind = 'account' THEN COALESCE(a.priority, 50) ELSE 0 END,
         t.name, t.entity_id`

func targetStats(
	samples, successful, rateLimited, hardFailures int,
	firstFastest sql.NullInt64,
	firstMedian sql.NullFloat64,
	firstP95 sql.NullFloat64,
	latencyFastest sql.NullInt64,
	latencyMedian, latencyP95 sql.NullFloat64,
) model.TargetStats {
	stats := model.TargetStats{
		Samples: samples, Successful: successful, Errors: samples - successful,
		RateLimited: rateLimited, HardFailures: hardFailures,
	}
	if samples > 0 {
		stats.Availability = float64(successful) * 100 / float64(samples)
		stats.RateLimitRate = float64(rateLimited) * 100 / float64(samples)
		stats.HardFailureRate = float64(hardFailures) * 100 / float64(samples)
	}
	stats.FirstByte = metricStats(firstFastest, firstMedian, firstP95)
	stats.Latency = metricStats(latencyFastest, latencyMedian, latencyP95)
	return stats
}

func evaluateCurrentHealth(
	windowSeconds, samples, successful, rateLimited, hardFailures, attempts int,
	latestAt sql.NullTime, fastest sql.NullInt64, median, p95 sql.NullFloat64,
	affectedAccounts, observedAccounts, memberAccounts int,
) model.HealthWindow {
	window := model.HealthWindow{
		WindowSeconds: windowSeconds,
		Samples:       samples, Attempts: attempts, Successful: successful,
		RateLimited: rateLimited, HardFailures: hardFailures,
		AffectedAccounts: affectedAccounts, ObservedAccounts: observedAccounts, MemberAccounts: memberAccounts,
		Latency: metricStats(fastest, median, p95),
	}
	if latestAt.Valid {
		value := latestAt.Time.UTC()
		window.LatestAt = &value
	}
	if window.WindowSeconds <= 0 {
		window.WindowSeconds = 300
	}
	return stats.EvaluateHealth(window, stats.DefaultHealthPolicy)
}

func applyCurrentHealth(target *model.DashboardTarget) {
	if target == nil {
		return
	}
	health := target.CurrentHealth
	if !currentHealthOverridesTarget(*target, health) {
		return
	}
	target.Status = health.Status
	target.Available = health.Available
	target.HealthReason = health.Reason
	if health.Reason != model.HealthReasonRateLimited && isCurrentRateLimitMessage(target.LatestMessage) {
		target.LatestMessage = ""
	}
	if health.LatestAt != nil {
		target.LastCheckedAt = health.LatestAt
		target.Stale = false
	}
	if health.Reason == model.HealthReasonRateLimited {
		if target.Kind == model.KindGroup {
			target.LatestMessage = fmt.Sprintf("当前仍可用；近 %d 分钟阶段性限速 %.1f%%，受影响账户 %d/%d",
				health.WindowSeconds/60, health.RateLimitRate, health.AffectedAccounts, health.MemberAccounts)
		} else {
			target.LatestMessage = fmt.Sprintf("当前可用，但近 %d 分钟有 %.1f%% 上游尝试被限速",
				health.WindowSeconds/60, health.RateLimitRate)
		}
	}
}

func currentHealthOverridesTarget(target model.DashboardTarget, health model.HealthWindow) bool {
	if health.Samples == 0 {
		return false
	}
	if target.Kind != model.KindGroup || health.Available || !target.Available {
		return true
	}
	return health.MemberAccounts > 0 && health.ObservedAccounts >= health.MemberAccounts
}

func isCurrentRateLimitMessage(message string) bool {
	return strings.HasPrefix(message, "当前仍可用；近 ") && strings.Contains(message, "分钟阶段性限速 ") ||
		strings.HasPrefix(message, "当前可用，但近 ") && strings.Contains(message, "上游尝试被限速")
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
	return target.Stats.Samples > 0 || target.CurrentHealth.Samples > 0
}

func targetAvailabilityValue(target model.DashboardTarget) float64 {
	if target.CurrentHealth.Samples > 0 {
		return target.CurrentHealth.SuccessRate
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

func scanDashboardTarget(rows *sql.Rows, now time.Time, staleAfter time.Duration) (model.DashboardTarget, bool, error) {
	var target model.DashboardTarget
	var latestStatus, latestHealthReason, latestSource, latestMessage sql.NullString
	var routeConfigured bool
	var routeMessage sql.NullString
	var latestLatency, latestFirst sql.NullInt64
	var latestAt sql.NullTime
	var samples, successful, rateLimited, hardFailures int
	var firstFastest, latencyFastest sql.NullInt64
	var firstMedian, firstP95, latencyMedian, latencyP95 sql.NullFloat64
	var recentJSON []byte
	var currentRate sql.NullFloat64
	var priority sql.NullInt64
	var recoveryTrigger sql.NullTime
	var currentWindowSeconds, currentSamples, currentSuccessful int
	var currentRateLimited, currentHardFailures, currentAttempts int
	var currentLatestAt sql.NullTime
	var currentFastest sql.NullInt64
	var currentMedian, currentP95 sql.NullFloat64
	var affectedAccounts, observedAccounts, memberAccounts int
	if err := rows.Scan(
		&target.Key, &target.Kind, &target.EntityID, &target.Name, &target.Platform,
		&target.SourceStatus, &target.ProbeEnabled, &recoveryTrigger,
		&currentRate, &priority, &routeConfigured, &routeMessage,
		&latestStatus, &latestHealthReason, &latestLatency,
		&latestFirst, &latestAt, &latestSource, &latestMessage, &samples, &successful,
		&rateLimited, &hardFailures,
		&firstFastest, &firstMedian, &firstP95, &latencyFastest,
		&latencyMedian, &latencyP95,
		&currentWindowSeconds, &currentSamples, &currentSuccessful,
		&currentRateLimited, &currentHardFailures, &currentAttempts, &currentLatestAt,
		&currentFastest, &currentMedian, &currentP95, &affectedAccounts, &observedAccounts, &memberAccounts,
		&recentJSON,
	); err != nil {
		return model.DashboardTarget{}, false, fmt.Errorf("scan dashboard: %w", err)
	}
	if currentRate.Valid {
		value := currentRate.Float64
		target.RateMultiplier = &value
	}
	if priority.Valid {
		value := int(priority.Int64)
		target.Priority = &value
	}
	if recoveryTrigger.Valid {
		value := recoveryTrigger.Time.UTC()
		target.RecoveryTriggerAt = &value
	}
	target.RouteConfigured = routeConfigured
	target.RouteMessage = strings.TrimSpace(routeMessage.String)
	applyLatestTargetStateWithMessage(&target, latestStatus, latestSource, latestMessage, latestLatency, latestFirst, latestAt, now, staleAfter)
	target.HealthReason = strings.TrimSpace(latestHealthReason.String)
	target.Stats = targetStats(samples, successful, rateLimited, hardFailures, firstFastest, firstMedian, firstP95, latencyFastest, latencyMedian, latencyP95)
	target.CurrentHealth = evaluateCurrentHealth(currentWindowSeconds, currentSamples, currentSuccessful,
		currentRateLimited, currentHardFailures, currentAttempts, currentLatestAt,
		currentFastest, currentMedian, currentP95, affectedAccounts, observedAccounts, memberAccounts)
	applyCurrentHealth(&target)
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
			// Keep the winning error visible in the current and later buckets even
			// when an older successful sample occupies the same display window.
			overlayLatestTargetStatus(target.RecentSamples, target.Status, target.LatestSource, *target.LastCheckedAt, now.Add(-dashboardWindow))
		}
	}
	return target, targetContributesAvailability(target), nil
}

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
	target.Available = false
	// Groups are never probed directly. Their aggregate check is still valid
	// evidence, so a disabled group probe must not erase that health state.
	// A disabled account, on the other hand, is not a routable target.
	if !target.ProbeEnabled && target.Kind != model.KindGroup {
		target.Status = model.StatusDisabled
		return
	}
	if status.Valid {
		target.Status = status.String
		target.Available = target.Status == model.StatusOperational || target.Status == model.StatusDegraded
		if target.Status == model.StatusUnknown {
			if target.Kind == model.KindGroup {
				target.LatestMessage = "暂无账户健康证据，等待真实请求或恢复探测"
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
			target.LatestMessage = "暂无账户健康证据，等待真实请求或恢复探测"
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

func carryForwardStatusSamples(samples []model.StatusSample) {
	// Carry the latest known state through empty buckets. A source change marks
	// evidence as old, but it does not erase the last user-visible state: a
	// silent interval is not proof that the account or route became unknown.
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
	sample.HealthReason = previous.HealthReason
	sample.Source = previous.Source
	if previous.LatencyMs == nil {
		sample.LatencyMs = nil
	} else {
		latency := *previous.LatencyMs
		sample.LatencyMs = &latency
	}
	carriedFrom := previous.CheckedAt
	if previous.CarriedFrom != nil {
		carriedFrom = *previous.CarriedFrom
	}
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
		if status == model.StatusFailed || status == model.StatusError {
			sample.HealthReason = model.HealthReasonUpstreamError
		}
		sample.Source = source
		sample.CarriedFrom = nil
	}
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
