package store

import (
	"context"
	"database/sql"
	"os"
	"testing"
	"time"

	"github.com/DaisWW/Sub2ApiExt/monitoring/internal/model"
	_ "github.com/lib/pq"
)

// Only temporary tables are written; the fixture connection drops them on close.
func TestDashboardRequestWindowsPostgres(t *testing.T) {
	dsn := os.Getenv("MONITORING_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set MONITORING_TEST_DATABASE_URL to run the isolated PostgreSQL fixture")
	}
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	exec := func(query string) {
		t.Helper()
		if _, err := db.ExecContext(ctx, query); err != nil {
			t.Fatal(err)
		}
	}
	exec(`
CREATE TEMP TABLE accounts (id bigint, name text, status text, schedulable boolean, deleted_at timestamptz, priority integer, rate_multiplier numeric);
CREATE TEMP TABLE groups (id bigint, name text, status text, deleted_at timestamptz, rate_multiplier numeric);
CREATE TEMP TABLE account_groups (account_id bigint, group_id bigint);
CREATE TEMP TABLE channels (id bigint, status text);
CREATE TEMP TABLE channel_groups (channel_id bigint, group_id bigint);
CREATE TEMP TABLE monitoring_targets (
 target_key text, kind text, entity_id bigint, name text, platform text, source_status text,
 probe_enabled boolean DEFAULT true, active boolean DEFAULT true, source_updated_at timestamptz,
 last_activity_at timestamptz, last_channel_error_at timestamptz, last_channel_error_resolved_at timestamptz,
 last_channel_error_class text, last_channel_error_status_code integer
);
CREATE TEMP TABLE monitoring_checks (
 id bigserial, target_key text, kind text, entity_id bigint, group_id bigint, status text,
 health_reason text DEFAULT '', latency_ms integer, first_byte_ms integer,
 checked_at timestamptz, source text, message text DEFAULT '', status_code integer, error_class text
);
CREATE TEMP TABLE usage_logs (
 id bigserial, account_id bigint, group_id bigint, duration_ms integer, first_token_ms integer,
 created_at timestamptz, actual_cost numeric DEFAULT 1, request_id text
);
CREATE TEMP TABLE ops_error_logs (
 id bigserial, account_id bigint, group_id bigint, client_request_id text, request_id text,
 created_at timestamptz, upstream_status_code integer, status_code integer,
 error_type text, is_business_limited boolean DEFAULT false,
 error_owner text DEFAULT 'provider', error_phase text, error_source text
);
INSERT INTO accounts VALUES (1, 'shared', 'active', true, NULL, 1, 1);
INSERT INTO groups VALUES (10, 'fast', 'active', NULL, 1), (20, 'slow', 'active', NULL, 1);
INSERT INTO account_groups VALUES (1, 10), (1, 20);
INSERT INTO monitoring_targets (target_key, kind, entity_id, name, platform, source_status)
VALUES ('account:1', 'account', 1, 'shared', 'openai', 'active'),
       ('group:10', 'group', 10, 'fast', 'openai', 'active'),
       ('group:20', 'group', 20, 'slow', 'openai', 'active');
INSERT INTO monitoring_checks (target_key, kind, status, health_reason, latency_ms, checked_at, source)
VALUES ('group:10', 'group', 'degraded', 'slow', 90000, NOW() - INTERVAL '10 minutes', 'aggregate'),
       ('group:20', 'group', 'operational', '', 1000, NOW() - INTERVAL '10 minutes', 'aggregate');
INSERT INTO usage_logs (account_id, group_id, duration_ms, first_token_ms, created_at, request_id)
SELECT 1, 10, 90000, 2000, NOW() - n * INTERVAL '10 seconds', 'fast-' || n FROM generate_series(1, 5) n;
INSERT INTO usage_logs (account_id, group_id, duration_ms, first_token_ms, created_at, request_id)
SELECT 1, 20, 40000, 30000, NOW() - n * INTERVAL '10 seconds', 'slow-' || n FROM generate_series(1, 5) n;
`)
	load := func() map[string]model.DashboardTarget {
		t.Helper()
		dashboard, err := New(db).Dashboard(ctx, 2*time.Minute, 60)
		if err != nil {
			t.Fatal(err)
		}
		targets := make(map[string]model.DashboardTarget)
		for _, target := range dashboard.Targets {
			targets[target.Key] = target
		}
		return targets
	}
	targets := load()
	fast, slow := targets["group:10"], targets["group:20"]
	if fast.Status != model.StatusOperational || slow.Status != model.StatusDegraded || slow.HealthReason != model.HealthReasonSlow {
		t.Fatalf("group quality crossed request boundaries: fast=%+v slow=%+v", fast, slow)
	}
	if fast.Stats.Samples != 5 || fast.CurrentHealth.FirstByteSamples != 5 ||
		fast.CurrentHealth.FirstByte.MedianMs == nil || *fast.CurrentHealth.FirstByte.MedianMs != 2000 ||
		fast.Stats.Latency.P95Ms == nil || *fast.Stats.Latency.P95Ms != 90000 {
		t.Fatalf("request metrics/counts are not group-local: %+v", fast)
	}

	// A retry on a different account in the same group must close the failure.
	exec(`INSERT INTO ops_error_logs (account_id, group_id, client_request_id, created_at, status_code)
VALUES (2, 10, 'client:FAST-1', NOW() - INTERVAL '20 seconds', 429);`)
	fast = load()["group:10"]
	if fast.CurrentHealth.Samples != 5 || fast.CurrentHealth.Successful != 5 || fast.CurrentHealth.HardFailures != 0 ||
		fast.CurrentHealth.RateLimited != 1 || fast.CurrentHealth.Attempts != 6 || fast.Stats.Availability != 100 ||
		fast.Status != model.StatusDegraded || fast.HealthReason != model.HealthReasonRateLimited {
		t.Fatalf("recovered retry was counted as user failure: %+v", fast)
	}
	exec(`INSERT INTO ops_error_logs (account_id, group_id, client_request_id, created_at, status_code)
VALUES (1, 10, 'fast-2', NOW() - INTERVAL '6 minutes', 429);`)
	fast = load()["group:10"]
	if fast.CurrentHealth.RateLimited != 1 || fast.CurrentHealth.Attempts != 6 || fast.Stats.RateLimited != 2 || fast.Stats.Attempts != 7 {
		t.Fatalf("retry attempts crossed observation windows: %+v", fast)
	}

	// Missing first-token measurements cannot borrow total duration.
	exec(`UPDATE usage_logs SET first_token_ms = NULL WHERE group_id = 20;`)
	slow = load()["group:20"]
	if slow.Status != model.StatusOperational || slow.CurrentHealth.FirstByteSamples != 0 || slow.CurrentHealth.FirstByte.MedianMs != nil {
		t.Fatalf("missing first byte was replaced with total duration: %+v", slow)
	}
	exec(`INSERT INTO ops_error_logs (account_id, group_id, request_id, created_at, status_code)
VALUES (1, 20, 'rate-only', NOW() - INTERVAL '1 minute', 429);`)
	slow = load()["group:20"]
	if slow.Stats.Samples != 6 || slow.Stats.Successful != 5 || slow.Stats.HardFailures != 0 || slow.Stats.RateLimited != 1 {
		t.Fatalf("rate-limit-only outcome was counted as a hard failure: %+v", slow.Stats)
	}

	// Idle groups retain their own last five-minute window even beyond 24 hours.
	exec(`DELETE FROM ops_error_logs;
UPDATE usage_logs SET created_at = created_at - INTERVAL '25 hours';
UPDATE usage_logs SET first_token_ms = 30000 WHERE group_id = 20;
UPDATE monitoring_targets SET last_activity_at = (SELECT MAX(created_at) FROM usage_logs) WHERE kind = 'account';`)
	targets = load()
	fast, slow = targets["group:10"], targets["group:20"]
	if fast.CurrentHealth.Samples != 0 || fast.LastRequestHealth.Samples != 5 || !fast.LastRequestHealth.Applied || !fast.Stale ||
		fast.Status != model.StatusOperational || slow.Status != model.StatusDegraded || !slow.LastRequestHealth.Applied {
		t.Fatalf("idle request evidence lost or shared: fast=%+v slow=%+v", fast, slow)
	}
	if fast.LastCheckedAt == nil || time.Since(*fast.LastCheckedAt) < 24*time.Hour {
		t.Fatalf("idle evidence time was refreshed: %+v", fast)
	}
	exec(`INSERT INTO ops_error_logs (account_id, group_id, client_request_id, created_at, status_code)
SELECT 1, 10, 'fast-1', MAX(created_at) - INTERVAL '10 seconds', 429 FROM usage_logs WHERE group_id = 10;`)
	fast = load()["group:10"]
	if fast.Status != model.StatusDegraded || fast.HealthReason != model.HealthReasonRateLimited || fast.LastRequestHealth.RateLimited != 1 || !fast.Stale {
		t.Fatalf("idle rate-limit evidence was lost beyond 24 hours: %+v", fast)
	}

	// Newer member failure must win over older successful group traffic.
	exec(`INSERT INTO monitoring_checks (target_key, kind, status, health_reason, checked_at, source)
VALUES ('group:10', 'group', 'failed', 'upstream_error', NOW() - INTERVAL '1 minute', 'aggregate');`)
	fast = load()["group:10"]
	if fast.Status != model.StatusFailed || fast.Available || fast.LastRequestHealth.Applied {
		t.Fatalf("old traffic erased explicit member failure: %+v", fast)
	}
	exec(`UPDATE monitoring_targets SET source_updated_at = NOW() WHERE target_key = 'group:20';`)
	slow = load()["group:20"]
	if slow.LastRequestHealth.Samples != 0 || slow.LastRequestHealth.Applied {
		t.Fatalf("source change did not invalidate old request quality: %+v", slow)
	}
}
