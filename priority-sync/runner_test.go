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

type windowedFakeMetricsSource struct {
	fakeMetricsSource
	windows int
}

func (s *windowedFakeMetricsSource) LoadAccountMetricsWindows(context.Context, time.Time) ([]AccountMetrics, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.windows++
	result := make([]AccountMetrics, len(s.accounts))
	copy(result, s.accounts)
	return result, nil
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
	got := make(map[string]int, 1)
	for range 1 {
		update := <-updates
		got[update.id] = update.priority
	}
	if got["1"] != 70 || got["2"] != 0 {
		t.Fatalf("updates = %+v", got)
	}
}

func TestRunnerUsesFixedWindowSourceWhenAvailable(t *testing.T) {
	source := &windowedFakeMetricsSource{fakeMetricsSource: fakeMetricsSource{accounts: []AccountMetrics{{
		ID: 1, Name: "windowed", Status: "active", CurrentPriority: priorityNeutral,
	}}}}
	runner := NewRunner(testRunnerConfig(t, "http://127.0.0.1:1", true), source, http.DefaultClient, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	runner.tableWriter = io.Discard
	if err := runner.RunOnce(context.Background(), nowForTest()); err != nil {
		t.Fatal(err)
	}
	if source.windows != 1 {
		t.Fatalf("fixed-window source calls = %d, want 1", source.windows)
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

func TestRunnerExplorationExpiresAndRestoresDefaultAnchor(t *testing.T) {
	type update struct {
		id       string
		priority int
	}
	updates := make(chan update, 8)
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
	if got := <-updates; got.id != "9" || got.priority != priorityExplore {
		t.Fatalf("start update = %+v", got)
	}
	source.accounts[0].CurrentPriority = priorityExplore
	if err := runner.RunOnce(context.Background(), start.Add(explorationDuration)); err != nil {
		t.Fatal(err)
	}
	if runner.state.Exploration == nil || runner.state.Exploration.AccountID != 9 {
		t.Fatalf("exploration cleared before anchor restoration: %+v", runner.state)
	}
	if got := <-updates; got.id != "9" || got.priority != 40 {
		t.Fatalf("first restoration update = %+v", got)
	}
	source.accounts[0].CurrentPriority = 40
	if err := runner.RunOnce(context.Background(), start.Add(2*explorationDuration)); err != nil {
		t.Fatal(err)
	}
	if runner.state.Exploration != nil {
		t.Fatalf("exploration was not cleared after anchor restoration: %+v", runner.state)
	}
	if got := <-updates; got.id != "9" || got.priority != priorityNeutral {
		t.Fatalf("final restoration update = %+v", got)
	}
}

func TestRunnerExplorationWithEnoughEvidenceUsesNormalConfirmation(t *testing.T) {
	type update struct {
		id       string
		priority int
	}
	updates := make(chan update, 8)
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
	if got := <-updates; got.id != "9" || got.priority != priorityExplore {
		t.Fatalf("start update = %+v", got)
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
	if runner.state.Exploration == nil || len(updates) != 1 {
		t.Fatalf("exploration did not take the first confirmed step: state=%+v updates=%d", runner.state, len(updates))
	}
	if got := <-updates; got.id != "9" || got.priority != 40 {
		t.Fatalf("first scored step = %+v", got)
	}
	source.accounts[0].CurrentPriority = 40
	if err := runner.RunOnce(context.Background(), start.Add(30*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if runner.state.Exploration == nil || runner.state.Exploration.AccountID != 9 || len(updates) != 0 {
		t.Fatalf("second scored step bypassed confirmation: state=%+v updates=%d", runner.state, len(updates))
	}
	if err := runner.RunOnce(context.Background(), start.Add(40*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if runner.state.Exploration != nil {
		t.Fatalf("exploration did not finish at continuous target: %+v", runner.state)
	}
	if got := <-updates; got.id != "9" || got.priority != 50 {
		t.Fatalf("final scored update = %+v", got)
	}
}

func TestPrepareExplorationPreservesImmediateFailureDowngrade(t *testing.T) {
	now := nowForTest()
	accounts := []AccountMetrics{{
		ID: 9, Name: "failing-exploration", Status: "active", CurrentPriority: priorityExplore,
		SuccessfulRequests: 2, TerminalFailures: 3, TrailingTerminalFailures: 3,
		TotalTokens: 1_000_000, AccountCost: 1,
	}}
	recommendations := scoreAccounts(accounts, now, 5)
	runner := NewRunner(testRunnerConfig(t, "http://127.0.0.1:1", false), &fakeMetricsSource{}, http.DefaultClient, &syncState{
		Accounts: map[int64]accountState{},
		Exploration: &explorationState{
			AccountID:        9,
			OriginalPriority: priorityPoor,
			StartedAt:        timePtr(now.Add(-10 * time.Minute)),
		},
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	runner.prepareExploration(accounts, recommendations, now)
	if len(recommendations) != 1 {
		t.Fatalf("got %d recommendations", len(recommendations))
	}
	got := recommendations[0]
	if got.RecommendedPriority != priorityDegraded || !got.applyImmediately || !got.explorationEnd {
		t.Fatalf("exploration overwrote the immediate failure downgrade: %+v", got)
	}
}

func TestRunnerExplorationExpiryUsesMeasuredTargetWhenEvidenceIsReady(t *testing.T) {
	start := nowForTest()
	accounts := []AccountMetrics{
		{ID: 9, Name: "measured", Status: "active", CurrentPriority: priorityExplore, SuccessfulRequests: 5, TotalTokens: 1_000_000, AccountCost: 1, LatencyP90Ms: 100},
		{ID: 10, Name: "peer", Status: "active", CurrentPriority: priorityNeutral, SuccessfulRequests: 5, TotalTokens: 1_000_000, AccountCost: 2, LatencyP90Ms: 100},
	}
	recommendations := scoreAccounts(accounts, start.Add(explorationDuration), 5)
	runner := NewRunner(testRunnerConfig(t, "http://127.0.0.1:1", true), &fakeMetricsSource{}, http.DefaultClient, &syncState{
		Accounts: map[int64]accountState{},
		Exploration: &explorationState{
			AccountID:        9,
			OriginalPriority: priorityPoor,
			StartedAt:        timePtr(start),
		},
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	runner.prepareExploration(accounts, recommendations, start.Add(explorationDuration))
	var measured Recommendation
	for _, item := range recommendations {
		if item.ID == 9 {
			measured = item
			break
		}
	}
	if measured.RecommendedPriority != 14 || !measured.explorationEnd {
		t.Fatalf("expiry ignored measured target: %+v", measured)
	}
}

func TestRunnerExplorationDoesNotBlockMatureAccount(t *testing.T) {
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
		{ID: 1, Name: "cold", Status: "active", CurrentPriority: 90},
		{ID: 2, Name: "mature", Status: "active", CurrentPriority: 90, SuccessfulRequests: 5, TotalTokens: 1_000_000, AccountCost: 1, LatencyP90Ms: 100},
	}}
	runner := NewRunner(testRunnerConfig(t, server.URL, false), source, server.Client(), nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	runner.tableWriter = io.Discard
	if err := runner.RunOnce(context.Background(), nowForTest()); err != nil {
		t.Fatal(err)
	}
	if runner.state.Exploration == nil || runner.state.Exploration.AccountID != 1 {
		t.Fatalf("exploration did not start: %+v", runner.state)
	}
	got := make(map[string]int, 2)
	for range 2 {
		update := <-updates
		got[update.id] = update.priority
	}
	if got["1"] != priorityExplore {
		t.Fatalf("exploration update = %d, want %d", got["1"], priorityExplore)
	}
	if got["2"] != priorityDegraded {
		t.Fatalf("mature account update = %d, want %d", got["2"], priorityDegraded)
	}
}

func TestRunnerConfirmsLatestPriorityInSameDirection(t *testing.T) {
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
	config := testRunnerConfig(t, server.URL, false)
	config.Confirmations = 2
	runner := NewRunner(config, &fakeMetricsSource{}, server.Client(), nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	runner.tableWriter = io.Discard
	current := 90
	for index, want := range []int{70, 50, 30, 10} {
		first := Recommendation{ID: 21, Name: "oscillating", CurrentPriority: current, RecommendedPriority: 21, SuccessfulRequests: 5}
		if changed, pending := runner.applyRecommendations(context.Background(), []Recommendation{first}, "secret", nowForTest().Add(time.Duration(index*20)*time.Minute)); changed != 0 || pending != 1 {
			t.Fatalf("step %d first confirmation changed=%d pending=%d", index, changed, pending)
		}
		second := first
		second.RecommendedPriority = 10
		if changed, pending := runner.applyRecommendations(context.Background(), []Recommendation{second}, "secret", nowForTest().Add(time.Duration(index*20+10)*time.Minute)); changed != 1 || pending != 0 {
			t.Fatalf("step %d second confirmation changed=%d pending=%d", index, changed, pending)
		}
		if got := <-updates; got != want {
			t.Fatalf("step %d updated priority=%d, want %d", index, got, want)
		}
		current = want
	}
	if state := runner.state.Accounts[21]; state.CandidatePriority != 0 || state.CandidateCount != 0 {
		t.Fatalf("applied candidate state=%+v", state)
	}
}

func TestRunnerConfirmationResetsWhenPriorityDirectionChanges(t *testing.T) {
	runner := NewRunner(testRunnerConfig(t, "http://127.0.0.1:1", true), &fakeMetricsSource{}, http.DefaultClient, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	runner.tableWriter = io.Discard
	first := Recommendation{ID: 22, Name: "direction-change", CurrentPriority: 90, RecommendedPriority: 21, SuccessfulRequests: 5}
	if changed, pending := runner.applyRecommendations(context.Background(), []Recommendation{first}, "", nowForTest()); changed != 0 || pending != 1 {
		t.Fatalf("first confirmation changed=%d pending=%d", changed, pending)
	}
	second := first
	second.RecommendedPriority = 100
	if changed, pending := runner.applyRecommendations(context.Background(), []Recommendation{second}, "", nowForTest().Add(10*time.Minute)); changed != 0 || pending != 1 {
		t.Fatalf("direction reversal changed=%d pending=%d", changed, pending)
	}
	state := runner.state.Accounts[22]
	if state.CandidatePriority != 100 || state.CandidateCount != 1 {
		t.Fatalf("reversed candidate state=%+v", state)
	}
}

func TestRampPriorityConvergesAcrossFormalBands(t *testing.T) {
	current := 90
	for _, want := range []int{70, 50, 30, 10} {
		current = rampPriority(current, 10)
		if current != want {
			t.Fatalf("ramp priority = %d, want %d", current, want)
		}
	}
}

func TestRampPriorityReentersFromUnavailableAtNeutral(t *testing.T) {
	if got := rampPriority(priorityUnavailable, priorityBest); got != priorityNeutral {
		t.Fatalf("re-entry priority = %d, want %d", got, priorityNeutral)
	}
}

func TestRampPriorityDoesNotOvershootPoorRecoveryTarget(t *testing.T) {
	if got := rampPriority(priorityUnavailable, priorityPoor); got != priorityPoor {
		t.Fatalf("poor recovery priority = %d, want %d", got, priorityPoor)
	}
}

func TestRunnerExplorationOnlyDefersOtherColdAccountPromotions(t *testing.T) {
	type update struct {
		id       string
		priority int
	}
	updates := make(chan update, 2)
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

	config := testRunnerConfig(t, server.URL, false)
	config.Confirmations = 1
	runner := NewRunner(config, &fakeMetricsSource{}, server.Client(), &syncState{
		Accounts:    map[int64]accountState{},
		Exploration: &explorationState{AccountID: 1, StartedAt: timePtr(nowForTest())},
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	runner.tableWriter = io.Discard
	recommendations := []Recommendation{
		{ID: 1, CurrentPriority: priorityExplore, RecommendedPriority: priorityExplore, Exploration: true},
		{ID: 2, CurrentPriority: priorityBest, RecommendedPriority: priorityGood},
		{ID: 3, CurrentPriority: priorityPoor, RecommendedPriority: priorityGood},
	}

	changed, pending := runner.applyRecommendations(context.Background(), recommendations, "secret", nowForTest())
	if changed != 1 || pending != 1 {
		t.Fatalf("changed=%d pending=%d", changed, pending)
	}
	got := <-updates
	if got.id != "2" || got.priority != priorityGood {
		t.Fatalf("update = %+v", got)
	}
	if recommendations[2].ApplyStatus != "deferred-exploration" {
		t.Fatalf("cold promotion status = %q", recommendations[2].ApplyStatus)
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

func TestRunnerRetainsStateForAccountMissingFromCurrentSnapshot(t *testing.T) {
	runner := NewRunner(testRunnerConfig(t, "http://127.0.0.1:1", false), &fakeMetricsSource{}, http.DefaultClient, &syncState{
		Accounts: map[int64]accountState{
			99: {CandidatePriority: priorityBest, CandidateCount: 1, LastCostPerMillion: 1.25, HasCostBaseline: true},
		},
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	runner.tableWriter = io.Discard

	if changed, pending := runner.applyRecommendations(context.Background(), nil, "", nowForTest()); changed != 0 || pending != 0 {
		t.Fatalf("empty snapshot changed state: changed=%d pending=%d", changed, pending)
	}
	retained, ok := runner.state.Accounts[99]
	if !ok {
		t.Fatal("state for an account missing from the snapshot was deleted")
	}
	if retained.CandidatePriority != priorityBest || retained.CandidateCount != 1 || !retained.HasCostBaseline {
		t.Fatalf("retained state was modified: %+v", retained)
	}
	if retained.LastSeenAt == nil || !retained.LastSeenAt.Equal(nowForTest()) {
		t.Fatalf("missing account was not given a retention timestamp: %+v", retained)
	}
}

func TestPrepareExplorationReleasesMissingStartedAtLease(t *testing.T) {
	now := nowForTest()
	accounts := []AccountMetrics{{
		ID: 8, Name: "stale", Status: "active", CurrentPriority: priorityExplore,
	}}
	recommendations := scoreAccounts(accounts, now, 5)
	runner := NewRunner(testRunnerConfig(t, "http://127.0.0.1:1", false), &fakeMetricsSource{}, http.DefaultClient, &syncState{
		Exploration: &explorationState{AccountID: 8},
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	runner.prepareExploration(accounts, recommendations, now)
	got := recommendations[0]
	if got.RecommendedPriority != 50 || got.Reason != "探索超时，向默认优先级 50 缓慢回退" {
		t.Fatalf("missing StartedAt lease was not released: %+v", got)
	}
}

func TestPrepareExplorationKeepsMissingAccountUntilExpiry(t *testing.T) {
	now := nowForTest()
	started := now.Add(-10 * time.Minute)
	runner := NewRunner(testRunnerConfig(t, "http://127.0.0.1:1", false), &fakeMetricsSource{}, http.DefaultClient, &syncState{
		Exploration: &explorationState{AccountID: 8, StartedAt: &started},
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	recommendations := []Recommendation{{ID: 9, CurrentPriority: priorityPoor, RecommendedPriority: priorityPoor}}
	runner.prepareExploration([]AccountMetrics{{ID: 9, Status: "active", CurrentPriority: priorityPoor}}, recommendations, now)
	if runner.state.Exploration == nil || runner.state.Exploration.AccountID != 8 {
		t.Fatal("in-window missing account lease was discarded")
	}
	runner.prepareExploration([]AccountMetrics{{ID: 9, Status: "active", CurrentPriority: priorityPoor}}, recommendations, now.Add(explorationDuration+time.Minute))
	if runner.state.Exploration != nil {
		t.Fatalf("expired missing account lease was retained: %+v", runner.state.Exploration)
	}
}

func TestRunnerPrunesExpiredExplorationAccountState(t *testing.T) {
	now := nowForTest()
	started := now.Add(-explorationDuration - time.Minute)
	lastSeen := now.Add(-stateRetentionDuration - time.Minute)
	runner := NewRunner(testRunnerConfig(t, "http://127.0.0.1:1", false), &fakeMetricsSource{}, http.DefaultClient, &syncState{
		Accounts: map[int64]accountState{
			8: {LastSeenAt: &lastSeen, CandidatePriority: priorityExplore, CandidateCount: 1},
		},
		Exploration: &explorationState{AccountID: 8, StartedAt: &started},
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	runner.tableWriter = io.Discard
	runner.applyRecommendations(context.Background(), nil, "", now)
	if _, ok := runner.state.Accounts[8]; ok {
		t.Fatalf("expired exploration account state was retained: %+v", runner.state.Accounts[8])
	}
	if runner.state.Exploration == nil || runner.state.Exploration.AccountID != 8 {
		t.Fatalf("expired exploration lease was dropped before the next snapshot: %+v", runner.state.Exploration)
	}
}

func TestRunnerPrunesStateAfterRetentionPeriod(t *testing.T) {
	lastSeen := nowForTest().Add(-stateRetentionDuration - time.Minute)
	runner := NewRunner(testRunnerConfig(t, "http://127.0.0.1:1", false), &fakeMetricsSource{}, http.DefaultClient, &syncState{
		Accounts: map[int64]accountState{
			99: {LastSeenAt: &lastSeen, CandidatePriority: priorityBest, CandidateCount: 1},
		},
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	runner.tableWriter = io.Discard

	runner.applyRecommendations(context.Background(), nil, "", nowForTest())
	if _, ok := runner.state.Accounts[99]; ok {
		t.Fatalf("stale account state was retained: %+v", runner.state.Accounts[99])
	}
}

func TestRunnerGatesPromotionBelowEightPercentCostAdvantage(t *testing.T) {
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
	config := testRunnerConfig(t, server.URL, false)
	config.Confirmations = 1
	runner := NewRunner(config, &fakeMetricsSource{}, server.Client(), nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	runner.tableWriter = io.Discard
	recommendation := Recommendation{ID: 1, Name: "slightly-cheaper", CurrentPriority: priorityPoor, RecommendedPriority: priorityBest,
		CostPerMillionTokens: 1, CostAdvantage: 0.05, PoolCount: 1, costAdvantageKnown: true}
	if changed, pending := runner.applyRecommendations(context.Background(), []Recommendation{recommendation}, "secret", nowForTest()); changed != 0 || pending != 1 {
		t.Fatalf("promotion below eight percent was not gated: changed=%d pending=%d", changed, pending)
	}
	if len(updates) != 0 {
		t.Fatal("promotion below eight percent cost advantage was applied")
	}
}

func TestRunnerDoesNotGatePromotionWithoutPeerCostEvidence(t *testing.T) {
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
	config := testRunnerConfig(t, server.URL, false)
	config.Confirmations = 1
	runner := NewRunner(config, &fakeMetricsSource{}, server.Client(), nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	runner.tableWriter = io.Discard
	recommendation := Recommendation{ID: 1, Name: "single-pool", CurrentPriority: priorityPoor, RecommendedPriority: priorityBest,
		CostPerMillionTokens: 1, CostAdvantage: 0, PoolCount: 1}
	if changed, pending := runner.applyRecommendations(context.Background(), []Recommendation{recommendation}, "secret", nowForTest()); changed != 1 || pending != 0 {
		t.Fatalf("promotion without peer evidence was incorrectly gated: changed=%d pending=%d", changed, pending)
	}
	if got := <-updates; got != priorityDegraded {
		t.Fatalf("promotion step = %d, want %d", got, priorityDegraded)
	}
}

func TestRunnerRequiresTwoQualifyingCyclesBeforePromotion(t *testing.T) {
	updates := make(chan int, 2)
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
	config := testRunnerConfig(t, server.URL, false)
	config.Confirmations = 2
	runner := NewRunner(config, &fakeMetricsSource{}, server.Client(), nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	runner.tableWriter = io.Discard
	start := nowForTest()
	recommendation := Recommendation{ID: 1, Name: "cheap", CurrentPriority: priorityPoor, RecommendedPriority: priorityBest,
		CostPerMillionTokens: 1, CostAdvantage: 0.20, PoolCount: 1, costAdvantageKnown: true}
	if changed, pending := runner.applyRecommendations(context.Background(), []Recommendation{recommendation}, "secret", start); changed != 0 || pending != 1 {
		t.Fatalf("first promotion cycle changed=%d pending=%d", changed, pending)
	}
	if len(updates) != 0 {
		t.Fatal("promotion applied before confirmation")
	}
	if changed, pending := runner.applyRecommendations(context.Background(), []Recommendation{recommendation}, "secret", start.Add(10*time.Minute)); changed != 1 || pending != 0 {
		t.Fatalf("second promotion cycle changed=%d pending=%d", changed, pending)
	}
	if len(updates) != 1 {
		t.Fatalf("promotion was not applied after two qualifying cycles: %d", len(updates))
	}
}

func TestRunnerFreezesPromotionOnCostAndCacheShock(t *testing.T) {
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
	config := testRunnerConfig(t, server.URL, false)
	config.Confirmations = 1
	runner := NewRunner(config, &fakeMetricsSource{}, server.Client(), &syncState{
		Accounts: map[int64]accountState{1: {
			LastCostPerMillion: 1, HasCostBaseline: true,
			LastCacheHitRate: 0.20, HasCacheBaseline: true,
		}},
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	runner.tableWriter = io.Discard
	now := nowForTest()
	recommendation := Recommendation{ID: 1, Name: "shock", CurrentPriority: priorityPoor, RecommendedPriority: priorityBest,
		CostPerMillionTokens: 1.4, CostAdvantage: 0.5, CacheHitRate: 0.50, CacheHitRateKnown: true, PoolCount: 1}
	if changed, pending := runner.applyRecommendations(context.Background(), []Recommendation{recommendation}, "secret", now); changed != 0 || pending != 1 {
		t.Fatalf("cost/cache shock was not frozen: changed=%d pending=%d", changed, pending)
	}
	if runner.state.Accounts[1].PromotionFrozenCycles != promotionFreezeCycles {
		t.Fatalf("freeze cycles = %d, want %d", runner.state.Accounts[1].PromotionFrozenCycles, promotionFreezeCycles)
	}
	for cycle := 1; cycle <= promotionFreezeCycles; cycle++ {
		if changed, pending := runner.applyRecommendations(context.Background(), []Recommendation{recommendation}, "secret", now.Add(time.Duration(cycle)*10*time.Minute)); changed != 0 || pending != 1 {
			t.Fatalf("freeze cycle %d changed=%d pending=%d", cycle, changed, pending)
		}
	}
	if changed, pending := runner.applyRecommendations(context.Background(), []Recommendation{recommendation}, "secret", now.Add(30*time.Minute)); changed != 1 || pending != 0 {
		t.Fatalf("promotion did not resume after two freeze cycles: changed=%d pending=%d", changed, pending)
	}
	if got := <-updates; got != priorityDegraded {
		t.Fatalf("promotion step = %d, want %d", got, priorityDegraded)
	}
}

func TestRunnerAllowsDowngradeDuringPromotionFreeze(t *testing.T) {
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
	runner := NewRunner(testRunnerConfig(t, server.URL, false), &fakeMetricsSource{}, server.Client(), &syncState{
		Accounts: map[int64]accountState{1: {PromotionFrozenCycles: 2}},
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	runner.tableWriter = io.Discard
	recommendation := Recommendation{ID: 1, Name: "degrade", CurrentPriority: priorityBest, RecommendedPriority: priorityPoor,
		CostPerMillionTokens: 1, CostAdvantage: 0, PoolCount: 1}
	if changed, pending := runner.applyRecommendations(context.Background(), []Recommendation{recommendation}, "secret", nowForTest()); changed != 1 || pending != 0 {
		t.Fatalf("downgrade was blocked during freeze: changed=%d pending=%d", changed, pending)
	}
	if got := <-updates; got != priorityGood {
		t.Fatalf("downgrade step = %d, want %d", got, priorityGood)
	}
}
