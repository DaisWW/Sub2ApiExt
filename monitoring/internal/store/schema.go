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
	last_observed_activity_at TIMESTAMPTZ,
	last_observed_channel_error_at TIMESTAMPTZ,
	source_fingerprint TEXT NOT NULL DEFAULT '',
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
    ADD COLUMN IF NOT EXISTS last_observed_activity_at TIMESTAMPTZ;
ALTER TABLE monitoring_targets
    ADD COLUMN IF NOT EXISTS last_observed_channel_error_at TIMESTAMPTZ;
ALTER TABLE monitoring_targets
    ADD COLUMN IF NOT EXISTS source_fingerprint TEXT NOT NULL DEFAULT '';
ALTER TABLE monitoring_targets
    ADD COLUMN IF NOT EXISTS source_updated_at TIMESTAMPTZ;
-- Rows created before source fingerprints existed carried an upstream
-- updated_at watermark that may have been changed by billing-only writes.
-- Rebaseline those rows once; the next cycle stores the meaningful identity.
UPDATE monitoring_targets
SET source_updated_at = NULL
WHERE source_fingerprint = '';
CREATE TABLE IF NOT EXISTS monitoring_checks (
    id BIGSERIAL PRIMARY KEY,
    target_key TEXT NOT NULL,
    kind TEXT NOT NULL,
    entity_id BIGINT NOT NULL,
    group_id BIGINT,
    status TEXT NOT NULL,
    health_reason TEXT NOT NULL DEFAULT '',
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
ALTER TABLE monitoring_checks
    ADD COLUMN IF NOT EXISTS health_reason TEXT NOT NULL DEFAULT '';
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
CREATE TABLE IF NOT EXISTS monitoring_cost_alert_states (
    alert_key TEXT PRIMARY KEY,
    last_alerted_at TIMESTAMPTZ,
    active BOOLEAN NOT NULL DEFAULT FALSE,
    first_seen_at TIMESTAMPTZ,
    last_seen_at TIMESTAMPTZ,
    normal_since_at TIMESTAMPTZ,
    last_severity TEXT NOT NULL DEFAULT '',
    last_event JSONB NOT NULL DEFAULT '{}'::jsonb,
    pending_recovery BOOLEAN NOT NULL DEFAULT FALSE,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
ALTER TABLE monitoring_cost_alert_states
    ADD COLUMN IF NOT EXISTS active BOOLEAN NOT NULL DEFAULT FALSE;
ALTER TABLE monitoring_cost_alert_states
    ADD COLUMN IF NOT EXISTS first_seen_at TIMESTAMPTZ;
ALTER TABLE monitoring_cost_alert_states
    ADD COLUMN IF NOT EXISTS last_seen_at TIMESTAMPTZ;
ALTER TABLE monitoring_cost_alert_states
    ADD COLUMN IF NOT EXISTS normal_since_at TIMESTAMPTZ;
ALTER TABLE monitoring_cost_alert_states
    ADD COLUMN IF NOT EXISTS last_severity TEXT NOT NULL DEFAULT '';
ALTER TABLE monitoring_cost_alert_states
    ADD COLUMN IF NOT EXISTS last_event JSONB NOT NULL DEFAULT '{}'::jsonb;
ALTER TABLE monitoring_cost_alert_states
    ADD COLUMN IF NOT EXISTS pending_recovery BOOLEAN NOT NULL DEFAULT FALSE;
CREATE INDEX IF NOT EXISTS monitoring_cost_alert_states_active_idx
    ON monitoring_cost_alert_states (alert_key)
    WHERE active = TRUE;
CREATE TABLE IF NOT EXISTS monitoring_cost_alerts (
    id BIGSERIAL PRIMARY KEY,
    alert_key TEXT NOT NULL,
    kind TEXT NOT NULL,
    notification_type TEXT NOT NULL DEFAULT 'start',
    severity TEXT NOT NULL,
    target_key TEXT NOT NULL,
    title TEXT NOT NULL,
    message TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
ALTER TABLE monitoring_cost_alerts
    ADD COLUMN IF NOT EXISTS notification_type TEXT NOT NULL DEFAULT 'start';
CREATE INDEX IF NOT EXISTS monitoring_cost_alerts_created_idx
    ON monitoring_cost_alerts (created_at DESC);
CREATE INDEX IF NOT EXISTS monitoring_cost_alerts_key_created_idx
    ON monitoring_cost_alerts (alert_key, created_at DESC);
`
	_, err := s.db.ExecContext(ctx, schema)
	return err
}
