package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/DaisWW/Sub2ApiExt/monitoring/internal/model"
)

const syncTargetsUpsert = `
INSERT INTO monitoring_targets (target_key, kind, entity_id, name, platform, source_status, probe_enabled, active, last_activity_at, last_channel_error_at, last_channel_error_class, last_channel_error_status_code, last_channel_error_resolved_at, source_fingerprint, source_updated_at, updated_at)
VALUES ($1,$2,$3,$4,$5,$6,$7,TRUE,$8,$9,$10,$11,$12,$13,$14,NOW())
ON CONFLICT (target_key) DO UPDATE SET
    kind = EXCLUDED.kind, entity_id = EXCLUDED.entity_id, name = EXCLUDED.name,
    platform = EXCLUDED.platform, source_status = EXCLUDED.source_status,
    probe_enabled = EXCLUDED.probe_enabled, active = TRUE,
    last_activity_at = CASE
        WHEN EXCLUDED.last_activity_at IS NULL THEN monitoring_targets.last_activity_at
        WHEN monitoring_targets.last_activity_at IS NULL
             OR EXCLUDED.last_activity_at >= monitoring_targets.last_activity_at
        THEN EXCLUDED.last_activity_at
        ELSE monitoring_targets.last_activity_at
    END,
    -- Accept an incoming error only when it is newer than the incoming
    -- resolution watermark. Keep stored metadata only while it is newer than
    -- both watermarks and the latest source update.
    last_channel_error_at = CASE
        WHEN EXCLUDED.last_channel_error_at IS NOT NULL
             AND (EXCLUDED.last_channel_error_resolved_at IS NULL
                  OR EXCLUDED.last_channel_error_at > EXCLUDED.last_channel_error_resolved_at)
             AND (EXCLUDED.source_updated_at IS NULL
                  OR EXCLUDED.last_channel_error_at >= EXCLUDED.source_updated_at - INTERVAL '2 minutes')
             AND (monitoring_targets.last_channel_error_at IS NULL
                  OR EXCLUDED.last_channel_error_at >= monitoring_targets.last_channel_error_at)
        THEN EXCLUDED.last_channel_error_at
        WHEN monitoring_targets.last_channel_error_at IS NOT NULL
             AND (monitoring_targets.last_channel_error_resolved_at IS NULL
                  OR monitoring_targets.last_channel_error_at > monitoring_targets.last_channel_error_resolved_at)
             AND (EXCLUDED.last_channel_error_resolved_at IS NULL
                  OR monitoring_targets.last_channel_error_at > EXCLUDED.last_channel_error_resolved_at)
             AND (EXCLUDED.source_updated_at IS NULL
                  OR monitoring_targets.last_channel_error_at >= EXCLUDED.source_updated_at - INTERVAL '2 minutes')
        THEN monitoring_targets.last_channel_error_at
        ELSE NULL
    END,
    last_channel_error_class = CASE
        WHEN EXCLUDED.last_channel_error_at IS NOT NULL
             AND (EXCLUDED.last_channel_error_resolved_at IS NULL
                  OR EXCLUDED.last_channel_error_at > EXCLUDED.last_channel_error_resolved_at)
             AND (EXCLUDED.source_updated_at IS NULL
                  OR EXCLUDED.last_channel_error_at >= EXCLUDED.source_updated_at - INTERVAL '2 minutes')
             AND (monitoring_targets.last_channel_error_at IS NULL
                  OR EXCLUDED.last_channel_error_at >= monitoring_targets.last_channel_error_at)
        THEN EXCLUDED.last_channel_error_class
        WHEN monitoring_targets.last_channel_error_at IS NOT NULL
             AND (monitoring_targets.last_channel_error_resolved_at IS NULL
                  OR monitoring_targets.last_channel_error_at > monitoring_targets.last_channel_error_resolved_at)
             AND (EXCLUDED.last_channel_error_resolved_at IS NULL
                  OR monitoring_targets.last_channel_error_at > EXCLUDED.last_channel_error_resolved_at)
             AND (EXCLUDED.source_updated_at IS NULL
                  OR monitoring_targets.last_channel_error_at >= EXCLUDED.source_updated_at - INTERVAL '2 minutes')
        THEN monitoring_targets.last_channel_error_class
        ELSE ''
    END,
    last_channel_error_status_code = CASE
        WHEN EXCLUDED.last_channel_error_at IS NOT NULL
             AND (EXCLUDED.last_channel_error_resolved_at IS NULL
                  OR EXCLUDED.last_channel_error_at > EXCLUDED.last_channel_error_resolved_at)
             AND (EXCLUDED.source_updated_at IS NULL
                  OR EXCLUDED.last_channel_error_at >= EXCLUDED.source_updated_at - INTERVAL '2 minutes')
             AND (monitoring_targets.last_channel_error_at IS NULL
                  OR EXCLUDED.last_channel_error_at >= monitoring_targets.last_channel_error_at)
        THEN EXCLUDED.last_channel_error_status_code
        WHEN monitoring_targets.last_channel_error_at IS NOT NULL
             AND (monitoring_targets.last_channel_error_resolved_at IS NULL
                  OR monitoring_targets.last_channel_error_at > monitoring_targets.last_channel_error_resolved_at)
             AND (EXCLUDED.last_channel_error_resolved_at IS NULL
                  OR monitoring_targets.last_channel_error_at > EXCLUDED.last_channel_error_resolved_at)
             AND (EXCLUDED.source_updated_at IS NULL
                  OR monitoring_targets.last_channel_error_at >= EXCLUDED.source_updated_at - INTERVAL '2 minutes')
        THEN monitoring_targets.last_channel_error_status_code
        ELSE NULL
    END,
    last_channel_error_resolved_at = CASE
        WHEN EXCLUDED.last_channel_error_resolved_at IS NULL THEN monitoring_targets.last_channel_error_resolved_at
        WHEN monitoring_targets.last_channel_error_resolved_at IS NULL
             OR EXCLUDED.last_channel_error_resolved_at > monitoring_targets.last_channel_error_resolved_at
        THEN EXCLUDED.last_channel_error_resolved_at
        ELSE monitoring_targets.last_channel_error_resolved_at
    END,
    source_fingerprint = EXCLUDED.source_fingerprint,
    source_updated_at = CASE
        -- A blank fingerprint marks a row from before meaningful source
        -- identity was introduced. Rebaseline it instead of retaining an
        -- upstream rate-sync timestamp that would hide valid history.
        WHEN COALESCE(monitoring_targets.source_fingerprint, '') = '' THEN NULL
        -- A real source change invalidates old evidence even if its timestamp
        -- is earlier than a stale value left by a previous write.
        WHEN EXCLUDED.source_fingerprint IS DISTINCT FROM monitoring_targets.source_fingerprint
            THEN EXCLUDED.source_updated_at
        -- Activity and rate-only writes must not advance the meaningful
        -- watermark while the source identity is unchanged.
        ELSE monitoring_targets.source_updated_at
    END,
    updated_at = NOW()`

type persistedTargetState struct {
	exists          bool
	fingerprint     string
	sourceUpdatedAt *time.Time
}

// SyncTargets 将本轮发现结果同步到监控自有表，不修改网关路由或账户状态。
// 保持值签名兼容既有 Store 调用方；本函数内部只更新本轮同步所需的水位。
func (s *Store) SyncTargets(ctx context.Context, snapshot model.Snapshot) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `UPDATE monitoring_targets SET active = FALSE, updated_at = NOW()`); err != nil {
		return err
	}
	states, err := loadPersistedTargetStates(ctx, tx)
	if err != nil {
		return err
	}
	accounts := make(map[int64]*model.Account, len(snapshot.Accounts))
	for index := range snapshot.Accounts {
		account := &snapshot.Accounts[index]
		if !accountIsMonitored(*account) {
			continue
		}
		accounts[account.ID] = account
		if account.SourceFingerprint == "" {
			account.SourceFingerprint = accountSourceFingerprint(*account)
		}
		candidate := accountSourceCandidate(*account)
		sourceUpdatedAt := resolveTargetSourceUpdatedAt(
			states[model.TargetKey(model.KindAccount, account.ID)],
			account.SourceFingerprint, candidate,
		)
		account.SourceUpdatedAt = cloneTime(sourceUpdatedAt)
		if _, err := tx.ExecContext(
			ctx, syncTargetsUpsert, model.TargetKey(model.KindAccount, account.ID), model.KindAccount,
			account.ID, account.Name, account.Platform, account.Status, accountHistoryEligible(*account), account.LastActivityAt,
			account.LastChannelErrorAt, account.LastChannelErrorClass, account.LastChannelErrorStatusCode,
			accountChannelErrorResolvedAt(*account), account.SourceFingerprint, sourceUpdatedAt,
		); err != nil {
			return fmt.Errorf("upsert account target %d: %w", account.ID, err)
		}
	}
	for index := range snapshot.Groups {
		group := &snapshot.Groups[index]
		if !groupIsActive(group.Status) {
			continue
		}
		if group.SourceFingerprint == "" {
			group.SourceFingerprint = groupSourceFingerprint(*group, accounts)
		}
		candidate := groupSourceCandidate(*group, accounts)
		sourceUpdatedAt := resolveTargetSourceUpdatedAt(
			states[model.TargetKey(model.KindGroup, group.ID)],
			group.SourceFingerprint, candidate,
		)
		group.SourceUpdatedAt = cloneTime(sourceUpdatedAt)
		if _, err := tx.ExecContext(
			ctx, syncTargetsUpsert, model.TargetKey(model.KindGroup, group.ID), model.KindGroup,
			group.ID, group.Name, group.Platform, group.Status, group.ProbeEnabled, groupLastActivity(*group, accounts),
			nil, "", nil, nil, group.SourceFingerprint, sourceUpdatedAt,
		); err != nil {
			return fmt.Errorf("upsert group target %d: %w", group.ID, err)
		}
	}
	return tx.Commit()
}

func loadPersistedTargetStates(ctx context.Context, tx *sql.Tx) (map[string]persistedTargetState, error) {
	rows, err := tx.QueryContext(ctx, `SELECT target_key, source_fingerprint, source_updated_at FROM monitoring_targets`)
	if err != nil {
		return nil, fmt.Errorf("load target watermarks: %w", err)
	}
	defer rows.Close()
	states := make(map[string]persistedTargetState)
	for rows.Next() {
		var key, fingerprint sql.NullString
		var sourceUpdatedAt sql.NullTime
		if err := rows.Scan(&key, &fingerprint, &sourceUpdatedAt); err != nil {
			return nil, fmt.Errorf("scan target watermark: %w", err)
		}
		if !key.Valid || strings.TrimSpace(key.String) == "" {
			continue
		}
		state := persistedTargetState{exists: true, fingerprint: strings.TrimSpace(fingerprint.String)}
		if sourceUpdatedAt.Valid {
			value := sourceUpdatedAt.Time.UTC()
			state.sourceUpdatedAt = &value
		}
		states[key.String] = state
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read target watermarks: %w", err)
	}
	return states, nil
}

// resolveTargetSourceUpdatedAt separates the persisted meaningful watermark
// from the upstream row's raw updated_at value. A legacy row is rebaselined on
// its first fingerprinted sync; an unchanged fingerprint keeps its watermark
// even when rate-sync has advanced updated_at.
func resolveTargetSourceUpdatedAt(state persistedTargetState, fingerprint string, candidate *time.Time) *time.Time {
	if !state.exists {
		// There is no old evidence to invalidate for a newly discovered target.
		return cloneTime(candidate)
	}
	if !currentSourceFingerprint(state.fingerprint) {
		// A blank or older-version fingerprint predates the current source
		// identity rules. Clear it once so an implementation upgrade does not
		// masquerade as a real source change and hide valid history.
		return nil
	}
	if state.fingerprint == fingerprint {
		return cloneTime(state.sourceUpdatedAt)
	}
	return cloneTime(candidate)
}

func cloneTime(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	copy := value.UTC()
	return &copy
}

func groupSourceUpdatedAt(group model.Group, accounts map[int64]*model.Account) *time.Time {
	if group.SourceFingerprint != "" {
		return cloneTime(group.SourceUpdatedAt)
	}
	return groupSourceCandidate(group, accounts)
}

func groupSourceCandidate(group model.Group, accounts map[int64]*model.Account) *time.Time {
	activity := groupLastActivity(group, accounts)
	var latest *time.Time
	if group.UpdatedAt != nil {
		latest = effectiveSourceUpdatedAt(group.UpdatedAt, activity)
	}
	for _, accountID := range group.AccountIDs {
		account, ok := accounts[accountID]
		if !ok || account == nil {
			continue
		}
		accountUpdatedAt := accountSourceCandidate(*account)
		if accountUpdatedAt == nil {
			continue
		}
		value := accountUpdatedAt.UTC()
		if latest == nil || value.After(*latest) {
			latest = &value
		}
	}
	return latest
}

// effectiveSourceUpdatedAt ignores the brief accounting lag observed between
// a successful usage row and the owning account's updated_at value. A larger
// gap remains a real source change and invalidates older evidence.
func effectiveSourceUpdatedAt(updatedAt, activity *time.Time) *time.Time {
	if updatedAt == nil {
		return nil
	}
	if activity != nil {
		lag := updatedAt.Sub(*activity)
		if lag < 0 {
			lag = -lag
		}
		if lag <= model.SourceUpdateActivityGrace {
			value := activity.UTC()
			return &value
		}
	}
	value := updatedAt.UTC()
	return &value
}

func accountSourceUpdatedAt(account model.Account) *time.Time {
	if account.SourceFingerprint != "" {
		return cloneTime(account.SourceUpdatedAt)
	}
	return accountSourceCandidate(account)
}

func accountSourceCandidate(account model.Account) *time.Time {
	return effectiveSourceUpdatedAt(account.UpdatedAt, account.LastActivityAt)
}

// accountChannelErrorResolvedAt returns a monotonic evidence watermark. A
// source update is accepted only when it is clearly later than the channel
// error; the small grace window covers the gateway's status bookkeeping lag.
func accountChannelErrorResolvedAt(account model.Account) *time.Time {
	var latest *time.Time
	add := func(value *time.Time) {
		if value == nil {
			return
		}
		candidate := value.UTC()
		if latest == nil || candidate.After(*latest) {
			latest = &candidate
		}
	}

	add(account.LastActivityAt)
	if account.LastProbeAt != nil &&
		(account.LastProbeStatus == model.StatusOperational || account.LastProbeStatus == model.StatusDegraded) {
		add(account.LastProbeAt)
	}
	if source := accountSourceUpdatedAt(account); source != nil && sourceUpdateResolvesChannelError(account) {
		add(source)
	}
	return latest
}

func sourceUpdateResolvesChannelError(account model.Account) bool {
	source := accountSourceUpdatedAt(account)
	if source == nil || account.LastChannelErrorAt == nil ||
		!source.After(*account.LastChannelErrorAt) {
		return true
	}
	return source.Sub(*account.LastChannelErrorAt) > model.SourceUpdateActivityGrace
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
