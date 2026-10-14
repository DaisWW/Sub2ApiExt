package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

// Explicitly opt in: this test consumes upstream usage and only writes to temporary SQLite.
func TestLiveAccountBatch(t *testing.T) {
	configPath := os.Getenv("EVAL_LIVE_CONFIG")
	if configPath == "" {
		t.Skip("EVAL_LIVE_CONFIG is unset; real account requests are disabled")
	}
	var config struct {
		Email    string            `json:"email"`
		Password string            `json:"password"`
		Database map[string]string `json:"database"`
		Accounts []struct {
			ID    int64  `json:"id"`
			Model string `json:"model"`
		} `json:"accounts"`
	}
	b, err := os.ReadFile(configPath)
	if err != nil || json.Unmarshal(b, &config) != nil || len(config.Accounts) == 0 {
		t.Fatal("invalid live-test configuration")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	for _, key := range []string{"PGHOST", "PGPORT", "PGUSER", "PGPASSWORD", "PGDATABASE", "PGSSLMODE"} {
		if value := config.Database[key]; value != "" {
			t.Setenv(key, value)
		}
	}
	pg, err := sql.Open("postgres", "connect_timeout=10 options='-c default_transaction_read_only=on'")
	if err != nil {
		t.Fatal("cannot configure read-only PostgreSQL")
	}
	defer pg.Close()
	source := &Source{db: pg}
	catalog, _, err := source.catalog(ctx)
	if err != nil {
		t.Fatal("cannot read live account catalog")
	}
	settings := Settings{Models: map[string]string{}, Concurrency: 3, Timeout: 90}
	ids := []int64{}
	for _, requested := range config.Accounts {
		found := false
		for _, a := range catalog {
			if a.ID == requested.ID {
				if a.Unsupported != "" {
					t.Fatalf("account %d: %s", a.ID, a.Unsupported)
				}
				if previous := settings.Models[a.Platform]; previous != "" && previous != requested.Model {
					t.Fatal("accounts on the same platform must use the same model in a batch")
				}
				settings.Models[a.Platform] = requested.Model
				ids = append(ids, a.ID)
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("account %d not found", requested.ID)
		}
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	defer transport.CloseIdleConnections()
	gateway := &Gateway{base: env("EVAL_SUB2API_URL", "http://sub2api:8080"), client: &http.Client{Transport: transport, Timeout: 15 * time.Second}}
	var login struct {
		Token string `json:"access_token"`
	}
	if gateway.call(ctx, "POST", "/api/v1/auth/login", map[string]string{"email": config.Email, "password": config.Password}, nil, &login) != nil || login.Token == "" {
		t.Fatal("live administrator login failed")
	}
	store := testStore(t)
	evaluator := &Evaluator{}
	engine := &Engine{store: store, load: source.credential, evaluate: evaluator.evaluate, wake: make(chan struct{}, 1)}
	done := make(chan error, 1)
	go func() { done <- engine.loop(ctx) }()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Error("worker persistence failed")
		}
	}()
	server := httptest.NewServer((&Server{source: source, store: store, engine: engine, gateway: gateway}).handler())
	defer server.Close()
	call := func(method, path string, body, out any, want int) {
		t.Helper()
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal("cannot encode test request")
		}
		req, err := http.NewRequestWithContext(ctx, method, server.URL+"/account-eval/api/"+path, bytes.NewReader(b))
		if err != nil {
			t.Fatal("cannot build test request")
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+login.Token)
		resp, err := server.Client().Do(req)
		if err != nil {
			t.Fatal("workbench HTTP request failed")
		}
		defer resp.Body.Close()
		if resp.StatusCode != want {
			t.Fatalf("%s %s: HTTP %d", method, path, resp.StatusCode)
		}
		if out != nil && json.NewDecoder(resp.Body).Decode(out) != nil {
			t.Fatal("cannot decode workbench response")
		}
	}
	call("GET", "catalog", nil, nil, 200)
	suite := Suite{Name: "真实请求验收", Questions: []Question{
		{Name: "逻辑", Prompt: "甲比乙高，乙比丙高。谁最矮？只回答名字。", Format: "text"},
		{Name: "SVG HTML", Prompt: "只输出一个可直接打开的完整 HTML，用内联 SVG 绘制鹈鹕骑自行车，并用 CSS 动画让车轮旋转。使用简单几何图形，不使用外部资源，代码保持在 60 行以内。", Format: "html"},
	}}
	call("POST", "suites", suite, &suite, 200)
	var run Run
	call("POST", "runs", map[string]any{"suite_id": suite.ID, "account_ids": ids, "settings": settings}, &run, 201)
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for run.Status == "queued" || run.Status == "running" {
		select {
		case <-ctx.Done():
			t.Fatal("live batch timed out")
		case <-ticker.C:
			run, err = store.run(ctx, run.ID)
			if err != nil {
				t.Fatal("cannot read live batch")
			}
		}
	}
	var detail struct {
		Run     Run      `json:"run"`
		Results []Result `json:"results"`
	}
	call("GET", "runs/"+run.ID, nil, &detail, 200)
	if detail.Run.Status != "completed" {
		t.Errorf("live batch ended with status %s", detail.Run.Status)
	}
	if len(detail.Results) != len(ids)*2 {
		t.Fatal("batch result count mismatch")
	}
	for _, summary := range detail.Results {
		var result Result
		call("GET", fmt.Sprintf("results/%d", summary.ID), nil, &result, 200)
		t.Logf("account=%d question=%d model=%s status=%s duration_ms=%d input_tokens=%d output_tokens=%d answer_chars=%d html=%t error=%s", result.AccountID, result.Question+1, result.Model, result.Status, result.DurationMS, result.InputTokens, result.OutTokens, len([]rune(result.Answer)), result.HTML != "", result.Error)
		if result.Status != "completed" || strings.TrimSpace(result.Answer) == "" {
			t.Errorf("account %d question %d did not complete", result.AccountID, result.Question+1)
		}
		if result.Question == 0 && !strings.Contains(result.Answer, "丙") {
			t.Errorf("account %d: logic answer did not contain 丙", result.AccountID)
		}
		if result.Question == 1 && (result.HTML == "" || !strings.Contains(strings.ToLower(result.HTML), "<svg")) {
			t.Errorf("account %d: HTML/SVG answer missing", result.AccountID)
		}
	}
}
