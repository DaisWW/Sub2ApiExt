package store

import (
	"context"
	"fmt"
	"time"

	"github.com/DaisWW/Sub2ApiExt/monitoring/internal/model"
)

const syncTargetsUpsert = `
INSERT INTO monitoring_targets (target_key, kind, entity_id, name, platform, source_status, probe_enabled, active, last_activity_at, source_updated_at, updated_at)
VALUES ($1,$2,$3,$4,$5,$6,$7,TRUE,$8,$9,NOW())
ON CONFLICT (target_key) DO UPDATE SET
    kind = EXCLUDED.kind, entity_id = EXCLUDED.entity_id, name = EXCLUDED.name,
    platform = EXCLUDED.platform, source_status = EXCLUDED.source_status,
    probe_enabled = EXCLUDED.probe_enabled, active = TRUE,
    last_activity_at = EXCLUDED.last_activity_at,
    source_updated_at = EXCLUDED.source_updated_at, updated_at = NOW()`

// SyncTargets 将本轮发现结果同步到监控自有表，不修改网关路由或账户状态。
func (s *Store) SyncTargets(ctx context.Context, snapshot model.Snapshot) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `UPDATE monitoring_targets SET active = FALSE, updated_at = NOW()`); err != nil {
		return err
	}
	accounts := make(map[int64]*model.Account, len(snapshot.Accounts))
	for index := range snapshot.Accounts {
		account := &snapshot.Accounts[index]
		if !accountIsMonitored(*account) {
			continue
		}
		accounts[account.ID] = account
		if _, err := tx.ExecContext(
			ctx, syncTargetsUpsert, model.TargetKey(model.KindAccount, account.ID), model.KindAccount,
			account.ID, account.Name, account.Platform, account.Status, accountHistoryEligible(*account), account.LastActivityAt, account.UpdatedAt,
		); err != nil {
			return fmt.Errorf("upsert account target %d: %w", account.ID, err)
		}
	}
	for _, group := range snapshot.Groups {
		if !groupIsActive(group.Status) {
			continue
		}
		if _, err := tx.ExecContext(
			ctx, syncTargetsUpsert, model.TargetKey(model.KindGroup, group.ID), model.KindGroup,
			group.ID, group.Name, group.Platform, group.Status, group.ProbeEnabled, groupLastActivity(group, accounts), groupSourceUpdatedAt(group, accounts),
		); err != nil {
			return fmt.Errorf("upsert group target %d: %w", group.ID, err)
		}
	}
	return tx.Commit()
}

func groupSourceUpdatedAt(group model.Group, accounts map[int64]*model.Account) *time.Time {
	var latest *time.Time
	if group.UpdatedAt != nil {
		value := group.UpdatedAt.UTC()
		latest = &value
	}
	for _, accountID := range group.AccountIDs {
		account, ok := accounts[accountID]
		if !ok || account == nil || account.UpdatedAt == nil {
			continue
		}
		value := account.UpdatedAt.UTC()
		if latest == nil || value.After(*latest) {
			latest = &value
		}
	}
	return latest
}

func groupLastActivity(group model.Group, accounts map[int64]*model.Account) *time.Time {
	var latest *time.Time
	for _, accountID := range group.AccountIDs {
		account, ok := accounts[accountID]
		if !ok || account == nil {
			continue
		}
		activity := account.LastActivityAt
		if activity != nil && (latest == nil || activity.After(*latest)) {
			value := *activity
			latest = &value
		}
	}
	return latest
}
