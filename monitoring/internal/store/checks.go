package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/DaisWW/Sub2ApiExt/monitoring/internal/model"
)

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
	}
	return tx.Commit()
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
	const query = `
SELECT target_key, kind, entity_id, group_id, status, latency_ms, first_byte_ms,
       status_code, error_class, message, checked_at, source
FROM (
    SELECT target_key, kind, entity_id, group_id, status, latency_ms, first_byte_ms,
           status_code, error_class, message, checked_at, source
    FROM monitoring_checks
    WHERE target_key = $1 AND checked_at >= NOW() - INTERVAL '24 hours'
    UNION ALL
	SELECT 'account:' || account_id::text, 'account', account_id, group_id, 'operational',
	           duration_ms, first_token_ms, NULL::integer, '', '真实请求历史', created_at, 'history'
	FROM usage_logs
	WHERE $1 = 'account:' || account_id::text
	  AND actual_cost > 0
	  AND created_at >= NOW() - INTERVAL '24 hours'
    UNION ALL
	SELECT 'group:' || COALESCE(group_id, -1)::text, 'group', COALESCE(group_id, -1), group_id, 'operational',
	           duration_ms, first_token_ms, NULL::integer, '', '真实请求历史', created_at, 'history'
	FROM usage_logs
	WHERE $1 = 'group:' || COALESCE(group_id, -1)::text
	  AND actual_cost > 0
	  AND created_at >= NOW() - INTERVAL '24 hours'
) combined
ORDER BY checked_at DESC
LIMIT $2`
	rows, err := s.db.QueryContext(ctx, query, key, limit)
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
