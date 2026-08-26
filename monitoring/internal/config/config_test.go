package config

import (
	"net/url"
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

func TestLoadRejectsWindowBeyondHistoryLimit(t *testing.T) {
	t.Setenv("DATABASE_HOST", "db.example.test")
	t.Setenv("MONITORING_WINDOW_DAYS", "91")
	if _, err := Load(); err == nil {
		t.Fatal("expected window beyond history limit to be rejected")
	}
}
