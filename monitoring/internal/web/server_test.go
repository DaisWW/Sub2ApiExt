package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
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
	for _, path := range []string{"/", "/api/v1/monitor/probe", "/api/v1/monitor/activity", "/api/v1/monitor/alerts/1/ack"} {
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
	policy := response.Header().Get("Content-Security-Policy")
	if policy == "" {
		t.Fatal("Content-Security-Policy header is missing")
	}
	if !strings.Contains(policy, "frame-ancestors 'self'") || strings.Contains(policy, "frame-ancestors *") {
		t.Fatalf("CSP %q does not restrict iframe embedding to same origin", policy)
	}
	if frameOptions := response.Header().Get("X-Frame-Options"); frameOptions != "SAMEORIGIN" {
		t.Fatalf("got X-Frame-Options %q, want SAMEORIGIN", frameOptions)
	}
	if cacheControl := response.Header().Get("Cache-Control"); cacheControl != "no-store" {
		t.Fatalf("got Cache-Control %q, want no-store", cacheControl)
	}
}

func TestUsageShareControlsExcludeUnitCost(t *testing.T) {
	server := New((*monitor.Service)(nil))
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("got status %d", response.Code)
	}
	body := response.Body.String()
	for _, kind := range []string{"model", "group"} {
		prefix := `data-share-kind="` + kind + `" data-share-metric=`
		if got := strings.Count(body, prefix); got != 2 {
			t.Errorf("%s share controls = %d, want Tokens and cost only", kind, got)
		}
		if strings.Contains(body, prefix+`"unit_cost"`) {
			t.Errorf("%s share controls still expose unit_cost", kind)
		}
	}
	if !strings.Contains(body, `data-trend-metric="unit_cost"`) {
		t.Error("trend controls lost the unit_cost metric")
	}
}

func TestUsageShareUsesFilledSvgPaths(t *testing.T) {
	server := New((*monitor.Service)(nil))
	request := httptest.NewRequest(http.MethodGet, "/js/usage.js", nil)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("got status %d", response.Code)
	}
	body := response.Body.String()
	for _, marker := range []string{"function donutSlicePath", "<path class=\"donut-slice\"", "fill-rule=\"evenodd\""} {
		if !strings.Contains(body, marker) {
			t.Errorf("usage.js is missing %q", marker)
		}
	}
	if strings.Contains(body, "stroke-dasharray") {
		t.Error("usage.js still relies on dashed circles for donut slices")
	}
}

func TestStaticResponsesUseConfiguredFrameAncestors(t *testing.T) {
	server := New((*monitor.Service)(nil), "'self' https://dashboard.example.test:8443")
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	policy := response.Header().Get("Content-Security-Policy")
	if !strings.Contains(policy, "frame-ancestors 'self' https://dashboard.example.test:8443") {
		t.Fatalf("CSP %q does not contain configured frame ancestors", policy)
	}
	if frameOptions := response.Header().Get("X-Frame-Options"); frameOptions != "" {
		t.Fatalf("got X-Frame-Options %q for a multi-origin policy", frameOptions)
	}
}

func TestNewRejectsWildcardFrameAncestors(t *testing.T) {
	server := New((*monitor.Service)(nil), "*")
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if policy := response.Header().Get("Content-Security-Policy"); strings.Contains(policy, "frame-ancestors *") {
		t.Fatalf("wildcard frame ancestor leaked into CSP %q", policy)
	}
}
