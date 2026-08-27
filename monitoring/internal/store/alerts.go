package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/DaisWW/Sub2ApiExt/monitoring/internal/model"
)

type alertState struct {
	failureStreak  int
	recoveryStreak int
	alertedStatus  string
	updatedAt      sql.NullTime
}

func (s *Store) Alerts(ctx context.Context, limit int) ([]model.Alert, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id, target_key, target_name, kind, status, title, message, created_at
FROM monitoring_alerts ORDER BY created_at DESC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	alerts := make([]model.Alert, 0)
	for rows.Next() {
		var alert model.Alert
		if err := rows.Scan(
			&alert.ID, &alert.TargetKey, &alert.TargetName, &alert.Kind,
			&alert.Status, &alert.Title, &alert.Message, &alert.CreatedAt,
		); err != nil {
			return nil, err
		}
		alert.Message = sanitizeUpstreamMessage(alert.Message)
		alerts = append(alerts, alert)
	}
	return alerts, rows.Err()
}

// EvaluateAlert 通过连续失败和恢复阈值抑制状态抖动，并写入监控告警。
func (s *Store) EvaluateAlert(ctx context.Context, result model.ProbeResult, name string, policy model.AlertPolicy) error {
	if !alertableStatus(result.Status) {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	state, exists, err := loadAlertState(ctx, tx, result.TargetKey)
	if err != nil {
		return err
	}
	if exists {
		sourceUpdatedAt, err := loadTargetSourceUpdatedAt(ctx, tx, result.TargetKey)
		if err != nil {
			return err
		}
		if !alertStateCurrent(state.updatedAt, sourceUpdatedAt) {
			state.reset()
		}
	}
	eventStatus := state.observe(result.Status, policy)
	if err := saveAlertState(ctx, tx, result.TargetKey, result.Status, state, exists); err != nil {
		return err
	}
	if eventStatus != "" {
		if err := insertAlert(ctx, tx, result, name, eventStatus); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func loadAlertState(ctx context.Context, tx *sql.Tx, key string) (alertState, bool, error) {
	var state alertState
	err := tx.QueryRowContext(ctx, `SELECT failure_streak, recovery_streak, alerted_status, updated_at
FROM monitoring_alert_states WHERE target_key = $1 FOR UPDATE`, key).
		Scan(&state.failureStreak, &state.recoveryStreak, &state.alertedStatus, &state.updatedAt)
	if err == sql.ErrNoRows {
		return alertState{}, false, nil
	}
	return state, true, err
}

func loadTargetSourceUpdatedAt(ctx context.Context, tx *sql.Tx, key string) (sql.NullTime, error) {
	var updatedAt sql.NullTime
	err := tx.QueryRowContext(ctx, `SELECT source_updated_at
FROM monitoring_targets WHERE target_key = $1`, key).Scan(&updatedAt)
	if err == sql.ErrNoRows {
		return sql.NullTime{}, nil
	}
	return updatedAt, err
}

func saveAlertState(ctx context.Context, tx *sql.Tx, key, observed string, state alertState, exists bool) error {
	if !exists {
		_, err := tx.ExecContext(ctx, `INSERT INTO monitoring_alert_states
 (target_key, observed_status, failure_streak, recovery_streak, alerted_status)
 VALUES ($1,$2,$3,$4,$5)`, key, observed, state.failureStreak, state.recoveryStreak, state.alertedStatus)
		return err
	}
	_, err := tx.ExecContext(ctx, `UPDATE monitoring_alert_states SET observed_status=$2, failure_streak=$3,
 recovery_streak=$4, alerted_status=$5, updated_at=NOW() WHERE target_key=$1`,
		key, observed, state.failureStreak, state.recoveryStreak, state.alertedStatus)
	return err
}

func insertAlert(ctx context.Context, tx *sql.Tx, result model.ProbeResult, name, status string) error {
	title, message := alertText(name, result, status)
	_, err := tx.ExecContext(ctx, `INSERT INTO monitoring_alerts
 (target_key, target_name, kind, status, title, message) VALUES ($1,$2,$3,$4,$5,$6)`,
		result.TargetKey, name, result.Kind, status, title, message)
	return err
}

func (s *alertState) observe(status string, policy model.AlertPolicy) string {
	switch status {
	case model.StatusFailed, model.StatusError:
		s.failureStreak++
		s.recoveryStreak = 0
		if s.failureStreak >= policy.FailureThreshold && s.alertedStatus != model.StatusFailed {
			s.alertedStatus = model.StatusFailed
			return model.StatusFailed
		}
		return ""
	case model.StatusDegraded:
		// Degraded means the route is still serving traffic but remains risky;
		// it must not count as a clean recovery.
		s.failureStreak = 0
		s.recoveryStreak = 0
		return ""
	case model.StatusOperational:
		s.failureStreak = 0
		s.recoveryStreak++
		if s.recoveryStreak >= policy.RecoveryThreshold && s.alertedStatus == model.StatusFailed {
			s.alertedStatus = model.StatusOperational
			return model.StatusOperational
		}
		return ""
	default:
		return ""
	}
}

func (s *alertState) reset() {
	s.failureStreak = 0
	s.recoveryStreak = 0
	s.alertedStatus = ""
}

func alertableStatus(status string) bool {
	switch status {
	case model.StatusOperational, model.StatusDegraded, model.StatusFailed, model.StatusError:
		return true
	default:
		return false
	}
}

func alertText(name string, result model.ProbeResult, status string) (string, string) {
	if status == model.StatusOperational {
		return "服务已恢复", fmt.Sprintf("%s 已由%s确认恢复。", name, observationLabel(result.Source))
	}
	message := strings.TrimSpace(result.Message)
	if message == "" {
		message = observationLabel(result.Source) + "连续失败"
	}
	message = sanitizeUpstreamMessage(message)
	return "服务不可用", fmt.Sprintf("%s 不可用：%s", name, message)
}

func observationLabel(source string) string {
	switch source {
	case "history":
		return "真实请求"
	case "aggregate":
		return "分组健康信号"
	default:
		return "主动探测"
	}
}
