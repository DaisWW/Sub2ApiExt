package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type fakeSource struct{}

func (fakeSource) catalog(context.Context) ([]Account, []Group, error) {
	return []Account{{ID: 1, Name: "Test", Platform: "openai", GroupIDs: []int64{1, 2}}}, []Group{{ID: 1, Name: "A"}, {ID: 2, Name: "B"}}, nil
}
func TestAdminBoundarySnapshotsRetryAndMarks(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/auth/me" {
			t.Error("unexpected auth endpoint")
		}
		if r.Header.Get("User-Agent") != "browser-ua" || r.Header.Get("X-Forwarded-For") != "203.0.113.10" {
			t.Error("session fingerprint headers lost")
		}
		role := "admin"
		if r.Header.Get("Authorization") == "Bearer user-token" {
			role = "user"
		}
		if r.Header.Get("Authorization") == "Bearer expired-token" {
			w.WriteHeader(401)
			return
		}
		fmt.Fprintf(w, `{"code":0,"data":{"role":%q,"status":"active"}}`, role)
	}))
	defer up.Close()
	s := testStore(t)
	e := &Engine{store: s, wake: make(chan struct{}, 1)}
	server := (&Server{store: s, engine: e, source: fakeSource{}, gateway: &Gateway{base: up.URL, client: up.Client()}}).handler()
	request := func(method, path, body, token string) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(method, "/account-eval/api/"+path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("User-Agent", "browser-ua")
		r.Header.Set("X-Forwarded-For", "203.0.113.10")
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		w := httptest.NewRecorder()
		server.ServeHTTP(w, r)
		return w
	}
	for _, token := range []string{"", "user-token", "expired-token"} {
		for _, endpoint := range []string{"catalog", "suites", "runs", "results/1"} {
			w := request("GET", endpoint, "", token)
			want := 401
			if token == "user-token" {
				want = 403
			}
			if w.Code != want {
				t.Fatalf("%s %s: %d %s", token, endpoint, w.Code, w.Body)
			}
		}
	}
	w := request("GET", "catalog", "", "admin-token")
	if w.Code != 200 || strings.Contains(w.Body.String(), "secret") {
		t.Fatalf("catalog: %d %s", w.Code, w.Body)
	}
	suite := Suite{Name: "saved", Questions: []Question{{Name: "draw", Prompt: "full prompt", Format: "html"}}}
	b, _ := json.Marshal(suite)
	w = request("POST", "suites", string(b), "admin-token")
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	json.Unmarshal(w.Body.Bytes(), &suite)
	payload := fmt.Sprintf(`{"suite_id":%q,"account_ids":[1,1],"settings":{"models":{"openai":"m"},"concurrency":2,"timeout_seconds":30,"max_tokens":0}}`, suite.ID)
	w = request("POST", "runs", payload, "admin-token")
	if w.Code != 201 {
		t.Fatal(w.Body.String())
	}
	var run Run
	json.Unmarshal(w.Body.Bytes(), &run)
	if len(run.Accounts) != 1 || run.Total != 1 {
		t.Fatal("account not deduplicated")
	}
	suite.Questions[0].Prompt = "edited"
	s.saveSuite(context.Background(), suite)
	got, _ := s.run(context.Background(), run.ID)
	if got.Suite.Questions[0].Prompt != "full prompt" {
		t.Fatal("run changed when suite edited")
	}
	cells, _ := s.results(context.Background(), run.ID, false)
	s.db.Exec(`UPDATE runs SET status='interrupted' WHERE id=?`, run.ID)
	s.db.Exec(`UPDATE results SET status='interrupted' WHERE run_id=?`, run.ID)
	w = request("POST", "runs/"+run.ID+"/retry", "", "admin-token")
	if w.Code != 201 {
		t.Fatal(w.Body.String())
	}
	var retry Run
	json.Unmarshal(w.Body.Bytes(), &retry)
	if retry.ID == run.ID || retry.ParentID != run.ID || retry.Suite.Questions[0].Prompt != "full prompt" {
		t.Fatalf("retry: %+v", retry)
	}
	w = request("PATCH", fmt.Sprintf("results/%d", cells[0].ID), `{"mark":"pass"}`, "admin-token")
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	cell, _ := s.result(context.Background(), cells[0].ID)
	if cell.Mark != "pass" {
		t.Fatal("mark not persisted")
	}
	w = request("POST", "runs", strings.Replace(payload, "[1,1]", "[999]", 1), "admin-token")
	if w.Code != 400 {
		t.Fatal("missing account accepted")
	}
	w = request("POST", "suites", `{"name":"x","questions":[],"credentials":"secret"}`, "admin-token")
	if w.Code != 400 {
		t.Fatal("unknown fields accepted")
	}
}

func TestSVGAndIdempotentAdminMenuPreservesOtherSettings(t *testing.T) {
	icon, _ := assets.ReadFile("web/icon.svg")
	clean, err := cleanSVG(string(icon))
	if err != nil {
		t.Fatal(err)
	}
	if clean == "" {
		t.Fatal("empty icon")
	}
	for _, bad := range []string{`<svg onload="alert(1)"></svg>`, `<svg><script>evil</script></svg>`, `<svg><image href="https://evil"/></svg>`, `<svg><path fill="url(https://evil)"/></svg>`, `<!DOCTYPE svg [<!ENTITY a "evil">]><svg>&a;</svg>`, `<svg/><svg/>`, `<svg style="background:url(https://evil)"></svg>`} {
		if _, err := cleanSVG(bad); err == nil {
			t.Fatalf("unsafe SVG accepted: %s", bad)
		}
	}
	menus := []json.RawMessage{json.RawMessage(`{"id":"monitoring","label":"渠道监控","url":"https://monitor.example","visibility":"user","icon_svg":"","sort_order":7,"future_field":"keep"}`)}
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/admin/settings" {
			t.Error("wrong settings path")
		}
		if r.Method == "GET" {
			b, _ := json.Marshal(map[string]any{"code": 0, "data": map[string]any{"custom_menu_items": menus, "site_name": "unchanged"}})
			w.Write(b)
			return
		}
		b, _ := io.ReadAll(r.Body)
		var payload map[string]json.RawMessage
		json.Unmarshal(b, &payload)
		if len(payload) != 1 || payload["custom_menu_items"] == nil {
			t.Fatalf("unrelated settings sent: %s", b)
		}
		json.Unmarshal(payload["custom_menu_items"], &menus)
		fmt.Fprint(w, `{"code":0,"data":{}}`)
	}))
	defer up.Close()
	g := &Gateway{base: up.URL, client: up.Client()}
	headers := http.Header{"X-Api-Key": []string{"internal-only"}}
	for i := 0; i < 2; i++ {
		m, err := g.menu(context.Background(), headers, Menu{URL: "https://site.example/account-eval/"})
		if err != nil {
			t.Fatal(err)
		}
		if m.Visibility != "admin" || m.ID != "account-eval" {
			t.Fatalf("menu: %+v", m)
		}
	}
	if len(menus) != 2 || !strings.Contains(string(menus[0]), `"future_field":"keep"`) {
		t.Fatalf("existing menu overwritten: %s", menus)
	}
}
