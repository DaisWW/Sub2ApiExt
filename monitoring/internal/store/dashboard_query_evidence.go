package store

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
