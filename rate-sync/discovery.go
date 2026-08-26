package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"
)

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

type ChannelSource interface {
	List(context.Context) ([]Channel, error)
}

// GroupUsageStats is the recent raw-cost summary used to derive a shared
// group multiplier when a group can route requests through multiple accounts.
type GroupUsageStats struct {
	GroupID      int64
	Requests     int64
	StandardCost float64
	AccountCost  float64
}

type GroupUsageWindowStats struct {
	Window time.Duration
	GroupUsageStats
}

type groupUsageSource interface {
	ListGroupUsageStats(context.Context, time.Time, time.Time) ([]GroupUsageStats, error)
}

type groupUsageWindowSource interface {
	ListGroupUsageStatsByWindows(context.Context, time.Time, []time.Duration) ([]GroupUsageWindowStats, error)
}

type adminAPIKeySource interface {
	AdminAPIKey(context.Context) (string, error)
}

type PostgresChannelSource struct {
	db *sql.DB
}

type Channel struct {
	AccountID             int64
	AccountName           string
	AccountRateMultiplier float64
	BaseURL               string
	APIKey                string
	ProxyURL              string
	Group                 sub2APIGroup
}

type sub2APIGroup struct {
	ID              int64
	Name            string
	RateMultiplier  float64
	DailyLimitUSD   *float64
	WeeklyLimitUSD  *float64
	MonthlyLimitUSD *float64
}

func NewPostgresChannelSource(db *sql.DB) *PostgresChannelSource {
	return &PostgresChannelSource{db: db}
}

func (s *PostgresChannelSource) AdminAPIKey(ctx context.Context) (string, error) {
	var key string
	if err := s.db.QueryRowContext(ctx, adminAPIKeySQL).Scan(&key); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", nil
		}
		return "", fmt.Errorf("查询 Admin API Key: %w", err)
	}
	return strings.TrimSpace(key), nil
}

func (s *PostgresChannelSource) List(ctx context.Context) ([]Channel, error) {
	rows, err := s.db.QueryContext(ctx, discoverChannelsSQL)
	if err != nil {
		return nil, fmt.Errorf("查询可用渠道: %w", err)
	}
	defer rows.Close()

	var channels []Channel
	for rows.Next() {
		var channel Channel
		var daily, weekly, monthly sql.NullFloat64
		var proxyProtocol, proxyHost, proxyUsername, proxyPassword sql.NullString
		var proxyPort sql.NullInt64
		if err := rows.Scan(
			&channel.AccountID,
			&channel.AccountName,
			&channel.AccountRateMultiplier,
			&channel.BaseURL,
			&channel.APIKey,
			&channel.Group.ID,
			&channel.Group.Name,
			&channel.Group.RateMultiplier,
			&daily,
			&weekly,
			&monthly,
			&proxyProtocol,
			&proxyHost,
			&proxyPort,
			&proxyUsername,
			&proxyPassword,
		); err != nil {
			return nil, fmt.Errorf("读取可用渠道: %w", err)
		}
		channel.ProxyURL, err = buildProxyURL(proxyProtocol, proxyHost, proxyPort, proxyUsername, proxyPassword)
		if err != nil {
			return nil, fmt.Errorf("读取账号 %d 代理: %w", channel.AccountID, err)
		}
		channel.Group.DailyLimitUSD = nullableFloat(daily)
		channel.Group.WeeklyLimitUSD = nullableFloat(weekly)
		channel.Group.MonthlyLimitUSD = nullableFloat(monthly)
		channels = append(channels, channel)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历可用渠道: %w", err)
	}
	return channels, nil
}

func (s *PostgresChannelSource) ListGroupUsageStats(ctx context.Context, start, end time.Time) ([]GroupUsageStats, error) {
	rows, err := s.db.QueryContext(ctx, groupUsageStatsSQL, start, end)
	if err != nil {
		return nil, fmt.Errorf("查询分组历史用量: %w", err)
	}
	defer rows.Close()

	var results []GroupUsageStats
	for rows.Next() {
		var row GroupUsageStats
		if err := rows.Scan(&row.GroupID, &row.Requests, &row.StandardCost, &row.AccountCost); err != nil {
			return nil, fmt.Errorf("读取分组历史用量: %w", err)
		}
		results = append(results, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历分组历史用量: %w", err)
	}
	return results, nil
}

func (s *PostgresChannelSource) ListGroupUsageStatsByWindows(ctx context.Context, end time.Time, windows []time.Duration) ([]GroupUsageWindowStats, error) {
	if len(windows) == 0 {
		return nil, nil
	}
	values := make([]string, 0, len(windows))
	seen := make(map[int64]struct{}, len(windows))
	for _, window := range windows {
		seconds := int64(window / time.Second)
		if seconds <= 0 {
			return nil, fmt.Errorf("历史统计窗口无效: %s", window)
		}
		if _, exists := seen[seconds]; exists {
			continue
		}
		seen[seconds] = struct{}{}
		// seconds is derived from validated time.Duration values, not user SQL.
		values = append(values, "("+strconv.FormatInt(seconds, 10)+")")
	}
	query := fmt.Sprintf(groupUsageStatsByWindowsSQL, strings.Join(values, ","))
	rows, err := s.db.QueryContext(ctx, query, end)
	if err != nil {
		return nil, fmt.Errorf("查询分组多窗口历史用量: %w", err)
	}
	defer rows.Close()

	results := make([]GroupUsageWindowStats, 0)
	for rows.Next() {
		var seconds int64
		var result GroupUsageWindowStats
		if err := rows.Scan(
			&seconds,
			&result.GroupID,
			&result.Requests,
			&result.StandardCost,
			&result.AccountCost,
		); err != nil {
			return nil, fmt.Errorf("读取分组多窗口历史用量: %w", err)
		}
		result.Window = time.Duration(seconds) * time.Second
		results = append(results, result)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历分组多窗口历史用量: %w", err)
	}
	return results, nil
}

func buildProxyURL(protocol, host sql.NullString, port sql.NullInt64, username, password sql.NullString) (string, error) {
	if !protocol.Valid && !host.Valid && !port.Valid {
		return "", nil
	}
	proxyProtocol := strings.ToLower(strings.TrimSpace(protocol.String))
	proxyHost := strings.TrimSpace(host.String)
	if proxyProtocol == "" || proxyHost == "" || !port.Valid || port.Int64 < 1 || port.Int64 > 65535 {
		return "", fmt.Errorf("代理配置不完整")
	}
	if proxyProtocol != "http" && proxyProtocol != "https" {
		return "", fmt.Errorf("暂不支持代理协议 %q", proxyProtocol)
	}
	proxyURL := &url.URL{
		Scheme: proxyProtocol,
		Host:   net.JoinHostPort(proxyHost, strconv.FormatInt(port.Int64, 10)),
	}
	if username.Valid && password.Valid && username.String != "" && password.String != "" {
		proxyURL.User = url.UserPassword(username.String, password.String)
	}
	return proxyURL.String(), nil
}

func nullableFloat(value sql.NullFloat64) *float64 {
	if !value.Valid {
		return nil
	}
	result := value.Float64
	return &result
}
