package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestUpdateAdminResourceDoesNotExposeResponseMessage(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":123,"message":"secret response details"}`))
	}))
	defer server.Close()

	syncer := &Syncer{
		config: &Config{Sub2APIURL: server.URL, AdminAPIKey: "admin-secret"},
		client: server.Client(),
	}
	err := syncer.updateAdminResource(
		context.Background(), groupAdminUpdateSpec(), 24,
		groupUpdate{RateMultiplier: 0.1},
	)
	if err == nil {
		t.Fatal("non-zero Admin API code should return an error")
	}
	if strings.Contains(err.Error(), "secret response details") || strings.Contains(err.Error(), "admin-secret") {
		t.Fatalf("Admin API response leaked into error: %v", err)
	}
}
