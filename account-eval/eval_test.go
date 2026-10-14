package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	s, err := openStore(context.Background(), filepath.Join(t.TempDir(), "eval.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.db.Close() })
	return s
}

func testRun(id string) Run {
	return Run{ID: id, Name: id, Status: "queued", CreatedAt: time.Now(), Suite: Suite{ID: newID(), Name: "SVG", Questions: []Question{{Name: "one", Prompt: "one", Format: "html"}, {Name: "two", Prompt: "two", Format: "text"}}}, Accounts: []Account{{ID: 1, Name: "A", Platform: "openai"}, {ID: 2, Name: "B", Platform: "openai"}}, Settings: Settings{Models: map[string]string{"openai": "test-model"}, Concurrency: 2, Timeout: 30}}
}
func addRun(t *testing.T, s *Store, r Run) {
	t.Helper()
	cells := []Result{}
	for _, a := range r.Accounts {
		for q := range r.Suite.Questions {
			cells = append(cells, Result{AccountID: a.ID, Question: q})
		}
	}
	if err := s.createRun(context.Background(), r, cells); err != nil {
		t.Fatal(err)
	}
}

func TestStreamCompletionAndPartialAnswers(t *testing.T) {
	tests := []struct{ name, protocol, body, status, answer string }{
		{"responses complete", "responses", "data: {\"type\":\"response.output_text.delta\",\"delta\":\"<html>yes</html>\"}\n\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"usage\":{\"input_tokens\":4,\"output_tokens\":9}}}\n\n", "completed", "<html>yes</html>"},
		{"responses interrupted", "responses", "data: {\"type\":\"response.output_text.delta\",\"delta\":\"partial\"}\n\n", "incomplete", "partial"},
		{"responses error redacted", "responses", "data: {\"type\":\"response.failed\",\"error\":{\"message\":\"secret-token\"}}\n\n", "failed", ""},
		{"chat stop and done", "chat", "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":null}]}\n\ndata: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n", "completed", "ok"},
		{"chat output limit", "chat", "data: {\"choices\":[{\"delta\":{\"content\":\"partial\"},\"finish_reason\":\"length\"}]}\n\ndata: [DONE]\n\n", "incomplete", "partial"},
		{"chat done alone", "chat", "data: [DONE]\n\n", "failed", ""},
		{"chat no final event", "chat", "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\n", "incomplete", "ok"},
		{"anthropic complete", "anthropic", "data: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"yes\"}}\n\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"}}\n\ndata: {\"type\":\"message_stop\"}\n\n", "completed", "yes"},
		{"multiline data", "responses", "data: {\"type\":\"response.completed\",\n data ignored\ndata: \"response\":{\"status\":\"completed\",\"output\":[{\"content\":[{\"type\":\"output_text\",\"text\":\"fallback\"}]}]}}\n\n", "completed", "fallback"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := Result{}
			readAnswer(strings.NewReader(tc.body), true, tc.protocol, &r)
			if r.Status != tc.status || r.Answer != tc.answer {
				t.Fatalf("got %+v", r)
			}
			if strings.Contains(r.Error, "secret-token") {
				t.Fatal("leaked provider error")
			}
		})
	}
	r := Result{}
	readAnswer(strings.NewReader(`{"candidates":[{"content":{"parts":[{"text":"hidden","thought":true},{"text":"visible"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":3,"candidatesTokenCount":5}}`), false, "gemini", &r)
	if r.Status != "completed" || r.Answer != "visible" || r.InputTokens != 3 || r.OutTokens != 5 {
		t.Fatalf("Gemini: %+v", r)
	}
}

func TestUpstreamStreamDetection(t *testing.T) {
	complete := "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"output\":[{\"content\":[{\"type\":\"output_text\",\"text\":\"ok\"}]}]}}\n\n"
	for _, tc := range []struct{ name, contentType, body, status string }{
		{"missing SSE header", "", complete, "completed"},
		{"plain text SSE", "text/plain", complete, "completed"},
		{"mislabelled SSE", "application/json", complete, "completed"},
		{"heartbeat SSE", "", ": keepalive\n\n" + complete, "completed"},
		{"JSON fallback", "application/json", `{"status":"completed","output":[{"content":[{"type":"output_text","text":"ok"}]}]}`, "completed"},
		{"truncated SSE", "", "event: response.created\ndata: {\"type\":\"response.created\"}\n\n", "incomplete"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := &http.Response{Header: http.Header{}, Body: io.NopCloser(strings.NewReader(tc.body))}
			defer resp.Body.Close()
			resp.Header.Set("Content-Type", tc.contentType)
			var r Result
			readUpstreamAnswer(resp, "responses", &r)
			if r.Status != tc.status || (tc.status == "completed" && r.Answer != "ok") {
				t.Fatalf("stream detection: %+v", r)
			}
		})
	}
}

func TestExactAccountRequestAndProxyBoundary(t *testing.T) {
	var received map[string]any
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer secret-key" || r.URL.Path != "/v1/responses" {
			t.Errorf("wrong account request")
		}
		json.NewDecoder(r.Body).Decode(&received)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"secret-key ```html\\n<svg></svg>\\n```\"}\n\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n")
	}))
	defer up.Close()
	a := CredentialAccount{Account: Account{ID: 7, Platform: "openai", Type: "api_key"}, Credentials: map[string]any{"api_key": "secret-key", "base_url": up.URL + "/v1", "model_mapping": map[string]any{"alias": "real-model"}}}
	s := Settings{Models: map[string]string{"openai": "alias"}, MaxTokens: 2000, Reasoning: "high"}
	r := (&Evaluator{allowPrivate: true}).evaluate(context.Background(), a, s, Question{Prompt: "draw a pelican", Format: "html"})
	if r.Status != "completed" || r.Model != "real-model" || strings.Contains(r.Answer, "secret-key") || r.HTML == "" {
		t.Fatalf("result: %+v", r)
	}
	if received["model"] != "real-model" || received["max_output_tokens"] != float64(2000) || obj(received["reasoning"])["effort"] != "high" || !strings.Contains(fmt.Sprint(received["input"]), "draw a pelican") {
		t.Fatalf("payload: %v", received)
	}
	r = (&Evaluator{}).evaluate(context.Background(), a, s, Question{Prompt: "blocked"})
	if r.Status != "failed" || !strings.Contains(r.Error, "允许范围") {
		t.Fatalf("private target allowed: %+v", r)
	}
	a.ProxyError = "账户代理不可用，已停止请求"
	r = (&Evaluator{allowPrivate: true}).evaluate(context.Background(), a, s, Question{Prompt: "must not reach"})
	if r.Error != a.ProxyError {
		t.Fatalf("proxy fail open: %+v", r)
	}
	a.ProxyError = ""
	a.ProxyURL = "socks4://127.0.0.1:9"
	r = (&Evaluator{allowPrivate: true}).evaluate(context.Background(), a, s, Question{Prompt: "must not reach"})
	if r.Status != "failed" {
		t.Fatal("invalid proxy fell back to direct")
	}
	a.ProxyURL = ""
	a.Extra = map[string]any{"openai_responses_supported": false}
	req, _, protocol, err := buildRequest(context.Background(), a, s, "chat prompt")
	if err != nil || protocol != "chat" || req.URL.Path != "/v1/chat/completions" {
		t.Fatalf("stored capability ignored: %v %s %v", req, protocol, err)
	}
	a.Extra["openai_responses_mode"] = "force_responses"
	a.Credentials["header_overrides"] = map[string]any{"authorization": "Bearer override-secret"}
	req, _, protocol, err = buildRequest(context.Background(), a, s, "responses prompt")
	if err != nil || protocol != "responses" || req.Header.Get("Authorization") != "Bearer override-secret" {
		t.Fatalf("mode/header override ignored: %v %s %v", req, protocol, err)
	}
	a.Platform = "openai"
	a.Type = "oauth"
	a.Credentials = map[string]any{"access_token": "oauth-secret", "chatgpt_account_id": "account-id"}
	s.MaxTokens = 0
	req, _, protocol, err = buildRequest(context.Background(), a, s, "full prompt")
	if err != nil {
		t.Fatal(err)
	}
	if protocol != "responses" || req.URL.String() != "https://chatgpt.com/backend-api/codex/responses" || req.Header.Get("ChatGPT-Account-ID") != "account-id" {
		t.Fatalf("OAuth request: %v", req)
	}
	b, _ := io.ReadAll(req.Body)
	if !strings.Contains(string(b), "full prompt") || strings.Contains(string(b), "max_output_tokens") {
		t.Fatal("OAuth payload missing prompt or has unsupported limit")
	}
}

func TestEvaluatorRedactsOverlappingAndHeaderOverrideSecrets(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		text := "<html>" + strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ") + " " + r.Header.Get("X-Upstream-Token") + " credential-password</html>"
		json.NewEncoder(w).Encode(map[string]any{"status": "completed", "output": []any{map[string]any{"content": []any{map[string]string{"type": "output_text", "text": text}}}}})
	}))
	defer up.Close()
	a := CredentialAccount{Account: Account{Platform: "openai", Type: "apikey"}, Credentials: map[string]any{
		"base_url": up.URL, "api_key": "api-secret-key", "access_token": "api-secret", "password": "credential-password",
		"header_overrides": map[string]any{"X-Upstream-Token": "override-token"},
	}}
	r := (&Evaluator{allowPrivate: true}).evaluate(context.Background(), a, Settings{Models: map[string]string{"openai": "test"}}, Question{Prompt: "echo", Format: "html"})
	want := "<html>[REDACTED] [REDACTED] [REDACTED]</html>"
	if r.Status != "completed" || r.Answer != want || r.HTML != want {
		t.Fatalf("secret echo retained: %+v", r)
	}
}

func TestEvaluatorTimeoutKeepsPartialAnswer(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"partial\"}\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer up.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	a := CredentialAccount{Account: Account{Platform: "openai", Type: "apikey"}, Credentials: map[string]any{"base_url": up.URL, "api_key": "test-key"}}
	r := (&Evaluator{allowPrivate: true}).evaluate(ctx, a, Settings{Models: map[string]string{"openai": "test"}}, Question{Prompt: "wait"})
	if r.Status != "incomplete" || r.Answer != "partial" || r.Error != "请求超时或已取消" {
		t.Fatalf("timeout misreported or partial answer lost: %+v", r)
	}
}

func TestSnapshotRecoveryKeepsCompletedAndQueuedBatches(t *testing.T) {
	ctx := context.Background()
	p := filepath.Join(t.TempDir(), "state.db")
	s, err := openStore(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	r := testRun(newID())
	addRun(t, s, r)
	queued := testRun(newID())
	addRun(t, s, queued)
	if err = s.saveSuite(ctx, r.Suite); err != nil {
		t.Fatal(err)
	}
	r.Suite.Questions[0].Prompt = "edited later"
	s.saveSuite(ctx, r.Suite)
	s.db.ExecContext(ctx, `UPDATE runs SET status='running' WHERE id=?`, r.ID)
	cells, _ := s.results(ctx, r.ID, false)
	s.db.ExecContext(ctx, `UPDATE results SET status='running' WHERE id=?`, cells[0].ID)
	out := Result{ID: cells[0].ID, Status: "completed", Answer: "saved answer", HTML: "<svg/>"}
	if err = s.finish(ctx, out); err != nil {
		t.Fatal(err)
	}
	s.db.ExecContext(ctx, `UPDATE results SET status='running' WHERE id=?`, cells[1].ID)
	s.db.Close()
	s, err = openStore(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	defer s.db.Close()
	got, _ := s.run(ctx, r.ID)
	if got.Status != "interrupted" || got.Suite.Questions[0].Prompt != "one" {
		t.Fatalf("snapshot: %+v", got)
	}
	rows, _ := s.results(ctx, r.ID, true)
	if rows[0].Answer != "saved answer" || rows[0].Status != "completed" {
		t.Fatal("completed result lost")
	}
	for _, c := range rows[1:] {
		if c.Status != "interrupted" {
			t.Fatalf("unsafe resume: %+v", c)
		}
	}
	q, _ := s.run(ctx, queued.ID)
	if q.Status != "queued" {
		t.Fatalf("queued batch lost: %+v", q)
	}
}

func TestBatchQueueParallelAccountsAndSequentialQuestions(t *testing.T) {
	s := testStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	first, second := testRun(newID()), testRun(newID())
	second.Suite.Questions[0].Prompt = "second"
	second.Suite.Questions[1].Prompt = "second2"
	addRun(t, s, first)
	addRun(t, s, second)
	var mu sync.Mutex
	active := map[int64]int{}
	order := map[int64][]string{}
	total, max := 0, 0
	firstFinished := 0
	e := &Engine{store: s, wake: make(chan struct{}, 1), load: func(_ context.Context, id int64) (CredentialAccount, error) {
		return CredentialAccount{Account: Account{ID: id}}, nil
	}, evaluate: func(ctx context.Context, a CredentialAccount, _ Settings, q Question) Result {
		mu.Lock()
		if active[a.ID] != 0 {
			t.Error("same account executed in parallel")
		}
		if strings.HasPrefix(q.Prompt, "second") && firstFinished != 4 {
			t.Error("batches overlapped")
		}
		active[a.ID]++
		total++
		if total > max {
			max = total
		}
		order[a.ID] = append(order[a.ID], q.Prompt)
		mu.Unlock()
		select {
		case <-time.After(20 * time.Millisecond):
		case <-ctx.Done():
		}
		mu.Lock()
		active[a.ID]--
		total--
		if q.Prompt == "one" || q.Prompt == "two" {
			firstFinished++
		}
		mu.Unlock()
		return Result{Status: "completed", Answer: q.Prompt}
	}}
	done := make(chan error, 1)
	go func() { done <- e.loop(ctx) }()
	deadline := time.Now().Add(3 * time.Second)
	for {
		r, _ := s.run(context.Background(), second.ID)
		if r.Status == "completed" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("queue stalled")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if max != 2 {
		t.Fatalf("max parallelism=%d", max)
	}
	for _, v := range order {
		if strings.Join(v, ",") != "one,two,second,second2" {
			t.Fatalf("order: %v", v)
		}
	}
}

func TestCancellationPreservesPartialAnswersAndSkipsRemaining(t *testing.T) {
	s := testStore(t)
	r := testRun(newID())
	r.Accounts = r.Accounts[:1]
	r.Settings.Concurrency = 1
	addRun(t, s, r)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan struct{})
	calls := 0
	e := &Engine{store: s, wake: make(chan struct{}, 1), load: func(_ context.Context, id int64) (CredentialAccount, error) {
		return CredentialAccount{Account: Account{ID: id}}, nil
	}, evaluate: func(ctx context.Context, _ CredentialAccount, _ Settings, _ Question) Result {
		calls++
		close(started)
		<-ctx.Done()
		return Result{Status: "incomplete", Answer: "partial preserved"}
	}}
	done := make(chan error, 1)
	go func() { done <- e.loop(ctx) }()
	<-started
	if err := e.cancelRun(context.Background(), r.ID); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		run, _ := s.run(context.Background(), r.ID)
		if run.Status == "cancelled" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("cancel stalled")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	rows, _ := s.results(context.Background(), r.ID, true)
	if calls != 1 || rows[0].Status != "cancelled" || rows[0].Answer != "partial preserved" || rows[1].Status != "cancelled" {
		t.Fatalf("cancel result: %+v", rows)
	}
}
