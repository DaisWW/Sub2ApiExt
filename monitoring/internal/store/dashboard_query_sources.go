package store

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
