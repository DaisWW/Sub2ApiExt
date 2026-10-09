package store

const dashboardQueryCurrentHealth = `
), latest_request_usage AS MATERIALIZED (
	SELECT targets.target_key, latest.created_at
	FROM active_targets targets
	LEFT JOIN LATERAL (
		SELECT usage.created_at
		FROM usage_logs usage
		WHERE targets.kind = 'group' AND usage.group_id = targets.entity_id
		  AND usage.actual_cost > 0 AND usage.created_at < NOW()
		  AND usage.created_at >= COALESCE(targets.source_updated_at - INTERVAL '2 minutes', '-infinity'::timestamptz)
		ORDER BY usage.created_at DESC, usage.id DESC LIMIT 1
	) latest ON TRUE
	WHERE targets.kind = 'group'
	UNION ALL
	SELECT target_key, created_at FROM latest_account_usage
), old_request_windows AS MATERIALIZED (
	SELECT latest.target_key, latest.created_at, targets.kind, targets.entity_id, targets.source_updated_at
	FROM latest_request_usage latest
	JOIN active_targets targets ON targets.target_key = latest.target_key
	CROSS JOIN bounds
	WHERE latest.created_at < bounds.start_at + INTERVAL '5 minutes'
), request_success_rows AS MATERIALIZED (
	SELECT targets.target_key, usage.id, usage.account_id, usage.request_key,
	       usage.duration_ms, usage.first_token_ms, usage.created_at
	FROM period_usage usage
	CROSS JOIN LATERAL (VALUES ('account:' || usage.account_id::text), ('group:' || usage.group_id::text)) keys(target_key)
	JOIN active_targets targets ON targets.target_key = keys.target_key
	WHERE targets.source_updated_at IS NULL
	   OR usage.created_at >= targets.source_updated_at - INTERVAL '2 minutes'
	UNION ALL
	SELECT latest.target_key, usage.id, usage.account_id,
	       CASE WHEN NULLIF(BTRIM(usage.request_id), '') IS NULL THEN 'usage:' || usage.id::text
	            ELSE 'request:' || LOWER(REGEXP_REPLACE(BTRIM(usage.request_id), '^client:', '', 'i')) END,
	       usage.duration_ms, usage.first_token_ms, usage.created_at
	FROM old_request_windows latest
	CROSS JOIN bounds
	JOIN usage_logs usage
	  ON ((latest.kind = 'account' AND usage.account_id = latest.entity_id)
	      OR (latest.kind = 'group' AND usage.group_id = latest.entity_id))
	 AND usage.created_at >= latest.created_at - INTERVAL '5 minutes'
	 AND usage.created_at <= latest.created_at AND usage.actual_cost > 0
	WHERE usage.created_at < bounds.start_at
	  AND (latest.source_updated_at IS NULL
	       OR usage.created_at >= latest.source_updated_at - INTERVAL '2 minutes')
), request_success AS MATERIALIZED (
	SELECT DISTINCT ON (target_key, request_key) *
	FROM request_success_rows
	ORDER BY target_key, request_key, created_at DESC, id DESC
), error_time_bounds AS (
	SELECT NULL::text AS target_key, start_at, end_at FROM bounds
	UNION ALL
	SELECT old.target_key, old.created_at - INTERVAL '5 minutes', LEAST(old.created_at, bounds.start_at)
	FROM old_request_windows old CROSS JOIN bounds
), request_error_candidates AS MATERIALIZED (
	SELECT windows.target_key, errors.id, errors.account_id, errors.group_id,
	       errors.client_request_id, errors.request_id, errors.created_at,
	       errors.upstream_status_code, errors.status_code, errors.error_type,
	       errors.is_business_limited, errors.error_owner, errors.error_phase, errors.error_source
	FROM error_time_bounds windows
	JOIN ops_error_logs errors ON errors.created_at >= windows.start_at AND errors.created_at <= windows.end_at
	CROSS JOIN bounds
	WHERE errors.created_at < bounds.end_at
	  AND (windows.target_key IS NULL OR errors.created_at < bounds.start_at)
), request_error_rows AS MATERIALIZED (
	SELECT targets.target_key, errors.id, errors.account_id,
	       CASE
	           WHEN COALESCE(NULLIF(BTRIM(errors.client_request_id), ''),
	                         NULLIF(BTRIM(errors.request_id), '')) IS NULL THEN 'error:' || errors.id::text
	           ELSE 'request:' || LOWER(REGEXP_REPLACE(
	                    COALESCE(NULLIF(BTRIM(errors.client_request_id), ''),
	                             NULLIF(BTRIM(errors.request_id), '')), '^client:', '', 'i'))
	       END AS request_key,
	       errors.created_at,
	       CASE WHEN errors.upstream_status_code = 429 OR errors.status_code = 429
	                  OR LOWER(BTRIM(COALESCE(errors.error_type, ''))) IN ('rate_limit_error', 'rate_limited', 'rate_limit')
	            THEN TRUE ELSE FALSE END AS rate_limited
	FROM request_error_candidates errors
	CROSS JOIN LATERAL (VALUES ('account:' || errors.account_id::text), ('group:' || errors.group_id::text)) keys(target_key)
	JOIN active_targets targets ON targets.target_key = keys.target_key
	WHERE (errors.target_key IS NULL OR errors.target_key = targets.target_key)
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
), request_errors AS MATERIALIZED (
	SELECT target_key, request_key, MAX(account_id) AS account_id, MAX(created_at) AS error_at,
	       BOOL_OR(NOT rate_limited) AS hard_failure
	FROM request_error_rows GROUP BY target_key, request_key
), request_outcomes AS MATERIALIZED (
	SELECT COALESCE(success.target_key, errors.target_key) AS target_key,
	       COALESCE(success.account_id, errors.account_id) AS account_id,
	       GREATEST(success.created_at, errors.error_at) AS checked_at,
	       success.request_key IS NOT NULL AND
	           (errors.error_at IS NULL OR success.created_at >= errors.error_at) AS successful,
	       COALESCE(errors.hard_failure, FALSE) AND
	           (success.created_at IS NULL OR success.created_at < errors.error_at) AS hard_failure,
	       success.duration_ms, success.first_token_ms
	FROM request_success success
	FULL OUTER JOIN request_errors errors
	  ON success.target_key = errors.target_key AND success.request_key = errors.request_key
), request_windows AS (
	SELECT target_key, TRUE AS is_current, NOW() AS end_at FROM active_targets
	UNION ALL
	SELECT target_key, FALSE, MAX(checked_at) FROM request_outcomes GROUP BY target_key
), request_window_errors AS MATERIALIZED (
	SELECT windows.target_key, windows.is_current,
	       COUNT(errors.created_at)::integer AS attempts,
	       COUNT(*) FILTER (WHERE errors.rate_limited)::integer AS rate_limited,
	       COUNT(DISTINCT errors.account_id) FILTER (WHERE errors.rate_limited)::integer AS affected_accounts
	FROM request_windows windows
	LEFT JOIN request_error_rows errors ON errors.target_key = windows.target_key
	 AND errors.created_at >= windows.end_at - INTERVAL '5 minutes' AND errors.created_at <= windows.end_at
	GROUP BY windows.target_key, windows.is_current
), request_window_health AS MATERIALIZED (
	SELECT windows.target_key, windows.is_current, 300::integer AS window_seconds,
	       COUNT(outcomes.checked_at)::integer AS samples,
	       COUNT(*) FILTER (WHERE outcomes.successful)::integer AS successful,
	       errors.rate_limited,
	       COUNT(*) FILTER (WHERE outcomes.hard_failure)::integer AS hard_failures,
	       (COUNT(*) FILTER (WHERE outcomes.successful) + errors.attempts)::integer AS attempts,
	       MAX(outcomes.checked_at) AS latest_at,
	       COUNT(*) FILTER (WHERE outcomes.successful AND outcomes.first_token_ms >= 0)::integer AS first_samples,
	       MIN(outcomes.first_token_ms) FILTER (WHERE outcomes.successful AND outcomes.first_token_ms >= 0) AS first_fastest,
	       percentile_cont(0.5) WITHIN GROUP (ORDER BY outcomes.first_token_ms)
	           FILTER (WHERE outcomes.successful AND outcomes.first_token_ms >= 0) AS first_median,
	       percentile_cont(0.95) WITHIN GROUP (ORDER BY outcomes.first_token_ms)
	           FILTER (WHERE outcomes.successful AND outcomes.first_token_ms >= 0) AS first_p95,
	       MIN(outcomes.duration_ms) FILTER (WHERE outcomes.successful AND outcomes.duration_ms >= 0) AS latency_fastest,
	       percentile_cont(0.5) WITHIN GROUP (ORDER BY outcomes.duration_ms)
	           FILTER (WHERE outcomes.successful AND outcomes.duration_ms >= 0) AS latency_median,
	       percentile_cont(0.95) WITHIN GROUP (ORDER BY outcomes.duration_ms)
	           FILTER (WHERE outcomes.successful AND outcomes.duration_ms >= 0) AS latency_p95,
	       errors.affected_accounts,
	       COUNT(DISTINCT outcomes.account_id)::integer AS observed_accounts,
	       (SELECT COUNT(DISTINCT ag.account_id)::integer
	        FROM account_groups ag JOIN active_accounts a ON a.id = ag.account_id
	        WHERE windows.target_key = 'group:' || ag.group_id::text AND LOWER(TRIM(a.status)) = 'active') AS member_accounts
	FROM request_windows windows
	JOIN request_window_errors errors ON errors.target_key = windows.target_key AND errors.is_current = windows.is_current
	LEFT JOIN request_outcomes outcomes ON outcomes.target_key = windows.target_key
	 AND outcomes.checked_at >= windows.end_at - INTERVAL '5 minutes' AND outcomes.checked_at <= windows.end_at
	GROUP BY windows.target_key, windows.is_current, errors.rate_limited, errors.attempts, errors.affected_accounts
), current_health AS (
	SELECT * FROM request_window_health WHERE is_current
), last_request_health AS (
	SELECT target_key, jsonb_build_object(
	    'window_seconds', window_seconds, 'samples', samples, 'successful', successful,
	    'rate_limited', rate_limited, 'hard_failures', hard_failures, 'attempts', attempts,
	    'latest_at', latest_at, 'first_byte_samples', first_samples,
	    'first_byte', jsonb_build_object('fastest_ms', first_fastest, 'median_ms', first_median, 'p95_ms', first_p95),
	    'latency', jsonb_build_object('fastest_ms', latency_fastest, 'median_ms', latency_median, 'p95_ms', latency_p95),
	    'affected_accounts', affected_accounts, 'observed_accounts', observed_accounts, 'member_accounts', member_accounts
	) AS health
	FROM request_window_health WHERE NOT is_current
`
