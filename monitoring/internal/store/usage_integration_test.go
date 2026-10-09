package store

import (
	"context"
	"database/sql"
	"math"
	"os"
	"testing"
	"time"

	"github.com/DaisWW/Sub2ApiExt/monitoring/internal/model"
	_ "github.com/lib/pq"
)

func TestUsageCostBasisPostgres(t *testing.T) {
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
	// Temporary tables shadow gateway tables and disappear when this connection closes.
	exec(`
CREATE TEMP TABLE accounts (id bigint, name text, platform text, priority integer, rate_multiplier numeric);
CREATE TEMP TABLE groups (id bigint, name text, platform text, rate_multiplier numeric);
CREATE TEMP TABLE channels (id bigint, name text);
CREATE TEMP TABLE usage_logs (
 account_id bigint, group_id bigint, channel_id bigint, model text DEFAULT 'model',
 input_tokens bigint DEFAULT 0, output_tokens bigint DEFAULT 0,
 cache_creation_tokens bigint DEFAULT 0, cache_read_tokens bigint DEFAULT 0,
 input_cost numeric DEFAULT 0, output_cost numeric DEFAULT 0,
 cache_creation_cost numeric DEFAULT 0, cache_read_cost numeric DEFAULT 0,
 total_cost numeric, actual_cost numeric, account_stats_cost numeric, account_rate_multiplier numeric,
 created_at timestamptz DEFAULT NOW() - INTERVAL '1 minute'
);
INSERT INTO accounts VALUES (1, 'account-one', 'openai', 1, 0.9), (2, 'account-two', 'openai', 2, 0.9);
INSERT INTO groups VALUES (10, 'group-ten', 'openai', 0.8), (20, 'group-twenty', 'openai', 0.8);
INSERT INTO usage_logs (account_id, group_id, input_tokens, input_cost, total_cost, actual_cost, account_stats_cost, account_rate_multiplier)
VALUES (1, 10, 1000000, 100, 100, 10, 100, 0.2);
`)
	load := func(limit int) model.UsageRanking {
		t.Helper()
		ranking, err := New(db).UsageRanking(ctx, "24h", limit)
		if err != nil {
			t.Fatal(err)
		}
		return ranking
	}
	assertAmount := func(label string, got, want float64) {
		t.Helper()
		if math.Abs(got-want) > 1e-9 {
			t.Fatalf("%s = %v, want %v", label, got, want)
		}
	}
	assertCosts := func(item model.UsageRankItem, base, actual, multiplier, unitCost float64) {
		t.Helper()
		assertAmount(item.Key+" base", item.BaseCost, base)
		assertAmount(item.Key+" actual", item.TotalCost, actual)
		assertAmount(item.Key+" multiplier", item.EffectiveRateMultiplier, multiplier)
		assertAmount(item.Key+" per million tokens", item.CostPerMillionTokens, unitCost)
	}

	ranking := load(10)
	if len(ranking.Accounts) != 1 || len(ranking.Groups) != 1 || len(ranking.Models) != 1 {
		t.Fatalf("unexpected ranking dimensions: %+v", ranking)
	}
	assertCosts(ranking.Accounts[0], 100, 20, 0.2, 20)
	assertCosts(ranking.Groups[0], 100, 10, 0.1, 10)
	assertCosts(ranking.Models[0], 100, 10, 0.1, 10)
	assertAmount("summary user charge", ranking.Summary.TotalCost, 10)
	var timelineCost float64
	for _, bucket := range ranking.Timeline {
		timelineCost += bucket.TotalCost
	}
	assertAmount("timeline user charge", timelineCost, 10)
	assertAmount("account cost share", ranking.Accounts[0].CostSharePercent, 100)
	assertAmount("group charge share", ranking.Groups[0].CostSharePercent, 100)
	assertAmount("model charge share", ranking.Models[0].CostSharePercent, 100)

	// Historical rates and account-specific base costs override current settings.
	// Missing legacy fields fall back to standard cost and 1; explicit zeroes stay zero.
	exec(`
INSERT INTO usage_logs (account_id, group_id, input_tokens, input_cost, total_cost, actual_cost, account_stats_cost, account_rate_multiplier)
VALUES (1, 20, 1000000, 100, 100, 30, 80, 0.4),
       (2, 10, 1000000, 50, 50, 5, NULL, NULL),
       (2, 10, 1000000, 10, 10, 1, 0, 0.2),
       (2, 10, 1000000, 10, 10, 1, 10, 0),
       (1, 10, 1000000, 100, 100, 0, 100, 0.9);
`)
	ranking = load(10)
	if ranking.Summary.Requests != 5 || len(ranking.Accounts) != 2 || len(ranking.Groups) != 2 {
		t.Fatalf("failed placeholder counted or dimension missing: %+v", ranking)
	}
	for _, item := range ranking.Accounts {
		if item.Key == "account:1" {
			assertCosts(item, 200, 52, 0.26, 26)
			assertAmount("account-one share", item.CostSharePercent, 52.0/102*100)
		} else {
			assertCosts(item, 70, 50, 50.0/70, 50.0/3)
			assertAmount("account-two share", item.CostSharePercent, 50.0/102*100)
		}
	}
	for _, item := range ranking.Groups {
		assertAmount(item.Key+" group charge share", item.CostSharePercent, item.TotalCost/47*100)
		if item.Key == "group:10" {
			assertCosts(item, 170, 17, 0.1, 17.0/4)
		} else {
			assertCosts(item, 100, 30, 0.3, 30)
		}
	}
	assertAmount("summary still user charge", ranking.Summary.TotalCost, 47)
	assertAmount("models still user charge", ranking.Models[0].TotalCost, 47)
	assertAmount("model charge share after account cost changes", ranking.Models[0].CostSharePercent, 100)

	// The account share denominator includes accounts omitted by the rank limit.
	exec(`
TRUNCATE usage_logs;
INSERT INTO accounts VALUES (3, 'account-three', 'openai', 3, 0.9);
INSERT INTO usage_logs (account_id, group_id, input_tokens, input_cost, total_cost, actual_cost, account_stats_cost, account_rate_multiplier)
VALUES (1, 10, 1000000, 100, 100, 10, 100, 0.2),
       (2, 10, 500000, 50, 50, 5, 50, 0.3),
       (3, 10, 100000, 10, 10, 1, 10, 0.4);
`)
	ranking = load(1)
	meta := ranking.DimensionMeta.Accounts
	if len(ranking.Accounts) != 2 || meta.TotalItems != 3 || meta.OmittedItems != 1 || meta.OmittedRequests != 1 {
		t.Fatalf("unexpected limited account ranks: items=%+v meta=%+v", ranking.Accounts, meta)
	}
	assertAmount("omitted account cost", meta.OmittedCost, 15)
	for _, item := range ranking.Accounts {
		assertAmount(item.Key+" limited cost share", item.CostSharePercent, item.TotalCost/39*100)
	}
}
