package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"
)

type browserSource struct{}

func (browserSource) catalog(context.Context) ([]Account, []Group, error) {
	until := time.Now().Add(time.Hour)
	return []Account{
		{ID: 1, Name: "OpenAI Alpha", Platform: "openai", Type: "apikey", Status: "active", Schedulable: true, GroupIDs: []int64{1, 2}, Models: []string{"account-alias"}, ModelMetadata: map[string]ModelMetadata{"gpt-6.1-sol": {SupportedReasoningLevels: []string{"low", "medium", "high", "max"}}}},
		{ID: 2, Name: "OpenAI Beta", Platform: "openai", Type: "api_key", Status: "active", GroupIDs: []int64{1}, ModelMetadata: map[string]ModelMetadata{"gpt-6.1-sol": {SupportedReasoningLevels: []string{"high"}}}},
		{ID: 3, Name: "Claude Gamma", Platform: "anthropic", Type: "apikey", Status: "active", Schedulable: true, GroupIDs: []int64{2}},
		{ID: 4, Name: "OpenAI Delta", Platform: "openai", Type: "oauth", Status: "active", Schedulable: true, RateLimitResetAt: &until, PrivacyMode: "training_off", GroupIDs: []int64{1}},
		{ID: 5, Name: "OpenAI Epsilon", Platform: "openai", Type: "oauth", Status: "active", Schedulable: true, RateLimitResetAt: &until, TempUnschedulableUntil: &until, PrivacyMode: "training_set_cf_blocked", GroupIDs: []int64{}, Models: []string{"gpt-6-sol"}},
		{ID: 6, Name: "Claude Zeta", Platform: "anthropic", Type: "setup-token", Status: "inactive", Schedulable: true, PrivacyMode: "training_set_failed", GroupIDs: []int64{}},
		{ID: 7, Name: "Bedrock Eta", Platform: "anthropic", Type: "bedrock", Status: "error", Schedulable: true, GroupIDs: []int64{2}, Unsupported: "暂不支持此账户协议"},
	}, []Group{{ID: 1, Name: "逻辑测试组", Models: map[string][]string{"openai": {"gpt-6.1-sol", "gpt-6-sol"}}}, {ID: 2, Name: "创作测试组", Models: map[string][]string{"anthropic": {"claude-sonnet"}, "openai": {"gpt-6.1-sol"}}}}, nil
}

// An opt-in fixture for browser verification; never included in the service binary.
func TestWorkbenchBrowserFixture(t *testing.T) {
	addr := os.Getenv("EVAL_BROWSER_ADDR")
	if addr == "" {
		t.Skip("EVAL_BROWSER_ADDR is unset")
	}
	s := testStore(t)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/auth/me" {
			if r.Header.Get("Authorization") != "Bearer browser-fixture" {
				w.WriteHeader(401)
				return
			}
			fmt.Fprint(w, `{"code":0,"data":{"role":"admin","status":"active"}}`)
			return
		}
		if r.Method == "GET" {
			fmt.Fprint(w, `{"code":0,"data":{"custom_menu_items":[]}}`)
		} else {
			fmt.Fprint(w, `{"code":0,"data":{}}`)
		}
	}))
	defer up.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	e := &Engine{store: s, wake: make(chan struct{}, 1), load: func(_ context.Context, id int64) (CredentialAccount, error) {
		return CredentialAccount{Account: Account{ID: id}}, nil
	}, evaluate: func(ctx context.Context, a CredentialAccount, _ Settings, q Question) Result {
		select {
		case <-time.After(8 * time.Second):
		case <-ctx.Done():
		}
		if a.ID == 2 {
			return Result{Status: "failed", Error: "模拟 HTTP 429"}
		}
		if q.Format == "html" {
			return Result{Status: "completed", Answer: "```html\n<html><body><svg viewBox=\"0 0 400 240\"><circle cx=\"120\" cy=\"160\" r=\"40\" fill=\"none\" stroke=\"#2863cb\" stroke-width=\"6\"/><circle cx=\"280\" cy=\"160\" r=\"40\" fill=\"none\" stroke=\"#2863cb\" stroke-width=\"6\"/><path d=\"M120 160L180 90L220 160Z M180 90H250L280 160\" stroke=\"#2863cb\" fill=\"none\" stroke-width=\"5\"/><ellipse cx=\"190\" cy=\"70\" rx=\"35\" ry=\"20\" fill=\"#e8b75e\"/></svg></body></html>\n```", HTML: "<html><body><svg viewBox=\"0 0 400 240\"><circle cx=\"120\" cy=\"160\" r=\"40\" fill=\"none\" stroke=\"#2863cb\" stroke-width=\"6\"/><circle cx=\"280\" cy=\"160\" r=\"40\" fill=\"none\" stroke=\"#2863cb\" stroke-width=\"6\"/><path d=\"M120 160L180 90L220 160Z M180 90H250L280 160\" stroke=\"#2863cb\" fill=\"none\" stroke-width=\"5\"/><ellipse cx=\"190\" cy=\"70\" rx=\"35\" ry=\"20\" fill=\"#e8b75e\"/></svg></body></html>", OutTokens: 500, DurationMS: 8000}
		}
		return Result{Status: "completed", Answer: "独立问题回答：" + q.Prompt, OutTokens: 25, DurationMS: 8000}
	}}
	done := make(chan error, 1)
	go func() { done <- e.loop(ctx) }()
	defer func() { cancel(); <-done }()
	h := (&Server{store: s, source: browserSource{}, engine: e, gateway: &Gateway{base: up.URL, client: up.Client()}}).handler()
	mux := http.NewServeMux()
	mux.Handle("/", h)
	mux.HandleFunc("/login", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html;charset=utf-8")
		fmt.Fprint(w, `<html><button onclick="localStorage.setItem('auth_token','browser-fixture');location.href='/account-eval/'">登录测试管理员</button></html>`)
	})
	fmt.Println("Browser fixture: http://" + addr + "/account-eval/")
	if err := http.ListenAndServe(addr, mux); err != nil {
		t.Fatal(err)
	}
}
