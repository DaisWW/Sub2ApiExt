package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAdminAPIUpdatesOnlyPriority(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut || r.URL.Path != "/api/v1/admin/accounts/7" {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("x-api-key") != "secret" {
			t.Fatalf("missing admin key")
		}
		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		if len(payload) != 1 || payload["priority"] != float64(120) {
			t.Fatalf("payload = %#v", payload)
		}
		_, _ = w.Write([]byte(`{"code":0}`))
	}))
	defer server.Close()
	if err := newAdminAPI(server.URL, server.Client()).updatePriority(context.Background(), "secret", 7, 120); err != nil {
		t.Fatal(err)
	}
}

func TestAdminAPIDoesNotExposeResponseBodyOnHTTPError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "secret response", http.StatusBadGateway)
	}))
	defer server.Close()
	err := newAdminAPI(server.URL, server.Client()).updatePriority(context.Background(), "secret", 7, 120)
	if err == nil || containsSecret(err.Error()) {
		t.Fatalf("error = %v", err)
	}
}

func containsSecret(value string) bool {
	return strings.Contains(value, "secret response")
}
