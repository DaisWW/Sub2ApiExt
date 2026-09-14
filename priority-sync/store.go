package main

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"strings"
	"time"
)

// priorityMetricsQuery 只读取 Sub2API 原始账户、用量和错误表。
// 成功请求按 (account_id, client_request_id/request_id) 去重，并统一使用每个请求键最新的
// 最终用量记录计算成本、Token 和延迟；同一账户的错误后有成功记录时，
// 该错误只作为软 429 证据，不把整次请求判为失败。retry_after_seconds
// 会把短暂恢复和长等待区分开，避免固定惩罚过重或过轻。
const priorityMetricsQuery = `
WITH usage_candidates AS (
    SELECT ul.*,
           LOWER(REGEXP_REPLACE(
               BTRIM(COALESCE(
                   NULLIF(BTRIM(ul.client_request_id::text), ''),
                   NULLIF(BTRIM(ul.request_id::text), ''),
                   'usage:' || ul.id::text
               )),
               '^client:', '', 'i'
           )) AS request_key,
           CASE WHEN GREATEST(COALESCE(ul.input_tokens, 0), 0) +
                          GREATEST(COALESCE(ul.output_tokens, 0), 0) +
                          GREATEST(COALESCE(ul.cache_creation_tokens, 0), 0) +
                          GREATEST(COALESCE(ul.cache_read_tokens, 0), 0) > 0
                THEN CASE
                    WHEN COALESCE(ul.account_stats_cost, 0) > 0 OR COALESCE(ul.total_cost, 0) > 0
                        THEN COALESCE(
                                 NULLIF(GREATEST(COALESCE(ul.account_stats_cost, 0), 0), 0),
                                 NULLIF(GREATEST(COALESCE(ul.total_cost, 0), 0), 0),
                                 0
                             ) * COALESCE(
                                 NULLIF(GREATEST(COALESCE(ul.account_rate_multiplier, 0), 0), 0),
                                 NULLIF(GREATEST(COALESCE(ul.rate_multiplier, 0), 0), 0),
                                 1
                             )
                    ELSE GREATEST(COALESCE(ul.actual_cost, 0), 0)
                END * 1000000.0 /
                (GREATEST(COALESCE(ul.input_tokens, 0), 0) +
                 GREATEST(COALESCE(ul.output_tokens, 0), 0) +
                 GREATEST(COALESCE(ul.cache_creation_tokens, 0), 0) +
                 GREATEST(COALESCE(ul.cache_read_tokens, 0), 0))
           END AS unit_cost
    FROM usage_logs ul
    WHERE ul.account_id IS NOT NULL
      AND ul.created_at >= $1 AND ul.created_at < $2
), usage_rows AS MATERIALIZED (
    SELECT DISTINCT ON (account_id, request_key) *
    FROM usage_candidates
    ORDER BY account_id, request_key, created_at DESC, id DESC
), successful_keys AS (
    SELECT account_id, request_key, created_at AS success_at
    FROM usage_rows
), latest_success AS (
    SELECT account_id, MAX(success_at) AS success_at
    FROM successful_keys
    GROUP BY account_id
), usage AS (
    SELECT ul.account_id,
           COUNT(*)::bigint AS successful_requests,
           COALESCE(SUM(GREATEST(COALESCE(ul.input_tokens, 0), 0)::bigint +
                        GREATEST(COALESCE(ul.output_tokens, 0), 0)::bigint +
                        GREATEST(COALESCE(ul.cache_creation_tokens, 0), 0)::bigint +
                        GREATEST(COALESCE(ul.cache_read_tokens, 0), 0)::bigint), 0)::bigint AS total_tokens,
           COALESCE(SUM(GREATEST(COALESCE(ul.input_tokens, 0), 0)::bigint), 0)::bigint AS input_tokens,
           COALESCE(SUM(GREATEST(COALESCE(ul.output_tokens, 0), 0)::bigint), 0)::bigint AS output_tokens,
           COALESCE(SUM(GREATEST(COALESCE(ul.cache_creation_tokens, 0), 0)::bigint), 0)::bigint AS cache_creation_tokens,
           COALESCE(SUM(GREATEST(COALESCE(ul.cache_read_tokens, 0), 0)::bigint), 0)::bigint AS cache_read_tokens,
           COALESCE(SUM(
                CASE WHEN GREATEST(COALESCE(ul.input_tokens, 0), 0) +
                               GREATEST(COALESCE(ul.output_tokens, 0), 0) +
                               GREATEST(COALESCE(ul.cache_creation_tokens, 0), 0) +
                               GREATEST(COALESCE(ul.cache_read_tokens, 0), 0) > 0
                     THEN CASE
                         WHEN COALESCE(ul.account_stats_cost, 0) > 0 OR COALESCE(ul.total_cost, 0) > 0
                             THEN COALESCE(
                                      NULLIF(GREATEST(COALESCE(ul.account_stats_cost, 0), 0), 0),
                                      NULLIF(GREATEST(COALESCE(ul.total_cost, 0), 0), 0),
                                      0
                                  ) *
                                  COALESCE(
                                      NULLIF(GREATEST(COALESCE(ul.account_rate_multiplier, 0), 0), 0),
                                      NULLIF(GREATEST(COALESCE(ul.rate_multiplier, 0), 0), 0),
                                      1
                                  )
                         ELSE GREATEST(COALESCE(ul.actual_cost, 0), 0)
                     END
                     ELSE 0
                END
           ), 0)::double precision AS account_cost,
            COALESCE(SUM(CASE WHEN GREATEST(COALESCE(ul.input_tokens, 0), 0) +
                                       GREATEST(COALESCE(ul.output_tokens, 0), 0) +
                                       GREATEST(COALESCE(ul.cache_creation_tokens, 0), 0) +
                                       GREATEST(COALESCE(ul.cache_read_tokens, 0), 0) > 0
                               THEN GREATEST(COALESCE(ul.actual_cost, 0), 0) ELSE 0 END), 0)::double precision AS actual_cost,
           COALESCE(SUM(GREATEST(COALESCE(ul.input_cost, 0), 0) * COALESCE(
                            NULLIF(GREATEST(COALESCE(ul.account_rate_multiplier, 0), 0), 0),
                            NULLIF(GREATEST(COALESCE(ul.rate_multiplier, 0), 0), 0),
                            1)), 0)::double precision AS input_cost,
           COALESCE(SUM(GREATEST(COALESCE(ul.output_cost, 0), 0) * COALESCE(
                            NULLIF(GREATEST(COALESCE(ul.account_rate_multiplier, 0), 0), 0),
                            NULLIF(GREATEST(COALESCE(ul.rate_multiplier, 0), 0), 0),
                            1)), 0)::double precision AS output_cost,
           COALESCE(SUM(GREATEST(COALESCE(ul.cache_creation_cost, 0), 0) * COALESCE(
                            NULLIF(GREATEST(COALESCE(ul.account_rate_multiplier, 0), 0), 0),
                            NULLIF(GREATEST(COALESCE(ul.rate_multiplier, 0), 0), 0),
                            1)), 0)::double precision AS cache_creation_cost,
           COALESCE(SUM(GREATEST(COALESCE(ul.cache_read_cost, 0), 0) * COALESCE(
                            NULLIF(GREATEST(COALESCE(ul.account_rate_multiplier, 0), 0), 0),
                            NULLIF(GREATEST(COALESCE(ul.rate_multiplier, 0), 0), 0),
                            1)), 0)::double precision AS cache_read_cost,
           percentile_cont(0.75) WITHIN GROUP (ORDER BY ul.unit_cost)
             FILTER (WHERE ul.unit_cost IS NOT NULL AND ul.unit_cost > 0) AS cost_p75_per_million,
           percentile_cont(0.90) WITHIN GROUP (ORDER BY ul.duration_ms)
             FILTER (WHERE ul.duration_ms IS NOT NULL AND ul.duration_ms > 0) AS latency_p90_ms,
           percentile_cont(0.90) WITHIN GROUP (ORDER BY ul.first_token_ms)
             FILTER (WHERE ul.first_token_ms IS NOT NULL AND ul.first_token_ms > 0) AS first_token_p90_ms
    FROM usage_rows ul
    GROUP BY ul.account_id
), error_requests AS (
    SELECT oe.account_id,
            LOWER(REGEXP_REPLACE(
               BTRIM(COALESCE(
                   NULLIF(BTRIM(oe.client_request_id::text), ''),
                   NULLIF(BTRIM(oe.request_id::text), ''),
                   'error:' || oe.id::text
               )),
               '^client:', '', 'i'
           )) AS request_key,
           MAX(oe.created_at) AS error_at,
           bool_or(
               COALESCE(NULLIF(oe.upstream_status_code, 0), oe.status_code, 0) = 429
               OR oe.upstream_status_code = 429
               OR oe.status_code = 429
               OR LOWER(BTRIM(COALESCE(oe.error_type, ''))) IN ('rate_limit_error', 'rate_limited', 'rate_limit')
           ) AS rate_limited,
           COALESCE(MAX(oe.retry_after_seconds), 0)::double precision AS retry_after_seconds
    FROM ops_error_logs oe
    WHERE oe.account_id IS NOT NULL
      AND oe.created_at >= $1 AND oe.created_at < $2
      AND COALESCE(oe.is_business_limited, FALSE) = FALSE
      AND (
          LOWER(BTRIM(COALESCE(oe.error_owner, ''))) = 'provider'
          OR LOWER(BTRIM(COALESCE(oe.error_source, ''))) IN ('upstream_http', 'upstream_network')
          OR LOWER(BTRIM(COALESCE(oe.error_phase, ''))) IN ('account_auth', 'network', 'upstream')
          OR oe.upstream_status_code = 429
          OR oe.upstream_status_code >= 500
      )
    GROUP BY oe.account_id,
             LOWER(REGEXP_REPLACE(
                 BTRIM(COALESCE(
                     NULLIF(BTRIM(oe.client_request_id::text), ''),
                     NULLIF(BTRIM(oe.request_id::text), ''),
                     'error:' || oe.id::text
                 )),
                 '^client:', '', 'i'
             ))
), errors AS (
    SELECT e.account_id,
           COUNT(*)::bigint AS error_requests,
           COUNT(*) FILTER (WHERE e.rate_limited)::bigint AS rate_limited_requests,
           COUNT(*) FILTER (WHERE e.rate_limited AND s.success_at IS NOT NULL)::bigint AS recovered_rate_limited,
           COALESCE(SUM(CASE WHEN e.rate_limited AND s.success_at IS NOT NULL
                             THEN GREATEST(0.25, LEAST(1.0, e.retry_after_seconds / 30.0))
                             ELSE 0 END), 0)::double precision AS recovered_rate_limit_weight,
           COUNT(*) FILTER (WHERE s.success_at IS NULL)::bigint AS terminal_failures,
           COUNT(*) FILTER (
               WHERE s.success_at IS NULL
                 AND (latest.success_at IS NULL OR e.error_at > latest.success_at)
           )::bigint AS trailing_terminal_failures
    FROM error_requests e
    LEFT JOIN successful_keys s
      ON s.account_id = e.account_id
     AND s.request_key = e.request_key
     AND s.success_at >= e.error_at
    LEFT JOIN latest_success latest ON latest.account_id = e.account_id
    GROUP BY e.account_id
)
SELECT a.id,
       COALESCE(BTRIM(a.name), '') AS name,
       COALESCE(BTRIM(a.platform), '') AS platform,
       COALESCE(BTRIM(a.status), '') AS status,
       COALESCE(a.priority, 50),
       COALESCE(a.rate_multiplier, 1)::double precision,
       a.rate_limit_reset_at,
       a.temp_unschedulable_until,
       a.overload_until,
       COALESCE(u.successful_requests, 0),
       COALESCE(u.total_tokens, 0),
       COALESCE(u.input_tokens, 0),
       COALESCE(u.output_tokens, 0),
       COALESCE(u.cache_creation_tokens, 0),
       COALESCE(u.cache_read_tokens, 0),
       COALESCE(u.account_cost, 0),
       COALESCE(u.actual_cost, 0),
       COALESCE(u.input_cost, 0),
       COALESCE(u.output_cost, 0),
       COALESCE(u.cache_creation_cost, 0),
       COALESCE(u.cache_read_cost, 0),
       u.cost_p75_per_million,
       u.latency_p90_ms,
       u.first_token_p90_ms,
       COALESCE(e.error_requests, 0),
       COALESCE(e.rate_limited_requests, 0),
       COALESCE(e.recovered_rate_limited, 0),
       COALESCE(e.recovered_rate_limit_weight, 0),
       COALESCE(e.terminal_failures, 0),
       COALESCE(e.trailing_terminal_failures, 0)
FROM accounts a
LEFT JOIN usage u ON u.account_id = a.id
LEFT JOIN errors e ON e.account_id = a.id
WHERE a.deleted_at IS NULL
  AND a.schedulable = TRUE
  AND LOWER(TRIM(a.status)) IN ('active', 'error')
ORDER BY a.id`

// priorityMetricsCompatQueryTemplate keeps the account-level metrics query
// executable when optional usage or error columns are absent on an older
// Sub2API schema. Core account and timestamp columns are checked before this
// query is used; missing evidence columns become typed zero/NULL expressions.
const priorityMetricsCompatQueryTemplate = `
WITH usage_candidates AS (
    SELECT ul.*,
           __USAGE_REQUEST_KEY__ AS request_key,
           __UNIT_COST__ AS unit_cost
    FROM usage_logs ul
    WHERE ul.account_id IS NOT NULL
      AND ul.created_at >= $1 AND ul.created_at < $2
), usage_rows AS MATERIALIZED (
    SELECT DISTINCT ON (account_id, request_key) *
    FROM usage_candidates
    ORDER BY account_id, request_key, created_at DESC, id DESC
), successful_keys AS (
    SELECT account_id, request_key, created_at AS success_at
    FROM usage_rows
), latest_success AS (
    SELECT account_id, MAX(success_at) AS success_at
    FROM successful_keys
    GROUP BY account_id
), usage AS (
    SELECT ul.account_id,
           COUNT(*)::bigint AS successful_requests,
           COALESCE(SUM(__INPUT_TOKENS__::bigint +
                        __OUTPUT_TOKENS__::bigint +
                        __CACHE_CREATION_TOKENS__::bigint +
                        __CACHE_READ_TOKENS__::bigint), 0)::bigint AS total_tokens,
           COALESCE(SUM(__INPUT_TOKENS__::bigint), 0)::bigint AS input_tokens,
           COALESCE(SUM(__OUTPUT_TOKENS__::bigint), 0)::bigint AS output_tokens,
           COALESCE(SUM(__CACHE_CREATION_TOKENS__::bigint), 0)::bigint AS cache_creation_tokens,
           COALESCE(SUM(__CACHE_READ_TOKENS__::bigint), 0)::bigint AS cache_read_tokens,
           COALESCE(SUM(
               CASE WHEN __INPUT_TOKENS__ + __OUTPUT_TOKENS__ +
                              __CACHE_CREATION_TOKENS__ + __CACHE_READ_TOKENS__ > 0
                    THEN CASE
                        WHEN __ACCOUNT_STATS_COST__ > 0 OR __TOTAL_COST__ > 0
                            THEN COALESCE(
                                     NULLIF(GREATEST(__ACCOUNT_STATS_COST__, 0), 0),
                                     NULLIF(GREATEST(__TOTAL_COST__, 0), 0),
                                     0
                                 ) * GREATEST(__ACCOUNT_RATE__, 0)
                        ELSE __ACTUAL_COST__
                    END
                    ELSE 0
               END
           ), 0)::double precision AS account_cost,
           COALESCE(SUM(CASE WHEN __INPUT_TOKENS__ + __OUTPUT_TOKENS__ +
                                      __CACHE_CREATION_TOKENS__ + __CACHE_READ_TOKENS__ > 0
                             THEN __ACTUAL_COST__ ELSE 0 END), 0)::double precision AS actual_cost,
           COALESCE(SUM(__INPUT_COST__), 0)::double precision AS input_cost,
           COALESCE(SUM(__OUTPUT_COST__), 0)::double precision AS output_cost,
           COALESCE(SUM(__CACHE_CREATION_COST__), 0)::double precision AS cache_creation_cost,
           COALESCE(SUM(__CACHE_READ_COST__), 0)::double precision AS cache_read_cost,
           percentile_cont(0.75) WITHIN GROUP (ORDER BY ul.unit_cost)
             FILTER (WHERE ul.unit_cost IS NOT NULL AND ul.unit_cost > 0) AS cost_p75_per_million,
           percentile_cont(0.90) WITHIN GROUP (ORDER BY __DURATION__)
             FILTER (WHERE __DURATION__ IS NOT NULL AND __DURATION__ > 0) AS latency_p90_ms,
           percentile_cont(0.90) WITHIN GROUP (ORDER BY __FIRST_TOKEN__)
             FILTER (WHERE __FIRST_TOKEN__ IS NOT NULL AND __FIRST_TOKEN__ > 0) AS first_token_p90_ms
    FROM usage_rows ul
    GROUP BY ul.account_id
), error_requests AS (
    __ERROR_REQUESTS_BODY__
), errors AS (
    SELECT e.account_id,
           COUNT(*)::bigint AS error_requests,
           COUNT(*) FILTER (WHERE e.rate_limited)::bigint AS rate_limited_requests,
           COUNT(*) FILTER (WHERE e.rate_limited AND s.success_at IS NOT NULL)::bigint AS recovered_rate_limited,
           COALESCE(SUM(CASE WHEN e.rate_limited AND s.success_at IS NOT NULL
                             THEN GREATEST(0.25, LEAST(1.0, e.retry_after_seconds / 30.0))
                             ELSE 0 END), 0)::double precision AS recovered_rate_limit_weight,
           COUNT(*) FILTER (WHERE s.success_at IS NULL)::bigint AS terminal_failures,
           COUNT(*) FILTER (
               WHERE s.success_at IS NULL
                 AND (latest.success_at IS NULL OR e.error_at > latest.success_at)
           )::bigint AS trailing_terminal_failures
    FROM error_requests e
    LEFT JOIN successful_keys s
      ON s.account_id = e.account_id
     AND s.request_key = e.request_key
     AND s.success_at >= e.error_at
    LEFT JOIN latest_success latest ON latest.account_id = e.account_id
    GROUP BY e.account_id
)
SELECT a.id,
       COALESCE(BTRIM(a.name), '') AS name,
       COALESCE(BTRIM(a.platform), '') AS platform,
       COALESCE(BTRIM(a.status), '') AS status,
       COALESCE(a.priority, 50),
       COALESCE(a.rate_multiplier, 1)::double precision,
       a.rate_limit_reset_at,
       a.temp_unschedulable_until,
       a.overload_until,
       COALESCE(u.successful_requests, 0),
       COALESCE(u.total_tokens, 0),
       COALESCE(u.input_tokens, 0),
       COALESCE(u.output_tokens, 0),
       COALESCE(u.cache_creation_tokens, 0),
       COALESCE(u.cache_read_tokens, 0),
       COALESCE(u.account_cost, 0),
       COALESCE(u.actual_cost, 0),
       COALESCE(u.input_cost, 0),
       COALESCE(u.output_cost, 0),
       COALESCE(u.cache_creation_cost, 0),
       COALESCE(u.cache_read_cost, 0),
       u.cost_p75_per_million,
       u.latency_p90_ms,
       u.first_token_p90_ms,
       COALESCE(e.error_requests, 0),
       COALESCE(e.rate_limited_requests, 0),
       COALESCE(e.recovered_rate_limited, 0),
       COALESCE(e.recovered_rate_limit_weight, 0),
       COALESCE(e.terminal_failures, 0),
       COALESCE(e.trailing_terminal_failures, 0)
FROM accounts a
LEFT JOIN usage u ON u.account_id = a.id
LEFT JOIN errors e ON e.account_id = a.id
WHERE a.deleted_at IS NULL
  AND a.schedulable = TRUE
  AND LOWER(TRIM(a.status)) IN ('active', 'error')
ORDER BY a.id`

type MetricsStore struct {
	db *sql.DB
}

const priorityAccountGroupsQuery = `
SELECT ag.account_id, ag.group_id, COALESCE(ag.priority, 0)
FROM account_groups ag
JOIN accounts a ON a.id = ag.account_id
WHERE a.deleted_at IS NULL
  AND a.schedulable = TRUE
  AND LOWER(TRIM(a.status)) IN ('active', 'error')
ORDER BY ag.account_id, ag.group_id`

// windowedMetricsSource is an optional source capability. Callers can keep
// using metricsSource when this method is unavailable; the 24h snapshot is
// always the primary account and pool result.
type windowedMetricsSource interface {
	LoadAccountMetricsWindows(context.Context, time.Time) ([]AccountMetrics, error)
}

func NewMetricsStore(db *sql.DB) *MetricsStore {
	return &MetricsStore{db: db}
}

var priorityMetricsUsageColumns = []string{
	"id", "account_id", "created_at", "request_id", "client_request_id", "actual_cost", "total_cost",
	"account_stats_cost", "account_rate_multiplier", "rate_multiplier", "input_tokens",
	"output_tokens", "cache_creation_tokens", "cache_read_tokens", "input_cost", "output_cost",
	"cache_creation_cost", "cache_read_cost", "duration_ms", "first_token_ms",
}

var priorityMetricsErrorColumns = []string{
	"id", "account_id", "created_at", "request_id", "client_request_id", "status_code",
	"upstream_status_code", "error_type", "retry_after_seconds", "is_business_limited",
	"error_owner", "error_source", "error_phase",
}

var priorityMetricsUsageCoreColumns = []string{"id", "account_id", "created_at"}

func (s *MetricsStore) LoadAccountMetrics(ctx context.Context, now time.Time, window time.Duration) ([]AccountMetrics, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("priority metrics database is not configured")
	}
	end := now.UTC()
	start := end.Add(-window)
	usageColumns, err := s.usageLogColumns(ctx)
	if err != nil {
		return nil, err
	}
	errorColumns, err := s.errorLogColumns(ctx)
	if err != nil {
		return nil, err
	}
	if missing := missingColumns(usageColumns, priorityMetricsUsageCoreColumns); len(missing) > 0 {
		return nil, fmt.Errorf("usage_logs 缺少必需列: %s", strings.Join(missing, ", "))
	}
	query := priorityMetricsQuery
	if !hasColumns(usageColumns, priorityMetricsUsageColumns) ||
		!hasColumns(errorColumns, priorityMetricsErrorColumns) {
		query = priorityMetricsQueryForColumns(usageColumns, errorColumns)
	}
	rows, err := s.db.QueryContext(ctx, query, start, end)
	if err != nil {
		return nil, fmt.Errorf("load priority metrics: %w", err)
	}
	defer rows.Close()
	metrics := make([]AccountMetrics, 0)
	for rows.Next() {
		var item AccountMetrics
		var priority sql.NullInt64
		var rateLimitReset, tempUnschedulable, overload sql.NullTime
		var costP75, latencyP90, firstTokenP90 sql.NullFloat64
		if err := rows.Scan(
			&item.ID, &item.Name, &item.Platform, &item.Status, &priority,
			&item.RateMultiplier, &rateLimitReset, &tempUnschedulable, &overload,
			&item.SuccessfulRequests, &item.TotalTokens,
			&item.InputTokens, &item.OutputTokens, &item.CacheCreationTokens, &item.CacheReadTokens,
			&item.AccountCost, &item.ActualCost,
			&item.InputCost, &item.OutputCost, &item.CacheCreationCost, &item.CacheReadCost,
			&costP75, &latencyP90, &firstTokenP90, &item.ErrorRequests,
			&item.RateLimitedRequests, &item.RecoveredRateLimited, &item.RecoveredRateLimitWeight, &item.TerminalFailures,
			&item.TrailingTerminalFailures,
		); err != nil {
			return nil, fmt.Errorf("scan priority metrics: %w", err)
		}
		if priority.Valid && priority.Int64 >= 0 {
			item.CurrentPriority = int(priority.Int64)
		} else {
			item.CurrentPriority = priorityNeutral
		}
		item.RateLimitResetAt = nullTimePtr(rateLimitReset)
		item.TempUnschedulableTill = nullTimePtr(tempUnschedulable)
		item.OverloadUntil = nullTimePtr(overload)
		if latencyP90.Valid {
			item.LatencyP90Ms = latencyP90.Float64
		}
		if firstTokenP90.Valid {
			item.FirstTokenP90Ms = firstTokenP90.Float64
		}
		if costP75.Valid {
			item.CostP75PerMillion = costP75.Float64
		}
		item.HasCacheReadCost = usageColumns["cache_read_cost"]
		item.GroupDataAvailable = usageColumns["group_id"]
		metrics = append(metrics, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate priority metrics: %w", err)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("close priority metrics rows: %w", err)
	}
	return metrics, nil
}

func (s *MetricsStore) loadGroupPriorities(ctx context.Context, accounts []AccountMetrics) error {
	rows, err := s.db.QueryContext(ctx, priorityAccountGroupsQuery)
	if err != nil {
		return fmt.Errorf("load priority account groups: %w", err)
	}
	defer rows.Close()
	byID := make(map[int64]int, len(accounts))
	for index := range accounts {
		byID[accounts[index].ID] = index
		accounts[index].GroupPriorities = make(map[int64]int)
	}
	for rows.Next() {
		var accountID, groupID int64
		var priority int
		if err := rows.Scan(&accountID, &groupID, &priority); err != nil {
			return fmt.Errorf("scan priority account group: %w", err)
		}
		if index, ok := byID[accountID]; ok {
			accounts[index].GroupPriorities[groupID] = priority
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate priority account groups: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close priority account group rows: %w", err)
	}
	return nil
}

// LoadAccountMetricsWindows loads the fixed policy windows. The 24h result is
// authoritative for the returned account and pool set; shorter/longer windows
// are attached as optional snapshots when their query succeeds.
func (s *MetricsStore) LoadAccountMetricsWindows(ctx context.Context, now time.Time) ([]AccountMetrics, error) {
	primary, err := s.LoadAccountMetrics(ctx, now, defaultWindow)
	if err != nil {
		return nil, err
	}
	attachWindowSnapshots(primary, nil, snapshotWindow24h)

	for _, window := range []struct {
		duration time.Duration
		kind     snapshotWindow
	}{
		{duration: fastWindow, kind: snapshotWindow6h},
		{duration: trafficWindow, kind: snapshotWindow7d},
	} {
		metrics, loadErr := s.LoadAccountMetrics(ctx, now, window.duration)
		if loadErr != nil {
			if ctx.Err() != nil {
				return nil, fmt.Errorf("load fixed window metrics: %w", ctx.Err())
			}
			// Fixed-window data is enrichment. Keep the authoritative 24h result
			// usable when a secondary query is unavailable.
			slog.Default().Warn("固定窗口指标不可用，继续使用 24h 数据", "window", window.duration.String(), "error", loadErr)
			continue
		}
		attachWindowSnapshots(primary, metrics, window.kind)
	}
	return primary, nil
}

type snapshotWindow uint8

const (
	snapshotWindow24h snapshotWindow = iota
	snapshotWindow6h
	snapshotWindow7d
)

func attachWindowSnapshots(primary, secondary []AccountMetrics, window snapshotWindow) {
	if window == snapshotWindow24h {
		for index := range primary {
			primary[index].Window24h = accountMetricSnapshot(primary[index])
			for poolIndex := range primary[index].Pools {
				primary[index].Pools[poolIndex].Window24h = poolMetricSnapshot(primary[index].Pools[poolIndex])
			}
		}
		return
	}
	byID := make(map[int64]AccountMetrics, len(secondary))
	for _, account := range secondary {
		byID[account.ID] = account
	}
	for index := range primary {
		secondaryAccount, ok := byID[primary[index].ID]
		if !ok {
			continue
		}
		if window == snapshotWindow6h {
			primary[index].Window6h = accountMetricSnapshot(secondaryAccount)
		} else {
			primary[index].Window7d = accountMetricSnapshot(secondaryAccount)
		}
		pools := make(map[string]PoolMetrics, len(secondaryAccount.Pools))
		for _, pool := range secondaryAccount.Pools {
			pools[poolIdentity(pool)] = pool
		}
		seenPools := make(map[string]struct{}, len(primary[index].Pools))
		for poolIndex := range primary[index].Pools {
			key := poolIdentity(primary[index].Pools[poolIndex])
			seenPools[key] = struct{}{}
			pool, ok := pools[key]
			if !ok {
				continue
			}
			if window == snapshotWindow6h {
				primary[index].Pools[poolIndex].Window6h = poolMetricSnapshot(pool)
			} else {
				primary[index].Pools[poolIndex].Window7d = poolMetricSnapshot(pool)
			}
		}
		if window == snapshotWindow7d {
			for _, pool := range secondaryAccount.Pools {
				key := poolIdentity(pool)
				if _, exists := seenPools[key]; exists {
					continue
				}
				// Keep a 7d-only pool available for traffic weighting, but do not
				// let its long-window aggregates masquerade as current evidence.
				primary[index].Pools = append(primary[index].Pools, PoolMetrics{
					Key:                pool.Key,
					Platform:           pool.Platform,
					RequestedModel:     pool.RequestedModel,
					UpstreamModel:      pool.UpstreamModel,
					UpstreamEndpoint:   pool.UpstreamEndpoint,
					GroupID:            pool.GroupID,
					GroupPriority:      pool.GroupPriority,
					GroupDataAvailable: pool.GroupDataAvailable,
					LongContext:        pool.LongContext,
					Model:              pool.Model,
					RateMultiplier:     pool.RateMultiplier,
					Window7d:           poolMetricSnapshot(pool),
				})
			}
		}
	}
}

func accountMetricSnapshot(account AccountMetrics) *MetricSnapshot {
	return &MetricSnapshot{
		SuccessfulRequests:       account.SuccessfulRequests,
		TerminalFailures:         account.TerminalFailures,
		RecoveredRateLimitWeight: account.RecoveredRateLimitWeight,
		TotalTokens:              account.TotalTokens,
		InputTokens:              account.InputTokens,
		OutputTokens:             account.OutputTokens,
		CacheCreationTokens:      account.CacheCreationTokens,
		CacheReadTokens:          account.CacheReadTokens,
		AccountCost:              account.AccountCost,
		ActualCost:               account.ActualCost,
		CostP75PerMillion:        account.CostP75PerMillion,
		InputCost:                account.InputCost,
		OutputCost:               account.OutputCost,
		CacheCreationCost:        account.CacheCreationCost,
		CacheReadCost:            account.CacheReadCost,
		HasCacheReadCost:         account.HasCacheReadCost,
		TrailingTerminalFailures: account.TrailingTerminalFailures,
	}
}

func poolMetricSnapshot(pool PoolMetrics) *MetricSnapshot {
	return &MetricSnapshot{
		SuccessfulRequests:  pool.SuccessfulRequests,
		TotalTokens:         pool.TotalTokens,
		InputTokens:         pool.InputTokens,
		OutputTokens:        pool.OutputTokens,
		CacheCreationTokens: pool.CacheCreationTokens,
		CacheReadTokens:     pool.CacheReadTokens,
		AccountCost:         pool.AccountCost,
		ActualCost:          pool.ActualCost,
		CostP75PerMillion:   pool.CostP75PerMillion,
		InputCost:           pool.InputCost,
		OutputCost:          pool.OutputCost,
		CacheCreationCost:   pool.CacheCreationCost,
		CacheReadCost:       pool.CacheReadCost,
		HasCacheReadCost:    pool.HasCacheReadCost,
	}
}

func hasColumns(columns map[string]bool, required []string) bool {
	for _, name := range required {
		if !columns[name] {
			return false
		}
	}
	return true
}

func missingColumns(columns map[string]bool, required []string) []string {
	missing := make([]string, 0)
	for _, name := range required {
		if !columns[name] {
			missing = append(missing, name)
		}
	}
	return missing
}

func priorityMetricsQueryForColumns(usageColumns, errorColumns map[string]bool) string {
	correlationUsageColumns, errorRequestColumns := priorityCorrelationColumns(usageColumns, errorColumns)
	usageRequestColumns := usageColumns
	if len(correlationUsageColumns) > 0 {
		usageRequestColumns = cloneColumns(correlationUsageColumns)
		usageRequestColumns["id"] = usageColumns["id"]
	}
	return strings.NewReplacer(
		"__USAGE_REQUEST_KEY__", priorityRequestKeyExpr("ul", usageRequestColumns, "usage"),
		"__UNIT_COST__", priorityMetricsUnitCostExpr(usageColumns),
		"__ACTUAL_COST__", priorityMetricsValueExpr(usageColumns, "actual_cost"),
		"__TOTAL_COST__", priorityMetricsValueExpr(usageColumns, "total_cost"),
		"__ACCOUNT_STATS_COST__", priorityMetricsValueExpr(usageColumns, "account_stats_cost"),
		"__ACCOUNT_RATE__", priorityMetricsRateExpr(usageColumns),
		"__INPUT_TOKENS__", priorityMetricsValueExpr(usageColumns, "input_tokens"),
		"__OUTPUT_TOKENS__", priorityMetricsValueExpr(usageColumns, "output_tokens"),
		"__CACHE_CREATION_TOKENS__", priorityMetricsValueExpr(usageColumns, "cache_creation_tokens"),
		"__CACHE_READ_TOKENS__", priorityMetricsValueExpr(usageColumns, "cache_read_tokens"),
		"__INPUT_COST__", priorityMetricsCostExpr(usageColumns, "input_cost"),
		"__OUTPUT_COST__", priorityMetricsCostExpr(usageColumns, "output_cost"),
		"__CACHE_CREATION_COST__", priorityMetricsCostExpr(usageColumns, "cache_creation_cost"),
		"__CACHE_READ_COST__", priorityMetricsCostExpr(usageColumns, "cache_read_cost"),
		"__DURATION__", priorityMetricsNullableExpr(usageColumns, "duration_ms"),
		"__FIRST_TOKEN__", priorityMetricsNullableExpr(usageColumns, "first_token_ms"),
		"__ERROR_REQUESTS_BODY__", priorityErrorRequestsExpr(errorColumns, errorRequestColumns),
	).Replace(priorityMetricsCompatQueryTemplate)
}

func priorityCorrelationColumns(usageColumns, errorColumns map[string]bool) (map[string]bool, map[string]bool) {
	pairs := [][2]string{
		{"client_request_id", "client_request_id"},
		{"request_id", "client_request_id"},
		{"request_id", "request_id"},
		{"client_request_id", "request_id"},
	}
	for _, pair := range pairs {
		if usageColumns[pair[0]] && errorColumns[pair[1]] {
			return map[string]bool{pair[0]: true}, map[string]bool{pair[1]: true}
		}
	}
	return nil, nil
}

func cloneColumns(columns map[string]bool) map[string]bool {
	clone := make(map[string]bool, len(columns)+1)
	for name, present := range columns {
		clone[name] = present
	}
	return clone
}

func priorityMetricsValueExpr(columns map[string]bool, name string) string {
	if columns[name] {
		return "GREATEST(COALESCE(ul." + name + ", 0), 0)"
	}
	return "0"
}

func priorityMetricsCostExpr(columns map[string]bool, name string) string {
	return priorityMetricsValueExpr(columns, name) + " * " + priorityMetricsRateExpr(columns)
}

func priorityMetricsUnitCostExpr(columns map[string]bool) string {
	tokens := strings.Join([]string{
		priorityMetricsValueExpr(columns, "input_tokens"),
		priorityMetricsValueExpr(columns, "output_tokens"),
		priorityMetricsValueExpr(columns, "cache_creation_tokens"),
		priorityMetricsValueExpr(columns, "cache_read_tokens"),
	}, " + ")
	cost := "CASE WHEN " + priorityMetricsValueExpr(columns, "account_stats_cost") + " > 0 OR " + priorityMetricsValueExpr(columns, "total_cost") + " > 0 THEN " +
		"COALESCE(NULLIF(" + priorityMetricsValueExpr(columns, "account_stats_cost") + ", 0), NULLIF(" + priorityMetricsValueExpr(columns, "total_cost") + ", 0), 0) * " +
		priorityMetricsRateExpr(columns) + " ELSE " + priorityMetricsValueExpr(columns, "actual_cost") + " END"
	return "CASE WHEN " + tokens + " > 0 THEN (" + cost + ") * 1000000.0 / (" + tokens + ") END"
}

func priorityMetricsNullableExpr(columns map[string]bool, name string) string {
	if columns[name] {
		return "ul." + name
	}
	return "NULL::double precision"
}

func priorityMetricsRateExpr(columns map[string]bool) string {
	values := make([]string, 0, 3)
	if columns["account_rate_multiplier"] {
		values = append(values, "NULLIF(GREATEST(COALESCE(ul.account_rate_multiplier, 0), 0), 0)")
	}
	if columns["rate_multiplier"] {
		values = append(values, "NULLIF(GREATEST(COALESCE(ul.rate_multiplier, 0), 0), 0)")
	}
	values = append(values, "1")
	return "COALESCE(" + strings.Join(values, ", ") + ")"
}

func priorityRequestKeyExpr(alias string, columns map[string]bool, prefix string) string {
	values := make([]string, 0, 3)
	if columns["request_id"] {
		values = append(values, "NULLIF(BTRIM("+alias+".request_id::text), '')")
	}
	if columns["client_request_id"] {
		values = append(values, "NULLIF(BTRIM("+alias+".client_request_id::text), '')")
	}
	if columns["id"] {
		values = append(values, "'"+prefix+":' || "+alias+".id::text")
	} else {
		values = append(values, "'"+prefix+":unknown'")
	}
	return "LOWER(REGEXP_REPLACE(BTRIM(COALESCE(" + strings.Join(values, ", ") + ")), '^client:', '', 'i'))"
}

func priorityErrorRequestsExpr(columns, sharedRequestColumns map[string]bool) string {
	if !columns["account_id"] || !columns["created_at"] ||
		len(sharedRequestColumns) == 0 {
		return "SELECT NULL::bigint AS account_id, NULL::text AS request_key, NULL::timestamptz AS error_at, FALSE AS rate_limited, 0::double precision AS retry_after_seconds WHERE FALSE"
	}
	rateLimited := []string{}
	if columns["upstream_status_code"] {
		rateLimited = append(rateLimited, "oe.upstream_status_code = 429")
	}
	if columns["status_code"] {
		rateLimited = append(rateLimited, "oe.status_code = 429")
	}
	if columns["error_type"] {
		rateLimited = append(rateLimited, "LOWER(BTRIM(COALESCE(oe.error_type, ''))) IN ('rate_limit_error', 'rate_limited', 'rate_limit')")
	}
	if len(rateLimited) == 0 {
		rateLimited = append(rateLimited, "FALSE")
	}
	filters := []string{}
	if columns["is_business_limited"] {
		filters = append(filters, "COALESCE(oe.is_business_limited, FALSE) = FALSE")
	}
	ownerFilters := []string{}
	if columns["error_owner"] {
		ownerFilters = append(ownerFilters, "LOWER(BTRIM(COALESCE(oe.error_owner, ''))) = 'provider'")
	}
	if columns["error_source"] {
		ownerFilters = append(ownerFilters, "LOWER(BTRIM(COALESCE(oe.error_source, ''))) IN ('upstream_http', 'upstream_network')")
	}
	if columns["error_phase"] {
		ownerFilters = append(ownerFilters, "LOWER(BTRIM(COALESCE(oe.error_phase, ''))) IN ('account_auth', 'network', 'upstream')")
	}
	upstreamEvidence := append([]string{}, ownerFilters...)
	if columns["upstream_status_code"] {
		upstreamEvidence = append(upstreamEvidence, "oe.upstream_status_code = 429")
		upstreamEvidence = append(upstreamEvidence, "oe.upstream_status_code >= 500")
	}
	if len(upstreamEvidence) == 0 {
		return "SELECT NULL::bigint AS account_id, NULL::text AS request_key, NULL::timestamptz AS error_at, FALSE AS rate_limited, 0::double precision AS retry_after_seconds WHERE FALSE"
	}
	filters = append(filters, "("+strings.Join(upstreamEvidence, " OR ")+")")
	requestColumns := cloneColumns(sharedRequestColumns)
	requestColumns["id"] = columns["id"]
	requestKey := priorityRequestKeyExpr("oe", requestColumns, "error")
	retryAfter := "0"
	if columns["retry_after_seconds"] {
		retryAfter = "COALESCE(oe.retry_after_seconds, 0)"
	}
	where := "FROM ops_error_logs oe WHERE oe.account_id IS NOT NULL AND oe.created_at >= $1 AND oe.created_at < $2"
	if len(filters) > 0 {
		where += " AND " + strings.Join(filters, " AND ")
	}
	return "SELECT oe.account_id, " + requestKey + " AS request_key, MAX(oe.created_at) AS error_at, " +
		"bool_or(" + strings.Join(rateLimited, " OR ") + ") AS rate_limited, COALESCE(MAX(" + retryAfter + "), 0)::double precision AS retry_after_seconds " +
		where + " GROUP BY oe.account_id, " + requestKey
}

// priorityPoolMetricsQueryTemplate keeps platform + requested model + actual
// upstream model + endpoint pools separate. Newer model and cost-detail
// columns are optional, so the concrete query uses only columns present on
// the target Sub2API schema.
const priorityPoolMetricsQueryTemplate = `
WITH usage_candidates AS (
	SELECT ul.*,
           __USAGE_REQUEST_KEY__ AS request_key,
           __UNIT_COST__ AS unit_cost
    FROM usage_logs ul
    WHERE ul.account_id IS NOT NULL
      AND ul.created_at >= $1 AND ul.created_at < $2
), usage_rows AS MATERIALIZED (
    SELECT DISTINCT ON (account_id, request_key) *
    FROM usage_candidates
    ORDER BY account_id, request_key, created_at DESC, id DESC
)
SELECT ul.account_id,
       LOWER(BTRIM(COALESCE(NULLIF(a.platform, ''), 'unknown'))),
       LOWER(BTRIM(__REQUESTED_MODEL__)),
       LOWER(BTRIM(__UPSTREAM_MODEL__)),
       LOWER(BTRIM(__UPSTREAM_ENDPOINT__)),
       __GROUP_ID__,
       __GROUP_PRIORITY__,
       __LONG_CONTEXT__,
       COUNT(*)::bigint,
       COALESCE(SUM(__INPUT_TOKENS__::bigint +
                    __OUTPUT_TOKENS__::bigint +
                    __CACHE_CREATION_TOKENS__::bigint +
                    __CACHE_READ_TOKENS__::bigint), 0)::bigint,
       COALESCE(SUM(__INPUT_TOKENS__), 0)::bigint,
       COALESCE(SUM(__OUTPUT_TOKENS__), 0)::bigint,
       COALESCE(SUM(__CACHE_CREATION_TOKENS__), 0)::bigint,
       COALESCE(SUM(__CACHE_READ_TOKENS__), 0)::bigint,
       COALESCE(SUM(
           CASE WHEN __INPUT_TOKENS__ + __OUTPUT_TOKENS__ +
                          __CACHE_CREATION_TOKENS__ + __CACHE_READ_TOKENS__ > 0
                THEN CASE
                    WHEN __ACCOUNT_STATS_COST__ > 0 OR __TOTAL_COST__ > 0
                        THEN COALESCE(
                                 NULLIF(GREATEST(__ACCOUNT_STATS_COST__, 0), 0),
                                 NULLIF(GREATEST(__TOTAL_COST__, 0), 0),
                                 0
                             ) * GREATEST(__ACCOUNT_RATE__, 0)
                    ELSE __ACTUAL_COST__
                END
                ELSE 0
           END
       ), 0)::double precision,
       COALESCE(SUM(CASE WHEN __INPUT_TOKENS__ + __OUTPUT_TOKENS__ +
                                  __CACHE_CREATION_TOKENS__ + __CACHE_READ_TOKENS__ > 0
                         THEN __ACTUAL_COST__ ELSE 0 END), 0)::double precision,
       COALESCE(SUM(CASE WHEN __INPUT_TOKENS__ > 0 THEN __INPUT_COST__ * __ACCOUNT_RATE__ ELSE 0 END), 0)::double precision,
       COALESCE(SUM(CASE WHEN __OUTPUT_TOKENS__ > 0 THEN __OUTPUT_COST__ * __ACCOUNT_RATE__ ELSE 0 END), 0)::double precision,
       COALESCE(SUM(CASE WHEN __CACHE_CREATION_TOKENS__ > 0 THEN __CACHE_CREATION_COST__ * __ACCOUNT_RATE__ ELSE 0 END), 0)::double precision,
       COALESCE(SUM(CASE WHEN __CACHE_READ_TOKENS__ > 0 THEN __CACHE_READ_COST__ * __ACCOUNT_RATE__ ELSE 0 END), 0)::double precision,
       percentile_cont(0.75) WITHIN GROUP (ORDER BY ul.unit_cost)
         FILTER (WHERE ul.unit_cost IS NOT NULL AND ul.unit_cost > 0),
       COALESCE(percentile_cont(0.5) WITHIN GROUP (ORDER BY __ACCOUNT_RATE__), 1)::double precision,
       percentile_cont(0.90) WITHIN GROUP (ORDER BY __DURATION__)
         FILTER (WHERE __DURATION__ IS NOT NULL AND __DURATION__ > 0),
       percentile_cont(0.90) WITHIN GROUP (ORDER BY __FIRST_TOKEN__)
         FILTER (WHERE __FIRST_TOKEN__ IS NOT NULL AND __FIRST_TOKEN__ > 0)
FROM usage_rows ul
JOIN accounts a ON a.id = ul.account_id
__ACCOUNT_GROUP_JOIN__
WHERE a.deleted_at IS NULL
  AND a.schedulable = TRUE
  AND LOWER(TRIM(a.status)) IN ('active', 'error')
GROUP BY ul.account_id,
         LOWER(BTRIM(COALESCE(NULLIF(a.platform, ''), 'unknown'))),
         LOWER(BTRIM(__REQUESTED_MODEL__)),
         LOWER(BTRIM(__UPSTREAM_MODEL__)),
         LOWER(BTRIM(__UPSTREAM_ENDPOINT__)),
         __GROUP_ID__,
         __GROUP_PRIORITY__,
         __LONG_CONTEXT__
ORDER BY ul.account_id, 2, 3, 4, 5, 6, 7, 8`

var priorityPoolMetricsQuery = priorityPoolMetricsQueryForColumns(priorityPoolColumns(
	"id",
	"request_id",
	"client_request_id",
	"model",
	"input_tokens",
	"output_tokens",
	"cache_creation_tokens",
	"cache_read_tokens",
	"account_stats_cost",
	"total_cost",
	"actual_cost",
	"account_rate_multiplier",
	"rate_multiplier",
	"requested_model",
	"upstream_response_model",
	"upstream_model",
	"upstream_endpoint",
	"group_id",
	"long_context_billing_applied",
	"input_cost",
	"output_cost",
	"cache_creation_cost",
	"cache_read_cost",
	"duration_ms",
	"first_token_ms",
))

func priorityPoolColumns(names ...string) map[string]bool {
	columns := make(map[string]bool, len(names))
	for _, name := range names {
		columns[name] = true
	}
	return columns
}

func priorityPoolMetricsQueryForColumns(columns map[string]bool) string {
	return strings.NewReplacer(
		"__USAGE_REQUEST_KEY__", priorityRequestKeyExpr("ul", columns, "usage"),
		"__UNIT_COST__", priorityPoolUnitCostExpr(columns),
		"__REQUESTED_MODEL__", priorityPoolModelExpr(columns, "requested_model", "model"),
		"__UPSTREAM_MODEL__", priorityPoolModelExpr(columns, "upstream_response_model", "upstream_model", "model"),
		"__UPSTREAM_ENDPOINT__", priorityPoolModelExpr(columns, "upstream_endpoint"),
		"__GROUP_ID__", priorityPoolGroupIDExpr(columns),
		"__GROUP_PRIORITY__", priorityPoolGroupPriorityExpr(columns),
		"__LONG_CONTEXT__", priorityPoolLongContextExpr(columns),
		"__ACCOUNT_GROUP_JOIN__", priorityPoolAccountGroupJoin(columns),
		"__INPUT_TOKENS__", priorityPoolValueExpr(columns, "input_tokens"),
		"__OUTPUT_TOKENS__", priorityPoolValueExpr(columns, "output_tokens"),
		"__CACHE_CREATION_TOKENS__", priorityPoolValueExpr(columns, "cache_creation_tokens"),
		"__CACHE_READ_TOKENS__", priorityPoolValueExpr(columns, "cache_read_tokens"),
		"__INPUT_COST__", priorityPoolCostExpr(columns, "input_cost"),
		"__OUTPUT_COST__", priorityPoolCostExpr(columns, "output_cost"),
		"__CACHE_CREATION_COST__", priorityPoolCostExpr(columns, "cache_creation_cost"),
		"__CACHE_READ_COST__", priorityPoolCostExpr(columns, "cache_read_cost"),
		"__ACCOUNT_STATS_COST__", priorityPoolValueExpr(columns, "account_stats_cost"),
		"__TOTAL_COST__", priorityPoolValueExpr(columns, "total_cost"),
		"__ACTUAL_COST__", priorityPoolValueExpr(columns, "actual_cost"),
		"__ACCOUNT_RATE__", priorityPoolRateExpr(columns),
		"__DURATION__", priorityPoolNullableExpr(columns, "duration_ms"),
		"__FIRST_TOKEN__", priorityPoolNullableExpr(columns, "first_token_ms"),
	).Replace(priorityPoolMetricsQueryTemplate)
}

func priorityPoolGroupIDExpr(columns map[string]bool) string {
	if columns["group_id"] {
		return "COALESCE(ul.group_id, 0)::bigint"
	}
	return "0::bigint"
}

func priorityPoolGroupPriorityExpr(columns map[string]bool) string {
	if columns["group_id"] {
		return "COALESCE(ag.priority, 0)"
	}
	return "0"
}

func priorityPoolLongContextExpr(columns map[string]bool) string {
	if columns["long_context_billing_applied"] {
		return "COALESCE(ul.long_context_billing_applied, FALSE)"
	}
	return "FALSE"
}

func priorityPoolAccountGroupJoin(columns map[string]bool) string {
	if columns["group_id"] {
		return "LEFT JOIN account_groups ag ON ag.account_id = ul.account_id AND ag.group_id = ul.group_id"
	}
	return ""
}

func priorityPoolModelExpr(columns map[string]bool, names ...string) string {
	values := make([]string, 0, len(names)+1)
	for _, name := range names {
		if columns[name] {
			values = append(values, "NULLIF(BTRIM(ul."+name+"), '')")
		}
	}
	values = append(values, "'unknown'")
	return "COALESCE(" + strings.Join(values, ", ") + ")"
}

func priorityPoolCostExpr(columns map[string]bool, name string) string {
	return priorityPoolValueExpr(columns, name)
}

func priorityPoolRateExpr(columns map[string]bool) string {
	values := make([]string, 0, 3)
	if columns["account_rate_multiplier"] {
		values = append(values, "NULLIF(GREATEST(COALESCE(ul.account_rate_multiplier, 0), 0), 0)")
	}
	if columns["rate_multiplier"] {
		values = append(values, "NULLIF(GREATEST(COALESCE(ul.rate_multiplier, 0), 0), 0)")
	}
	values = append(values, "1")
	return "COALESCE(" + strings.Join(values, ", ") + ")"
}

func priorityPoolNullableExpr(columns map[string]bool, name string) string {
	if columns[name] {
		return "ul." + name
	}
	return "NULL::double precision"
}

func priorityPoolValueExpr(columns map[string]bool, name string) string {
	if columns[name] {
		return "GREATEST(COALESCE(ul." + name + ", 0), 0)"
	}
	return "0"
}

func priorityPoolUnitCostExpr(columns map[string]bool) string {
	tokens := strings.Join([]string{
		priorityPoolValueExpr(columns, "input_tokens"),
		priorityPoolValueExpr(columns, "output_tokens"),
		priorityPoolValueExpr(columns, "cache_creation_tokens"),
		priorityPoolValueExpr(columns, "cache_read_tokens"),
	}, " + ")
	cost := "CASE WHEN " + priorityPoolValueExpr(columns, "account_stats_cost") + " > 0 OR " + priorityPoolValueExpr(columns, "total_cost") + " > 0 THEN " +
		"COALESCE(NULLIF(" + priorityPoolValueExpr(columns, "account_stats_cost") + ", 0), NULLIF(" + priorityPoolValueExpr(columns, "total_cost") + ", 0), 0) * " +
		priorityPoolRateExpr(columns) + " ELSE " + priorityPoolValueExpr(columns, "actual_cost") + " END"
	return "CASE WHEN " + tokens + " > 0 THEN (" + cost + ") * 1000000.0 / (" + tokens + ") END"
}

func (s *MetricsStore) loadPoolMetrics(ctx context.Context, start, end time.Time, accounts []AccountMetrics, columns map[string]bool) error {
	rows, err := s.db.QueryContext(ctx, priorityPoolMetricsQueryForColumns(columns), start, end)
	if err != nil {
		return fmt.Errorf("load priority model pools: %w", err)
	}
	defer rows.Close()
	byID := make(map[int64]int, len(accounts))
	loaded := make([][]PoolMetrics, len(accounts))
	for index := range accounts {
		byID[accounts[index].ID] = index
	}
	for rows.Next() {
		var pool PoolMetrics
		var accountID int64
		var costP75, latencyP90, firstTokenP90 sql.NullFloat64
		if err := rows.Scan(
			&accountID, &pool.Platform, &pool.RequestedModel, &pool.UpstreamModel,
			&pool.UpstreamEndpoint, &pool.GroupID, &pool.GroupPriority, &pool.LongContext,
			&pool.SuccessfulRequests, &pool.TotalTokens,
			&pool.InputTokens, &pool.OutputTokens, &pool.CacheCreationTokens, &pool.CacheReadTokens,
			&pool.AccountCost, &pool.ActualCost,
			&pool.InputCost, &pool.OutputCost, &pool.CacheCreationCost, &pool.CacheReadCost,
			&costP75, &pool.RateMultiplier,
			&latencyP90, &firstTokenP90,
		); err != nil {
			return fmt.Errorf("scan priority model pool: %w", err)
		}
		if latencyP90.Valid {
			pool.LatencyP90Ms = latencyP90.Float64
		}
		if firstTokenP90.Valid {
			pool.FirstTokenP90Ms = firstTokenP90.Float64
		}
		if costP75.Valid {
			pool.CostP75PerMillion = costP75.Float64
		}
		pool.HasCacheReadCost = columns["cache_read_cost"]
		pool.GroupDataAvailable = columns["group_id"]
		pool.Model = pool.UpstreamModel
		pool.Key = poolIdentity(pool)
		if index, ok := byID[accountID]; ok {
			loaded[index] = append(loaded[index], pool)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate priority model pools: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close priority model pool rows: %w", err)
	}
	for index := range accounts {
		accounts[index].Pools = append(accounts[index].Pools, loaded[index]...)
	}
	return nil
}

func (s *MetricsStore) usageLogColumns(ctx context.Context) (map[string]bool, error) {
	return s.tableColumns(ctx, "usage_logs")
}

func (s *MetricsStore) errorLogColumns(ctx context.Context) (map[string]bool, error) {
	return s.tableColumns(ctx, "ops_error_logs")
}

func (s *MetricsStore) tableColumns(ctx context.Context, table string) (map[string]bool, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT column_name
FROM information_schema.columns
WHERE table_name = $1
  AND table_schema = ANY(current_schemas(true))`, table)
	if err != nil {
		return nil, fmt.Errorf("inspect %s columns: %w", table, err)
	}
	defer rows.Close()
	columns := make(map[string]bool)
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("scan %s column: %w", table, err)
		}
		columns[name] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate %s columns: %w", table, err)
	}
	return columns, nil
}

func (s *MetricsStore) AdminAPIKey(ctx context.Context) (string, error) {
	if s == nil || s.db == nil {
		return "", fmt.Errorf("priority metrics database is not configured")
	}
	var value string
	if err := s.db.QueryRowContext(ctx,
		`SELECT value FROM settings WHERE key = 'admin_api_key' AND BTRIM(value) <> '' LIMIT 1`,
	).Scan(&value); err != nil {
		if err == sql.ErrNoRows {
			return "", nil
		}
		return "", fmt.Errorf("load Admin API key: %w", err)
	}
	return strings.TrimSpace(value), nil
}

func nullTimePtr(value sql.NullTime) *time.Time {
	if !value.Valid {
		return nil
	}
	result := value.Time.UTC()
	return &result
}
