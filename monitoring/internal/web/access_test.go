package web

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/DaisWW/Sub2ApiExt/monitoring/internal/access"
	"github.com/DaisWW/Sub2ApiExt/monitoring/internal/model"
)

type scopedService struct {
	last  access.Scope
	calls int
}

func (s *scopedService) Dashboard(_ context.Context, scope access.Scope) (model.Dashboard, error) {
	s.last = scope
	s.calls++
	return model.Dashboard{}, nil
}
func (s *scopedService) LiveActivity(_ context.Context, scope access.Scope) (model.LiveActivity, error) {
	s.last = scope
	s.calls++
	return model.LiveActivity{}, nil
}
func (s *scopedService) UsageRanking(_ context.Context, scope access.Scope, _ string, _ int) (model.UsageRanking, error) {
	s.last = scope
	s.calls++
	return model.UsageRanking{}, nil
}
func (s *scopedService) History(_ context.Context, scope access.Scope, _ string, _ int) ([]model.ProbeResult, error) {
	s.last = scope
	s.calls++
	return []model.ProbeResult{}, nil
}
func (s *scopedService) Alerts(_ context.Context, scope access.Scope, _ int) ([]model.Alert, error) {
	s.last = scope
	s.calls++
	return []model.Alert{}, nil
}

func TestEveryDataRouteRequiresVerifiedScope(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := r.Header.Get("Authorization")
		if token == "Bearer expired" {
			w.WriteHeader(401)
			return
		}
		if token == "Bearer down" {
			w.WriteHeader(503)
			return
		}
		if r.URL.Path == "/api/v1/groups/available" {
			fmt.Fprint(w, `{"code":0,"data":[{"id":20}]}`)
			return
		}
		role := "user"
		if token == "Bearer admin" {
			role = "admin"
		}
		fmt.Fprintf(w, `{"code":0,"data":{"id":1,"role":%q,"status":"active"}}`, role)
	}))
	defer upstream.Close()
	a, err := access.NewAuthorizer(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	service := &scopedService{}
	handler := New(service, a).Handler()
	for _, prefix := range []string{"", "/monitoring"} {
		for _, route := range []string{"dashboard", "activity", "usage-ranking", "history?target=group:20", "alerts"} {
			for _, token := range []string{"", "user", "admin", "expired", "down"} {
				path := prefix + "/api/v1/monitor/" + route
				r := httptest.NewRequest(http.MethodGet, path, nil)
				if token != "" {
					r.Header.Set("Authorization", "Bearer "+token)
				}
				r.Header.Set("X-User-Role", "admin")
				before := service.calls
				w := httptest.NewRecorder()
				handler.ServeHTTP(w, r)
				status := http.StatusOK
				if token == "expired" {
					status = http.StatusUnauthorized
				}
				if token == "down" {
					status = http.StatusServiceUnavailable
				}
				if w.Code != status {
					t.Fatalf("%s %s: HTTP %d %s", token, path, w.Code, w.Body)
				}
				if status != http.StatusOK {
					if service.calls != before {
						t.Fatal("failed identity reached data service")
					}
					continue
				}
				if service.calls != before+1 || service.last.IsAdmin() != (token == "admin") || service.last.IsPublic() != (token == "") {
					t.Fatalf("unverified scope for %s %s: %+v", token, path, service.last)
				}
				if token == "user" && !reflect.DeepEqual(service.last.GroupIDs(), []int64{20}) {
					t.Fatal("main-site group restrictions were not passed to data service")
				}
			}
		}
	}
	w := httptest.NewRecorder()
	New(service, nil).Handler().ServeHTTP(w, httptest.NewRequest("GET", "/api/v1/monitor/dashboard", nil))
	if w.Code != 503 {
		t.Fatal("missing authorizer must fail closed")
	}
}

func TestMonitoringPrefixServesPageAndAssets(t *testing.T) {
	handler := New(nil, nil).Handler()
	for _, path := range []string{"/monitoring/", "/monitoring/app.js", "/monitoring/js/shared.js", "/monitoring/styles.css", "/healthz"} {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != 200 {
			t.Fatalf("%s: %d", path, w.Code)
		}
	}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest("GET", "/monitoring", nil))
	if w.Code != 308 || w.Header().Get("Location") != "/monitoring/" {
		t.Fatal("monitoring prefix must normalize its trailing slash")
	}
}
