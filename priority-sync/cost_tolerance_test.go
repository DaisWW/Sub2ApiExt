package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestRunnerCostDowngradeWaitsForWarmupAndFreshEvidenceAcrossRestart(t *testing.T) {
	var updates []int
	failWrite := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if failWrite {
			http.Error(w, "try again", http.StatusBadGateway)
			return
		}
		var payload struct {
			Priority int `json:"priority"`
		}
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			t.Error(err)
		}
		updates = append(updates, payload.Priority)
		_, _ = io.WriteString(w, `{"code":0}`)
	}))
	defer server.Close()
	start := nowForTest()
	source := &fakeMetricsSource{accounts: []AccountMetrics{
		{ID: 1, Status: "active", CurrentPriority: priorityBest,
			Window2h: &MetricSnapshot{SuccessfulRequests: 5, PricedRequests: 5, PricedTokens: 500_000,
				AccountCost: 0.1, LastPricedSuccessAt: timePtr(start)}},
		{ID: 2, Status: "active", CurrentPriority: priorityBest,
			Window2h: &MetricSnapshot{SuccessfulRequests: 5, PricedRequests: 5, PricedTokens: 500_000,
				AccountCost: 0.025, LastPricedSuccessAt: timePtr(start)}},
	}}
	config := testRunnerConfig(t, server.URL, false)
	config.Confirmations = 2
	config.ExplorationEnabled = false
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	runner := NewRunner(config, source, server.Client(), nil, logger)
	runner.tableWriter = io.Discard
	run := func(minutes int) {
		t.Helper()
		if err := runner.RunOnce(context.Background(), start.Add(time.Duration(minutes)*time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	run(0)
	run(10)
	if len(updates) != 0 {
		t.Fatal("the initial batch caused a cost downgrade after only two cycles")
	}
	state, err := loadState(config.StateFile)
	if err != nil {
		t.Fatal(err)
	}
	runner = NewRunner(config, source, server.Client(), state, logger)
	runner.tableWriter = io.Discard
	// More requests during warmup must not become proof after warmup ends.
	source.accounts[0].Window2h.LastPricedSuccessAt = timePtr(start.Add(20 * time.Minute))
	run(20)
	run(30)
	if len(updates) != 0 {
		t.Fatal("re-reading old cost evidence caused a downgrade")
	}
	source.accounts[0].Window2h.LastPricedSuccessAt = timePtr(start.Add(31 * time.Minute))
	failWrite = true
	run(31)
	if runner.state.Accounts[1].LastAppliedAt != nil {
		t.Fatal("a failed write consumed the cost observation")
	}
	failWrite = false
	run(32)
	if len(updates) != 1 || updates[0] != priorityGood {
		t.Fatalf("persistent high cost did not apply one bounded step: %v", updates)
	}
	source.accounts[0].CurrentPriority = priorityGood
	run(40)
	run(50)
	if len(updates) != 1 {
		t.Fatalf("one high-cost batch caused repeated downgrades: %v", updates)
	}
	// A real fault must still roll back during the next cost grace period.
	source.accounts[0].Window2h.TerminalFailures = 5
	source.accounts[0].Window2h.TrailingTerminalFailures = 5
	source.accounts[0].Window2h.LastTerminalFailureAt = timePtr(start.Add(51 * time.Minute))
	source.accounts[0].Window2h.FifthTerminalFailureAt = timePtr(start.Add(50 * time.Minute))
	run(51)
	if len(updates) != 2 || updates[1] != priorityNeutral {
		t.Fatalf("cost tolerance blocked a fault rollback: %v", updates)
	}
}

func TestProgressiveRecoveryCostIncreaseUsesWarmupTolerance(t *testing.T) {
	start := nowForTest()
	accounts := []AccountMetrics{
		{ID: 1, Status: "active", CurrentPriority: priorityGood,
			Window2h: &MetricSnapshot{SuccessfulRequests: 50, PricedRequests: 50, PricedTokens: 2_000_000,
				AccountCost: 0.4, LastPricedSuccessAt: timePtr(start)},
			RecoveryWindow: &MetricSnapshot{SuccessfulRequests: 50, PricedRequests: 50, LastPricedSuccessAt: timePtr(start)}},
		{ID: 2, Status: "active", CurrentPriority: priorityBest,
			Window2h: &MetricSnapshot{SuccessfulRequests: 50, PricedRequests: 50, PricedTokens: 2_000_000,
				AccountCost: 0.1, LastPricedSuccessAt: timePtr(start)}},
	}
	runner := NewRunner(testRunnerConfig(t, "http://127.0.0.1:1", false), &fakeMetricsSource{}, http.DefaultClient, &syncState{
		Strategy: strategyVersion,
		Accounts: map[int64]accountState{1: {RecoveryOriginalPriority: priorityNeutral, RecoveryFailures: 1,
			LastAppliedAt: timePtr(start.Add(-20 * time.Minute)), LastRecoveryFailureAt: timePtr(start.Add(-20 * time.Minute))}},
		Exploration: &explorationState{AccountID: 1, OriginalPriority: priorityNeutral, Recovery: true, RecoveryProgressive: true,
			RecoveryTargetPriority: priorityPoor, StartedAt: timePtr(start.Add(-10 * time.Minute))},
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	recommendations := scoreAccounts(accounts, start, 5)
	runner.prepareExploration(accounts, recommendations, start)
	if changed, pending := runner.applyRecommendations(context.Background(), recommendations, "secret", start); changed != 0 || pending != 1 {
		t.Fatalf("recovery cost increase bypassed warmup: changed=%d pending=%d", changed, pending)
	}
	for _, recommendation := range recommendations {
		if recommendation.ID == 1 && recommendation.ApplyStatus != "cost-observing" {
			t.Fatalf("recovery skipped cost observation: %+v", recommendation)
		}
	}
	if runner.state.Exploration == nil {
		t.Fatal("cost observation discarded the active recovery lease")
	}
}

func TestRunnerCostSpikeClearsWithoutDelayingCheaperPriority(t *testing.T) {
	var updates []int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		var payload struct {
			Priority int `json:"priority"`
		}
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			t.Error(err)
		}
		updates = append(updates, payload.Priority)
		_, _ = io.WriteString(w, `{"code":0}`)
	}))
	defer server.Close()
	start := nowForTest()
	source := &fakeMetricsSource{accounts: []AccountMetrics{
		{ID: 1, Status: "active", CurrentPriority: priorityNeutral,
			Window2h: &MetricSnapshot{PricedRequests: 40, PricedTokens: 2_000_000, AccountCost: 0.4, LastPricedSuccessAt: timePtr(start)}},
		{ID: 2, Status: "active", CurrentPriority: priorityNeutral,
			Window2h: &MetricSnapshot{PricedRequests: 40, PricedTokens: 2_000_000, AccountCost: 0.2, LastPricedSuccessAt: timePtr(start)}},
	}}
	config := testRunnerConfig(t, server.URL, false)
	config.Confirmations = 2
	config.ExplorationEnabled = false
	runner := NewRunner(config, source, server.Client(), nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	runner.tableWriter = io.Discard
	for cycle := 0; cycle < 3; cycle++ {
		if cycle > 0 {
			source.accounts[0].Window2h.AccountCost = 0.1
			source.accounts[0].Window2h.LastPricedSuccessAt = timePtr(start.Add(time.Duration(cycle*10) * time.Minute))
		}
		if err := runner.RunOnce(context.Background(), start.Add(time.Duration(cycle*10)*time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	if len(updates) != 1 || updates[0] != priorityGood {
		t.Fatalf("a transient spike prevented the cheaper account from improving: %v", updates)
	}
}
