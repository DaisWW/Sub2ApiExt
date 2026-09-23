package store

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/DaisWW/Sub2ApiExt/monitoring/internal/config"
	"github.com/DaisWW/Sub2ApiExt/monitoring/internal/model"
)

// costUsageQuery reads a bounded baseline window and aggregates the current
// window in the same pass. The query only returns dimensions that have current
// traffic, so an inactive user does not create work or alerts.
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
       COALESCE(MAX(CASE WHEN base_cost > 0 AND base_cost >= $5 THEN actual_cost / base_cost END)
                FILTER (WHERE created_at >= $1), 0)::double precision AS current_max_multiplier,
       COUNT(*) FILTER (WHERE created_at >= $1
                         AND base_cost > 0
                         AND base_cost >= $5
                         AND actual_cost / base_cost >= $4)::bigint AS current_high_multiplier_requests,
       COUNT(*) FILTER (WHERE created_at < $1)::bigint AS baseline_requests,
       COALESCE(SUM(input_tokens) FILTER (WHERE created_at < $1), 0)::bigint AS baseline_input_tokens,
       COALESCE(SUM(output_tokens) FILTER (WHERE created_at < $1), 0)::bigint AS baseline_output_tokens,
       COALESCE(SUM(cache_creation_tokens) FILTER (WHERE created_at < $1), 0)::bigint AS baseline_cache_creation_tokens,
       COALESCE(SUM(cache_read_tokens) FILTER (WHERE created_at < $1), 0)::bigint AS baseline_cache_read_tokens,
       COALESCE(SUM(base_cost) FILTER (WHERE created_at < $1), 0)::double precision AS baseline_base_cost,
       COALESCE(SUM(actual_cost) FILTER (WHERE created_at < $1), 0)::double precision AS baseline_actual_cost
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

// costAlertRequestQuery returns a bounded sample for both alert scopes. The
// same row can be selected for its detailed model/channel/account group and
// for its user/API-key budget scope; each scope is independently capped.
const costAlertRequestQuery = `
WITH raw AS (
    SELECT ul.id AS usage_log_id,
           COALESCE(NULLIF(BTRIM(ul.user_id::text), ''), 'unknown') AS user_key,
           COALESCE(ul.api_key_id, 0)::bigint AS api_key_id,
           COALESCE(NULLIF(BTRIM(ul.model), ''), 'unknown') AS model,
           COALESCE(ul.channel_id, 0)::bigint AS channel_id,
           COALESCE(NULLIF(BTRIM(c.name), ''), '未归属渠道') AS channel_name,
           COALESCE(ul.account_id, 0)::bigint AS account_id,
           COALESCE(NULLIF(BTRIM(a.name), ''), '未归属账户') AS account_name,
           ul.created_at,
           GREATEST(COALESCE(ul.input_tokens, 0), 0)::bigint AS input_tokens,
           GREATEST(COALESCE(ul.output_tokens, 0), 0)::bigint AS output_tokens,
           GREATEST(COALESCE(ul.cache_creation_tokens, 0), 0)::bigint AS cache_creation_tokens,
           GREATEST(COALESCE(ul.cache_read_tokens, 0), 0)::bigint AS cache_read_tokens,
           GREATEST(COALESCE(ul.total_cost, 0), 0)::double precision AS base_cost,
           GREATEST(COALESCE(ul.actual_cost, 0), 0)::double precision AS actual_cost,
           GREATEST(COALESCE(ul.duration_ms, 0), 0)::bigint AS duration_ms,
           GREATEST(COALESCE(ul.first_token_ms, 0), 0)::bigint AS first_token_ms
    FROM usage_logs ul
    LEFT JOIN channels c ON c.id = ul.channel_id
    LEFT JOIN accounts a ON a.id = ul.account_id
    WHERE ul.created_at >= $1
      AND ul.created_at < $2
      AND ul.actual_cost > 0
), ranked AS (
    SELECT raw.*,
           ROW_NUMBER() OVER (
               PARTITION BY user_key, model, api_key_id, channel_id, account_id
               ORDER BY actual_cost DESC,
                        CASE WHEN base_cost > 0 THEN actual_cost / base_cost ELSE 0 END DESC,
                        created_at DESC, usage_log_id DESC
           ) AS group_rank,
           ROW_NUMBER() OVER (
               PARTITION BY user_key, api_key_id
               ORDER BY actual_cost DESC,
                        CASE WHEN base_cost > 0 THEN actual_cost / base_cost ELSE 0 END DESC,
                        created_at DESC, usage_log_id DESC
           ) AS user_rank
    FROM raw
)
SELECT usage_log_id, user_key, api_key_id, model, channel_id, channel_name,
       account_id, account_name, created_at,
       input_tokens, output_tokens, cache_creation_tokens, cache_read_tokens,
       base_cost, actual_cost, duration_ms, first_token_ms, group_rank, user_rank
FROM ranked
WHERE group_rank <= $3 OR user_rank <= $3
ORDER BY user_key, api_key_id, model, channel_id, account_id, group_rank, user_rank`

// costAlertCacheMissQuery returns every request that independently meets the
// request-level cache-miss threshold. It also counts accounts used by the
// same session and scope so a cache miss can be annotated when routing moved
// the session between upstream accounts. Session IDs stay in memory only and
// are never included in the notification or persisted alert row.
const costAlertCacheMissQuery = `
WITH raw_base AS MATERIALIZED (
    SELECT ul.id AS usage_log_id,
           COALESCE(NULLIF(BTRIM(ul.user_id::text), ''), 'unknown') AS user_key,
           COALESCE(NULLIF(BTRIM(u.username), ''), '') AS user_name,
           COALESCE(NULLIF(BTRIM(u.email), ''), '') AS user_email,
           COALESCE(ul.api_key_id, 0)::bigint AS api_key_id,
           COALESCE(NULLIF(BTRIM(k.name), ''), '') AS api_key_name,
           COALESCE(NULLIF(BTRIM(ul.model), ''), 'unknown') AS model,
           COALESCE(ul.channel_id, 0)::bigint AS channel_id,
           COALESCE(NULLIF(BTRIM(c.name), ''), '未归属渠道') AS channel_name,
           COALESCE(ul.group_id, 0)::bigint AS group_id,
           COALESCE(ul.account_id, 0)::bigint AS account_id,
           COALESCE(NULLIF(BTRIM(a.name), ''), '未归属账户') AS account_name,
           COALESCE(NULLIF(BTRIM(ul.session_id), ''), '') AS session_key,
           ul.created_at,
           GREATEST(COALESCE(ul.input_tokens, 0), 0)::bigint AS input_tokens,
           GREATEST(COALESCE(ul.output_tokens, 0), 0)::bigint AS output_tokens,
           GREATEST(COALESCE(ul.cache_creation_tokens, 0), 0)::bigint AS cache_creation_tokens,
           GREATEST(COALESCE(ul.cache_read_tokens, 0), 0)::bigint AS cache_read_tokens,
           GREATEST(COALESCE(ul.total_cost, 0), 0)::double precision AS base_cost,
           GREATEST(COALESCE(ul.actual_cost, 0), 0)::double precision AS actual_cost,
           GREATEST(COALESCE(ul.duration_ms, 0), 0)::bigint AS duration_ms,
           GREATEST(COALESCE(ul.first_token_ms, 0), 0)::bigint AS first_token_ms
    FROM usage_logs ul
    LEFT JOIN users u ON u.id = ul.user_id
    LEFT JOIN api_keys k ON k.id = ul.api_key_id
    LEFT JOIN channels c ON c.id = ul.channel_id
    LEFT JOIN accounts a ON a.id = ul.account_id
    WHERE ul.created_at >= $1
      AND ul.created_at < $2
      AND ul.actual_cost > 0
), raw AS MATERIALIZED (
    SELECT raw_base.*,
           CASE WHEN input_tokens + cache_creation_tokens + cache_read_tokens > 0
                THEN cache_read_tokens::double precision /
                     (input_tokens + cache_creation_tokens + cache_read_tokens)
                ELSE 0
           END AS cache_hit_rate
    FROM raw_base
), low AS MATERIALIZED (
    SELECT raw.*
    FROM raw
    WHERE input_tokens >= $3
      AND cache_hit_rate < $4
), affected_sessions AS (
    SELECT DISTINCT user_key, api_key_id, model, channel_id, group_id, session_key
    FROM low
    WHERE session_key <> ''
), session_accounts AS (
    SELECT raw.user_key, raw.api_key_id, raw.model, raw.channel_id, raw.group_id,
           raw.session_key, COUNT(DISTINCT raw.account_id)::bigint AS account_count
    FROM raw
    JOIN affected_sessions sessions
      ON sessions.user_key = raw.user_key
     AND sessions.api_key_id = raw.api_key_id
     AND sessions.model = raw.model
     AND sessions.channel_id = raw.channel_id
     AND sessions.group_id = raw.group_id
     AND sessions.session_key = raw.session_key
    GROUP BY raw.user_key, raw.api_key_id, raw.model, raw.channel_id, raw.group_id, raw.session_key
)
SELECT low.usage_log_id, low.user_key, low.user_name, low.user_email,
       low.api_key_id, low.api_key_name, low.model, low.channel_id, low.channel_name,
       low.group_id, low.account_id, low.account_name, low.session_key, low.created_at,
       low.input_tokens, low.output_tokens, low.cache_creation_tokens, low.cache_read_tokens,
       low.base_cost, low.actual_cost, low.duration_ms, low.first_token_ms,
       low.cache_hit_rate, COALESCE(session_accounts.account_count, 1)::bigint AS session_account_count
FROM low
LEFT JOIN session_accounts
  ON session_accounts.user_key = low.user_key
 AND session_accounts.api_key_id = low.api_key_id
 AND session_accounts.model = low.model
 AND session_accounts.channel_id = low.channel_id
 AND session_accounts.group_id = low.group_id
 AND session_accounts.session_key = low.session_key
ORDER BY low.user_key, low.api_key_id, low.model, low.channel_id, low.group_id,
         low.created_at DESC, low.usage_log_id DESC`

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
	// cache reads are both part of the cacheable context, so omitting creation
	// tokens would make a cache miss look artificially healthy.
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

func (s *Store) AnalyzeCostAlerts(ctx context.Context, policy config.CostAlertConfig) ([]model.CostAlertEvent, error) {
	if !policy.Enabled {
		return nil, nil
	}
	now := time.Now().UTC()
	currentStart := now.Add(-policy.Window)
	baselineStart := now.Add(-policy.Baseline)
	groups, err := s.loadCostUsageGroups(ctx, currentStart, now, baselineStart, policy.MultiplierRatio, policy.MinBaseCost)
	if err != nil {
		return nil, fmt.Errorf("load cost usage: %w", err)
	}
	daily := map[string]costDailyUsage{}
	if policy.DailyBudget > 0 {
		localNow := now.In(time.Local)
		dayStart := time.Date(localNow.Year(), localNow.Month(), localNow.Day(), 0, 0, 0, 0, time.Local)
		daily, err = s.loadCostDailyUsage(ctx, currentStart, now, dayStart)
		if err != nil {
			return nil, fmt.Errorf("load daily cost: %w", err)
		}
	}

	candidates := make([]model.CostAlertEvent, 0)
	for _, group := range groups {
		candidates = append(candidates, evaluateCostUsageGroup(group, policy, currentStart, now)...)
	}
	cacheMissRequests, err := s.loadCostAlertCacheMissRequests(ctx, currentStart, now, policy)
	if err != nil {
		return nil, fmt.Errorf("load request cache misses: %w", err)
	}
	candidates = append(candidates, evaluateCacheMissRequests(cacheMissRequests, policy, currentStart, now)...)
	candidates = append(candidates, evaluateBudgetBurn(daily, policy, now)...)
	if len(candidates) == 0 {
		return nil, nil
	}
	if err := s.attachCostAlertRequestSamples(ctx, candidates, currentStart, now); err != nil {
		return nil, fmt.Errorf("load cost alert requests: %w", err)
	}
	return s.persistCostAlertCandidates(ctx, candidates, now, policy.Cooldown)
}

func (s *Store) loadCostUsageGroups(ctx context.Context, currentStart, currentEnd, baselineStart time.Time, multiplierThreshold, minBaseCost float64) ([]costUsageGroup, error) {
	rows, err := s.db.QueryContext(ctx, costUsageQuery, currentStart, currentEnd, baselineStart, multiplierThreshold, minBaseCost)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	groups := make([]costUsageGroup, 0)
	for rows.Next() {
		var group costUsageGroup
		var current, baseline costUsageMetrics
		if err := rows.Scan(
			&group.userKey, &group.userName, &group.userEmail, &group.model, &group.apiKeyID, &group.apiKeyName,
			&group.channelID, &group.channelName,
			&group.accountID, &group.accountName,
			&current.Requests, &current.InputTokens, &current.OutputTokens,
			&current.CacheCreationTokens, &current.CacheReadTokens,
			&current.BaseCost, &current.ActualCost, &current.InputCost,
			&current.OutputCost, &current.CacheCreationCost, &current.CacheReadCost,
			&group.maxRequestCost, &group.maxMultiplier, &group.highMultiplierRequests,
			&baseline.Requests, &baseline.InputTokens, &baseline.OutputTokens,
			&baseline.CacheCreationTokens, &baseline.CacheReadTokens,
			&baseline.BaseCost, &baseline.ActualCost,
		); err != nil {
			return nil, err
		}
		group.current = current
		group.baseline = baseline
		groups = append(groups, group)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return groups, nil
}

func (s *Store) loadCostDailyUsage(ctx context.Context, windowStart, end, dayStart time.Time) (map[string]costDailyUsage, error) {
	rows, err := s.db.QueryContext(ctx, costDailyQuery, windowStart, end, dayStart)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	usage := make(map[string]costDailyUsage)
	for rows.Next() {
		var item costDailyUsage
		if err := rows.Scan(
			&item.userKey, &item.userName, &item.userEmail, &item.apiKeyID, &item.apiKeyName,
			&item.dayStart, &item.windowRequests, &item.windowTokens,
			&item.windowCost, &item.dailyCost,
		); err != nil {
			return nil, err
		}
		// Keep API keys separate. A user can have several keys, and assigning by
		// user alone would silently overwrite whichever row was read first.
		usage[costUserTargetKey(item.userKey, item.apiKeyID)] = item
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return usage, nil
}

const costAlertRequestSampleLimit = 8

type costAlertRequestSamples struct {
	byGroup map[string][]model.CostAlertRequest
	byUser  map[string][]model.CostAlertRequest
}

type costAlertCacheMissRequest struct {
	request             model.CostAlertRequest
	userKey             string
	userName            string
	userEmail           string
	apiKeyID            int64
	apiKeyName          string
	channelID           int64
	groupID             int64
	sessionKey          string
	sessionAccountCount int64
}

func (s *Store) attachCostAlertRequestSamples(ctx context.Context, events []model.CostAlertEvent, start, end time.Time) error {
	samples, err := s.loadCostAlertRequestSamples(ctx, start, end, costAlertRequestSampleLimit)
	if err != nil {
		return err
	}
	for index := range events {
		if events[index].Kind == model.CostAlertCacheMiss {
			// Request-level events already carry their filtered samples. The
			// generic sample query is ranked by cost and could replace them with
			// unrelated healthy requests.
			continue
		}
		if events[index].Kind == model.CostAlertBudgetBurn {
			events[index].RequestSamples = samples.byUser[events[index].TargetKey]
			continue
		}
		events[index].RequestSamples = samples.byGroup[events[index].TargetKey]
	}
	return nil
}

func (s *Store) loadCostAlertRequestSamples(ctx context.Context, start, end time.Time, limit int) (costAlertRequestSamples, error) {
	if limit <= 0 {
		limit = costAlertRequestSampleLimit
	}
	rows, err := s.db.QueryContext(ctx, costAlertRequestQuery, start, end, limit)
	if err != nil {
		return costAlertRequestSamples{}, err
	}
	defer rows.Close()
	samples := costAlertRequestSamples{
		byGroup: make(map[string][]model.CostAlertRequest),
		byUser:  make(map[string][]model.CostAlertRequest),
	}
	for rows.Next() {
		var (
			usageLogID, apiKeyID, channelID, accountID                      int64
			userKey, modelName, channelName, accountName                    string
			createdAt                                                       time.Time
			inputTokens, outputTokens, cacheCreationTokens, cacheReadTokens int64
			baseCost, actualCost                                            float64
			durationMS, firstTokenMS                                        int64
			groupRank, userRank                                             int64
		)
		if err := rows.Scan(
			&usageLogID, &userKey, &apiKeyID, &modelName, &channelID, &channelName,
			&accountID, &accountName, &createdAt,
			&inputTokens, &outputTokens, &cacheCreationTokens, &cacheReadTokens,
			&baseCost, &actualCost, &durationMS, &firstTokenMS, &groupRank, &userRank,
		); err != nil {
			return costAlertRequestSamples{}, err
		}
		request := model.CostAlertRequest{
			UsageLogID:          usageLogID,
			CreatedAt:           createdAt,
			Model:               modelName,
			ChannelName:         channelName,
			AccountName:         accountName,
			AccountID:           accountID,
			InputTokens:         inputTokens,
			OutputTokens:        outputTokens,
			CacheCreationTokens: cacheCreationTokens,
			CacheReadTokens:     cacheReadTokens,
			TotalTokens:         inputTokens + outputTokens + cacheCreationTokens + cacheReadTokens,
			BaseCost:            baseCost,
			ActualCost:          actualCost,
			DurationMS:          durationMS,
			FirstTokenMS:        firstTokenMS,
		}
		if baseCost > 0 {
			request.Multiplier = actualCost / baseCost
		}
		cacheableTokens := inputTokens + cacheCreationTokens + cacheReadTokens
		if cacheableTokens > 0 {
			request.CacheHitRate = float64(cacheReadTokens) * 100 / float64(cacheableTokens)
		}
		if groupRank <= int64(limit) {
			key := costTargetKey(userKey, apiKeyID, modelName, channelID, accountID)
			samples.byGroup[key] = append(samples.byGroup[key], request)
		}
		if userRank <= int64(limit) {
			key := costUserTargetKey(userKey, apiKeyID)
			samples.byUser[key] = append(samples.byUser[key], request)
		}
	}
	if err := rows.Err(); err != nil {
		return costAlertRequestSamples{}, err
	}
	return samples, nil
}

func (s *Store) loadCostAlertCacheMissRequests(ctx context.Context, start, end time.Time, policy config.CostAlertConfig) ([]costAlertCacheMissRequest, error) {
	rows, err := s.db.QueryContext(ctx, costAlertCacheMissQuery, start, end,
		policy.CacheMissInputTokens, policy.CacheCurrentMax)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	requests := make([]costAlertCacheMissRequest, 0)
	for rows.Next() {
		var (
			usageLogID, apiKeyID, channelID, groupID, accountID int64
			userKey, userName, userEmail, apiKeyName, modelName string
			channelName, accountName, sessionKey                string
			createdAt                                           time.Time
			inputTokens, outputTokens, cacheCreationTokens      int64
			cacheReadTokens, durationMS, firstTokenMS           int64
			baseCost, actualCost, cacheHitRate                  float64
			sessionAccountCount                                 int64
		)
		if err := rows.Scan(
			&usageLogID, &userKey, &userName, &userEmail, &apiKeyID, &apiKeyName,
			&modelName, &channelID, &channelName, &groupID, &accountID, &accountName,
			&sessionKey, &createdAt, &inputTokens, &outputTokens, &cacheCreationTokens,
			&cacheReadTokens, &baseCost, &actualCost, &durationMS, &firstTokenMS,
			&cacheHitRate, &sessionAccountCount,
		); err != nil {
			return nil, err
		}
		request := model.CostAlertRequest{
			UsageLogID:          usageLogID,
			CreatedAt:           createdAt,
			Model:               modelName,
			ChannelName:         channelName,
			AccountName:         accountName,
			AccountID:           accountID,
			InputTokens:         inputTokens,
			OutputTokens:        outputTokens,
			CacheCreationTokens: cacheCreationTokens,
			CacheReadTokens:     cacheReadTokens,
			TotalTokens:         inputTokens + outputTokens + cacheCreationTokens + cacheReadTokens,
			BaseCost:            baseCost,
			ActualCost:          actualCost,
			CacheHitRate:        cacheHitRate * 100,
			DurationMS:          durationMS,
			FirstTokenMS:        firstTokenMS,
		}
		if baseCost > 0 {
			request.Multiplier = actualCost / baseCost
		}
		requests = append(requests, costAlertCacheMissRequest{
			request:             request,
			userKey:             userKey,
			userName:            userName,
			userEmail:           userEmail,
			apiKeyID:            apiKeyID,
			apiKeyName:          apiKeyName,
			channelID:           channelID,
			groupID:             groupID,
			sessionKey:          sessionKey,
			sessionAccountCount: sessionAccountCount,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return requests, nil
}

type costAlertCacheMissGroup struct {
	key         string
	items       []costAlertCacheMissRequest
	userKey     string
	userName    string
	userEmail   string
	apiKeyID    int64
	apiKeyName  string
	model       string
	channelID   int64
	channelName string
	groupID     int64
}

func evaluateCacheMissRequests(items []costAlertCacheMissRequest, policy config.CostAlertConfig, start, end time.Time) []model.CostAlertEvent {
	groups := make(map[string]*costAlertCacheMissGroup)
	for _, item := range items {
		if item.request.InputTokens < policy.CacheMissInputTokens || item.request.CacheHitRate/100 >= policy.CacheCurrentMax {
			continue
		}
		key := costCacheMissTargetKey(item.userKey, item.apiKeyID, item.request.Model, item.channelID, item.groupID)
		group := groups[key]
		if group == nil {
			group = &costAlertCacheMissGroup{
				key:         key,
				userKey:     item.userKey,
				userName:    item.userName,
				userEmail:   item.userEmail,
				apiKeyID:    item.apiKeyID,
				apiKeyName:  item.apiKeyName,
				model:       item.request.Model,
				channelID:   item.channelID,
				channelName: item.request.ChannelName,
				groupID:     item.groupID,
			}
			groups[key] = group
		}
		group.items = append(group.items, item)
	}

	keys := make([]string, 0, len(groups))
	for key := range groups {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	events := make([]model.CostAlertEvent, 0, len(keys))
	for _, key := range keys {
		group := groups[key]
		if len(group.items) < policy.CacheMissMinRequests {
			continue
		}
		sort.SliceStable(group.items, func(i, j int) bool {
			left, right := group.items[i].request, group.items[j].request
			if left.ActualCost != right.ActualCost {
				return left.ActualCost > right.ActualCost
			}
			if left.Multiplier != right.Multiplier {
				return left.Multiplier > right.Multiplier
			}
			if !left.CreatedAt.Equal(right.CreatedAt) {
				return left.CreatedAt.After(right.CreatedAt)
			}
			return left.UsageLogID > right.UsageLogID
		})

		var (
			inputTokens, outputTokens, cacheCreationTokens, cacheReadTokens int64
			totalTokens                                                     int64
			maxRequestCost, baseCost, actualCost                            float64
			accountIDs                                                      = make(map[int64]struct{})
			accountNames                                                    = make(map[int64]string)
			crossAccount                                                    bool
		)
		for _, item := range group.items {
			request := item.request
			inputTokens += request.InputTokens
			outputTokens += request.OutputTokens
			cacheCreationTokens += request.CacheCreationTokens
			cacheReadTokens += request.CacheReadTokens
			totalTokens += request.TotalTokens
			baseCost += request.BaseCost
			actualCost += request.ActualCost
			if request.ActualCost > maxRequestCost {
				maxRequestCost = request.ActualCost
			}
			accountIDs[request.AccountID] = struct{}{}
			if _, exists := accountNames[request.AccountID]; !exists {
				accountNames[request.AccountID] = request.AccountName
			}
			if item.sessionAccountCount > 1 {
				crossAccount = true
			}
		}
		if len(accountIDs) > 1 {
			crossAccount = true
		}

		accountID, accountName := int64(0), ""
		if len(accountIDs) == 1 {
			for id := range accountIDs {
				accountID = id
				accountName = accountNames[id]
			}
		} else if len(accountIDs) > 1 {
			accountName = "多个账户"
		}
		currentCacheable := inputTokens + cacheCreationTokens + cacheReadTokens
		currentCacheHitRate := 0.0
		if currentCacheable > 0 {
			currentCacheHitRate = float64(cacheReadTokens) * 100 / float64(currentCacheable)
		}
		currentMultiplier := 0.0
		if baseCost > 0 {
			currentMultiplier = actualCost / baseCost
		}
		message := fmt.Sprintf(
			"用户 %s 的 %s 在 %s 最近窗口发现 %d 条输入 Tokens ≥ %d 且缓存命中率低于 %.1f%% 的请求；这些请求加权缓存命中率 %.1f%%，成本 %.4f。",
			costUserLabel(group.userName, group.userEmail, group.userKey), group.model, group.channelName,
			len(group.items), policy.CacheMissInputTokens, policy.CacheCurrentMax*100, currentCacheHitRate, actualCost,
		)
		if crossAccount {
			message += "同一 session 在当前窗口使用了多个账户，疑似账户切换导致缓存断档。"
		} else {
			message += "未发现同一 session 跨账户证据，仍需检查客户端上下文或缓存字段。"
		}

		samples := make([]model.CostAlertRequest, 0, len(group.items))
		for index, item := range group.items {
			if index >= costAlertRequestSampleLimit {
				break
			}
			samples = append(samples, item.request)
		}
		events = append(events, model.CostAlertEvent{
			AlertKey:            model.CostAlertCacheMiss + "|" + key,
			Kind:                model.CostAlertCacheMiss,
			Severity:            "warning",
			TargetKey:           key,
			Title:               "请求级缓存命中异常",
			Message:             message,
			UserKey:             group.userKey,
			UserName:            group.userName,
			UserEmail:           group.userEmail,
			APIKeyID:            group.apiKeyID,
			APIKeyName:          group.apiKeyName,
			Model:               group.model,
			ChannelName:         group.channelName,
			AccountName:         accountName,
			AccountID:           accountID,
			Requests:            int64(len(group.items)),
			TotalTokens:         totalTokens,
			MaxRequestCost:      maxRequestCost,
			InputTokens:         inputTokens,
			OutputTokens:        outputTokens,
			CacheCreationTokens: cacheCreationTokens,
			CacheReadTokens:     cacheReadTokens,
			CurrentCost:         actualCost,
			CurrentUnitCost:     costPerMillionTokens(actualCost, totalTokens),
			CurrentCacheHitRate: currentCacheHitRate,
			CurrentMultiplier:   currentMultiplier,
			WindowStart:         start,
			WindowEnd:           end,
			RequestSamples:      samples,
			CreatedAt:           end,
		})
	}
	return events
}

func evaluateCostUsageGroup(group costUsageGroup, policy config.CostAlertConfig, start, end time.Time) []model.CostAlertEvent {
	current := group.current
	baseline := group.baseline
	currentTokens := current.TotalTokens()
	baselineTokens := baseline.TotalTokens()
	baseEvent := model.CostAlertEvent{
		TargetKey:            costTargetKey(group.userKey, group.apiKeyID, group.model, group.channelID, group.accountID),
		UserKey:              group.userKey,
		UserName:             group.userName,
		UserEmail:            group.userEmail,
		APIKeyID:             group.apiKeyID,
		APIKeyName:           group.apiKeyName,
		Model:                group.model,
		ChannelName:          group.channelName,
		AccountName:          group.accountName,
		AccountID:            group.accountID,
		Requests:             current.Requests,
		TotalTokens:          currentTokens,
		MaxRequestCost:       group.maxRequestCost,
		InputTokens:          current.InputTokens,
		OutputTokens:         current.OutputTokens,
		CacheCreationTokens:  current.CacheCreationTokens,
		CacheReadTokens:      current.CacheReadTokens,
		CurrentCost:          current.ActualCost,
		BaselineCost:         baseline.ActualCost,
		InputCost:            current.InputCost,
		OutputCost:           current.OutputCost,
		CacheCreationCost:    current.CacheCreationCost,
		CacheReadCost:        current.CacheReadCost,
		CurrentUnitCost:      current.UnitCost(),
		BaselineUnitCost:     baseline.UnitCost(),
		CurrentCacheHitRate:  current.CacheHitRate(),
		BaselineCacheHitRate: baseline.CacheHitRate(),
		CurrentMultiplier:    current.Multiplier(),
		BaselineMultiplier:   baseline.Multiplier(),
		WindowStart:          start,
		WindowEnd:            end,
		CreatedAt:            end,
		Severity:             "warning",
	}
	baseEvent.AlertKey = baseEvent.TargetKey
	events := make([]model.CostAlertEvent, 0, 4)
	if policy.SingleRequestCost > 0 && group.maxRequestCost >= policy.SingleRequestCost {
		event := baseEvent
		event.Kind = model.CostAlertSingleRequest
		event.Title = "单条请求成本过高"
		event.Message = fmt.Sprintf(
			"用户 %s 的 %s 在 %s 出现单条成本 %.4f，已超过配置上限 %.4f。",
			costUserLabel(group.userName, group.userEmail, group.userKey), group.model, group.channelName,
			group.maxRequestCost, policy.SingleRequestCost,
		)
		event.Severity = "critical"
		events = append(events, event)
	}
	if multiplierSpike(group, policy) {
		event := baseEvent
		event.Kind = model.CostAlertMultiplier
		event.Title = "实际倍率异常"
		event.Message = fmt.Sprintf(
			"用户 %s 的 %s 在 %s 最近窗口实际倍率 %.2fx，历史 %.2fx，最高单条倍率 %.2fx。",
			costUserLabel(group.userName, group.userEmail, group.userKey), group.model, group.channelName, event.CurrentMultiplier,
			event.BaselineMultiplier, group.maxMultiplier,
		)
		event.Severity = "critical"
		events = append(events, event)
	}
	if current.Requests < int64(policy.MinRequests) || current.ActualCost < policy.MinCost || currentTokens < policy.MinTokens {
		for index := range events {
			events[index].AlertKey = events[index].Kind + "|" + events[index].TargetKey
		}
		return events
	}
	baselineReady := baseline.Requests >= int64(policy.MinRequests) && baselineTokens >= policy.MinTokens
	if baselineReady && cacheDegraded(current, baseline, policy) {
		event := baseEvent
		event.Kind = model.CostAlertCacheDegraded
		event.Title = "疑似缓存失效"
		event.Message = fmt.Sprintf(
			"用户 %s 的 %s 在 %s 最近窗口缓存命中率 %.1f%%（历史 %.1f%%），单位成本 %.4f（历史 %.4f）。",
			costUserLabel(group.userName, group.userEmail, group.userKey), group.model, group.channelName, event.CurrentCacheHitRate,
			event.BaselineCacheHitRate, event.CurrentUnitCost, event.BaselineUnitCost,
		)
		events = append(events, event)
	}
	if baselineReady && unitCostSpike(current, baseline, policy) {
		event := baseEvent
		event.Kind = model.CostAlertUnitCost
		event.Title = "每百万 Tokens 成本异常"
		event.Message = fmt.Sprintf(
			"用户 %s 的 %s 在 %s 最近窗口单位成本 %.4f，历史基线 %.4f，当前成本 %.4f，最高单条成本 %.4f。",
			costUserLabel(group.userName, group.userEmail, group.userKey), group.model, group.channelName, event.CurrentUnitCost,
			event.BaselineUnitCost, event.CurrentCost, event.MaxRequestCost,
		)
		if event.BaselineUnitCost > 0 && event.CurrentUnitCost >= event.BaselineUnitCost*policy.UnitCostRatio*1.5 {
			event.Severity = "critical"
		}
		events = append(events, event)
	}
	for index := range events {
		events[index].AlertKey = events[index].Kind + "|" + events[index].TargetKey
	}
	return events
}

func cacheDegraded(current, baseline costUsageMetrics, policy config.CostAlertConfig) bool {
	if current.CacheableTokens() < policy.MinTokens || baseline.CacheableTokens() < policy.MinTokens {
		return false
	}
	if baseline.CacheHitRate()/100 < policy.CacheBaselineMin || current.CacheHitRate()/100 > policy.CacheCurrentMax {
		return false
	}
	return current.UnitCost() > 0 && baseline.UnitCost() > 0 &&
		current.UnitCost() >= baseline.UnitCost()*policy.CacheCostRatio
}

func unitCostSpike(current, baseline costUsageMetrics, policy config.CostAlertConfig) bool {
	return current.TotalTokens() >= policy.MinTokens && baseline.TotalTokens() >= policy.MinTokens &&
		current.UnitCost() > 0 && baseline.UnitCost() > 0 &&
		current.UnitCost() >= baseline.UnitCost()*policy.UnitCostRatio
}

func multiplierSpike(group costUsageGroup, policy config.CostAlertConfig) bool {
	current := group.current.Multiplier()
	baseline := group.baseline.Multiplier()
	if group.current.BaseCost < policy.MinBaseCost || current <= 0 {
		return false
	}
	if baseline > 0 {
		threshold := baseline * policy.MultiplierRatio
		return current >= threshold || group.maxMultiplier >= threshold
	}
	return current >= policy.MultiplierRatio || group.highMultiplierRequests >= 2
}

func evaluateBudgetBurn(usage map[string]costDailyUsage, policy config.CostAlertConfig, now time.Time) []model.CostAlertEvent {
	if policy.DailyBudget <= 0 {
		return nil
	}
	events := make([]model.CostAlertEvent, 0)
	for userKey, item := range usage {
		if item.dayStart.IsZero() || (item.windowCost < policy.MinCost && item.dailyCost < policy.DailyBudget) {
			continue
		}
		elapsed := now.Sub(item.dayStart)
		if elapsed < 0 {
			elapsed = 0
		}
		if elapsed > 24*time.Hour {
			elapsed = 24 * time.Hour
		}
		remaining := 24*time.Hour - elapsed
		windowHours := policy.Window.Hours()
		if windowHours <= 0 {
			continue
		}
		burnRate := item.windowCost / windowHours
		allowedRate := policy.DailyBudget / 24
		projected := item.dailyCost + burnRate*remaining.Hours()
		if item.dailyCost < policy.DailyBudget*0.8 && burnRate < allowedRate*policy.BurnRatio && projected < policy.DailyBudget {
			continue
		}
		severity := "warning"
		if item.dailyCost >= policy.DailyBudget || projected >= policy.DailyBudget {
			severity = "critical"
		}
		target := costUserTargetKey(userKey, item.apiKeyID)
		events = append(events, model.CostAlertEvent{
			AlertKey:  model.CostAlertBudgetBurn + "|" + target,
			Kind:      model.CostAlertBudgetBurn,
			Severity:  severity,
			TargetKey: target,
			Title:     "消费速度过高",
			Message: fmt.Sprintf(
				"用户 %s 今日已消费 %.4f，最近窗口消费 %.4f，按当前速度预计今日消费 %.4f，预算 %.4f。",
				costUserLabel(item.userName, item.userEmail, userKey), item.dailyCost, item.windowCost,
				projected, policy.DailyBudget,
			),
			UserKey:       userKey,
			UserName:      item.userName,
			UserEmail:     item.userEmail,
			APIKeyID:      item.apiKeyID,
			APIKeyName:    item.apiKeyName,
			Requests:      item.windowRequests,
			TotalTokens:   item.windowTokens,
			CurrentCost:   item.windowCost,
			DailyCost:     item.dailyCost,
			ProjectedCost: projected,
			WindowStart:   now.Add(-policy.Window),
			WindowEnd:     now,
			CreatedAt:     now,
		})
	}
	return events
}

func costUserLabel(name, email, userKey string) string {
	name = strings.Join(strings.Fields(strings.TrimSpace(name)), " ")
	email = strings.Join(strings.Fields(strings.TrimSpace(email)), " ")
	label := name
	if email != "" && !strings.EqualFold(email, name) {
		if label == "" {
			label = email
		} else {
			label += " <" + email + ">"
		}
	}
	if label == "" {
		label = "用户"
	}
	if userKey != "" && userKey != "unknown" {
		label += " #" + userKey
	}
	return label
}

func costUserTargetKey(userKey string, apiKeyID int64) string {
	return fmt.Sprintf("user:%s|key:%d", userKey, apiKeyID)
}

func costTargetKey(userKey string, apiKeyID int64, modelName string, channelID, accountID int64) string {
	return fmt.Sprintf("%s|model:%s|channel:%d|account:%d", costUserTargetKey(userKey, apiKeyID), modelName, channelID, accountID)
}

func costCacheMissTargetKey(userKey string, apiKeyID int64, modelName string, channelID, groupID int64) string {
	return fmt.Sprintf("%s|model:%s|channel:%d|group:%d", costUserTargetKey(userKey, apiKeyID), modelName, channelID, groupID)
}

func (s *Store) persistCostAlertCandidates(ctx context.Context, candidates []model.CostAlertEvent, now time.Time, cooldown time.Duration) ([]model.CostAlertEvent, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	newAlerts := make([]model.CostAlertEvent, 0, len(candidates))
	for _, candidate := range candidates {
		var lastAlerted sql.NullTime
		err := tx.QueryRowContext(ctx, `
SELECT last_alerted_at
FROM monitoring_cost_alert_states
WHERE alert_key = $1
FOR UPDATE`, candidate.AlertKey).Scan(&lastAlerted)
		if err != nil && err != sql.ErrNoRows {
			return nil, err
		}
		shouldNotify := err == sql.ErrNoRows || !lastAlerted.Valid || now.Sub(lastAlerted.Time) >= cooldown
		if err == sql.ErrNoRows {
			_, err = tx.ExecContext(ctx, `
INSERT INTO monitoring_cost_alert_states (alert_key, last_alerted_at)
VALUES ($1, CASE WHEN $2 THEN $3::timestamptz ELSE NULL END)`, candidate.AlertKey, shouldNotify, now)
		} else {
			if shouldNotify {
				_, err = tx.ExecContext(ctx, `
UPDATE monitoring_cost_alert_states
SET last_alerted_at = $2, updated_at = NOW()
WHERE alert_key = $1`, candidate.AlertKey, now)
			} else {
				_, err = tx.ExecContext(ctx, `
UPDATE monitoring_cost_alert_states
SET updated_at = NOW()
WHERE alert_key = $1`, candidate.AlertKey)
			}
		}
		if err != nil {
			return nil, err
		}
		if !shouldNotify {
			continue
		}
		var id int64
		if err := tx.QueryRowContext(ctx, `
INSERT INTO monitoring_cost_alerts
 (alert_key, kind, severity, target_key, title, message)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING id`, candidate.AlertKey, candidate.Kind, candidate.Severity,
			candidate.TargetKey, candidate.Title, candidate.Message).Scan(&id); err != nil {
			return nil, err
		}
		candidate.ID = id
		candidate.CreatedAt = now
		newAlerts = append(newAlerts, candidate)
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return newAlerts, nil
}

// DiscardCostAlertNotifications discards rows created for a failed email send.
// The analysis transaction records the cooldown before the external SMTP call;
// keeping that timestamp makes retries bounded by the configured cooldown,
// while removing the row avoids presenting an unsent email as a delivered
// alert in a future history view.
func (s *Store) DiscardCostAlertNotifications(ctx context.Context, events []model.CostAlertEvent) error {
	if len(events) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, event := range events {
		if event.AlertKey == "" {
			continue
		}
		if event.ID > 0 {
			if _, err := tx.ExecContext(ctx, `
DELETE FROM monitoring_cost_alerts
WHERE id = $1 AND alert_key = $2`, event.ID, event.AlertKey); err != nil {
				return err
			}
		}
		if event.CreatedAt.IsZero() {
			if _, err := tx.ExecContext(ctx, `
UPDATE monitoring_cost_alert_states
SET updated_at = NOW()
WHERE alert_key = $1`, event.AlertKey); err != nil {
				return err
			}
			continue
		}
		if _, err := tx.ExecContext(ctx, `
UPDATE monitoring_cost_alert_states
SET updated_at = NOW()
WHERE alert_key = $1
  AND last_alerted_at IS NOT DISTINCT FROM $2::timestamptz`, event.AlertKey, event.CreatedAt); err != nil {
			return err
		}
	}
	return tx.Commit()
}
