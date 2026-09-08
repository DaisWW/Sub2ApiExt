package main

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// priorityMetricsQuery 只读取 Sub2API 原始账户、用量和错误表。
// 成功请求按 (account_id, request_id) 去重，并统一使用每个请求键最新的
// 最终用量记录计算成本、Token 和延迟；同一账户的错误后有成功记录时，
// 该错误只作为软 429 证据，不把整次请求判为失败。retry_after_seconds
// 会把短暂恢复和长等待区分开，避免固定惩罚过重或过轻。
const priorityMetricsQuery = `
WITH usage_candidates AS (
    SELECT ul.*,
           LOWER(REGEXP_REPLACE(
               BTRIM(COALESCE(NULLIF(BTRIM(ul.request_id), ''), 'usage:' || ul.id::text)),
               '^client:', '', 'i'
           )) AS request_key
    FROM usage_logs ul
    WHERE ul.account_id IS NOT NULL
      AND ul.created_at >= $1 AND ul.created_at < $2
      AND ul.actual_cost > 0
), usage_rows AS MATERIALIZED (
    SELECT DISTINCT ON (account_id, request_key) *
    FROM usage_candidates
    ORDER BY account_id, request_key, created_at DESC, id DESC
), successful_keys AS (
    SELECT account_id, request_key, created_at AS success_at
    FROM usage_rows
), usage AS (
    SELECT ul.account_id,
           COUNT(*)::bigint AS successful_requests,
           COALESCE(SUM(COALESCE(ul.input_tokens, 0)::bigint +
                        COALESCE(ul.output_tokens, 0)::bigint +
                        COALESCE(ul.cache_creation_tokens, 0)::bigint +
                        COALESCE(ul.cache_read_tokens, 0)::bigint), 0)::bigint AS total_tokens,
           COALESCE(SUM(
               CASE
                   WHEN ul.account_stats_cost IS NOT NULL OR ul.total_cost IS NOT NULL
                       THEN COALESCE(ul.account_stats_cost, ul.total_cost, 0) *
                            COALESCE(ul.account_rate_multiplier, ul.rate_multiplier, 1)
                   ELSE COALESCE(ul.actual_cost, 0)
               END
           ), 0)::double precision AS account_cost,
           COALESCE(SUM(COALESCE(ul.actual_cost, 0)), 0)::double precision AS actual_cost,
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
                   NULLIF(BTRIM(oe.client_request_id), ''),
                   NULLIF(BTRIM(oe.request_id), ''),
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
      )
    GROUP BY oe.account_id,
             LOWER(REGEXP_REPLACE(
                 BTRIM(COALESCE(
                     NULLIF(BTRIM(oe.client_request_id), ''),
                     NULLIF(BTRIM(oe.request_id), ''),
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
           COUNT(*) FILTER (WHERE s.success_at IS NULL)::bigint AS terminal_failures
    FROM error_requests e
    LEFT JOIN successful_keys s
      ON s.account_id = e.account_id
     AND s.request_key = e.request_key
     AND s.success_at >= e.error_at
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
       COALESCE(u.account_cost, 0),
       COALESCE(u.actual_cost, 0),
       u.latency_p90_ms,
       u.first_token_p90_ms,
       COALESCE(e.error_requests, 0),
       COALESCE(e.rate_limited_requests, 0),
       COALESCE(e.recovered_rate_limited, 0),
       COALESCE(e.recovered_rate_limit_weight, 0),
       COALESCE(e.terminal_failures, 0)
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

func NewMetricsStore(db *sql.DB) *MetricsStore {
	return &MetricsStore{db: db}
}

func (s *MetricsStore) LoadAccountMetrics(ctx context.Context, now time.Time, window time.Duration) ([]AccountMetrics, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("priority metrics database is not configured")
	}
	end := now.UTC()
	start := end.Add(-window)
	rows, err := s.db.QueryContext(ctx, priorityMetricsQuery, start, end)
	if err != nil {
		return nil, fmt.Errorf("load priority metrics: %w", err)
	}
	defer rows.Close()
	metrics := make([]AccountMetrics, 0)
	for rows.Next() {
		var item AccountMetrics
		var priority sql.NullInt64
		var rateLimitReset, tempUnschedulable, overload sql.NullTime
		var latencyP90, firstTokenP90 sql.NullFloat64
		if err := rows.Scan(
			&item.ID, &item.Name, &item.Platform, &item.Status, &priority,
			&item.RateMultiplier, &rateLimitReset, &tempUnschedulable, &overload,
			&item.SuccessfulRequests, &item.TotalTokens, &item.AccountCost, &item.ActualCost,
			&latencyP90, &firstTokenP90, &item.ErrorRequests,
			&item.RateLimitedRequests, &item.RecoveredRateLimited, &item.RecoveredRateLimitWeight, &item.TerminalFailures,
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
		metrics = append(metrics, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate priority metrics: %w", err)
	}
	return metrics, nil
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
