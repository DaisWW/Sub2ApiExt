package store

import "context"

func (s *Store) EnsureSchema(ctx context.Context) error {
	const schema = `
CREATE TABLE IF NOT EXISTS monitoring_targets (
    target_key TEXT PRIMARY KEY,
    kind TEXT NOT NULL,
    entity_id BIGINT NOT NULL,
    name TEXT NOT NULL,
    platform TEXT NOT NULL DEFAULT '',
    source_status TEXT NOT NULL DEFAULT '',
    probe_enabled BOOLEAN NOT NULL DEFAULT TRUE,
	active BOOLEAN NOT NULL DEFAULT TRUE,
	last_activity_at TIMESTAMPTZ,
	last_channel_error_at TIMESTAMPTZ,
	last_channel_error_class TEXT NOT NULL DEFAULT '',
	last_channel_error_status_code INTEGER,
	last_channel_error_resolved_at TIMESTAMPTZ,
	source_updated_at TIMESTAMPTZ,
	updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
ALTER TABLE monitoring_targets
    ADD COLUMN IF NOT EXISTS last_activity_at TIMESTAMPTZ;
ALTER TABLE monitoring_targets
    ADD COLUMN IF NOT EXISTS last_channel_error_at TIMESTAMPTZ;
ALTER TABLE monitoring_targets
    ADD COLUMN IF NOT EXISTS last_channel_error_class TEXT NOT NULL DEFAULT '';
ALTER TABLE monitoring_targets
    ADD COLUMN IF NOT EXISTS last_channel_error_status_code INTEGER;
ALTER TABLE monitoring_targets
    ADD COLUMN IF NOT EXISTS last_channel_error_resolved_at TIMESTAMPTZ;
ALTER TABLE monitoring_targets
    ADD COLUMN IF NOT EXISTS source_updated_at TIMESTAMPTZ;
CREATE TABLE IF NOT EXISTS monitoring_checks (
    id BIGSERIAL PRIMARY KEY,
    target_key TEXT NOT NULL,
    kind TEXT NOT NULL,
    entity_id BIGINT NOT NULL,
    group_id BIGINT,
    status TEXT NOT NULL,
    latency_ms INTEGER,
    first_byte_ms INTEGER,
    status_code INTEGER,
    error_class TEXT NOT NULL DEFAULT '',
    message TEXT NOT NULL DEFAULT '',
	source TEXT NOT NULL DEFAULT 'probe',
    checked_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
ALTER TABLE monitoring_checks
    ADD COLUMN IF NOT EXISTS source TEXT NOT NULL DEFAULT 'probe';
CREATE INDEX IF NOT EXISTS monitoring_checks_target_time_idx
    ON monitoring_checks (target_key, checked_at DESC);
CREATE INDEX IF NOT EXISTS monitoring_checks_time_idx
    ON monitoring_checks (checked_at);
CREATE TABLE IF NOT EXISTS monitoring_alert_states (
    target_key TEXT PRIMARY KEY,
    observed_status TEXT NOT NULL,
    failure_streak INTEGER NOT NULL DEFAULT 0,
    recovery_streak INTEGER NOT NULL DEFAULT 0,
    alerted_status TEXT NOT NULL DEFAULT '',
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE TABLE IF NOT EXISTS monitoring_alerts (
    id BIGSERIAL PRIMARY KEY,
    target_key TEXT NOT NULL,
    target_name TEXT NOT NULL,
    kind TEXT NOT NULL,
    status TEXT NOT NULL,
    title TEXT NOT NULL,
    message TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS monitoring_alerts_created_idx
    ON monitoring_alerts (created_at DESC);
`
	_, err := s.db.ExecContext(ctx, schema)
	return err
}
