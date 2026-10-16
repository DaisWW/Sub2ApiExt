package access

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync/atomic"
	"testing"
)

func TestAuthorizerUsesMainSiteIdentityAndAvailableGroups(t *testing.T) {
	for _, tc := range []struct {
		name, token, user, groups string
		status                    int
		public, admin             bool
		ids                       []int64
		wantErr                   error
	}{
		{name: "anonymous", public: true},
		{name: "admin", token: "admin", user: `{"id":1,"role":"admin","status":"active"}`, admin: true},
		{name: "authorized exclusive and subscription", token: "user", user: `{"id":2,"role":"user","status":"active"}`, groups: `[{"id":10},{"id":20},{"id":30}]`, ids: []int64{10, 20, 30}},
		{name: "public restriction", token: "restricted", user: `{"id":3,"role":"user","status":"active"}`, groups: `[{"id":20}]`, ids: []int64{20}},
		{name: "no grants", token: "empty", user: `{"id":4,"role":"user","status":"active"}`, groups: `[]`},
		{name: "disabled admin", token: "disabled", user: `{"id":1,"role":"admin","status":"disabled"}`, wantErr: ErrUnauthenticated},
		{name: "missing identity", token: "bad", user: `{"role":"admin","status":"active"}`, wantErr: ErrUnauthenticated},
		{name: "expired", token: "expired", status: 401, wantErr: ErrUnauthenticated},
		{name: "main unavailable", token: "down", status: 503, wantErr: ErrUnavailable},
		{name: "invalid groups", token: "badgroups", user: `{"id":2,"role":"user","status":"active"}`, groups: `null`, wantErr: ErrUnavailable},
		{name: "invalid group ID", token: "badid", user: `{"id":2,"role":"user","status":"active"}`, groups: `[{"id":0}]`, wantErr: ErrUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.Header.Get("Authorization") != "Bearer "+tc.token || r.Header.Get("User-Agent") != "viewer-browser" || r.Header.Get("X-Forwarded-For") != "192.0.2.1" {
					t.Error("main-site session headers were not preserved")
				}
				if r.Header.Get("X-User-Role") != "" || r.Header.Get("Cookie") != "" {
					t.Error("untrusted identity forwarded")
				}
				if tc.status != 0 {
					w.WriteHeader(tc.status)
					return
				}
				body := tc.user
				if r.URL.Path == "/api/v1/groups/available" {
					if tc.admin {
						t.Error("admin should not be limited by bindable groups")
					}
					body = tc.groups
				} else if r.URL.Path != "/api/v1/auth/me" {
					t.Error("unexpected auth route")
				}
				fmt.Fprintf(w, `{"code":0,"data":%s}`, body)
			}))
			defer upstream.Close()
			a, err := NewAuthorizer(upstream.URL)
			if err != nil {
				t.Fatal(err)
			}
			defer a.Close()
			headers := http.Header{"User-Agent": {"viewer-browser"}, "X-Forwarded-For": {"192.0.2.1"}, "X-User-Role": {"admin"}, "Cookie": {"session=untrusted"}}
			if tc.token != "" {
				headers.Set("Authorization", "Bearer "+tc.token)
			}
			scope, err := a.Resolve(context.Background(), headers)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("error = %v, want %v", err, tc.wantErr)
			}
			if scope.IsAdmin() != tc.admin || scope.IsPublic() != tc.public || !reflect.DeepEqual(scope.GroupIDs(), tc.ids) {
				t.Fatalf("unexpected scope: %+v", scope)
			}
			if tc.token == "" && calls != 0 {
				t.Fatal("anonymous viewer contacted main service")
			}
		})
	}
}

func TestAuthorizerRefreshesGrantsAndRejectsRedirects(t *testing.T) {
	var groups atomic.Value
	groups.Store(`[{"id":20}]`)
	var redirect atomic.Bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if redirect.Load() {
			http.Redirect(w, r, "/credential-sink", 302)
			return
		}
		if r.URL.Path == "/api/v1/auth/me" {
			fmt.Fprint(w, `{"code":0,"data":{"id":2,"role":"user","status":"active"}}`)
			return
		}
		fmt.Fprintf(w, `{"code":0,"data":%s}`, groups.Load())
	}))
	defer upstream.Close()
	a, _ := NewAuthorizer(upstream.URL)
	defer a.Close()
	headers := http.Header{"Authorization": {"Bearer user"}}
	first, err := a.Resolve(context.Background(), headers)
	if err != nil || len(first.GroupIDs()) != 1 {
		t.Fatalf("initial grant: %+v %v", first, err)
	}
	groups.Store(`[]`)
	second, err := a.Resolve(context.Background(), headers)
	if err != nil || len(second.GroupIDs()) != 0 {
		t.Fatalf("revocation was cached: %+v %v", second, err)
	}
	redirect.Store(true)
	if _, err := a.Resolve(context.Background(), headers); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("redirect: %v", err)
	}
	for _, value := range []string{"Basic user", "Bearer", "Bearer a b"} {
		headers.Set("Authorization", value)
		if _, err := a.Resolve(context.Background(), headers); !errors.Is(err, ErrUnauthenticated) {
			t.Fatalf("malformed authorization: %v", err)
		}
	}
}

func TestScopeDoesNotShareMutableGrants(t *testing.T) {
	ids := []int64{20}
	scope := Groups(ids)
	ids[0] = 10
	copy := scope.GroupIDs()
	copy[0] = 30
	if scope.GroupIDs()[0] != 20 {
		t.Fatal("grant mutated across requests")
	}
	if (Scope{}).IsAdmin() || (Scope{}).IsPublic() {
		t.Fatal("zero scope must deny access")
	}
	for _, base := range []string{"http://user:secret@example.com", "https://example.com/path", "https://example.com?token=secret", "file:///tmp/", "//example.com"} {
		if _, err := NewAuthorizer(base); err == nil {
			t.Fatal("accepted unsafe main-service origin")
		}
	}
}
