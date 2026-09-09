package store

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
