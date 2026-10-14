package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"testing"
)

func TestEvaluationRoutingDoesNotChangeMainAPI(t *testing.T) {
	main := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "main:"+r.URL.Path) }))
	defer main.Close()
	eval := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer token" {
			t.Error("admin token dropped")
		}
		fmt.Fprint(w, "eval:"+r.URL.Path)
	}))
	defer eval.Close()
	u, _ := url.Parse(main.URL)
	v, _ := url.Parse(eval.URL)
	h := &policyHandler{proxy: httputil.NewSingleHostReverseProxy(u), evalProxy: httputil.NewSingleHostReverseProxy(v)}
	for _, tc := range []struct{ path, want string }{{"/account-eval/", "eval:/account-eval/"}, {"/account-eval/api/runs", "eval:/account-eval/api/runs"}, {"/api/v1/auth/me", "main:/api/v1/auth/me"}, {"/account-eval-other", "main:/account-eval-other"}} {
		r := httptest.NewRequest("GET", tc.path, nil)
		r.Header.Set("Authorization", "Bearer token")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Body.String() != tc.want {
			t.Fatalf("%s: %s", tc.path, w.Body)
		}
	}
}
