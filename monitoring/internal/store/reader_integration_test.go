package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/DaisWW/Sub2ApiExt/monitoring/internal/access"
	"github.com/redis/go-redis/v9"
)

// Hooks supply Redis responses without opening sockets or writing gateway keys.
type concurrencyFixture struct{}

func (concurrencyFixture) DialHook(next redis.DialHook) redis.DialHook { return next }
func (concurrencyFixture) ProcessHook(_ redis.ProcessHook) redis.ProcessHook {
	return func(_ context.Context, cmd redis.Cmder) error {
		if value, ok := cmd.(*redis.TimeCmd); ok {
			value.SetVal(time.Now())
			return nil
		}
		return fmt.Errorf("unexpected Redis command %s", cmd.Name())
	}
}
func (concurrencyFixture) ProcessPipelineHook(_ redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(_ context.Context, commands []redis.Cmder) error {
		for _, cmd := range commands {
			if value, ok := cmd.(*redis.IntCmd); ok && cmd.Name() == "zcount" {
				value.SetVal(7)
			} else {
				return fmt.Errorf("unexpected Redis command %s", cmd.Name())
			}
		}
		return nil
	}
}

// This fixture writes only connection-local temporary tables in an isolated DB.
func TestReaderPermissionsPostgres(t *testing.T) {
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
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	_, err = db.ExecContext(ctx, `
CREATE TEMP TABLE accounts (id bigint, name text, platform text DEFAULT 'openai', status text DEFAULT 'active', schedulable boolean DEFAULT true, deleted_at timestamptz, priority integer DEFAULT 1, rate_multiplier numeric DEFAULT 1);
CREATE TEMP TABLE groups (id bigint, name text, platform text DEFAULT 'openai', status text DEFAULT 'active', deleted_at timestamptz, rate_multiplier numeric DEFAULT 1, is_exclusive boolean DEFAULT false, subscription_type text DEFAULT 'standard');
CREATE TEMP TABLE account_groups (account_id bigint, group_id bigint);
CREATE TEMP TABLE channels (id bigint, name text, status text DEFAULT 'active');
CREATE TEMP TABLE channel_groups (channel_id bigint, group_id bigint);
CREATE TEMP TABLE monitoring_targets (
 target_key text, kind text, entity_id bigint, name text, platform text DEFAULT 'openai', source_status text DEFAULT 'active',
 probe_enabled boolean DEFAULT true, active boolean DEFAULT true, source_updated_at timestamptz,
 last_activity_at timestamptz DEFAULT NOW(), last_channel_error_at timestamptz, last_channel_error_resolved_at timestamptz,
 last_channel_error_class text, last_channel_error_status_code integer
);
CREATE TEMP TABLE monitoring_checks (
 id bigserial, target_key text, kind text, entity_id bigint, group_id bigint, status text DEFAULT 'operational',
 health_reason text DEFAULT '', latency_ms integer, first_byte_ms integer,
 checked_at timestamptz DEFAULT NOW() - INTERVAL '2 minutes', source text, message text DEFAULT '', status_code integer, error_class text
);
CREATE TEMP TABLE ops_error_logs (
 id bigserial, account_id bigint, group_id bigint, client_request_id text, request_id text,
 created_at timestamptz DEFAULT NOW() - INTERVAL '1 minute', upstream_status_code integer, status_code integer,
 error_type text, is_business_limited boolean DEFAULT false, error_owner text DEFAULT 'provider', error_phase text, error_source text
);
CREATE TEMP TABLE usage_logs (
 id bigserial, account_id bigint, group_id bigint, channel_id bigint DEFAULT 1, user_id bigint DEFAULT 1,
 model text DEFAULT 'public-model', duration_ms integer DEFAULT 1000, first_token_ms integer DEFAULT 500,
 request_id text, created_at timestamptz DEFAULT NOW() - INTERVAL '1 minute',
 input_tokens bigint DEFAULT 1000, output_tokens bigint DEFAULT 0, cache_creation_tokens bigint DEFAULT 0, cache_read_tokens bigint DEFAULT 0,
 input_cost numeric DEFAULT 1, output_cost numeric DEFAULT 0, cache_creation_cost numeric DEFAULT 0, cache_read_cost numeric DEFAULT 0,
 total_cost numeric DEFAULT 10, actual_cost numeric DEFAULT 1, account_stats_cost numeric DEFAULT 10, account_rate_multiplier numeric DEFAULT 0.2
);
CREATE TEMP TABLE monitoring_alerts (id bigint, target_key text, target_name text, kind text DEFAULT 'account', status text DEFAULT 'failed', title text DEFAULT '告警', message text DEFAULT '', created_at timestamptz DEFAULT NOW());
INSERT INTO accounts (id,name) VALUES (1,'shared-account'),(2,'secret-account'),(3,'public-account'),(4,'subscription-account');
INSERT INTO groups (id,name,is_exclusive,subscription_type) VALUES (10,'public-group',false,'standard'),(20,'secret-group',true,'standard'),(30,'subscription-group',false,'subscription'),(40,'deleted-group',true,'standard');
UPDATE groups SET deleted_at=NOW() WHERE id=40;
INSERT INTO account_groups VALUES (1,10),(1,20),(2,20),(3,10),(4,30);
INSERT INTO channels (id,name) VALUES (1,'public-channel'),(2,'secret-channel');
INSERT INTO channel_groups VALUES (1,10),(2,20),(2,30);
INSERT INTO monitoring_targets (target_key,kind,entity_id,name) SELECT 'account:'||id::text,'account',id,name FROM accounts;
INSERT INTO monitoring_targets (target_key,kind,entity_id,name) SELECT 'group:'||id::text,'group',id,name FROM groups WHERE deleted_at IS NULL;
INSERT INTO monitoring_checks (target_key,kind,entity_id,source) SELECT target_key,kind,entity_id,CASE WHEN kind='group' THEN 'aggregate' ELSE 'probe' END FROM monitoring_targets;
INSERT INTO usage_logs (account_id,group_id) SELECT 3,10 FROM generate_series(1,5);
INSERT INTO usage_logs (account_id,group_id,channel_id,model,actual_cost) SELECT 2,20,2,'secret-model',7 FROM generate_series(1,5);
INSERT INTO usage_logs (account_id,group_id,actual_cost) VALUES (1,10,1);
INSERT INTO usage_logs (account_id,group_id,channel_id,model,actual_cost) VALUES (1,20,2,'secret-model',9),(4,30,2,'subscription-model',13),(3,40,2,'deleted-model',11),(3,NULL,2,'ungrouped-model',17);
INSERT INTO ops_error_logs (account_id,group_id,status_code,error_type) VALUES (1,20,503,'secret-error');
INSERT INTO monitoring_alerts (id,target_key,target_name,created_at) VALUES (1,'account:3','public-account',NOW()-INTERVAL '1 minute'),(2,'account:1','shared-account',NOW()),(3,'account:2','secret-account',NOW());
`)
	if err != nil {
		t.Fatal(err)
	}
	redisClient := redis.NewClient(&redis.Options{Addr: "unused.invalid:6379"})
	defer redisClient.Close()
	redisClient.AddHook(concurrencyFixture{})
	repository := New(db, redisClient)
	for _, tc := range []struct {
		name     string
		scope    access.Scope
		keys     []string
		requests int64
		cost     float64
	}{
		{"anonymous", access.Public(), []string{"account:3", "group:10"}, 6, 6},
		{"public user", access.Groups([]int64{10}), []string{"account:3", "group:10"}, 6, 6},
		{"exclusive grant", access.Groups([]int64{10, 20}), []string{"account:1", "account:2", "account:3", "group:10", "group:20"}, 12, 50},
		{"public restricted user", access.Groups([]int64{20}), []string{"account:2", "group:20"}, 6, 44},
		{"subscription", access.Groups([]int64{30}), []string{"account:4", "group:30"}, 1, 13},
		{"no grants", access.Groups(nil), []string{}, 0, 0},
		{"zero denies", access.Scope{}, []string{}, 0, 0},
		{"administrator", access.Administrator(), []string{"account:1", "account:2", "account:3", "account:4", "group:10", "group:20", "group:30"}, 15, 91},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reader := repository.Read(tc.scope)
			dashboard, err := reader.Dashboard(ctx, 2*time.Minute, 60)
			if err != nil {
				t.Fatal(err)
			}
			keys := make([]string, 0, len(dashboard.Targets))
			for _, target := range dashboard.Targets {
				keys = append(keys, target.Key)
			}
			sort.Strings(keys)
			if !reflect.DeepEqual(keys, tc.keys) || dashboard.Summary.Targets != len(tc.keys) {
				t.Fatalf("dashboard scope: %v summary=%+v", keys, dashboard.Summary)
			}
			activity, err := reader.LiveActivity(ctx)
			if err != nil {
				t.Fatal(err)
			}
			keys = keys[:0]
			for _, target := range activity.Targets {
				keys = append(keys, target.TargetKey)
				if !tc.scope.IsAdmin() && strings.HasPrefix(target.TargetKey, "group:") && target.CurrentConcurrency != 0 {
					t.Fatal("shared concurrency leaked into a group")
				}
				if strings.HasPrefix(target.TargetKey, "account:") && target.CurrentConcurrency != 14 {
					t.Fatal("visible account lost its own concurrency")
				}
				if tc.scope.IsAdmin() && target.TargetKey == "group:10" && target.CurrentConcurrency != 28 {
					t.Fatal("admin concurrency compatibility changed")
				}
			}
			if !activity.ConcurrencyOK {
				t.Fatal("Redis fixture was not used")
			}
			sort.Strings(keys)
			if !reflect.DeepEqual(keys, tc.keys) {
				t.Fatalf("activity scope: %v", keys)
			}
			ranking, err := reader.UsageRanking(ctx, "24h", 1)
			if err != nil {
				t.Fatal(err)
			}
			if ranking.Summary.Requests != tc.requests || ranking.Summary.TotalCost != tc.cost {
				t.Fatalf("summary includes inaccessible usage: %+v", ranking.Summary)
			}
			var requests int64
			var cost float64
			for _, bucket := range ranking.Timeline {
				requests += bucket.Requests
				cost += bucket.TotalCost
			}
			if requests != tc.requests || cost != tc.cost {
				t.Fatalf("timeline includes inaccessible usage: %d %f", requests, cost)
			}
			if tc.name == "anonymous" || tc.name == "public user" {
				encoded, _ := json.Marshal(ranking)
				for _, hidden := range []string{"secret-", "subscription-", "deleted-", "ungrouped-"} {
					if strings.Contains(string(encoded), hidden) {
						t.Fatalf("ranking leaks %s", hidden)
					}
				}
				if ranking.DimensionMeta.Groups.TotalItems != 1 || ranking.DimensionMeta.Models.TotalItems != 1 {
					t.Fatal("omitted totals reveal hidden groups/models")
				}
				for _, item := range ranking.Accounts {
					if item.Key == "account:1" && (item.Requests != 1 || item.TotalCost != 2) {
						t.Fatalf("shared account includes private usage: %+v", item)
					}
				}
				for _, key := range []string{"group:20", "group:30", "account:1", "account:2"} {
					history, err := reader.History(ctx, key, 100)
					if err != nil || len(history) != 0 {
						t.Fatalf("history bypass for %s: %v %v", key, history, err)
					}
				}
				history, err := reader.History(ctx, "group:10", 100)
				if err != nil || len(history) != 6 {
					t.Fatalf("public history: %d %v", len(history), err)
				}
				alerts, err := reader.Alerts(ctx, 1)
				if err != nil || len(alerts) != 1 || alerts[0].TargetKey != "account:3" {
					t.Fatalf("alert limit applied before authorization: %+v %v", alerts, err)
				}
			}
		})
	}
	// Readers share one repository, never a mutable current-user grant.
	public := repository.Read(access.Public())
	admin := repository.Read(access.Administrator())
	var workers sync.WaitGroup
	for _, read := range []struct {
		reader *Reader
		cost   float64
	}{{public, 6}, {admin, 91}} {
		workers.Add(1)
		go func(reader *Reader, want float64) {
			defer workers.Done()
			ranking, err := reader.UsageRanking(ctx, "24h", 10)
			if err != nil || ranking.Summary.TotalCost != want {
				t.Errorf("concurrent scope: %+v %v", ranking.Summary, err)
			}
		}(read.reader, read.cost)
	}
	workers.Wait()
	alerts, err := public.Alerts(ctx, 200)
	if err != nil || len(alerts) != 1 {
		t.Fatalf("shared grant state: %+v %v", alerts, err)
	}
	// A group becoming exclusive must be hidden on the next anonymous read.
	if _, err := db.ExecContext(ctx, `UPDATE groups SET is_exclusive=true WHERE id=10`); err != nil {
		t.Fatal(err)
	}
	ranking, err := public.UsageRanking(ctx, "24h", 10)
	if err != nil || ranking.Summary.Requests != 0 {
		t.Fatalf("public-group revocation: %+v %v", ranking.Summary, err)
	}
}
