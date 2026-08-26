package main

import (
	"database/sql"
	"testing"
)

func TestBuildProxyURL(t *testing.T) {
	got, err := buildProxyURL(
		sql.NullString{String: "http", Valid: true},
		sql.NullString{String: "proxy.example", Valid: true},
		sql.NullInt64{Int64: 7897, Valid: true},
		sql.NullString{String: "user", Valid: true},
		sql.NullString{String: "pass@word", Valid: true},
	)
	if err != nil {
		t.Fatal(err)
	}
	if got != "http://user:pass%40word@proxy.example:7897" {
		t.Fatalf("proxy URL = %q", got)
	}
}

func TestBuildProxyURLAllowsMissingProxy(t *testing.T) {
	got, err := buildProxyURL(sql.NullString{}, sql.NullString{}, sql.NullInt64{}, sql.NullString{}, sql.NullString{})
	if err != nil || got != "" {
		t.Fatalf("buildProxyURL() = %q, %v", got, err)
	}
}

func TestBuildProxyURLRejectsUnsupportedProtocol(t *testing.T) {
	_, err := buildProxyURL(
		sql.NullString{String: "ftp", Valid: true},
		sql.NullString{String: "proxy.example", Valid: true},
		sql.NullInt64{Int64: 1080, Valid: true},
		sql.NullString{},
		sql.NullString{},
	)
	if err == nil {
		t.Fatal("buildProxyURL() error = nil")
	}
}
