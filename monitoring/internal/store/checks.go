package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/DaisWW/Sub2ApiExt/monitoring/internal/model"
)

const historyQuery = `
WITH authorized_target AS (
    SELECT $1::text AS target_key, NULL::timestamptz AS source_updated_at
    WHERE $1 = 'group:-1'
    UNION
    SELECT target_key, source_updated_at
    FROM monitoring_targets
    WHERE target_key = $1 AND active = TRUE
), group_request_rows AS MATERIALIZED (
    SELECT 'group:' || oe.group_id::text AS target_key,
           oe.group_id, oe.id, oe.created_at,
           oe.status_code, oe.upstream_status_code,
           COALESCE(NULLIF(BTRIM(oe.request_id), ''),
                    NULLIF(BTRIM(oe.client_request_id), ''),
                    'error:' || oe.id::text) AS request_key,
           oe.is_business_limited,
           LOWER(BTRIM(COALESCE(oe.error_type, ''))) AS error_type,
           LOWER(BTRIM(COALESCE(oe.error_owner, ''))) AS error_owner,
           LOWER(BTRIM(COALESCE(oe.error_phase, ''))) AS error_phase,
           LOWER(BTRIM(COALESCE(oe.error_source, ''))) AS error_source
    FROM ops_error_logs oe
    JOIN authorized_target auth ON auth.target_key = 'group:' || oe.group_id::text
    WHERE oe.group_id IS NOT NULL
      AND oe.created_at >= NOW() - INTERVAL '24 hours'
      AND (auth.source_updated_at IS NULL OR oe.created_at >= auth.source_updated_at)
), group_request_ranked AS (
    SELECT group_request_rows.*,
           ROW_NUMBER() OVER (
               PARTITION BY group_id, request_key
               ORDER BY created_at DESC, id DESC
           ) AS position
    FROM group_request_rows
), group_error_candidates AS (
    SELECT target_key, group_id, id, created_at,
           COALESCE(NULLIF(upstream_status_code, 0), NULLIF(status_code, 0)) AS status_code,
           request_key
    FROM group_request_ranked
    WHERE position = 1
      AND COALESCE(is_business_limited, FALSE) = FALSE
      AND error_type NOT IN (
          'cyber_policy', 'client_cancelled', 'rate_limit_error', 'invalid_request_error'
      )
      AND COALESCE(NULLIF(upstream_status_code, 0), NULLIF(status_code, 0), 0) <> 429
      AND (
          COALESCE(NULLIF(upstream_status_code, 0), NULLIF(status_code, 0), 0) >= 500
          OR (
              COALESCE(NULLIF(upstream_status_code, 0), NULLIF(status_code, 0), 0) >= 400
              AND error_owner = 'provider'
              AND (
                  error_phase IN ('account_auth', 'network', 'upstream')
                  OR error_source IN ('upstream_http', 'upstream_network')
              )
          )
      )
), group_error_events AS (
    SELECT target_key, group_id, created_at, status_code,
           CASE WHEN status_code IS NULL THEN '最近请求失败'
                ELSE '最近请求失败：HTTP ' || status_code::text END AS message
    FROM group_error_candidates
), combined AS (
SELECT monitoring_checks.target_key, monitoring_checks.kind, monitoring_checks.entity_id, monitoring_checks.group_id,
       monitoring_checks.status, monitoring_checks.latency_ms, monitoring_checks.first_byte_ms,
       monitoring_checks.status_code, monitoring_checks.error_class, monitoring_checks.message,
       monitoring_checks.checked_at, monitoring_checks.source
FROM monitoring_checks
JOIN authorized_target auth ON auth.target_key = monitoring_checks.target_key
WHERE monitoring_checks.checked_at >= NOW() - INTERVAL '24 hours'
  AND (auth.source_updated_at IS NULL OR monitoring_checks.checked_at >= auth.source_updated_at)
  AND (monitoring_checks.kind <> 'group' OR monitoring_checks.source <> 'aggregate')
UNION ALL
SELECT 'account:' || account_id::text, 'account', account_id, group_id,
       CASE WHEN duration_ms >= 20000 THEN 'degraded' ELSE 'operational' END,
	       duration_ms, first_token_ms, NULL::integer, '', '真实请求历史', created_at, 'history'
FROM usage_logs
JOIN authorized_target auth ON TRUE
WHERE $1 = 'account:' || account_id::text
  AND actual_cost > 0
  AND created_at >= NOW() - INTERVAL '24 hours'
  AND (auth.source_updated_at IS NULL OR usage_logs.created_at >= auth.source_updated_at)
UNION ALL
SELECT 'group:' || COALESCE(group_id, -1)::text, 'group', COALESCE(group_id, -1), group_id,
       CASE WHEN duration_ms >= 20000 THEN 'degraded' ELSE 'operational' END,
	       duration_ms, first_token_ms, NULL::integer, '', '真实请求历史', created_at, 'history'
FROM usage_logs
JOIN authorized_target auth ON TRUE
WHERE $1 = 'group:' || COALESCE(group_id, -1)::text
  AND actual_cost > 0
  AND created_at >= NOW() - INTERVAL '24 hours'
  AND (auth.source_updated_at IS NULL OR usage_logs.created_at >= auth.source_updated_at)
UNION ALL
SELECT target_key, 'group', group_id, group_id, 'failed',
       NULL::integer, NULL::integer, status_code, '', message, created_at, 'request_error'
FROM group_error_events
)
SELECT target_key, kind, entity_id, group_id, status, latency_ms, first_byte_ms,
       status_code, error_class, message, checked_at, source
FROM combined
ORDER BY checked_at DESC
LIMIT $2`

func (s *Store) InsertResults(ctx context.Context, results []model.ProbeResult) error {
	if len(results) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	const query = `INSERT INTO monitoring_checks
    (target_key, kind, entity_id, group_id, status, latency_ms, first_byte_ms, status_code, error_class, message, checked_at, source)
    VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`
	for _, result := range results {
		if _, err := tx.ExecContext(
			ctx, query, result.TargetKey, result.Kind, result.EntityID, result.GroupID,
			result.Status, result.LatencyMs, result.FirstByteMs, result.StatusCode,
			result.ErrorClass, result.Message, result.CheckedAt, result.Source,
		); err != nil {
			return fmt.Errorf("insert monitoring result: %w", err)
		}
		if probeResolvesChannelError(result) {
			if _, err := tx.ExecContext(ctx, clearResolvedChannelErrorQuery, result.TargetKey, result.CheckedAt); err != nil {
				return fmt.Errorf("clear resolved channel error for %s: %w", result.TargetKey, err)
			}
		}
	}
	return tx.Commit()
}

const clearResolvedChannelErrorQuery = `
UPDATE monitoring_targets
SET last_channel_error_resolved_at = CASE
        WHEN last_channel_error_resolved_at IS NULL OR last_channel_error_resolved_at < $2 THEN $2
        ELSE last_channel_error_resolved_at
    END,
    last_channel_error_at = CASE
        WHEN last_channel_error_at IS NOT NULL AND last_channel_error_at <= $2 THEN NULL
        ELSE last_channel_error_at
    END,
    last_channel_error_class = CASE
        WHEN last_channel_error_at IS NOT NULL AND last_channel_error_at <= $2 THEN ''
        ELSE last_channel_error_class
    END,
    last_channel_error_status_code = CASE
        WHEN last_channel_error_at IS NOT NULL AND last_channel_error_at <= $2 THEN NULL
        ELSE last_channel_error_status_code
    END
WHERE target_key = $1
  AND kind = 'account'`

func probeResolvesChannelError(result model.ProbeResult) bool {
	return result.Source == "probe" &&
		(result.Status == model.StatusOperational || result.Status == model.StatusDegraded)
}

func (s *Store) Prune(ctx context.Context, before time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM monitoring_checks WHERE checked_at < $1`, before); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM monitoring_alerts WHERE created_at < $1`, before); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) History(ctx context.Context, key string, limit int) ([]model.ProbeResult, error) {
	limit = normalizeHistoryLimit(limit)
	rows, err := s.db.QueryContext(ctx, historyQuery, key, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanProbeResults(rows)
}

func normalizeHistoryLimit(limit int) int {
	if limit <= 0 || limit > 1000 {
		limit = 240
	}
	return limit
}

func scanProbeResults(rows *sql.Rows) ([]model.ProbeResult, error) {
	results := make([]model.ProbeResult, 0)
	for rows.Next() {
		result, err := scanProbeResult(rows)
		if err != nil {
			return nil, err
		}
		results = append(results, result)
	}
	return results, rows.Err()
}

func scanProbeResult(rows *sql.Rows) (model.ProbeResult, error) {
	var result model.ProbeResult
	var groupID, latency, firstByte, statusCode sql.NullInt64
	err := rows.Scan(
		&result.TargetKey, &result.Kind, &result.EntityID, &groupID, &result.Status,
		&latency, &firstByte, &statusCode, &result.ErrorClass, &result.Message,
		&result.CheckedAt, &result.Source,
	)
	if err != nil {
		return model.ProbeResult{}, err
	}
	result.Message = sanitizeUpstreamMessage(result.Message)
	if groupID.Valid {
		value := groupID.Int64
		result.GroupID = &value
	}
	if latency.Valid {
		value := int(latency.Int64)
		result.LatencyMs = &value
	}
	if firstByte.Valid {
		value := int(firstByte.Int64)
		result.FirstByteMs = &value
	}
	if statusCode.Valid {
		value := int(statusCode.Int64)
		result.StatusCode = &value
	}
	return result, nil
}
