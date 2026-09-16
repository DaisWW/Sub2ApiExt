package main

import (
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestLoadConfigDefaultsToAutoApply(t *testing.T) {
	for _, key := range []string{
		"PRIORITY_SYNC_DATABASE_URL", "DATABASE_HOST", "DATABASE_PORT", "DATABASE_USER",
		"DATABASE_PASSWORD", "DATABASE_DBNAME", "DATABASE_SSLMODE", "PRIORITY_SYNC_SUB2API_URL",
		"PRIORITY_SYNC_INTERVAL", "PRIORITY_SYNC_WINDOW", "PRIORITY_SYNC_CHANGE_COOLDOWN",
		"PRIORITY_SYNC_MIN_SAMPLES", "PRIORITY_SYNC_CONFIRMATIONS", "PRIORITY_SYNC_DRY_RUN",
		"PRIORITY_SYNC_EXPLORATION_ENABLED", "PRIORITY_SYNC_EXCLUDE_RULES",
		"PRIORITY_SYNC_STATE_FILE", "PRIORITY_SYNC_REPORT_FILE",
	} {
		t.Setenv(key, "")
	}
	t.Setenv("DATABASE_HOST", "db.example")
	t.Setenv("DATABASE_USER", "user")
	t.Setenv("DATABASE_PASSWORD", "password")
	config, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if config.DryRun || config.ExplorationEnabled || config.Interval != 10*time.Minute || config.Window != defaultWindow {
		t.Fatalf("unexpected defaults: %+v", config)
	}
	if !strings.Contains(config.DatabaseURL, "db.example") {
		t.Fatalf("database URL was not built: %q", config.DatabaseURL)
	}
}

func TestLoadConfigEnablesExplorationExplicitly(t *testing.T) {
	t.Setenv("PRIORITY_SYNC_DATABASE_URL", "postgres://user:pass@db/sub2api")
	t.Setenv("PRIORITY_SYNC_EXPLORATION_ENABLED", "true")
	config, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if !config.ExplorationEnabled {
		t.Fatal("explicit exploration setting was ignored")
	}
}

func TestParseExcludeRulesUsesAndWithinRuleAndOrAcrossRules(t *testing.T) {
	rules, err := parseExcludeRules(`[{"platform":"OpenAI","type":"OAuth"},{"id":64}]`)
	if err != nil {
		t.Fatal(err)
	}
	if len(rules) != 2 || rules[0].Platform != "OpenAI" || rules[0].Type != "OAuth" || rules[1].ID == nil || *rules[1].ID != 64 {
		t.Fatalf("parsed rules = %+v", rules)
	}
	if !rules[0].Matches(AccountMetrics{ID: 1, Platform: "openai", Type: "oauth"}) {
		t.Fatal("matching platform/type rule did not match case-insensitively")
	}
	if rules[0].Matches(AccountMetrics{ID: 1, Platform: "openai", Type: "apikey"}) {
		t.Fatal("platform/type rule matched an account with a different type")
	}
	if !rules[1].Matches(AccountMetrics{ID: 64, Platform: "openai", Type: "apikey"}) {
		t.Fatal("id rule did not match")
	}
}

func TestParseExcludeRulesRejectsUnknownOrEmptyRules(t *testing.T) {
	for _, value := range []string{`[{"platfrom":"openai"}]`, `[{}]`, `[{"id":0}]`} {
		if _, err := parseExcludeRules(value); err == nil {
			t.Fatalf("parseExcludeRules(%q) unexpectedly succeeded", value)
		}
	}
}

func TestLoadConfigParsesExcludeRules(t *testing.T) {
	t.Setenv("PRIORITY_SYNC_DATABASE_URL", "postgres://user:pass@db/sub2api")
	t.Setenv("PRIORITY_SYNC_EXCLUDE_RULES", `[{"platform":"openai","type":"oauth"}]`)
	config, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if len(config.ExcludeRules) != 1 || !config.ExcludesAccount(AccountMetrics{Platform: "OpenAI", Type: "OAuth"}) || config.ExcludesAccount(AccountMetrics{Platform: "openai", Type: "apikey"}) {
		t.Fatalf("exclude rules = %+v", config.ExcludeRules)
	}
}

func TestLoadConfigRejectsShortInterval(t *testing.T) {
	t.Setenv("PRIORITY_SYNC_DATABASE_URL", "postgres://user:pass@db/sub2api")
	t.Setenv("PRIORITY_SYNC_INTERVAL", "10s")
	_, err := LoadConfig()
	if err == nil || !strings.Contains(err.Error(), "不能小于") {
		t.Fatalf("error = %v", err)
	}
}

func TestLoadConfigDefaultsToTenMinuteInterval(t *testing.T) {
	t.Setenv("PRIORITY_SYNC_DATABASE_URL", "postgres://user:pass@db/sub2api")
	t.Setenv("PRIORITY_SYNC_INTERVAL", "")
	t.Setenv("PRIORITY_SYNC_DRY_RUN", "")
	config, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if config.Interval != 10*time.Minute || config.DryRun {
		t.Fatalf("interval=%s dry_run=%v", config.Interval, config.DryRun)
	}
}

func TestBuildDatabaseURLEscapesCredentials(t *testing.T) {
	t.Setenv("DATABASE_HOST", "db.example")
	t.Setenv("DATABASE_PORT", "5432")
	t.Setenv("DATABASE_USER", "a@b")
	t.Setenv("DATABASE_PASSWORD", "p@ss")
	t.Setenv("DATABASE_DBNAME", "sub2api")
	t.Setenv("DATABASE_SSLMODE", "disable")
	value := buildDatabaseURL()
	if !strings.Contains(value, "a%40b") || !strings.Contains(value, "p%40ss") {
		t.Fatalf("database URL credentials were not escaped: %q", value)
	}
}

func TestLoadConfigForcesExplicitDatabaseURLReadOnly(t *testing.T) {
	t.Setenv("PRIORITY_SYNC_DATABASE_URL", "postgres://user:pass@db.example/sub2api?sslmode=disable")
	config, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(config.DatabaseURL, "default_transaction_read_only") {
		t.Fatalf("database URL is not read-only: %q", config.DatabaseURL)
	}
}

func TestForceDatabaseReadOnlyPreservesExistingOptions(t *testing.T) {
	value, err := forceDatabaseReadOnly("postgres://db.example/sub2api?options=-c%20search_path%3Dpublic")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(value, "search_path") || !strings.Contains(value, "default_transaction_read_only") {
		t.Fatalf("database options were not preserved: %q", value)
	}
	parsed, err := url.Parse(value)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(parsed.Query().Get("options"), "-c default_transaction_read_only=on") {
		t.Fatalf("read-only option is not last: %q", parsed.Query().Get("options"))
	}

	value, err = forceDatabaseReadOnly("postgres://db.example/sub2api?options=-c%20default_transaction_read_only%3Doff")
	if err != nil {
		t.Fatal(err)
	}
	parsed, err = url.Parse(value)
	if err != nil {
		t.Fatal(err)
	}
	options := parsed.Query().Get("options")
	if !strings.HasSuffix(options, "-c default_transaction_read_only=on") {
		t.Fatalf("existing read-only option was not overridden: %q", options)
	}
}
