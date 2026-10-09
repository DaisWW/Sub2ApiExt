package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

func newDiscoveryTestDB(t *testing.T) (*sql.DB, context.Context) {
	t.Helper()
	dsn := os.Getenv("RATE_SYNC_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set RATE_SYNC_TEST_DATABASE_URL to run the isolated PostgreSQL fixture")
	}
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	db.SetMaxOpenConns(1)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	// Temporary tables isolate discovery and usage queries from gateway data.
	_, err = db.ExecContext(ctx, `
CREATE TEMP TABLE accounts (
 id bigint, name text, rate_multiplier numeric, type text, status text DEFAULT 'active',
 schedulable boolean DEFAULT true, deleted_at timestamptz, credentials jsonb, proxy_id bigint
);
CREATE TEMP TABLE groups (
 id bigint, name text, rate_multiplier numeric DEFAULT 0.1, status text DEFAULT 'active',
 deleted_at timestamptz, daily_limit_usd numeric, weekly_limit_usd numeric, monthly_limit_usd numeric
);
CREATE TEMP TABLE account_groups (account_id bigint, group_id bigint);
CREATE TEMP TABLE channels (id bigint, status text);
CREATE TEMP TABLE channel_groups (channel_id bigint, group_id bigint);
CREATE TEMP TABLE proxies (
 id bigint, protocol text, host text, port integer, username text, password text,
 deleted_at timestamptz, status text DEFAULT 'active', expires_at timestamptz
);
CREATE TEMP TABLE settings (key text, value text);
CREATE TEMP TABLE usage_logs (
 id bigserial, account_id bigint, group_id bigint, total_cost numeric, actual_cost numeric,
 account_stats_cost numeric, account_rate_multiplier numeric,
 created_at timestamptz DEFAULT NOW() - INTERVAL '10 minutes'
);
INSERT INTO settings VALUES ('admin_api_key', 'admin-test');
INSERT INTO proxies (id, protocol, host, port)
VALUES (1, 'socks5', 'oauth-proxy.invalid', 1080), (2, 'http', 'api-proxy.invalid', 8080);
INSERT INTO accounts (id, name, rate_multiplier, type, credentials, proxy_id)
VALUES (1, ' oauth-one ', 0.1, 'oauth', '{"base_url":"https://oauth.invalid","api_key":"oauth-placeholder"}', 1),
       (2, 'oauth-two', 0.2, 'oauth', NULL, NULL),
       (3, 'oauth-single', 0.2, 'oauth', '{}', NULL),
       (4, 'api-key', 0.3, 'apikey', '{"base_url":"https://api.invalid","api_key":"api-placeholder"}', 2),
       (5, 'api-no-key', 0.3, 'apikey', '{"base_url":"https://api.invalid"}', NULL),
       (6, 'api-blank-url', 0.3, 'apikey', '{"base_url":"  ","api_key":"api-placeholder"}', NULL),
       (7, 'inactive-account', 0.2, 'oauth', '{}', NULL),
       (8, 'unschedulable-account', 0.2, 'oauth', '{}', NULL),
       (9, 'deleted-account', 0.2, 'oauth', '{}', NULL);
UPDATE accounts SET status = 'inactive' WHERE id = 7;
UPDATE accounts SET schedulable = false WHERE id = 8;
UPDATE accounts SET deleted_at = NOW() WHERE id = 9;
INSERT INTO groups (id, name)
VALUES (50, ' oauth-multiple '), (51, 'oauth-single'), (52, 'mixed'), (53, 'missing-credentials'),
       (54, 'deleted-group'), (55, 'inactive-group'), (56, 'inactive-channel');
UPDATE groups SET daily_limit_usd = 0, weekly_limit_usd = 12.34 WHERE id = 51;
UPDATE groups SET rate_multiplier = 0.25 WHERE id = 52;
UPDATE groups SET deleted_at = NOW() WHERE id = 54;
UPDATE groups SET status = 'inactive' WHERE id = 55;
INSERT INTO channels VALUES (1, 'active'), (2, 'inactive');
INSERT INTO channel_groups VALUES (1, 50), (1, 51), (1, 52), (1, 53), (1, 54), (1, 55), (2, 56);
INSERT INTO account_groups VALUES (1, 50), (2, 50), (7, 50), (8, 50), (9, 50),
 (3, 51), (1, 52), (4, 52), (5, 53), (6, 53), (4, 54), (4, 55), (4, 56);
`)
	if err != nil {
		t.Fatal(err)
	}
	return db, ctx
}

func TestPostgresDiscoverySeparatesGroupAndAccountModes(t *testing.T) {
	db, ctx := newDiscoveryTestDB(t)
	for _, target := range []string{"group", "account"} {
		t.Run(target, func(t *testing.T) {
			source := NewPostgresChannelSource(db, target)
			channels, err := source.List(ctx)
			if err != nil {
				t.Fatal(err)
			}
			got := make(map[int64][]int64)
			for _, channel := range channels {
				got[channel.Group.ID] = append(got[channel.Group.ID], channel.AccountID)
				if target == "group" && (channel.APIKey != "" || channel.ProxyURL != "") {
					t.Fatalf("group discovery loaded upstream authentication or proxy for account %d", channel.AccountID)
				}
				if channel.AccountID == 2 && channel.BaseURL != "" {
					t.Fatal("missing OAuth base URL did not remain empty")
				}
				if channel.AccountID == 1 && (channel.BaseURL != "https://oauth.invalid" || channel.AccountName != "oauth-one") {
					t.Fatal("group discovery lost the migration address or trimmed account name")
				}
				if channel.Group.ID == 51 && (channel.Group.DailyLimitUSD == nil || *channel.Group.DailyLimitUSD != 0 ||
					channel.Group.WeeklyLimitUSD == nil || *channel.Group.WeeklyLimitUSD != 12.34 || channel.Group.MonthlyLimitUSD != nil) {
					t.Fatal("nullable group limits changed")
				}
			}
			want := map[int64][]int64{52: {4}}
			if target == "group" {
				want = map[int64][]int64{50: {1, 2}, 51: {3}, 52: {1, 4}, 53: {5, 6}}
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("discovered bindings = %v, want %v", got, want)
			}
			if target == "account" && (channels[0].BaseURL != "https://api.invalid" || channels[0].APIKey == "" ||
				channels[0].ProxyURL != "http://api-proxy.invalid:8080") {
				t.Fatal("account discovery lost upstream credentials or bound proxy")
			}
		})
	}
}

func TestPostgresDiscoveryUsesProxyOnlyForAccountMode(t *testing.T) {
	db, ctx := newDiscoveryTestDB(t)
	if _, err := db.ExecContext(ctx, `UPDATE proxies SET protocol = 'socks5' WHERE id = 2`); err != nil {
		t.Fatal(err)
	}
	if _, err := NewPostgresChannelSource(db, "group").List(ctx); err != nil {
		t.Fatalf("group discovery parsed an unused proxy: %v", err)
	}
	if _, err := NewPostgresChannelSource(db, "account").List(ctx); err == nil || !strings.Contains(err.Error(), "暂不支持代理协议") {
		t.Fatalf("account discovery did not reject the unsupported bound proxy: %v", err)
	}
}

func TestPostgresGroupSyncOAuthAndMixedAccounts(t *testing.T) {
	db, ctx := newDiscoveryTestDB(t)
	_, err := db.ExecContext(ctx, `
INSERT INTO usage_logs (account_id, group_id, total_cost, account_stats_cost, account_rate_multiplier, actual_cost)
SELECT a.account_id, a.group_id, a.standard, a.base, a.rate, a.standard * 0.1
FROM (VALUES (1, 50, 1, 1, 0.1), (2, 50, 4, 3, 0.2),
             (1, 52, 1, 1, 0.1), (4, 52, 4, 4, 0.3)) a(account_id, group_id, standard, base, rate)
CROSS JOIN generate_series(1, 20);
`)
	if err != nil {
		t.Fatal(err)
	}
	updates := make(map[int64]groupUpdate)
	admin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.ParseInt(strings.TrimPrefix(r.URL.Path, "/api/v1/admin/groups/"), 10, 64)
		if r.Method != http.MethodPut || err != nil {
			t.Errorf("unexpected HTTP request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		var update groupUpdate
		if err := json.NewDecoder(r.Body).Decode(&update); err != nil {
			t.Errorf("decode group update: %v", err)
		}
		updates[id] = update
		writeJSON(t, w, map[string]any{"code": 0})
	}))
	defer admin.Close()
	syncer := newTestSyncer(t, NewPostgresChannelSource(db, "group"), admin.URL, false, "", 1)
	if err := syncer.RunOnce(ctx, time.Now()); err != nil {
		t.Fatal(err)
	}
	if len(updates) != 3 {
		t.Fatalf("updated groups = %d, want 3", len(updates))
	}
	for groupID, want := range map[int64]float64{50: 0.12, 51: 0.2, 52: 0.26} {
		if got := updates[groupID].RateMultiplier; !almostEqual(got, want) {
			t.Fatalf("group %d multiplier = %.4f, want %.4f", groupID, got, want)
		}
	}
	if update := updates[51]; update.DailyLimitUSD != 0 || update.WeeklyLimitUSD != 12.34 || update.MonthlyLimitUSD != -1 {
		t.Fatalf("group limits changed: %+v", update)
	}
	persisted, err := syncer.store.Load()
	if err != nil {
		t.Fatal(err)
	}
	for groupID, want := range map[int64]float64{50: 0.14, 52: 0.26} {
		state := persisted.DynamicGroups[groupID]
		fast, slow, predicted, ok := dynamicRates(state)
		if !ok || !almostEqual(fast, want) || !almostEqual(slow, want) || !almostEqual(predicted, want) {
			t.Fatalf("group %d cost estimates = %.4f/%.4f/%.4f, valid=%t", groupID, fast, slow, predicted, ok)
		}
		if state.LastUsageID != 80 || state.HasPendingTarget {
			t.Fatalf("group %d did not persist its completed usage watermark", groupID)
		}
	}
	if len(syncer.state.Rules) != 0 {
		t.Fatal("group mode entered upstream price discovery")
	}
}

func TestPostgresAccountSyncOnlyPricesAPIKeyAccounts(t *testing.T) {
	db, ctx := newDiscoveryTestDB(t)
	upstreamCalls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/user/balance" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		upstreamCalls++
		if r.URL.Path != "/v1/sub2api/billing" {
			t.Errorf("unexpected upstream request: %s", r.URL.Path)
		}
		writeJSON(t, w, map[string]any{"resolved_rate_multiplier": 0.4})
	}))
	defer upstream.Close()
	if _, err := db.ExecContext(ctx, `UPDATE accounts SET proxy_id = NULL,
 credentials = jsonb_build_object('base_url', $1::text, 'api_key', 'api-placeholder') WHERE id = 4`, upstream.URL); err != nil {
		t.Fatal(err)
	}
	updates := 0
	admin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		updates++
		if r.Method != http.MethodPut || r.URL.Path != "/api/v1/admin/accounts/4" {
			t.Errorf("unexpected admin request: %s %s", r.Method, r.URL.Path)
		}
		var update accountUpdate
		if err := json.NewDecoder(r.Body).Decode(&update); err != nil {
			t.Errorf("decode account update: %v", err)
		}
		if !almostEqual(update.RateMultiplier, 0.4) {
			t.Errorf("account multiplier = %.4f, want 0.4", update.RateMultiplier)
		}
		writeJSON(t, w, map[string]any{"code": 0})
	}))
	defer admin.Close()
	syncer := newAccountTestSyncer(t, NewPostgresChannelSource(db, "account"), admin.URL, false, "", 1)
	if err := syncer.RunOnce(ctx, time.Now()); err != nil {
		t.Fatal(err)
	}
	if upstreamCalls != 1 || updates != 1 {
		t.Fatalf("upstream checks=%d account updates=%d, want 1 each", upstreamCalls, updates)
	}
	if len(syncer.state.Rules) != 1 || syncer.state.Rules["account:4"] == nil {
		t.Fatal("account mode processed accounts outside API Key discovery")
	}
}
