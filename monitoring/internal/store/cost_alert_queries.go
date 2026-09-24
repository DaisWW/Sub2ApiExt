package store

import (
	"context"
	"time"

	"github.com/DaisWW/Sub2ApiExt/monitoring/internal/config"
)

// costUsageQuery reads a bounded baseline window and aggregates the current
// window in the same pass. The baseline ends at the start of the local day so
// a current-day incident cannot raise its own baseline and suppress reminders.
// The query only returns dimensions that have current traffic, so an inactive
// user does not create work or alerts.
const costUsageQuery = `
WITH usage AS (
    SELECT ul.created_at,
           COALESCE(NULLIF(BTRIM(ul.user_id::text), ''), 'unknown') AS user_key,
           COALESCE(NULLIF(BTRIM(u.username), ''), '') AS user_name,
           COALESCE(NULLIF(BTRIM(u.email), ''), '') AS user_email,
           COALESCE(NULLIF(BTRIM(ul.model), ''), 'unknown') AS model,
           COALESCE(ul.api_key_id, 0)::bigint AS api_key_id,
           COALESCE(NULLIF(BTRIM(k.name), ''), '') AS api_key_name,
           COALESCE(ul.channel_id, 0)::bigint AS channel_id,
           COALESCE(NULLIF(BTRIM(c.name), ''), '未归属渠道') AS channel_name,
           COALESCE(ul.account_id, 0)::bigint AS account_id,
           COALESCE(NULLIF(BTRIM(a.name), ''), '未归属账户') AS account_name,
           GREATEST(COALESCE(ul.input_tokens, 0), 0)::bigint AS input_tokens,
           GREATEST(COALESCE(ul.output_tokens, 0), 0)::bigint AS output_tokens,
           GREATEST(COALESCE(ul.cache_creation_tokens, 0), 0)::bigint AS cache_creation_tokens,
           GREATEST(COALESCE(ul.cache_read_tokens, 0), 0)::bigint AS cache_read_tokens,
           GREATEST(COALESCE(ul.total_cost, 0), 0)::double precision AS base_cost,
           GREATEST(COALESCE(ul.actual_cost, 0), 0)::double precision AS actual_cost,
           GREATEST(COALESCE(ul.input_cost, 0), 0)::double precision AS input_cost,
           GREATEST(COALESCE(ul.output_cost, 0), 0)::double precision AS output_cost,
           GREATEST(COALESCE(ul.cache_creation_cost, 0), 0)::double precision AS cache_creation_cost,
           GREATEST(COALESCE(ul.cache_read_cost, 0), 0)::double precision AS cache_read_cost
    FROM usage_logs ul
    LEFT JOIN users u ON u.id = ul.user_id
    LEFT JOIN api_keys k ON k.id = ul.api_key_id
    LEFT JOIN channels c ON c.id = ul.channel_id
    LEFT JOIN accounts a ON a.id = ul.account_id
    WHERE ul.created_at >= $3
      AND ul.created_at < $2
      AND ul.actual_cost > 0
)
SELECT user_key,
       MAX(user_name) AS user_name,
       MAX(user_email) AS user_email,
       model,
       api_key_id,
       MAX(api_key_name) AS api_key_name,
       channel_id,
       MAX(channel_name) AS channel_name,
       account_id,
       MAX(account_name) AS account_name,
       COUNT(*) FILTER (WHERE created_at >= $1)::bigint AS current_requests,
       COALESCE(SUM(input_tokens) FILTER (WHERE created_at >= $1), 0)::bigint AS current_input_tokens,
       COALESCE(SUM(output_tokens) FILTER (WHERE created_at >= $1), 0)::bigint AS current_output_tokens,
       COALESCE(SUM(cache_creation_tokens) FILTER (WHERE created_at >= $1), 0)::bigint AS current_cache_creation_tokens,
       COALESCE(SUM(cache_read_tokens) FILTER (WHERE created_at >= $1), 0)::bigint AS current_cache_read_tokens,
       COALESCE(SUM(base_cost) FILTER (WHERE created_at >= $1), 0)::double precision AS current_base_cost,
       COALESCE(SUM(actual_cost) FILTER (WHERE created_at >= $1), 0)::double precision AS current_actual_cost,
       COALESCE(SUM(input_cost) FILTER (WHERE created_at >= $1), 0)::double precision AS current_input_cost,
       COALESCE(SUM(output_cost) FILTER (WHERE created_at >= $1), 0)::double precision AS current_output_cost,
       COALESCE(SUM(cache_creation_cost) FILTER (WHERE created_at >= $1), 0)::double precision AS current_cache_creation_cost,
       COALESCE(SUM(cache_read_cost) FILTER (WHERE created_at >= $1), 0)::double precision AS current_cache_read_cost,
       COALESCE(MAX(actual_cost) FILTER (WHERE created_at >= $1), 0)::double precision AS current_max_request_cost,
       COALESCE(MAX(CASE WHEN base_cost > 0 AND base_cost >= $6 THEN actual_cost / base_cost END)
                FILTER (WHERE created_at >= $1), 0)::double precision AS current_max_multiplier,
       COUNT(*) FILTER (WHERE created_at >= $1
                         AND base_cost > 0
                         AND base_cost >= $6
                         AND actual_cost / base_cost >= $5)::bigint AS current_high_multiplier_requests,
       COUNT(*) FILTER (WHERE created_at < $4)::bigint AS baseline_requests,
       COALESCE(SUM(input_tokens) FILTER (WHERE created_at < $4), 0)::bigint AS baseline_input_tokens,
       COALESCE(SUM(output_tokens) FILTER (WHERE created_at < $4), 0)::bigint AS baseline_output_tokens,
       COALESCE(SUM(cache_creation_tokens) FILTER (WHERE created_at < $4), 0)::bigint AS baseline_cache_creation_tokens,
       COALESCE(SUM(cache_read_tokens) FILTER (WHERE created_at < $4), 0)::bigint AS baseline_cache_read_tokens,
       COALESCE(SUM(base_cost) FILTER (WHERE created_at < $4), 0)::double precision AS baseline_base_cost,
       COALESCE(SUM(actual_cost) FILTER (WHERE created_at < $4), 0)::double precision AS baseline_actual_cost
FROM usage
GROUP BY user_key, model, api_key_id, channel_id, account_id
HAVING COUNT(*) FILTER (WHERE created_at >= $1) > 0
ORDER BY user_key, model, api_key_id, channel_id, account_id`

const costDailyQuery = `
WITH bounds AS (
    SELECT $1::timestamptz AS window_start,
           $2::timestamptz AS end_at,
           $3::timestamptz AS day_start
), usage AS (
    SELECT COALESCE(NULLIF(BTRIM(ul.user_id::text), ''), 'unknown') AS user_key,
           COALESCE(NULLIF(BTRIM(u.username), ''), '') AS user_name,
           COALESCE(NULLIF(BTRIM(u.email), ''), '') AS user_email,
           COALESCE(ul.api_key_id, 0)::bigint AS api_key_id,
           COALESCE(NULLIF(BTRIM(k.name), ''), '') AS api_key_name,
           ul.created_at,
           GREATEST(COALESCE(ul.actual_cost, 0), 0)::double precision AS actual_cost,
           (GREATEST(COALESCE(ul.input_tokens, 0), 0)::bigint +
            GREATEST(COALESCE(ul.output_tokens, 0), 0)::bigint +
            GREATEST(COALESCE(ul.cache_creation_tokens, 0), 0)::bigint +
            GREATEST(COALESCE(ul.cache_read_tokens, 0), 0)::bigint) AS total_tokens
    FROM usage_logs ul
    LEFT JOIN users u ON u.id = ul.user_id
    LEFT JOIN api_keys k ON k.id = ul.api_key_id
    CROSS JOIN bounds
    WHERE ul.created_at >= LEAST(bounds.window_start, bounds.day_start)
      AND ul.created_at < bounds.end_at
      AND ul.actual_cost > 0
)
SELECT usage.user_key,
       MAX(usage.user_name) AS user_name,
       MAX(usage.user_email) AS user_email,
       usage.api_key_id,
       MAX(usage.api_key_name) AS api_key_name,
       bounds.day_start,
       COUNT(*) FILTER (WHERE usage.created_at >= GREATEST(bounds.window_start, bounds.day_start))::bigint AS window_requests,
       COALESCE(SUM(usage.total_tokens) FILTER (WHERE usage.created_at >= GREATEST(bounds.window_start, bounds.day_start)), 0)::bigint AS window_tokens,
       COALESCE(SUM(usage.actual_cost) FILTER (WHERE usage.created_at >= GREATEST(bounds.window_start, bounds.day_start)), 0)::double precision AS window_cost,
       COALESCE(SUM(usage.actual_cost) FILTER (WHERE usage.created_at >= bounds.day_start), 0)::double precision AS daily_cost
FROM usage
CROSS JOIN bounds
GROUP BY usage.user_key, usage.api_key_id, bounds.day_start
ORDER BY usage.user_key, usage.api_key_id`

type costUsageGroup struct {
	userKey                string
	userName               string
	userEmail              string
	model                  string
	apiKeyID               int64
	apiKeyName             string
	channelID              int64
	channelName            string
	accountID              int64
	accountName            string
	current                costUsageMetrics
	baseline               costUsageMetrics
	maxRequestCost         float64
	maxMultiplier          float64
	highMultiplierRequests int64
}

type costUsageMetrics struct {
	Requests            int64
	InputTokens         int64
	OutputTokens        int64
	CacheCreationTokens int64
	CacheReadTokens     int64
	BaseCost            float64
	ActualCost          float64
	InputCost           float64
	OutputCost          float64
	CacheCreationCost   float64
	CacheReadCost       float64
}

func (m costUsageMetrics) TotalTokens() int64 {
	return m.InputTokens + m.OutputTokens + m.CacheCreationTokens + m.CacheReadTokens
}

func (m costUsageMetrics) CacheableTokens() int64 {
	// input_tokens is the uncached portion of the prompt. Cache creation and
	// cache reads are both part of the cacheable context.
	return m.InputTokens + m.CacheCreationTokens + m.CacheReadTokens
}

func (m costUsageMetrics) CacheHitRate() float64 {
	cacheable := m.CacheableTokens()
	if cacheable <= 0 {
		return 0
	}
	return float64(m.CacheReadTokens) * 100 / float64(cacheable)
}

func (m costUsageMetrics) UnitCost() float64 {
	return costPerMillionTokens(m.ActualCost, m.TotalTokens())
}

func (m costUsageMetrics) Multiplier() float64 {
	if m.BaseCost <= 0 {
		return 0
	}
	return m.ActualCost / m.BaseCost
}

type costDailyUsage struct {
	userKey        string
	userName       string
	userEmail      string
	apiKeyID       int64
	apiKeyName     string
	dayStart       time.Time
	windowRequests int64
	windowTokens   int64
	windowCost     float64
	dailyCost      float64
}

func (s *Store) loadCostUsageGroups(ctx context.Context, bounds costAlertBounds, policy config.CostAlertConfig) ([]costUsageGroup, error) {
	rows, err := s.db.QueryContext(ctx, costUsageQuery,
		bounds.currentStart, bounds.now, bounds.baselineStart, bounds.baselineEnd,
		policy.MultiplierRatio, policy.MinBaseCost)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	groups := make([]costUsageGroup, 0)
	for rows.Next() {
		group, err := scanCostUsageGroup(rows.Scan)
		if err != nil {
			return nil, err
		}
		groups = append(groups, group)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return groups, nil
}

func scanCostUsageGroup(scan func(...any) error) (costUsageGroup, error) {
	var group costUsageGroup
	var current, baseline costUsageMetrics
	err := scan(
		&group.userKey, &group.userName, &group.userEmail, &group.model,
		&group.apiKeyID, &group.apiKeyName, &group.channelID, &group.channelName,
		&group.accountID, &group.accountName,
		&current.Requests, &current.InputTokens, &current.OutputTokens,
		&current.CacheCreationTokens, &current.CacheReadTokens,
		&current.BaseCost, &current.ActualCost, &current.InputCost,
		&current.OutputCost, &current.CacheCreationCost, &current.CacheReadCost,
		&group.maxRequestCost, &group.maxMultiplier, &group.highMultiplierRequests,
		&baseline.Requests, &baseline.InputTokens, &baseline.OutputTokens,
		&baseline.CacheCreationTokens, &baseline.CacheReadTokens,
		&baseline.BaseCost, &baseline.ActualCost,
	)
	group.current = current
	group.baseline = baseline
	return group, err
}

func (s *Store) loadCostDailyUsage(ctx context.Context, bounds costAlertBounds) (map[string]costDailyUsage, error) {
	rows, err := s.db.QueryContext(ctx, costDailyQuery, bounds.currentStart, bounds.now, bounds.dayStart)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	usage := make(map[string]costDailyUsage)
	for rows.Next() {
		item, err := scanCostDailyUsage(rows.Scan)
		if err != nil {
			return nil, err
		}
		usage[costUserTargetKey(item.userKey, item.apiKeyID)] = item
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return usage, nil
}

func scanCostDailyUsage(scan func(...any) error) (costDailyUsage, error) {
	var item costDailyUsage
	err := scan(
		&item.userKey, &item.userName, &item.userEmail, &item.apiKeyID, &item.apiKeyName,
		&item.dayStart, &item.windowRequests, &item.windowTokens,
		&item.windowCost, &item.dailyCost,
	)
	return item, err
}
