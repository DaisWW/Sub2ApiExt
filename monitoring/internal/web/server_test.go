package web

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/DaisWW/Sub2ApiExt/monitoring/internal/monitor"
)

func TestValidTargetKeyRejectsMalformedValues(t *testing.T) {
	for _, key := range []string{"account:1", "group:23", "group:-1"} {
		if !validTargetKey(key) {
			t.Errorf("validTargetKey(%q) = false", key)
		}
	}
	for _, key := range []string{"", "account:", "account:-1", "account:0001", "group:01", "account:group:1", "group:1x", "user:1"} {
		if validTargetKey(key) {
			t.Errorf("validTargetKey(%q) = true", key)
		}
	}
}

func TestMonitoringWebServiceRejectsMutatingMethods(t *testing.T) {
	server := New((*monitor.Service)(nil))
	for _, path := range []string{"/", "/api/v1/monitor/probe", "/api/v1/monitor/alerts/1/ack"} {
		request := httptest.NewRequest(http.MethodPost, path, nil)
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, request)
		if response.Code != http.StatusMethodNotAllowed {
			t.Errorf("POST %s returned %d, want %d", path, response.Code, http.StatusMethodNotAllowed)
		}
		if allow := response.Header().Get("Allow"); allow != "GET, HEAD" {
			t.Errorf("POST %s returned Allow %q", path, allow)
		}
	}
}

func TestRemovedWriteAPIsAreNotExposed(t *testing.T) {
	server := New((*monitor.Service)(nil))
	for _, path := range []string{"/api/v1/monitor/probe", "/api/v1/monitor/alerts/1/ack"} {
		request := httptest.NewRequest(http.MethodGet, path, nil)
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, request)
		if response.Code != http.StatusNotFound {
			t.Errorf("GET %s returned %d, want %d", path, response.Code, http.StatusNotFound)
		}
	}
}

func TestBoundedQueryIntClampsValues(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "/?limit=9999", nil)
	if got := boundedQueryInt(request, "limit", 10, 1, 50); got != 50 {
		t.Fatalf("got %d, want max bound", got)
	}
	request = httptest.NewRequest(http.MethodGet, "/?limit=0", nil)
	if got := boundedQueryInt(request, "limit", 10, 1, 50); got != 10 {
		t.Fatalf("got %d, want fallback for non-positive value", got)
	}
}

func TestStaticResponsesSetContentSecurityPolicy(t *testing.T) {
	server := New((*monitor.Service)(nil))
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("got status %d", response.Code)
	}
	if policy := response.Header().Get("Content-Security-Policy"); policy == "" {
		t.Fatal("Content-Security-Policy header is missing")
	}
	if cacheControl := response.Header().Get("Cache-Control"); cacheControl != "no-store" {
		t.Fatalf("got Cache-Control %q, want no-store", cacheControl)
	}
}
