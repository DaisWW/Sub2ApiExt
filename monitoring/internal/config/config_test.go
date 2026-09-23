package config

import (
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestBuildDatabaseURLEscapesCredentials(t *testing.T) {
	t.Setenv("DATABASE_HOST", "db.example.test")
	t.Setenv("DATABASE_PORT", "5432")
	t.Setenv("DATABASE_USER", "monitor")
	t.Setenv("DATABASE_PASSWORD", "p@ss:word?x#1%")
	t.Setenv("DATABASE_DBNAME", "sub2api")
	t.Setenv("DATABASE_SSLMODE", "verify-full")

	raw := buildDatabaseURL()
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse database URL: %v", err)
	}
	if parsed.Host != "db.example.test:5432" {
		t.Fatalf("got host %q", parsed.Host)
	}
	if parsed.User.Username() != "monitor" {
		t.Fatalf("got user %q", parsed.User.Username())
	}
	password, ok := parsed.User.Password()
	if !ok || password != "p@ss:word?x#1%" {
		t.Fatalf("got password %q, present=%v", password, ok)
	}
	if parsed.Query().Get("sslmode") != "verify-full" {
		t.Fatalf("got sslmode %q", parsed.Query().Get("sslmode"))
	}
}

func TestLoadRejectsNonPositiveRetention(t *testing.T) {
	t.Setenv("DATABASE_HOST", "db.example.test")
	t.Setenv("REDIS_HOST", "redis.example.test")
	t.Setenv("MONITORING_RETENTION", "0")
	if _, err := Load(); err == nil {
		t.Fatal("expected non-positive retention to be rejected")
	}
}

func TestParseFrameAncestorsAcceptsOriginsAndNormalizesDuplicates(t *testing.T) {
	got, err := parseFrameAncestors("'self', HTTPS://Dashboard.Example:8443/ https://dashboard.example:8443")
	if err != nil {
		t.Fatal(err)
	}
	if got != "'self' https://dashboard.example:8443" {
		t.Fatalf("normalized frame ancestors = %q", got)
	}
}

func TestParseFrameAncestorsRejectsWildcardAndNonOriginValues(t *testing.T) {
	for _, value := range []string{"*", "https://*.example.com", "https://example.com/path", "javascript:alert(1)", "//example.com"} {
		if _, err := parseFrameAncestors(value); err == nil {
			t.Errorf("parseFrameAncestors(%q) unexpectedly succeeded", value)
		}
	}
}

func TestLoadUsesFrameAncestorsConfiguration(t *testing.T) {
	t.Setenv("DATABASE_HOST", "db.example.test")
	t.Setenv("REDIS_HOST", "redis.example.test")
	t.Setenv("MONITORING_FRAME_ANCESTORS", "https://dashboard.example.test")
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.FrameAncestors != "https://dashboard.example.test" {
		t.Fatalf("frame ancestors = %q", c.FrameAncestors)
	}
	t.Setenv("MONITORING_FRAME_ANCESTORS", "*")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "wildcard") {
		t.Fatalf("wildcard configuration error = %v", err)
	}
}

func TestLoadBuildsRedisConfiguration(t *testing.T) {
	t.Setenv("DATABASE_HOST", "db.example.test")
	t.Setenv("REDIS_HOST", "redis.example.test")
	t.Setenv("REDIS_PORT", "6380")
	t.Setenv("REDIS_PASSWORD", "secret")
	t.Setenv("REDIS_DB", "2")
	t.Setenv("REDIS_ENABLE_TLS", "true")
	t.Setenv("MONITORING_CONCURRENCY_SLOT_TTL", "45m")
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.RedisAddr != "redis.example.test:6380" || c.RedisPassword != "secret" || c.RedisDB != 2 || !c.RedisTLS {
		t.Fatalf("Redis configuration = %+v", c)
	}
	if c.ConcurrencySlotTTL.String() != "45m0s" {
		t.Fatalf("concurrency slot TTL = %s", c.ConcurrencySlotTTL)
	}
}

func TestLoadCostAlertEmailConfiguration(t *testing.T) {
	t.Setenv("DATABASE_HOST", "db.example.test")
	t.Setenv("REDIS_HOST", "redis.example.test")
	t.Setenv("MONITORING_COST_EMAIL_USERNAME", "monitor@qq.com")
	t.Setenv("MONITORING_COST_EMAIL_PASSWORD", "authorization-code")
	t.Setenv("MONITORING_COST_EMAIL_TO", "one@example.com, two@example.com")
	t.Setenv("MONITORING_COST_DAILY_BUDGET", "12.5")
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if !c.CostAlerts.Enabled || c.CostAlerts.Email.Host != "smtp.qq.com" || c.CostAlerts.Email.Port != 465 {
		t.Fatalf("unexpected cost alert email transport configuration")
	}
	if len(c.CostAlerts.Email.To) != 2 || c.CostAlerts.DailyBudget != 12.5 {
		t.Fatalf("unexpected recipients or budget: %+v / %v", c.CostAlerts.Email.To, c.CostAlerts.DailyBudget)
	}
	if c.CostAlerts.CacheMissMinRequests != 3 || c.CostAlerts.CacheMissInputTokens != 100_000 ||
		c.CostAlerts.CacheMissMinCost != 0.5 || c.CostAlerts.CacheMissMaxSpan != 5*time.Minute ||
		c.CostAlerts.AccountSwitchMinTransitions != 2 {
		t.Fatalf("unexpected request anomaly thresholds: %+v", c.CostAlerts)
	}
}

func TestLoadRejectsPartialCostAlertEmailConfiguration(t *testing.T) {
	t.Setenv("DATABASE_HOST", "db.example.test")
	t.Setenv("REDIS_HOST", "redis.example.test")
	t.Setenv("MONITORING_COST_EMAIL_TO", "admin@example.com")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "cost alert email") {
		t.Fatalf("partial email configuration error = %v", err)
	}
}

func TestLoadRejectsNonFiniteCostAlertThreshold(t *testing.T) {
	t.Setenv("DATABASE_HOST", "db.example.test")
	t.Setenv("REDIS_HOST", "redis.example.test")
	t.Setenv("MONITORING_COST_UNIT_COST_RATIO", "NaN")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "cost alert ratios") {
		t.Fatalf("non-finite cost threshold error = %v", err)
	}
}

func TestLoadRejectsUnknownCostAlertEmailSecurity(t *testing.T) {
	t.Setenv("DATABASE_HOST", "db.example.test")
	t.Setenv("REDIS_HOST", "redis.example.test")
	t.Setenv("MONITORING_COST_EMAIL_SECURITY", "plain")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "EMAIL_SECURITY") {
		t.Fatalf("unknown email security error = %v", err)
	}
}
