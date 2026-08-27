package config

import (
	"net/url"
	"strings"
	"testing"
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
