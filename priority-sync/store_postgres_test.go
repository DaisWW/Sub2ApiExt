package main

import (
	"context"
	"database/sql"
	"math"
	"os"
	"testing"
	"time"
)

// This optional integration check uses session-local temporary tables only.
// It exercises deduplication, mixed historical rates and window attachment.
func TestPriorityMetricsPostgres(t *testing.T) {
	dsn := os.Getenv("PRIORITY_SYNC_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("PRIORITY_SYNC_TEST_DATABASE_URL is not set")
	}
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// Pin all reads to the temporary schema, never to application tables.
	if _, err := db.ExecContext(ctx, `SET search_path TO pg_temp;
CREATE TEMP TABLE accounts (id bigint, name text, platform text, type text, status text,
    priority integer, rate_multiplier numeric, rate_limit_reset_at timestamptz,
    temp_unschedulable_until timestamptz, overload_until timestamptz,
    deleted_at timestamptz, schedulable boolean);
CREATE TEMP TABLE groups (id bigint, name text, deleted_at timestamptz);
CREATE TEMP TABLE account_groups (account_id bigint, group_id bigint, priority integer);
CREATE TEMP TABLE usage_logs (id bigint, account_id bigint, created_at timestamptz,
    request_id text, client_request_id text, model text, requested_model text, upstream_model text,
    upstream_response_model text, upstream_endpoint text, group_id bigint, long_context_billing_applied boolean,
    input_tokens bigint, output_tokens bigint, cache_creation_tokens bigint, cache_read_tokens bigint,
    account_stats_cost numeric, total_cost numeric, actual_cost numeric,
    account_rate_multiplier numeric, rate_multiplier numeric,
    input_cost numeric, output_cost numeric, cache_creation_cost numeric, cache_read_cost numeric,
    duration_ms integer, first_token_ms integer);
CREATE TEMP TABLE ops_error_logs (id bigint, account_id bigint, created_at timestamptz,
    request_id text, client_request_id text, status_code integer, upstream_status_code integer,
    error_type text, retry_after_seconds integer, is_business_limited boolean,
    error_owner text, error_source text, error_phase text);
INSERT INTO accounts (id, name, platform, type, status, priority, rate_multiplier, schedulable)
    VALUES (1, 'recover', 'openai', 'apikey', 'active', 1000, 0.1, true),
           (2, 'peer', 'openai', 'apikey', 'active', 10, 1, true),
           (3, 'peer', 'openai', 'apikey', 'active', 90, 2, true);
INSERT INTO groups VALUES (30, 'test group', NULL);
INSERT INTO account_groups VALUES (1, 30, 1), (2, 30, 1), (3, 30, 1);`); err != nil {
		t.Fatal(err)
	}
	now := nowForTest()
	for _, query := range []string{`
INSERT INTO usage_logs (id, account_id, created_at, request_id, client_request_id,
    model, requested_model, upstream_model, upstream_endpoint, group_id,
    input_tokens, output_tokens, cache_creation_tokens, cache_read_tokens,
    account_stats_cost, total_cost, actual_cost, account_rate_multiplier,
    input_cost, output_cost, cache_creation_cost, cache_read_cost)
SELECT account_id*100+i, account_id, $1::timestamptz-i*INTERVAL '1 minute',
    account_id||':'||i, 'client:'||account_id||':'||i,
    'gpt-6.1-sol', 'gpt-6.1-sol', 'gpt-6.1-sol', 'responses', 30,
    9000, 10000, 0, 81000, 0.371, 0.371, 1, CASE WHEN i%2=0 THEN 2 ELSE 4 END,
    0.09, 0.2, 0, 0.081
FROM generate_series(2,3) AS account_id CROSS JOIN generate_series(1,40) AS i;`, `
INSERT INTO usage_logs (id, account_id, created_at, request_id, model, requested_model, upstream_model,
    upstream_endpoint, group_id, input_tokens, output_tokens, cache_read_tokens,
    total_cost, account_rate_multiplier, output_cost)
VALUES (1, 1, $1::timestamptz-INTERVAL '3 days', 'old', 'gpt-6.1-sol', 'gpt-6.1-sol', 'gpt-6.1-sol',
        'responses', 30, 9000, 10000, 81000, 0.371, 4, 0.2),
       (9000, 2, $1::timestamptz-INTERVAL '90 minutes', '2:1', 'gpt-6.1-sol', 'gpt-6.1-sol', 'gpt-6.1-sol',
        'responses', 30, 9000, 10000, 81000, 1000, 4, 0.2),
       (9001, 2, $1::timestamptz-INTERVAL '30 seconds', 'unpriced', 'gpt-6.1-sol', 'gpt-6.1-sol', 'gpt-6.1-sol',
        'responses', 30, 1000000, 0, 0, 0, 4, 0);`, `
INSERT INTO ops_error_logs (id, account_id, created_at, request_id, error_owner, upstream_status_code)
VALUES (1, 1, $1::timestamptz-INTERVAL '5 minutes', 'new-failure', 'provider', 503),
       (2, 2, $1::timestamptz-INTERVAL '2 minutes', '2:1', 'provider', 429);`} {
		if _, err := db.ExecContext(ctx, query, now); err != nil {
			t.Fatal(err)
		}
	}
	store := NewMetricsStore(db)
	accounts, err := store.LoadAccountMetricsWindows(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(accounts) != 3 || accounts[0].GroupPriorities[30] != 1 || len(accounts[0].Pools) != 1 || accounts[0].Pools[0].Window7d == nil {
		t.Fatalf("group or seven-day model history was not loaded: %+v", accounts)
	}
	peer := accounts[1]
	if peer.Window2h.PricedRequests != 40 || peer.Window2h.PricedTokens != 4_000_000 || peer.Window2h.SuccessfulRequests != 41 || peer.Window2h.TotalTokens != 5_000_000 {
		t.Fatalf("deduplication or unpriced denominators failed: %+v", peer.Window2h)
	}
	pool := peer.Pools[0]
	if pool.Window30m == nil || pool.Window2h == nil || pool.Window2h.PricedRequests != 40 || pool.Window2h.ContinuousPricedRequests != 39 ||
		math.Abs(pool.Window2h.BaseCost-14.84) > 1e-9 || math.Abs(pool.Window2h.BaseOutputCost-8) > 1e-9 ||
		pool.Window2h.LastPricedSuccessAt == nil || !pool.Window2h.LastPricedSuccessAt.Equal(now.Add(-time.Minute)) {
		t.Fatalf("historical rates or continuous evidence were aggregated incorrectly: %+v", pool)
	}
	if peer.LastPricedSuccessAt == nil || !peer.LastPricedSuccessAt.Equal(now.Add(-time.Minute)) || peer.LastTerminalFailureAt != nil || peer.RecoveredRateLimited != 1 ||
		accounts[0].LastTerminalFailureAt == nil || !accounts[0].LastTerminalFailureAt.Equal(now.Add(-5*time.Minute)) {
		t.Fatalf("new result timestamps confused unpriced/recovered requests: peer=%+v candidate=%+v", peer, accounts[0])
	}
	candidate := testRecommendationByID(t, scoreAccounts(accounts, now, 5), 1)
	if !candidate.recoveryNeeded || candidate.AnchorPriority != priorityBest || math.Abs(candidate.CostPerMillionTokens-0.371) > 1e-9 {
		t.Fatalf("store evidence did not produce the expected recovery: %+v", candidate)
	}
	if _, err := db.ExecContext(ctx, `UPDATE usage_logs SET output_cost = NULL WHERE id = 201`); err != nil {
		t.Fatal(err)
	}
	partial, err := store.LoadAccountMetrics(ctx, now, decisionWindow)
	if err != nil {
		t.Fatal(err)
	}
	if partial[1].Pools[0].HasOutputCost {
		t.Fatal("missing output cost was treated as a known free output price")
	}
	// Removing optional group/output columns must retain a valid safe query.
	if _, err := db.ExecContext(ctx, `ALTER TABLE usage_logs DROP COLUMN group_id; ALTER TABLE usage_logs DROP COLUMN output_cost;`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.LoadAccountMetricsWindows(ctx, now); err != nil {
		t.Fatalf("optional schema compatibility: %v", err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE groups SET deleted_at = CURRENT_TIMESTAMP WHERE id = 30`); err != nil {
		t.Fatal(err)
	}
	removedGroup, err := store.LoadAccountMetrics(ctx, now, decisionWindow)
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := removedGroup[0].GroupPriorities[30]; exists {
		t.Fatal("deleted group remained eligible for a recovery prior")
	}
}
