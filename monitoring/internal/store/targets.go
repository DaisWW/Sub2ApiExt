package store

import (
	"context"
	"fmt"

	"github.com/DaisWW/Sub2ApiExt/monitoring/internal/model"
)

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
	const upsert = `
INSERT INTO monitoring_targets (target_key, kind, entity_id, name, platform, source_status, probe_enabled, active, updated_at)
VALUES ($1,$2,$3,$4,$5,$6,$7,TRUE,NOW())
ON CONFLICT (target_key) DO UPDATE SET
    kind = EXCLUDED.kind, entity_id = EXCLUDED.entity_id, name = EXCLUDED.name,
    platform = EXCLUDED.platform, source_status = EXCLUDED.source_status,
    probe_enabled = EXCLUDED.probe_enabled, active = TRUE, updated_at = NOW()`
	for _, account := range snapshot.Accounts {
		if !accountIsMonitored(account) {
			continue
		}
		if _, err := tx.ExecContext(
			ctx, upsert, model.TargetKey(model.KindAccount, account.ID), model.KindAccount,
			account.ID, account.Name, account.Platform, account.Status, accountHistoryEligible(account),
		); err != nil {
			return fmt.Errorf("upsert account target %d: %w", account.ID, err)
		}
	}
	for _, group := range snapshot.Groups {
		if !groupIsActive(group.Status) {
			continue
		}
		if _, err := tx.ExecContext(
			ctx, upsert, model.TargetKey(model.KindGroup, group.ID), model.KindGroup,
			group.ID, group.Name, group.Platform, group.Status, group.ProbeEnabled,
		); err != nil {
			return fmt.Errorf("upsert group target %d: %w", group.ID, err)
		}
	}
	return tx.Commit()
}
