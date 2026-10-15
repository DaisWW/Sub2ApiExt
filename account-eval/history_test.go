package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHistoryDeletion(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		role := "admin"
		if r.Header.Get("Authorization") == "Bearer user" {
			role = "user"
		}
		fmt.Fprintf(w, `{"code":0,"data":{"role":%q,"status":"active"}}`, role)
	}))
	defer up.Close()
	h := (&Server{store: s, gateway: &Gateway{base: up.URL, client: up.Client()}}).handler()
	request := func(method, path, token, body string) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(method, "/account-eval/api/runs/"+path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	add := func(status string) Run {
		t.Helper()
		r := testRun(newID())
		addRun(t, s, r)
		if _, err := s.db.ExecContext(ctx, `UPDATE runs SET status=? WHERE id=?`, status, r.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := s.db.ExecContext(ctx, `UPDATE results SET status=?,data='{"answer":"saved answer","html":"<svg/>"}',mark='review' WHERE run_id=?`, status, r.ID); err != nil {
			t.Fatal(err)
		}
		return r
	}
	batch := func(ids ...string) string {
		b, _ := json.Marshal(map[string]any{"run_ids": ids})
		return string(b)
	}
	assertRetained := func(r Run) {
		t.Helper()
		if _, err := s.run(ctx, r.ID); err != nil {
			t.Fatal(err)
		}
		cells, err := s.results(ctx, r.ID, true)
		if err != nil || len(cells) != 4 {
			t.Fatalf("retained results: %d %v", len(cells), err)
		}
		for _, c := range cells {
			if c.Answer != "saved answer" || c.HTML != "<svg/>" || c.Mark != "review" {
				t.Fatalf("result changed: %+v", c)
			}
		}
	}
	assertDeleted := func(r Run) {
		t.Helper()
		if _, err := s.run(ctx, r.ID); !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("run not deleted: %v", err)
		}
		cells, err := s.results(ctx, r.ID, true)
		if err != nil || len(cells) != 0 {
			t.Fatalf("orphan results: %d %v", len(cells), err)
		}
	}

	finished := add("completed")
	if err := s.saveSuite(ctx, finished.Suite); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		token string
		want  int
	}{{"", 401}, {"user", 403}} {
		for _, method := range []string{"DELETE", "POST"} {
			path := finished.ID
			if method == "POST" {
				path = "delete"
			}
			if w := request(method, path, tc.token, batch(finished.ID)); w.Code != tc.want {
				t.Fatalf("auth %s: %d %s", method, w.Code, w.Body)
			}
		}
	}
	for _, body := range []string{`{}`, `{"run_ids":[]}`, `{"run_ids":["invalid"]}`, batch(finished.ID, "invalid"), `{"run_ids":[],"all":true}`} {
		if w := request("POST", "delete", "admin", body); w.Code != 400 {
			t.Fatalf("invalid batch: %d %s", w.Code, w.Body)
		}
	}
	if w := request("DELETE", "invalid", "admin", ""); w.Code != 400 {
		t.Fatalf("invalid ID: %d", w.Code)
	}
	for _, status := range []string{"queued", "running", "cancelling"} {
		active := add(status)
		if w := request("POST", "delete", "admin", batch(finished.ID, active.ID)); w.Code != 409 {
			t.Fatalf("active batch %s: %d %s", status, w.Code, w.Body)
		}
		if w := request("DELETE", active.ID, "admin", ""); w.Code != 409 {
			t.Fatalf("active single %s: %d", status, w.Code)
		}
		assertRetained(finished)
		assertRetained(active)
	}
	failed := add("failed")
	if _, err := s.db.ExecContext(ctx, `CREATE TEMP TRIGGER block_history_delete BEFORE DELETE ON runs WHEN OLD.status='failed' BEGIN SELECT RAISE(ABORT,'simulated failure'); END`); err != nil {
		t.Fatal(err)
	}
	if w := request("POST", "delete", "admin", batch(finished.ID, failed.ID)); w.Code != 500 {
		t.Fatalf("failed transaction: %d %s", w.Code, w.Body)
	}
	assertRetained(finished)
	assertRetained(failed)
	if _, err := s.db.ExecContext(ctx, `DROP TRIGGER block_history_delete`); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if w := request("DELETE", finished.ID, "admin", ""); w.Code != 200 {
			t.Fatalf("single delete: %d %s", w.Code, w.Body)
		}
	}
	assertDeleted(finished)
	ids := []string{failed.ID, failed.ID, newID()}
	deleted := []Run{failed}
	for _, status := range []string{"completed", "completed_with_errors", "cancelled", "interrupted"} {
		r := add(status)
		ids = append(ids, r.ID)
		deleted = append(deleted, r)
	}
	if w := request("POST", "delete", "admin", batch(ids...)); w.Code != 200 {
		t.Fatalf("batch delete: %d %s", w.Code, w.Body)
	}
	for _, r := range deleted {
		assertDeleted(r)
	}
	suites, err := s.suites(ctx)
	if err != nil || len(suites) != 1 || suites[0].ID != finished.Suite.ID {
		t.Fatalf("suite changed: %v %v", suites, err)
	}
}
