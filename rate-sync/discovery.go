package main

const adminAPIKeySQL = `SELECT value FROM settings WHERE key = 'admin_api_key' AND btrim(value) <> '' LIMIT 1`

const discoverChannelsSQL = `
SELECT
    a.id,
    btrim(a.name),
    a.rate_multiplier::double precision,
    a.credentials->>'base_url',
    a.credentials->>'api_key',
    g.id,
    btrim(g.name),
    g.rate_multiplier::double precision,
    g.daily_limit_usd::double precision,
    g.weekly_limit_usd::double precision,
    g.monthly_limit_usd::double precision,
    p.protocol,
    p.host,
    p.port,
    p.username,
    p.password
FROM accounts a
JOIN account_groups ag ON ag.account_id = a.id
JOIN groups g ON g.id = ag.group_id
LEFT JOIN proxies p ON p.id = a.proxy_id
    AND p.deleted_at IS NULL
    AND p.status = 'active'
    AND (p.expires_at IS NULL OR p.expires_at > NOW())
WHERE a.deleted_at IS NULL
  AND g.deleted_at IS NULL
  AND a.type = 'apikey'
  AND a.status = 'active'
  AND a.schedulable = true
  AND g.status = 'active'
  AND EXISTS (
      SELECT 1
      FROM channel_groups cg
      JOIN channels c ON c.id = cg.channel_id
      WHERE cg.group_id = g.id
        AND c.status = 'active'
  )
  AND NULLIF(btrim(a.credentials->>'base_url'), '') IS NOT NULL
  AND NULLIF(btrim(a.credentials->>'api_key'), '') IS NOT NULL
ORDER BY g.id, a.id`

const groupUsageStatsSQL = `
SELECT
    ul.group_id,
    COUNT(*)::bigint,
    COALESCE(SUM(ul.total_cost), 0)::double precision,
    COALESCE(SUM(
        COALESCE(ul.account_stats_cost, ul.total_cost) *
        COALESCE(a.rate_multiplier, ul.account_rate_multiplier, 1)
    ), 0)::double precision
FROM usage_logs ul
LEFT JOIN accounts a ON a.id = ul.account_id
JOIN groups g ON g.id = ul.group_id
WHERE ul.group_id IS NOT NULL
  AND ul.created_at >= $1
  AND ul.created_at < $2
  AND ul.total_cost > 0
  AND g.deleted_at IS NULL
  AND g.status = 'active'
GROUP BY ul.group_id
ORDER BY ul.group_id`

const groupUsageStatsByWindowsSQL = `
WITH windows(window_seconds) AS (
    VALUES %s
)
SELECT
    w.window_seconds::bigint,
    ul.group_id,
    COUNT(*)::bigint,
    COALESCE(SUM(ul.total_cost), 0)::double precision,
    COALESCE(SUM(
        COALESCE(ul.account_stats_cost, ul.total_cost) *
        COALESCE(a.rate_multiplier, ul.account_rate_multiplier, 1)
    ), 0)::double precision
FROM windows w
JOIN usage_logs ul ON ul.created_at >= $1::timestamptz - w.window_seconds * interval '1 second'
    AND ul.created_at < $1::timestamptz
LEFT JOIN accounts a ON a.id = ul.account_id
JOIN groups g ON g.id = ul.group_id
WHERE ul.group_id IS NOT NULL
  AND ul.total_cost > 0
  AND g.deleted_at IS NULL
  AND g.status = 'active'
GROUP BY w.window_seconds, ul.group_id
ORDER BY ul.group_id, w.window_seconds`

const latestGroupUsageIDSQL = `SELECT COALESCE(MAX(id), 0)::bigint FROM usage_logs`

const groupUsageSinceSQL = `
WITH watermarks(group_id, last_id) AS (
    VALUES %s
)
SELECT
    ul.group_id,
    COALESCE(ul.account_id, 0)::bigint,
    COUNT(*)::bigint,
    COALESCE(SUM(ul.total_cost), 0)::double precision,
    COALESCE(SUM(COALESCE(ul.account_stats_cost, ul.total_cost)), 0)::double precision,
    COALESCE(MAX(a.rate_multiplier), MAX(ul.account_rate_multiplier), 1)::double precision
FROM watermarks w
JOIN usage_logs ul
  ON ul.group_id = w.group_id
 AND ul.id > w.last_id
 AND ul.id <= %s
LEFT JOIN accounts a ON a.id = ul.account_id
WHERE ul.total_cost > 0
GROUP BY ul.group_id, COALESCE(ul.account_id, 0)
ORDER BY ul.group_id, COALESCE(ul.account_id, 0)`

const groupUsageAccountsSQL = `
SELECT
    ul.group_id,
    COALESCE(ul.account_id, 0)::bigint,
    COUNT(*)::bigint,
    COALESCE(SUM(ul.total_cost), 0)::double precision,
    COALESCE(SUM(COALESCE(ul.account_stats_cost, ul.total_cost)), 0)::double precision,
    COALESCE(MAX(a.rate_multiplier), MAX(ul.account_rate_multiplier), 1)::double precision
FROM usage_logs ul
LEFT JOIN accounts a ON a.id = ul.account_id
WHERE ul.created_at >= $1
  AND ul.created_at < $2
  AND ul.id <= $3
  AND ul.total_cost > 0
  AND ul.group_id IN (%s)
GROUP BY ul.group_id, COALESCE(ul.account_id, 0)
ORDER BY ul.group_id, COALESCE(ul.account_id, 0)`
