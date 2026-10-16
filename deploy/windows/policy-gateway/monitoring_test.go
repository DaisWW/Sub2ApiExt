package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"testing"
)

func TestMonitoringRoutingPreservesLoginAndMainRoutes(t *testing.T) {
	main := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "main:"+r.URL.Path) }))
	defer main.Close()
	monitor := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer viewer" || r.Header.Get("User-Agent") != "viewer-browser" || r.Header.Get("X-Forwarded-For") == "" {
			t.Error("viewer session headers dropped")
		}
		fmt.Fprint(w, "monitor:"+r.URL.RequestURI())
	}))
	defer monitor.Close()
	u, _ := url.Parse(main.URL)
	v, _ := url.Parse(monitor.URL)
	h := &policyHandler{proxy: httputil.NewSingleHostReverseProxy(u), monitorProxy: httputil.NewSingleHostReverseProxy(v)}
	for _, tc := range []struct{ path, want string }{
		{"/monitoring", "monitor:/monitoring"},
		{"/monitoring/", "monitor:/monitoring/"},
		{"/monitoring/api/v1/monitor/history?target=group:20", "monitor:/monitoring/api/v1/monitor/history?target=group:20"},
		{"/api/v1/auth/me", "main:/api/v1/auth/me"},
		{"/monitoring-other", "main:/monitoring-other"},
	} {
		r := httptest.NewRequest("GET", tc.path, nil)
		r.Header.Set("Authorization", "Bearer viewer")
		r.Header.Set("User-Agent", "viewer-browser")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Body.String() != tc.want {
			t.Fatalf("%s: %s", tc.path, w.Body)
		}
	}
	h.monitorProxy = nil
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/monitoring/", nil))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("missing monitor: %d", w.Code)
	}
}
