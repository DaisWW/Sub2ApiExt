package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
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
	return withQualifiedTwoHourCosts(s.accounts), nil
}

func (s *fakeMetricsSource) LoadAccountMetricsWindows(context.Context, time.Time) ([]AccountMetrics, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return withQualifiedTwoHourCosts(s.accounts), nil
}

func withQualifiedTwoHourCosts(accounts []AccountMetrics) []AccountMetrics {
	result := make([]AccountMetrics, len(accounts))
	copy(result, accounts)
	for index := range result {
		result[index].DecisionWindowsAvailable = true
		if result[index].Window30m != nil || result[index].Window2h != nil || result[index].TotalTokens <= 0 ||
			(positiveMetric(result[index].AccountCost) <= 0 && positiveMetric(result[index].ActualCost) <= 0) {
			continue
		}
		snapshot := accountMetricSnapshot(result[index])
		snapshot.PricedRequests = maxInt64(result[index].SuccessfulRequests, 0)
		snapshot.PricedTokens = maxInt64(result[index].TotalTokens, 0)
		result[index].Window2h = snapshot
	}
	return result
}

func (s *fakeMetricsSource) AdminAPIKey(context.Context) (string, error) { return s.key, nil }

func testRunnerConfig(t *testing.T, endpoint string, dryRun bool) Config {
	t.Helper()
	directory := t.TempDir()
	return Config{
		Sub2APIURL:         endpoint,
		AdminAPIKey:        "secret",
		Interval:           10 * time.Minute,
		Window:             time.Hour,
		ChangeCooldown:     0,
		MinSamples:         5,
		Confirmations:      1,
		ExplorationEnabled: true,
		DryRun:             dryRun,
		StateFile:          filepath.Join(directory, "state.json"),
		ReportFile:         filepath.Join(directory, "report.json"),
	}
}

func TestPrepareExplorationRequiresExplicitEnablement(t *testing.T) {
	now := nowForTest()
	accounts := []AccountMetrics{{ID: 9, Status: "active", CurrentPriority: priorityPoor}}
	recommendations := scoreAccounts(accounts, now, 5)
	config := testRunnerConfig(t, "http://127.0.0.1:1", false)
	config.ExplorationEnabled = false
	runner := NewRunner(config, &fakeMetricsSource{}, http.DefaultClient, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	runner.prepareExploration(accounts, recommendations, now)
	if runner.state.Exploration != nil || recommendations[0].Exploration || recommendations[0].RecommendedPriority != priorityPoor {
		t.Fatalf("disabled exploration changed recommendations: state=%+v recommendation=%+v", runner.state, recommendations[0])
	}
}

func TestPrepareExplorationRestoresActiveLeaseWhenDisabled(t *testing.T) {
	now := nowForTest()
	accounts := []AccountMetrics{{ID: 9, Status: "active", CurrentPriority: priorityExplore}}
	recommendations := scoreAccounts(accounts, now, 5)
	config := testRunnerConfig(t, "http://127.0.0.1:1", false)
	config.ExplorationEnabled = false
	runner := NewRunner(config, &fakeMetricsSource{}, http.DefaultClient, &syncState{
		Accounts:    map[int64]accountState{},
		Exploration: &explorationState{AccountID: 9, OriginalPriority: priorityPoor, StartedAt: timePtr(now)},
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	runner.prepareExploration(accounts, recommendations, now)
	got := recommendations[0]
	if got.RecommendedPriority != priorityPoor || !got.applyImmediately || !got.explorationEnd {
		t.Fatalf("disabled active exploration was not restored: %+v", got)
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

func TestRunnerLogsEvaluationWeights(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	runner := NewRunner(testRunnerConfig(t, "http://127.0.0.1:1", true), &fakeMetricsSource{}, http.DefaultClient, nil, logger)
	runner.tableWriter = io.Discard
	if err := runner.RunOnce(context.Background(), nowForTest()); err != nil {
		t.Fatal(err)
	}
	if output := logs.String(); !strings.Contains(output, "evaluation_weights=") || !strings.Contains(output, evaluationWeights) ||
		!strings.Contains(output, decisionWindowPolicy) || !strings.Contains(output, recoveryAnchorPolicy) {
		t.Fatalf("cycle log does not include evaluation weights: %q", output)
	}
}

func TestRunnerResetsLegacyConfirmationOnStrategyChange(t *testing.T) {
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
	config.Confirmations = 2
	source := &fakeMetricsSource{accounts: []AccountMetrics{
		{ID: 1, Status: "active", CurrentPriority: priorityPoor, SuccessfulRequests: 5, TotalTokens: 1_000_000, AccountCost: 1},
		{ID: 2, Status: "active", CurrentPriority: priorityPoor, SuccessfulRequests: 5, TotalTokens: 1_000_000, AccountCost: 2},
	}}
	runner := NewRunner(config, source, server.Client(), &syncState{Accounts: map[int64]accountState{
		1: {CandidatePriority: priorityBest, CandidateCount: 1},
	}}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	runner.tableWriter = io.Discard
	start := nowForTest()
	if err := runner.RunOnce(context.Background(), start); err != nil {
		t.Fatal(err)
	}
	if len(updates) != 0 || runner.state.Accounts[1].CandidateCount != 1 || runner.state.Strategy != strategyVersion {
		t.Fatalf("legacy confirmation was reused: state=%+v updates=%d", runner.state, len(updates))
	}
	if err := runner.RunOnce(context.Background(), start.Add(10*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if got := <-updates; got != priorityDegraded {
		t.Fatalf("confirmed pure-cost step = %d, want %d", got, priorityDegraded)
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
	if runner.state.Exploration == nil {
		t.Fatalf("exploration cleared before original priority restoration: %+v", runner.state)
	}
	if got := <-updates; got.id != "9" || got.priority != 60 {
		t.Fatalf("second restoration update = %+v", got)
	}
	source.accounts[0].CurrentPriority = 60
	if err := runner.RunOnce(context.Background(), start.Add(3*explorationDuration)); err != nil {
		t.Fatal(err)
	}
	if got := <-updates; got.id != "9" || got.priority != 80 {
		t.Fatalf("third restoration update = %+v", got)
	}
	source.accounts[0].CurrentPriority = 80
	if err := runner.RunOnce(context.Background(), start.Add(4*explorationDuration)); err != nil {
		t.Fatal(err)
	}
	if runner.state.Exploration != nil {
		t.Fatalf("exploration was not cleared after original priority restoration: %+v", runner.state)
	}
	if got := <-updates; got.id != "9" || got.priority != priorityPoor {
		t.Fatalf("final restoration update = %+v", got)
	}
}

func TestRunnerExplorationWithCostUsesNormalConfirmation(t *testing.T) {
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
	if runner.state.Exploration != nil || len(updates) != 1 {
		t.Fatalf("exploration did not finish at the confirmed cost target: state=%+v updates=%d", runner.state, len(updates))
	}
	if got := <-updates; got.id != "9" || got.priority != priorityBest {
		t.Fatalf("confirmed cost update = %+v", got)
	}
}

func TestPrepareExplorationDoesNotTurnFailuresIntoImmediateDowngrade(t *testing.T) {
	now := nowForTest()
	accounts := []AccountMetrics{{
		ID: 9, Name: "failing-exploration", Status: "active", CurrentPriority: priorityExplore,
		SuccessfulRequests: 5, TerminalFailures: 3, TrailingTerminalFailures: 3,
		TotalTokens: 1_000_000, AccountCost: 1,
	}}
	accounts = withQualifiedTwoHourCosts(accounts)
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
	if got.RecommendedPriority != priorityBest || got.applyImmediately || !got.explorationEnd {
		t.Fatalf("failure history changed the measured cost target: %+v", got)
	}
}

func TestRunnerExplorationExpiryUsesMeasuredTargetWhenEvidenceIsReady(t *testing.T) {
	start := nowForTest()
	accounts := []AccountMetrics{
		{ID: 9, Name: "measured", Status: "active", CurrentPriority: priorityExplore, SuccessfulRequests: 5, TotalTokens: 1_000_000, AccountCost: 1, LatencyP90Ms: 100},
		{ID: 10, Name: "peer", Status: "active", CurrentPriority: priorityNeutral, SuccessfulRequests: 5, TotalTokens: 1_000_000, AccountCost: 2, LatencyP90Ms: 100},
	}
	accounts = withQualifiedTwoHourCosts(accounts)
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
	if measured.RecommendedPriority != priorityBest || !measured.explorationEnd {
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
		{ID: 3, Name: "mature-peer", Status: "active", CurrentPriority: 90, SuccessfulRequests: 5, TotalTokens: 1_000_000, AccountCost: 2, LatencyP90Ms: 100},
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
	source := &fakeMetricsSource{accounts: []AccountMetrics{
		{
			ID: 14, Name: "stable", Status: "active", CurrentPriority: priorityPoor,
			SuccessfulRequests: 5, TotalTokens: 1_000_000, AccountCost: 1, LatencyP90Ms: 100,
		},
		{
			ID: 15, Name: "peer", Status: "active", CurrentPriority: priorityPoor,
			SuccessfulRequests: 5, TotalTokens: 1_000_000, AccountCost: 2, LatencyP90Ms: 100,
		},
	}}
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

func TestRunnerDoesNotGateCostPromotionOnLegacySignals(t *testing.T) {
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
	runner := NewRunner(config, &fakeMetricsSource{}, server.Client(), &syncState{
		Accounts: map[int64]accountState{1: {
			LastCostPerMillion: 1, HasCostBaseline: true,
			LastCacheHitRate: 0.1, HasCacheBaseline: true,
		}},
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	runner.tableWriter = io.Discard
	recommendation := Recommendation{
		ID: 1, Name: "cheap", CurrentPriority: priorityPoor, RecommendedPriority: priorityBest,
		CostPerMillionTokens: 2, CacheHitRate: 0.9, CacheHitRateKnown: true,
	}
	if changed, pending := runner.applyRecommendations(context.Background(), []Recommendation{recommendation}, "secret", nowForTest()); changed != 1 || pending != 0 {
		t.Fatalf("legacy signals gated pure-cost promotion: changed=%d pending=%d", changed, pending)
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

func TestPrepareExplorationStartsMultiplierRecoveryWhenOrdinaryExplorationDisabled(t *testing.T) {
	now := nowForTest()
	accounts := recoveryAccounts(nil)
	recommendations := scoreAccounts(accounts, now, 5)
	before := testRecommendationByID(t, recommendations, 1)
	if before.CostPerMillionTokens != 0 || before.RecommendedPriority != priorityUnavailable {
		t.Fatalf("multiplier anchor leaked into formal scoring: %+v", before)
	}
	config := testRunnerConfig(t, "http://127.0.0.1:1", true)
	config.ExplorationEnabled = false
	runner := NewRunner(config, &fakeMetricsSource{}, http.DefaultClient, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	runner.prepareExploration(accounts, recommendations, now)
	candidate := testRecommendationByID(t, recommendations, 1)
	if !candidate.Recovery || !candidate.explorationStart || candidate.RecommendedPriority != priorityExplore {
		t.Fatalf("recovery trial was not prepared: %+v", candidate)
	}
	if candidate.RecoveryPeerCount != 2 || math.Abs(candidate.RecoveryAnchorCostPerMillion-1) > 1e-9 || candidate.AnchorPriority != priorityBest {
		t.Fatalf("unexpected recovery anchor: %+v", candidate)
	}
}

func TestMultiplierRecoveryRequiresTwoSamePlatformPeersAndHonorsRetryAt(t *testing.T) {
	now := nowForTest()
	accounts := recoveryAccounts(nil)
	accounts[2].Platform = "anthropic"
	runner := NewRunner(testRunnerConfig(t, "http://127.0.0.1:1", true), &fakeMetricsSource{}, http.DefaultClient, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if _, ok := runner.selectRecoveryCandidate(accounts, now); ok {
		t.Fatal("recovery used a peer from another platform")
	}

	accounts[2].Platform = "openai"
	retryAt := now.Add(time.Minute)
	runner.state.Accounts[1] = accountState{RecoveryRetryAt: &retryAt}
	if _, ok := runner.selectRecoveryCandidate(accounts, now); ok {
		t.Fatal("recovery ignored its persisted retry deadline")
	}
	if candidate, ok := runner.selectRecoveryCandidate(accounts, retryAt); !ok || candidate.Account.ID != 1 {
		t.Fatalf("recovery was not reconsidered at retry deadline: %+v, ok=%v", candidate, ok)
	}
}

func TestRecoveryTrialRestoresPriorityAndRecordsOutcomeAfterSuccessfulWrite(t *testing.T) {
	tests := []struct {
		name             string
		snapshot         *MetricSnapshot
		previousFailures int
		wantFailures     int
		wantRetry        time.Duration
	}{
		{name: "no result", wantRetry: 2 * time.Hour},
		{name: "terminal failure", snapshot: &MetricSnapshot{TerminalFailures: 1}, wantFailures: 1, wantRetry: 2 * time.Hour},
		{name: "still expensive", snapshot: &MetricSnapshot{SuccessfulRequests: 20, PricedRequests: 20, PricedTokens: 1_000_000, TotalTokens: 1_000_000, AccountCost: 30}, wantFailures: 1, wantRetry: 2 * time.Hour},
		{name: "cheap measurement", snapshot: &MetricSnapshot{SuccessfulRequests: 20, PricedRequests: 20, PricedTokens: 1_000_000, TotalTokens: 1_000_000, AccountCost: 1}, previousFailures: 2},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			updates := make(chan int, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				var payload struct {
					Priority int `json:"priority"`
				}
				if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
					t.Fatal(err)
				}
				updates <- payload.Priority
				_, _ = io.WriteString(w, `{"code":0}`)
			}))
			defer server.Close()

			now := nowForTest()
			accounts := recoveryAccounts(test.snapshot)
			accounts[0].CurrentPriority = priorityExplore
			recommendations := scoreAccounts(accounts, now, 5)
			state := &syncState{
				Accounts: map[int64]accountState{1: {RecoveryFailures: test.previousFailures}},
				Exploration: &explorationState{
					AccountID: 1, OriginalPriority: priorityPoor, StartedAt: timePtr(now.Add(-recoveryDuration)),
					Recovery: true, RecoveryAnchorCostPerMillion: 1, RecoveryPeerCount: 2, RecoveryTargetPriority: priorityBest,
				},
			}
			runner := NewRunner(testRunnerConfig(t, server.URL, false), &fakeMetricsSource{}, server.Client(), state, slog.New(slog.NewTextHandler(io.Discard, nil)))
			runner.prepareExploration(accounts, recommendations, now)
			candidate := testRecommendationByID(t, recommendations, 1)
			if !candidate.explorationEnd || candidate.RecommendedPriority != priorityPoor {
				t.Fatalf("expired recovery did not restore original priority: %+v", candidate)
			}
			if changed, pending := runner.applyRecommendations(context.Background(), []Recommendation{candidate}, "secret", now); changed != 1 || pending != 0 {
				t.Fatalf("recovery restore changed=%d pending=%d", changed, pending)
			}
			if got := <-updates; got != priorityPoor {
				t.Fatalf("restored priority = %d, want %d", got, priorityPoor)
			}
			gotState := runner.state.Accounts[1]
			if runner.state.Exploration != nil || gotState.RecoveryFailures != test.wantFailures {
				t.Fatalf("recovery outcome state = %+v", runner.state)
			}
			if test.wantRetry == 0 {
				if gotState.RecoveryRetryAt != nil {
					t.Fatalf("successful recovery retained retry deadline: %+v", gotState)
				}
			} else if gotState.RecoveryRetryAt == nil || !gotState.RecoveryRetryAt.Equal(now.Add(test.wantRetry)) {
				t.Fatalf("recovery retry deadline = %v, want %v", gotState.RecoveryRetryAt, now.Add(test.wantRetry))
			}
		})
	}
}

func TestRecoveryLeaseSurvivesMissingSnapshotAndRestoresWhenAccountReturns(t *testing.T) {
	now := nowForTest()
	state := &syncState{
		Accounts: map[int64]accountState{},
		Exploration: &explorationState{
			AccountID: 1, OriginalPriority: priorityPoor, StartedAt: timePtr(now.Add(-recoveryDuration)),
			Recovery: true, RecoveryAnchorCostPerMillion: 1, RecoveryPeerCount: 2, RecoveryTargetPriority: priorityBest,
		},
	}
	runner := NewRunner(testRunnerConfig(t, "http://127.0.0.1:1", true), &fakeMetricsSource{}, http.DefaultClient, state, slog.New(slog.NewTextHandler(io.Discard, nil)))

	missingAccounts := recoveryAccounts(nil)[1:]
	runner.prepareExploration(missingAccounts, scoreAccounts(missingAccounts, now, 5), now)
	if runner.state.Exploration == nil || runner.state.Exploration.AccountID != 1 {
		t.Fatalf("missing snapshot released active recovery lease: %+v", runner.state)
	}

	returnedAccounts := recoveryAccounts(nil)
	returnedAccounts[0].CurrentPriority = priorityExplore
	recommendations := scoreAccounts(returnedAccounts, now.Add(time.Minute), 5)
	runner.prepareExploration(returnedAccounts, recommendations, now.Add(time.Minute))
	returned := testRecommendationByID(t, recommendations, 1)
	if !returned.Recovery || !returned.explorationEnd || returned.RecommendedPriority != priorityPoor {
		t.Fatalf("returned recovery account was not restored: %+v", returned)
	}
}

func TestRunnerRestoresOrReleasesExpiredRecoveryMissingFromSnapshot(t *testing.T) {
	tests := []struct {
		name          string
		status        int
		wantLease     bool
		wantApply     string
		wantRetryTime bool
	}{
		{name: "restore succeeds", status: http.StatusOK, wantApply: "recovery-ended", wantRetryTime: true},
		{name: "account was deleted", status: http.StatusNotFound, wantApply: "recovery-account-removed"},
		{name: "temporary API failure", status: http.StatusBadGateway, wantLease: true, wantApply: "failed"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			updatedPriority := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				calls++
				if request.URL.Path != "/api/v1/admin/accounts/1" {
					t.Fatalf("unexpected recovery path: %s", request.URL.Path)
				}
				var payload struct {
					Priority int `json:"priority"`
				}
				if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
					t.Fatal(err)
				}
				updatedPriority = payload.Priority
				w.WriteHeader(test.status)
				if test.status == http.StatusOK {
					_, _ = io.WriteString(w, `{"code":0}`)
				}
			}))
			defer server.Close()

			now := nowForTest()
			state := &syncState{
				Accounts: map[int64]accountState{},
				Exploration: &explorationState{
					AccountID: 1, OriginalPriority: priorityPoor, StartedAt: timePtr(now.Add(-recoveryDuration)), Recovery: true,
				},
			}
			runner := NewRunner(testRunnerConfig(t, server.URL, false), &fakeMetricsSource{}, server.Client(), state, slog.New(slog.NewTextHandler(io.Discard, nil)))
			runner.tableWriter = io.Discard
			if err := runner.RunOnce(context.Background(), now); err != nil {
				t.Fatal(err)
			}
			if calls != 1 || updatedPriority != priorityPoor {
				t.Fatalf("missing recovery writes=%d priority=%d, want one write to %d", calls, updatedPriority, priorityPoor)
			}
			if gotLease := runner.state.Exploration != nil; gotLease != test.wantLease {
				t.Fatalf("recovery lease retained=%v, want %v: %+v", gotLease, test.wantLease, runner.state)
			}
			var report PriorityReport
			data, err := os.ReadFile(runner.config.ReportFile)
			if err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(data, &report); err != nil {
				t.Fatal(err)
			}
			if len(report.Accounts) != 1 || report.Accounts[0].ApplyStatus != test.wantApply {
				t.Fatalf("missing recovery report = %+v, want status %q", report.Accounts, test.wantApply)
			}
			if gotRetry := runner.state.Accounts[1].RecoveryRetryAt != nil; gotRetry != test.wantRetryTime {
				t.Fatalf("recovery retry recorded=%v, want %v: %+v", gotRetry, test.wantRetryTime, runner.state.Accounts[1])
			}
		})
	}
}

func TestRecoveryBackoffEscalatesAndCaps(t *testing.T) {
	tests := []struct {
		failures int
		want     time.Duration
	}{{1, 2 * time.Hour}, {2, 6 * time.Hour}, {3, 24 * time.Hour}, {20, 24 * time.Hour}}
	for _, test := range tests {
		if got := recoveryBackoff(test.failures); got != test.want {
			t.Fatalf("recoveryBackoff(%d) = %s, want %s", test.failures, got, test.want)
		}
	}
}

func TestRecoveryStartAdminFailureDoesNotCountAsTrialFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "temporarily unavailable", http.StatusBadGateway)
	}))
	defer server.Close()
	now := nowForTest()
	accounts := recoveryAccounts(nil)
	recommendations := scoreAccounts(accounts, now, 5)
	runner := NewRunner(testRunnerConfig(t, server.URL, false), &fakeMetricsSource{}, server.Client(), nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	runner.config.ExplorationEnabled = false
	runner.prepareExploration(accounts, recommendations, now)
	candidate := testRecommendationByID(t, recommendations, 1)
	if changed, pending := runner.applyRecommendations(context.Background(), []Recommendation{candidate}, "secret", now); changed != 0 || pending != 1 {
		t.Fatalf("failed recovery start changed=%d pending=%d", changed, pending)
	}
	state := runner.state.Accounts[1]
	if runner.state.Exploration != nil || state.RecoveryFailures != 0 || state.RecoveryRetryAt != nil {
		t.Fatalf("Admin failure was recorded as a recovery trial failure: %+v", runner.state)
	}
}

func recoveryAccounts(candidateSnapshot *MetricSnapshot) []AccountMetrics {
	return []AccountMetrics{
		{ID: 1, Name: "recover", Platform: "openai", Status: "active", CurrentPriority: priorityUnavailable, RateMultiplier: 0.1, DecisionWindowsAvailable: true, Window30m: candidateSnapshot},
		{ID: 2, Name: "peer-1", Platform: "openai", Status: "active", CurrentPriority: priorityBest, RateMultiplier: 1, DecisionWindowsAvailable: true, Window30m: &MetricSnapshot{SuccessfulRequests: 20, PricedRequests: 20, PricedTokens: 1_000_000, TotalTokens: 1_000_000, AccountCost: 10}},
		{ID: 3, Name: "peer-2", Platform: "openai", Status: "active", CurrentPriority: priorityPoor, RateMultiplier: 2, DecisionWindowsAvailable: true, Window30m: &MetricSnapshot{SuccessfulRequests: 20, PricedRequests: 20, PricedTokens: 1_000_000, TotalTokens: 1_000_000, AccountCost: 20}},
	}
}

func testRecommendationByID(t *testing.T, recommendations []Recommendation, id int64) Recommendation {
	t.Helper()
	for _, recommendation := range recommendations {
		if recommendation.ID == id {
			return recommendation
		}
	}
	t.Fatalf("recommendation %d not found", id)
	return Recommendation{}
}
