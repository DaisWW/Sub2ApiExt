package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestSourceReadOnlyCatalogAndProxy(t *testing.T) {
	dsn := os.Getenv("EVAL_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("EVAL_TEST_DATABASE_URL is unset")
	}
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	ctx := context.Background()
	// Only use a dedicated disposable database, never production credentials.
	_, err = db.ExecContext(ctx, `CREATE TEMP TABLE accounts(id bigint primary key,name text,platform text,type text,status text,credentials jsonb,proxy_id bigint,deleted_at timestamptz,extra jsonb,schedulable boolean NOT NULL DEFAULT true,rate_limit_reset_at timestamptz,temp_unschedulable_until timestamptz);
	CREATE TEMP TABLE groups(id bigint primary key,name text,deleted_at timestamptz);
	CREATE TEMP TABLE account_groups(account_id bigint,group_id bigint);
	CREATE TEMP TABLE proxies(id bigint,protocol text,host text,port int,username text,password text,status text,expires_at timestamptz,deleted_at timestamptz);
	CREATE TEMP TABLE settings(key text,value text);
	INSERT INTO accounts(id,name,platform,type,status,credentials,proxy_id,deleted_at,extra) VALUES(1,'shared','openai','api_key','error','{"api_key":"do-not-serialize"}',9,NULL,'{"privacy_mode":"training_off"}'),(2,'unassigned','gemini','oauth','disabled','{}',NULL,NULL,'{}'),(3,'deleted','openai','api_key','active','{}',NULL,NOW(),'{}');
	UPDATE accounts SET schedulable=false,rate_limit_reset_at=NOW()+INTERVAL '1 hour',temp_unschedulable_until=NOW()+INTERVAL '2 hours' WHERE id=1;
	INSERT INTO groups VALUES(10,'A',NULL),(11,'B',NULL);
	INSERT INTO account_groups VALUES(1,10),(1,11);
	INSERT INTO proxies VALUES(9,'socks5','127.0.0.1',1080,'user','proxy-secret','active',NULL,NULL);
	INSERT INTO settings VALUES('admin_api_key','admin-secret');`)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.ExecContext(ctx, `SET default_transaction_read_only=on`); err != nil {
		t.Fatal(err)
	}
	s := &Source{db: db}
	accounts, groups, err := s.catalog(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(accounts) != 2 || len(groups) != 2 || len(accounts[0].GroupIDs) != 2 || accounts[0].Status != "error" || accounts[1].Unsupported == "" {
		t.Fatalf("catalog: %+v %+v", accounts, groups)
	}
	if accounts[0].Schedulable || accounts[0].RateLimitResetAt == nil || accounts[0].TempUnschedulableUntil == nil || accounts[0].PrivacyMode != "training_off" || !accounts[1].Schedulable || accounts[1].RateLimitResetAt != nil || accounts[1].PrivacyMode != "" {
		t.Fatal("account filter metadata missing or null fields misread")
	}
	a, err := s.credential(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(a.ProxyURL, "socks5://user:proxy-secret@127.0.0.1:1080") {
		t.Fatalf("proxy not loaded")
	}
	b, _ := json.Marshal(a)
	if strings.Contains(string(b), "secret") || strings.Contains(string(b), "serialize") {
		t.Fatal("credentials serialized")
	}
	key, err := s.setting(ctx, "admin_api_key")
	if err != nil || key != "admin-secret" {
		t.Fatalf("admin key lookup failed")
	}
	db.ExecContext(ctx, `SET default_transaction_read_only=off`)
	db.ExecContext(ctx, `UPDATE proxies SET status='error' WHERE id=9`)
	db.ExecContext(ctx, `SET default_transaction_read_only=on`)
	a, err = s.credential(ctx, 1)
	if err != nil || a.ProxyError == "" || a.ProxyURL != "" {
		t.Fatal("unavailable proxy not stopped")
	}
}
