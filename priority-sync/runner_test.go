package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

type fakeMetricsSource struct {
	mu       sync.Mutex
	accounts []AccountMetrics
	key      string
}

func (s *fakeMetricsSource) LoadAccountMetrics(context.Context, time.Time, time.Duration) ([]AccountMetrics, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	result := make([]AccountMetrics, len(s.accounts))
	copy(result, s.accounts)
	return result, nil
}

func (s *fakeMetricsSource) AdminAPIKey(context.Context) (string, error) { return s.key, nil }

func testRunnerConfig(t *testing.T, endpoint string, dryRun bool) Config {
	t.Helper()
	directory := t.TempDir()
	return Config{
		Sub2APIURL:     endpoint,
		AdminAPIKey:    "secret",
		Interval:       10 * time.Minute,
		Window:         time.Hour,
		ChangeCooldown: 0,
		MinSamples:     5,
		Confirmations:  1,
		DryRun:         dryRun,
		StateFile:      filepath.Join(directory, "state.json"),
		ReportFile:     filepath.Join(directory, "report.json"),
	}
}

func TestRunnerAppliesScoredPriorityThroughAdminAPI(t *testing.T) {
	type update struct {
		id       string
		priority int
	}
	updates := make(chan update, 4)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload struct {
			Priority int `json:"priority"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		updates <- update{id: filepath.Base(r.URL.Path), priority: payload.Priority}
		_, _ = io.WriteString(w, `{"code":0}`)
	}))
	defer server.Close()
	source := &fakeMetricsSource{accounts: []AccountMetrics{
		{ID: 1, Name: "cheap-fast", Status: "active", CurrentPriority: 90, SuccessfulRequests: 5, TotalTokens: 1_000_000, AccountCost: 1, LatencyP90Ms: 100},
		{ID: 2, Name: "expensive-slow", Status: "active", CurrentPriority: 90, SuccessfulRequests: 5, TotalTokens: 1_000_000, AccountCost: 2, LatencyP90Ms: 10_000},
	}}
	runner := NewRunner(testRunnerConfig(t, server.URL, false), source, server.Client(), nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	runner.tableWriter = io.Discard
	if err := runner.RunOnce(context.Background(), nowForTest()); err != nil {
		t.Fatal(err)
	}
	if len(updates) != 1 {
		t.Fatalf("got %d updates", len(updates))
	}
	got := <-updates
	if got.id != "1" || got.priority != priorityBest {
		t.Fatalf("update = %+v", got)
	}
}

func TestRunnerDryRunDoesNotPersistExplorationOrCallAdminAPI(t *testing.T) {
	called := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		_, _ = io.WriteString(w, `{"code":0}`)
	}))
	defer server.Close()
	source := &fakeMetricsSource{accounts: []AccountMetrics{{ID: 4, Name: "new", Status: "active", CurrentPriority: 90}}}
	runner := NewRunner(testRunnerConfig(t, server.URL, true), source, server.Client(), nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	runner.tableWriter = io.Discard
	if err := runner.RunOnce(context.Background(), nowForTest()); err != nil {
		t.Fatal(err)
	}
	if called {
		t.Fatal("dry-run called Admin API")
	}
	if runner.state.Exploration != nil || runner.state.ExplorationCursor != 0 {
		t.Fatalf("dry-run mutated exploration state: %+v", runner.state)
	}
}

func TestRunnerExplorationExpiresAndRestoresOriginalPriority(t *testing.T) {
	updates := make(chan int, 4)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload struct {
			Priority int `json:"priority"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		updates <- payload.Priority
		_, _ = io.WriteString(w, `{"code":0}`)
	}))
	defer server.Close()
	source := &fakeMetricsSource{accounts: []AccountMetrics{
		{ID: 9, Name: "explore-me", Status: "active", CurrentPriority: 90},
		{ID: 10, Name: "next", Status: "active", CurrentPriority: 90},
	}}
	runner := NewRunner(testRunnerConfig(t, server.URL, false), source, server.Client(), nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	runner.tableWriter = io.Discard
	start := nowForTest()
	if err := runner.RunOnce(context.Background(), start); err != nil {
		t.Fatal(err)
	}
	if runner.state.Exploration == nil || runner.state.Exploration.AccountID != 9 {
		t.Fatalf("exploration did not start: %+v", runner.state)
	}
	if got := <-updates; got != priorityExplore {
		t.Fatalf("start priority = %d", got)
	}
	source.accounts[0].CurrentPriority = priorityExplore
	if err := runner.RunOnce(context.Background(), start.Add(explorationDuration)); err != nil {
		t.Fatal(err)
	}
	if runner.state.Exploration != nil {
		t.Fatalf("exploration was not cleared: %+v", runner.state)
	}
	if got := <-updates; got != 90 {
		t.Fatalf("restore priority = %d", got)
	}
}

func TestRunnerExplorationWithEnoughEvidenceUsesNormalConfirmation(t *testing.T) {
	updates := make(chan int, 4)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload struct {
			Priority int `json:"priority"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		updates <- payload.Priority
		_, _ = io.WriteString(w, `{"code":0}`)
	}))
	defer server.Close()
	source := &fakeMetricsSource{accounts: []AccountMetrics{
		{ID: 9, Name: "explore-me", Status: "active", CurrentPriority: 90},
		{ID: 10, Name: "next", Status: "active", CurrentPriority: 90},
	}}
	runner := NewRunner(testRunnerConfig(t, server.URL, false), source, server.Client(), nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	runner.tableWriter = io.Discard
	runner.config.Confirmations = 2
	start := nowForTest()
	if err := runner.RunOnce(context.Background(), start); err != nil {
		t.Fatal(err)
	}
	if got := <-updates; got != priorityExplore {
		t.Fatalf("start priority = %d", got)
	}
	source.accounts[0].CurrentPriority = priorityExplore
	source.accounts[0].SuccessfulRequests = 5
	source.accounts[0].TotalTokens = 1_000_000
	source.accounts[0].AccountCost = 1
	source.accounts[0].LatencyP90Ms = 100
	if err := runner.RunOnce(context.Background(), start.Add(10*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if runner.state.Exploration == nil {
		t.Fatalf("exploration state was cleared before normal confirmation: %+v", runner.state)
	}
	if len(updates) != 0 {
		t.Fatal("normal scoring bypassed confirmation and wrote immediately")
	}
	if err := runner.RunOnce(context.Background(), start.Add(20*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if runner.state.Exploration != nil || len(updates) != 1 {
		t.Fatalf("exploration did not transition after confirmation: state=%+v updates=%d", runner.state, len(updates))
	}
}

func TestRunnerExplorationFailureDoesNotPersistState(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "upstream unavailable", http.StatusBadGateway)
	}))
	defer server.Close()
	source := &fakeMetricsSource{accounts: []AccountMetrics{{ID: 12, Name: "candidate", Status: "active", CurrentPriority: 90}}}
	runner := NewRunner(testRunnerConfig(t, server.URL, false), source, server.Client(), nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	runner.tableWriter = io.Discard
	if err := runner.RunOnce(context.Background(), nowForTest()); err != nil {
		t.Fatal(err)
	}
	if runner.state.Exploration != nil || runner.state.ExplorationCursor != 0 {
		t.Fatalf("failed exploration changed durable state: %+v", runner.state)
	}
	if got := runner.state.Accounts[12].LastExploredAt; got != nil {
		t.Fatalf("failed exploration recorded cooldown: %v", got)
	}
}

func TestRunnerHardExcludedAccountAppliesImmediately(t *testing.T) {
	updates := make(chan int, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload struct {
			Priority int `json:"priority"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		updates <- payload.Priority
		_, _ = io.WriteString(w, `{"code":0}`)
	}))
	defer server.Close()
	cooling := nowForTest().Add(time.Minute)
	source := &fakeMetricsSource{accounts: []AccountMetrics{{
		ID: 13, Name: "cooling", Status: "active", CurrentPriority: priorityBest, RateLimitResetAt: &cooling,
	}}}
	config := testRunnerConfig(t, server.URL, false)
	config.Confirmations = 3
	runner := NewRunner(config, source, server.Client(), nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	runner.tableWriter = io.Discard
	if err := runner.RunOnce(context.Background(), nowForTest()); err != nil {
		t.Fatal(err)
	}
	if got := <-updates; got != priorityUnavailable {
		t.Fatalf("hard exclusion priority = %d, want %d", got, priorityUnavailable)
	}
}

func TestRunnerAdminFailureDoesNotRecordAppliedState(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "temporarily unavailable", http.StatusBadGateway)
	}))
	defer server.Close()
	source := &fakeMetricsSource{accounts: []AccountMetrics{{
		ID: 14, Name: "stable", Status: "active", CurrentPriority: priorityPoor,
		SuccessfulRequests: 5, TotalTokens: 1_000_000, AccountCost: 1, LatencyP90Ms: 100,
	}}}
	config := testRunnerConfig(t, server.URL, false)
	config.Confirmations = 1
	runner := NewRunner(config, source, server.Client(), nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	runner.tableWriter = io.Discard
	if err := runner.RunOnce(context.Background(), nowForTest()); err != nil {
		t.Fatal(err)
	}
	state := runner.state.Accounts[14]
	if state.LastAppliedAt != nil || state.LastApplied != 0 {
		t.Fatalf("failed update recorded as applied: %+v", state)
	}
	if state.CandidatePriority == 0 || state.CandidateCount != 1 {
		t.Fatalf("failed update did not retain retry candidate: %+v", state)
	}
}
