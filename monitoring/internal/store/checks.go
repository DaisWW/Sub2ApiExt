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
    SELECT target_key, kind, entity_id, source_updated_at,
           last_channel_error_at, last_channel_error_class,
           last_channel_error_status_code, last_channel_error_resolved_at
    FROM monitoring_targets
    WHERE target_key = $1 AND active = TRUE
), account_error_events AS (
    SELECT auth.target_key, auth.entity_id AS account_id,
           auth.last_channel_error_at AS created_at,
           auth.last_channel_error_status_code AS status_code,
           NULLIF(BTRIM(auth.last_channel_error_class), '') AS error_class
    FROM authorized_target auth
    WHERE auth.kind = 'account'
      AND auth.last_channel_error_at IS NOT NULL
      AND auth.last_channel_error_at >= NOW() - INTERVAL '24 hours'
      AND (auth.source_updated_at IS NULL
           OR auth.last_channel_error_at >= auth.source_updated_at - INTERVAL '2 minutes')
      AND (auth.last_channel_error_resolved_at IS NULL
           OR auth.last_channel_error_at > auth.last_channel_error_resolved_at)
), combined AS (
SELECT monitoring_checks.target_key, monitoring_checks.kind, monitoring_checks.entity_id, monitoring_checks.group_id,
       NULL::bigint AS account_id, '' AS account_name,
       monitoring_checks.status, monitoring_checks.latency_ms, monitoring_checks.first_byte_ms,
       monitoring_checks.status_code, monitoring_checks.error_class, monitoring_checks.message,
       monitoring_checks.checked_at, monitoring_checks.source
FROM monitoring_checks
JOIN authorized_target auth ON auth.target_key = monitoring_checks.target_key
WHERE monitoring_checks.checked_at >= NOW() - INTERVAL '24 hours'
  AND auth.kind = 'account'
  AND monitoring_checks.source IN ('probe', 'request_error')
UNION ALL
SELECT 'account:' || usage_logs.account_id::text, 'account', usage_logs.account_id, usage_logs.group_id,
       usage_logs.account_id,
       COALESCE(NULLIF(BTRIM(a.name), ''), CASE
           WHEN usage_logs.account_id IS NULL THEN '未知账户'
           ELSE '账户 #' || usage_logs.account_id::text
       END),
       CASE WHEN usage_logs.duration_ms >= 20000 THEN 'degraded' ELSE 'operational' END,
       usage_logs.duration_ms, usage_logs.first_token_ms, NULL::integer, '', '真实请求历史', usage_logs.created_at, 'history'
FROM usage_logs
LEFT JOIN accounts a ON a.id = usage_logs.account_id
JOIN authorized_target auth ON TRUE
WHERE auth.kind = 'account'
  AND auth.entity_id = usage_logs.account_id
  AND usage_logs.actual_cost > 0
  AND usage_logs.created_at >= NOW() - INTERVAL '24 hours'
UNION ALL
SELECT 'group:' || usage_logs.group_id::text, 'group', usage_logs.group_id, usage_logs.group_id,
       usage_logs.account_id,
       COALESCE(NULLIF(BTRIM(a.name), ''), CASE
           WHEN usage_logs.account_id IS NULL THEN '未知账户'
           ELSE '账户 #' || usage_logs.account_id::text
       END),
       CASE WHEN usage_logs.duration_ms >= 20000 THEN 'degraded' ELSE 'operational' END,
       usage_logs.duration_ms, usage_logs.first_token_ms, NULL::integer, '', '真实请求历史', usage_logs.created_at, 'history'
FROM usage_logs
LEFT JOIN accounts a ON a.id = usage_logs.account_id
JOIN authorized_target auth ON TRUE
WHERE auth.kind = 'group'
  AND auth.entity_id = usage_logs.group_id
  AND usage_logs.group_id IS NOT NULL
  AND usage_logs.actual_cost > 0
  AND usage_logs.created_at >= NOW() - INTERVAL '24 hours'
UNION ALL
SELECT errors.target_key, 'account', errors.account_id, NULL::bigint,
       errors.account_id,
       COALESCE(NULLIF(BTRIM((SELECT name FROM accounts WHERE id = errors.account_id)), ''), CASE
           WHEN errors.account_id IS NULL THEN '未知账户'
           ELSE '账户 #' || errors.account_id::text
       END),
       'failed',
       NULL::integer, NULL::integer, errors.status_code, COALESCE(errors.error_class, ''),
       '真实请求报错', errors.created_at, 'request_error'
FROM account_error_events errors
WHERE NOT EXISTS (
    SELECT 1
    FROM monitoring_checks consumed
    WHERE consumed.target_key = errors.target_key
      AND consumed.kind = 'account'
      AND consumed.source = 'request_error'
      AND consumed.checked_at = errors.created_at
)
)
SELECT target_key, kind, entity_id, group_id, account_id, account_name, status, latency_ms, first_byte_ms,
       status_code, error_class, message, checked_at, source
FROM combined
ORDER BY checked_at DESC,
         CASE WHEN source = 'history' THEN 0 ELSE 1 END
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
		if err := markObservedAccountEvidence(ctx, tx, result); err != nil {
			return err
		}
		if probeResolvesChannelError(result) {
			if _, err := tx.ExecContext(ctx, clearResolvedChannelErrorQuery, result.TargetKey, result.CheckedAt); err != nil {
				return fmt.Errorf("clear resolved channel error for %s: %w", result.TargetKey, err)
			}
		}
	}
	return tx.Commit()
}

func markObservedAccountEvidence(ctx context.Context, tx *sql.Tx, result model.ProbeResult) error {
	if result.Kind != model.KindAccount {
		return nil
	}
	column := observedEvidenceColumn(result.Source)
	if column == "" {
		return nil
	}
	query := `UPDATE monitoring_targets SET ` + column + ` = CASE
        WHEN ` + column + ` IS NULL OR ` + column + ` < $2 THEN $2
        ELSE ` + column + ` END
WHERE target_key = $1 AND kind = 'account'`
	if _, err := tx.ExecContext(ctx, query, result.TargetKey, result.CheckedAt); err != nil {
		return fmt.Errorf("advance %s evidence watermark for %s: %w", result.Source, result.TargetKey, err)
	}
	return nil
}

func observedEvidenceColumn(source string) string {
	switch source {
	case "history":
		return "last_observed_activity_at"
	case "request_error":
		return "last_observed_channel_error_at"
	default:
		return ""
	}
}

const clearResolvedChannelErrorQuery = `
UPDATE monitoring_targets
SET last_channel_error_resolved_at = CASE
        WHEN last_channel_error_at IS NOT NULL AND last_channel_error_at >= $2
        THEN last_channel_error_resolved_at
        WHEN last_channel_error_resolved_at IS NULL OR last_channel_error_resolved_at < $2
        THEN $2
        ELSE last_channel_error_resolved_at
    END,
    last_channel_error_at = CASE
        WHEN last_channel_error_at IS NOT NULL AND last_channel_error_at < $2 THEN NULL
        ELSE last_channel_error_at
    END,
    last_channel_error_class = CASE
        WHEN last_channel_error_at IS NOT NULL AND last_channel_error_at < $2 THEN ''
        ELSE last_channel_error_class
    END,
    last_channel_error_status_code = CASE
        WHEN last_channel_error_at IS NOT NULL AND last_channel_error_at < $2 THEN NULL
        ELSE last_channel_error_status_code
    END
WHERE target_key = $1
  AND kind = 'account'`

func probeResolvesChannelError(result model.ProbeResult) bool {
	return result.Source == "probe" &&
		(result.Status == model.StatusOperational || result.Status == model.StatusDegraded)
}

const pruneChecksQuery = `
DELETE FROM monitoring_checks old_check
WHERE old_check.checked_at < $1
  AND EXISTS (
      SELECT 1
      FROM monitoring_checks newer_check
      WHERE newer_check.target_key = old_check.target_key
        AND newer_check.source = old_check.source
        AND (newer_check.checked_at > old_check.checked_at
             OR (newer_check.checked_at = old_check.checked_at
                 AND newer_check.id > old_check.id))
  )`

func (s *Store) Prune(ctx context.Context, before time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, pruneChecksQuery, before); err != nil {
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
	var groupID, accountID, latency, firstByte, statusCode sql.NullInt64
	err := rows.Scan(
		&result.TargetKey, &result.Kind, &result.EntityID, &groupID, &accountID, &result.AccountName, &result.Status,
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
	if accountID.Valid {
		value := accountID.Int64
		result.AccountID = &value
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
