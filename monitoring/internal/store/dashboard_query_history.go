package store

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
