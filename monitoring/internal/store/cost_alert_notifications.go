package store

import (
	"context"
	"database/sql"

	"github.com/DaisWW/Sub2ApiExt/monitoring/internal/model"
)

// FinalizeCostAlertNotifications closes incidents after a recovery email was
// accepted by SMTP. Keeping the state active until this point makes a failed
// recovery send retryable without losing the incident.
func (s *Store) FinalizeCostAlertNotifications(ctx context.Context, events []model.CostAlertEvent) error {
	if len(events) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, event := range events {
		if event.NotificationType != model.CostAlertNotificationRecovery || event.AlertKey == "" {
			continue
		}
		if err := finalizeCostAlertRecovery(ctx, tx, event); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func finalizeCostAlertRecovery(ctx context.Context, tx *sql.Tx, event model.CostAlertEvent) error {
	_, err := tx.ExecContext(ctx, `
UPDATE monitoring_cost_alert_states
SET active = FALSE, pending_recovery = FALSE, normal_since_at = NULL, updated_at = NOW()
WHERE alert_key = $1
  AND pending_recovery = TRUE
  AND last_alerted_at IS NOT DISTINCT FROM $2::timestamptz`, event.AlertKey, event.CreatedAt)
	return err
}

// DiscardCostAlertNotifications removes rows for a failed email send. Active
// incidents keep their state so the next permitted cooldown can retry them.
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
		if err := discardCostAlertNotification(ctx, tx, event); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func discardCostAlertNotification(ctx context.Context, tx *sql.Tx, event model.CostAlertEvent) error {
	if event.AlertKey == "" {
		return nil
	}
	if event.ID > 0 {
		if _, err := tx.ExecContext(ctx, `
DELETE FROM monitoring_cost_alerts
WHERE id = $1 AND alert_key = $2`, event.ID, event.AlertKey); err != nil {
			return err
		}
	}
	if event.NotificationType == model.CostAlertNotificationRecovery {
		_, err := tx.ExecContext(ctx, `
UPDATE monitoring_cost_alert_states
SET pending_recovery = FALSE, updated_at = NOW()
WHERE alert_key = $1
  AND last_alerted_at IS NOT DISTINCT FROM $2::timestamptz`, event.AlertKey, event.CreatedAt)
		return err
	}
	_, err := tx.ExecContext(ctx, `
UPDATE monitoring_cost_alert_states
SET updated_at = NOW()
WHERE alert_key = $1`, event.AlertKey)
	return err
}
