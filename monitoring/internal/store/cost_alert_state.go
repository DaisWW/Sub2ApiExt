package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/DaisWW/Sub2ApiExt/monitoring/internal/config"
	"github.com/DaisWW/Sub2ApiExt/monitoring/internal/model"
)

type costAlertState struct {
	alertKey        string
	lastAlertedAt   sql.NullTime
	active          bool
	firstSeenAt     *time.Time
	lastSeenAt      *time.Time
	normalSinceAt   *time.Time
	lastSeverity    string
	lastEvent       model.CostAlertEvent
	pendingRecovery bool
}

func loadCostAlertState(scan func(...any) error) (costAlertState, error) {
	var state costAlertState
	var firstSeenAt, lastSeenAt, normalSinceAt sql.NullTime
	var rawEvent []byte
	if err := scan(
		&state.alertKey, &state.lastAlertedAt, &state.active,
		&firstSeenAt, &lastSeenAt, &normalSinceAt, &state.lastSeverity,
		&rawEvent, &state.pendingRecovery,
	); err != nil {
		return costAlertState{}, err
	}
	state.firstSeenAt = nullableCostAlertTime(firstSeenAt)
	state.lastSeenAt = nullableCostAlertTime(lastSeenAt)
	state.normalSinceAt = nullableCostAlertTime(normalSinceAt)
	if len(rawEvent) > 0 {
		if err := json.Unmarshal(rawEvent, &state.lastEvent); err != nil {
			return costAlertState{}, fmt.Errorf("decode cost alert state %s: %w", state.alertKey, err)
		}
	}
	return state, nil
}

func nullableCostAlertTime(value sql.NullTime) *time.Time {
	if !value.Valid {
		return nil
	}
	result := value.Time
	return &result
}

func marshalCostAlertEvent(event model.CostAlertEvent) ([]byte, error) {
	event.ID = 0
	event.NotificationType = ""
	return json.Marshal(event)
}

func (s *Store) loadCostAlertStates(ctx context.Context, tx *sql.Tx) (map[string]costAlertState, error) {
	rows, err := tx.QueryContext(ctx, `
SELECT alert_key, last_alerted_at, active, first_seen_at, last_seen_at,
       normal_since_at, last_severity, last_event, pending_recovery
FROM monitoring_cost_alert_states
WHERE active = TRUE
FOR UPDATE`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	states := make(map[string]costAlertState)
	for rows.Next() {
		state, err := loadCostAlertState(rows.Scan)
		if err != nil {
			return nil, err
		}
		states[state.alertKey] = state
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return states, nil
}

func nullableTimeValue(value sql.NullTime) any {
	if !value.Valid {
		return nil
	}
	return value.Time
}

func saveCostAlertState(ctx context.Context, tx *sql.Tx, state costAlertState, eventJSON []byte, exists bool) error {
	if exists {
		return updateCostAlertState(ctx, tx, state, eventJSON)
	}
	_, err := tx.ExecContext(ctx, `
INSERT INTO monitoring_cost_alert_states
 (alert_key, last_alerted_at, active, first_seen_at, last_seen_at,
  normal_since_at, last_severity, last_event, pending_recovery)
VALUES ($1, $2, TRUE, $3, $4, $5, $6, $7::jsonb, $8)
ON CONFLICT (alert_key) DO UPDATE
SET last_alerted_at = EXCLUDED.last_alerted_at,
    active = TRUE,
    first_seen_at = EXCLUDED.first_seen_at,
    last_seen_at = EXCLUDED.last_seen_at,
    normal_since_at = EXCLUDED.normal_since_at,
    last_severity = EXCLUDED.last_severity,
    last_event = EXCLUDED.last_event,
    pending_recovery = EXCLUDED.pending_recovery,
		updated_at = NOW()`, state.alertKey, nullableTimeValue(state.lastAlertedAt), state.firstSeenAt,
		state.lastSeenAt, state.normalSinceAt, state.lastSeverity, eventJSON, state.pendingRecovery)
	return err
}

func updateCostAlertState(ctx context.Context, tx *sql.Tx, state costAlertState, eventJSON []byte) error {
	_, err := tx.ExecContext(ctx, `
UPDATE monitoring_cost_alert_states
SET last_alerted_at = $2,
    active = $3,
    first_seen_at = $4,
    last_seen_at = $5,
    normal_since_at = $6,
    last_severity = $7,
    last_event = $8::jsonb,
    pending_recovery = $9,
    updated_at = NOW()
WHERE alert_key = $1`, state.alertKey, nullableTimeValue(state.lastAlertedAt), state.active,
		state.firstSeenAt, state.lastSeenAt, state.normalSinceAt, state.lastSeverity,
		eventJSON, state.pendingRecovery)
	return err
}

func observeCostAlert(state costAlertState, exists bool, candidate model.CostAlertEvent, now time.Time, cooldown time.Duration) (costAlertState, model.CostAlertEvent, bool) {
	shouldNotify := false
	if !exists || !state.active {
		state.active = true
		state.firstSeenAt = timePointer(now)
		state.normalSinceAt = nil
		state.pendingRecovery = false
		candidate.NotificationType = model.CostAlertNotificationStart
		shouldNotify = true
	} else {
		candidate.NotificationType = model.CostAlertNotificationReminder
		shouldNotify = !state.lastAlertedAt.Valid || now.Sub(state.lastAlertedAt.Time) >= cooldown
		if state.lastSeverity != "critical" && candidate.Severity == "critical" {
			candidate.NotificationType = model.CostAlertNotificationEscalation
			shouldNotify = true
		}
		state.normalSinceAt = nil
		state.pendingRecovery = false
	}
	if state.firstSeenAt == nil {
		state.firstSeenAt = timePointer(now)
	}
	candidate.IncidentStartedAt = timeValue(state.firstSeenAt, now)
	state.lastSeenAt = timePointer(now)
	state.lastSeverity = candidate.Severity
	state.lastEvent = candidate
	if shouldNotify {
		state.lastAlertedAt = sql.NullTime{Time: now, Valid: true}
	}
	return state, candidate, shouldNotify
}

func (s *Store) persistCostAlertCandidates(ctx context.Context, candidates []model.CostAlertEvent, now time.Time, policy config.CostAlertConfig) ([]model.CostAlertEvent, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	states, err := s.loadCostAlertStates(ctx, tx)
	if err != nil {
		return nil, err
	}
	byKey := costAlertCandidatesByKey(candidates)
	keys := sortedCostAlertKeys(byKey)
	seen := make(map[string]struct{}, len(keys))
	newAlerts := make([]model.CostAlertEvent, 0, len(keys))
	for _, key := range keys {
		candidate := byKey[key]
		state, exists := states[key]
		state, candidate, shouldNotify := observeCostAlert(state, exists, candidate, now, policy.Cooldown)
		if err := persistObservedCostAlert(ctx, tx, state, candidate, exists); err != nil {
			return nil, err
		}
		states[key] = state
		seen[key] = struct{}{}
		if shouldNotify {
			alert, err := insertCostAlert(ctx, tx, candidate, now)
			if err != nil {
				return nil, err
			}
			newAlerts = append(newAlerts, alert)
		}
	}
	recoveries, err := persistCostAlertRecoveries(ctx, tx, states, seen, now, policy)
	if err != nil {
		return nil, err
	}
	newAlerts = append(newAlerts, recoveries...)
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return newAlerts, nil
}

func costAlertCandidatesByKey(candidates []model.CostAlertEvent) map[string]model.CostAlertEvent {
	byKey := make(map[string]model.CostAlertEvent, len(candidates))
	for _, candidate := range candidates {
		if candidate.AlertKey != "" {
			byKey[candidate.AlertKey] = candidate
		}
	}
	return byKey
}

func sortedCostAlertKeys(events map[string]model.CostAlertEvent) []string {
	keys := make([]string, 0, len(events))
	for key := range events {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func persistObservedCostAlert(ctx context.Context, tx *sql.Tx, state costAlertState, candidate model.CostAlertEvent, exists bool) error {
	eventJSON, err := marshalCostAlertEvent(candidate)
	if err != nil {
		return fmt.Errorf("marshal cost alert state %s: %w", state.alertKey, err)
	}
	return saveCostAlertState(ctx, tx, state, eventJSON, exists)
}

func insertCostAlert(ctx context.Context, tx *sql.Tx, event model.CostAlertEvent, now time.Time) (model.CostAlertEvent, error) {
	var id int64
	err := tx.QueryRowContext(ctx, `
INSERT INTO monitoring_cost_alerts
 (alert_key, kind, notification_type, severity, target_key, title, message)
VALUES ($1, $2, $3, $4, $5, $6, $7)
RETURNING id`, event.AlertKey, event.Kind, event.NotificationType, event.Severity,
		event.TargetKey, event.Title, event.Message).Scan(&id)
	if err != nil {
		return model.CostAlertEvent{}, err
	}
	event.ID = id
	event.CreatedAt = now
	return event, nil
}

func persistCostAlertRecoveries(ctx context.Context, tx *sql.Tx, states map[string]costAlertState, seen map[string]struct{}, now time.Time, policy config.CostAlertConfig) ([]model.CostAlertEvent, error) {
	keys := sortedCostAlertStateKeys(states)
	recoveries := make([]model.CostAlertEvent, 0)
	for _, key := range keys {
		if _, exists := seen[key]; exists {
			continue
		}
		state := states[key]
		if !state.active {
			continue
		}
		state, recovery, shouldNotify, changed := prepareCostAlertRecovery(state, now, policy)
		if !changed {
			continue
		}
		if shouldNotify {
			if err := persistObservedCostAlert(ctx, tx, state, recovery, true); err != nil {
				return nil, err
			}
			alert, err := insertCostAlert(ctx, tx, recovery, now)
			if err != nil {
				return nil, err
			}
			recoveries = append(recoveries, alert)
		} else {
			if err := persistCostAlertStateSnapshot(ctx, tx, state); err != nil {
				return nil, err
			}
		}
		states[key] = state
	}
	return recoveries, nil
}

func sortedCostAlertStateKeys(states map[string]costAlertState) []string {
	keys := make([]string, 0, len(states))
	for key := range states {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func persistCostAlertStateSnapshot(ctx context.Context, tx *sql.Tx, state costAlertState) error {
	eventJSON, err := marshalCostAlertEvent(state.lastEvent)
	if err != nil {
		return fmt.Errorf("marshal cost alert state %s: %w", state.alertKey, err)
	}
	return updateCostAlertState(ctx, tx, state, eventJSON)
}

func prepareCostAlertRecovery(state costAlertState, now time.Time, policy config.CostAlertConfig) (costAlertState, model.CostAlertEvent, bool, bool) {
	changed := false
	if state.pendingRecovery {
		if !costAlertCooldownElapsed(state.lastAlertedAt, now, policy.Cooldown) {
			return state, model.CostAlertEvent{}, false, false
		}
		state.pendingRecovery = false
		changed = true
	}
	if state.normalSinceAt == nil {
		state.normalSinceAt = timePointer(now)
		return state, model.CostAlertEvent{}, false, true
	}
	if now.Sub(*state.normalSinceAt) < 2*policy.Window ||
		!costAlertCooldownElapsed(state.lastAlertedAt, now, policy.Cooldown) {
		return state, model.CostAlertEvent{}, false, changed
	}
	recovery := buildCostAlertRecovery(state, now, policy.Window)
	state.pendingRecovery = true
	state.lastAlertedAt = sql.NullTime{Time: now, Valid: true}
	state.lastEvent = recovery
	return state, recovery, true, true
}

func costAlertCooldownElapsed(last sql.NullTime, now time.Time, cooldown time.Duration) bool {
	return !last.Valid || now.Sub(last.Time) >= cooldown
}

func buildCostAlertRecovery(state costAlertState, now time.Time, window time.Duration) model.CostAlertEvent {
	event := state.lastEvent
	userLabel := model.FormatIdentity(event.UserName, event.UserEmail, event.UserKey, "用户")
	event.ID = 0
	event.NotificationType = model.CostAlertNotificationRecovery
	event.Severity = "info"
	event.Title = "费用异常已恢复"
	event.Message = fmt.Sprintf(
		"用户 %s 的 %s 费用异常已连续两个分析窗口未再触发，监控标记为恢复。",
		userLabel, event.Model,
	)
	event.WindowStart = now.Add(-window)
	event.WindowEnd = now
	event.CreatedAt = now
	event.IncidentStartedAt = timeValue(state.firstSeenAt, now)
	return event
}

func timePointer(value time.Time) *time.Time {
	result := value
	return &result
}

func timeValue(value *time.Time, fallback time.Time) time.Time {
	if value == nil {
		return fallback
	}
	return *value
}
