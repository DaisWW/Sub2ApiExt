package main

import (
	"math"
	"testing"
	"time"
)

func TestScoreAccountsKeepsRecovered429MostlyAvailable(t *testing.T) {
	now := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	accounts := []AccountMetrics{
		{
			ID: 1, Name: "fast", Status: "active", CurrentPriority: 20,
			SuccessfulRequests: 5, TotalTokens: 5_000_000, ActualCost: 0.5,
			LatencyP90Ms: 1000, RecoveredRateLimited: 1, RecoveredRateLimitWeight: 0.25, RateLimitedRequests: 1,
		},
		{
			ID: 2, Name: "failed", Status: "active", CurrentPriority: 20,
			SuccessfulRequests: 5, TotalTokens: 5_000_000, ActualCost: 0.5,
			LatencyP90Ms: 1000, TerminalFailures: 1,
		},
	}
	result := scoreAccounts(accounts, now, 1)
	if len(result) != 2 {
		t.Fatalf("got %d recommendations", len(result))
	}
	var recovered, failed Recommendation
	for _, item := range result {
		if item.ID == 1 {
			recovered = item
		} else {
			failed = item
		}
	}
	if recovered.Availability <= failed.Availability {
		t.Fatalf("recovered 429 availability=%v, terminal failure=%v", recovered.Availability, failed.Availability)
	}
	if recovered.HardExcluded || recovered.Reason == "" {
		t.Fatalf("recovered 429 should remain routable: %+v", recovered)
	}
}

func TestEffectiveAvailabilityWeightsRecovered429ByDelay(t *testing.T) {
	short := effectiveAvailability(AccountMetrics{SuccessfulRequests: 5, RecoveredRateLimitWeight: 0.25})
	long := effectiveAvailability(AccountMetrics{SuccessfulRequests: 5, RecoveredRateLimitWeight: 1})
	if !(short > long && short > 0.9 && long < 0.84) {
		t.Fatalf("short=%v long=%v", short, long)
	}
}

func TestScoreAccountsDoesNotDoubleCountRecovered429Evidence(t *testing.T) {
	now := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	result := scoreAccounts([]AccountMetrics{{
		ID: 1, Name: "recovered", Status: "active", CurrentPriority: 37,
		SuccessfulRequests: 1, RecoveredRateLimited: 20, RecoveredRateLimitWeight: 5,
		TotalTokens: 1_000_000, ActualCost: 0.1,
	}}, now, 5)
	if len(result) != 1 {
		t.Fatalf("got %d recommendations", len(result))
	}
	if result[0].Confidence != 0.2 {
		t.Fatalf("recovered 429 inflated confidence to %v", result[0].Confidence)
	}
	if result[0].RecommendedPriority != 37 {
		t.Fatalf("insufficient final outcomes changed priority to %d", result[0].RecommendedPriority)
	}
}

func TestScoreAccountsHardExcludesActiveCooldown(t *testing.T) {
	now := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	reset := now.Add(time.Minute)
	result := scoreAccounts([]AccountMetrics{{
		ID: 1, Name: "cooling", Status: "active", CurrentPriority: 10,
		RateLimitResetAt: &reset,
	}}, now, 5)
	if len(result) != 1 || !result[0].HardExcluded || result[0].RecommendedPriority != priorityUnavailable {
		t.Fatalf("cooling account recommendation = %+v", result)
	}
}

func TestScoreAccountsInsufficientEvidencePreservesPriority(t *testing.T) {
	now := time.Now().UTC()
	result := scoreAccounts([]AccountMetrics{{
		ID: 1, Name: "new", Status: "active", CurrentPriority: 37,
		SuccessfulRequests: 1, TotalTokens: 1000, ActualCost: 0.01,
	}}, now, 5)
	if result[0].RecommendedPriority != 37 {
		t.Fatalf("insufficient evidence changed priority to %d", result[0].RecommendedPriority)
	}
	if result[0].Confidence <= 0 || result[0].Confidence >= 1 {
		t.Fatalf("unexpected confidence %v", result[0].Confidence)
	}
}

func TestScoreAccountsMovesColdAccountTowardMultiplierAnchor(t *testing.T) {
	now := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	result := scoreAccounts([]AccountMetrics{
		{ID: 1, Name: "cold-cheap", Status: "active", CurrentPriority: priorityPoor, RateMultiplier: 0.1},
		{ID: 2, Name: "mature-expensive", Status: "active", CurrentPriority: priorityNeutral, RateMultiplier: 1, SuccessfulRequests: 5, TotalTokens: 1_000_000, AccountCost: 1, LatencyP90Ms: 100},
		{ID: 3, Name: "mature-mid", Status: "active", CurrentPriority: priorityNeutral, RateMultiplier: 0.5, SuccessfulRequests: 5, TotalTokens: 1_000_000, AccountCost: 1, LatencyP90Ms: 100},
	}, now, 5)
	var cold Recommendation
	for _, item := range result {
		if item.ID == 1 {
			cold = item
			break
		}
	}
	if cold.AnchorPriority != coldAnchorFloor || cold.RecommendedPriority != coldAnchorFloor {
		t.Fatalf("cold account anchor = %+v, want priority %d", cold, coldAnchorFloor)
	}
	if cold.Confidence != 0 {
		t.Fatalf("cold account confidence = %v", cold.Confidence)
	}
}

func TestScoreAccountsUsesMeasuredCostOverMultiplierAnchor(t *testing.T) {
	now := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	result := scoreAccounts([]AccountMetrics{
		{ID: 1, Name: "cheap-high-multiplier", Status: "active", CurrentPriority: priorityNeutral, RateMultiplier: 1, SuccessfulRequests: 5, TotalTokens: 1_000_000, AccountCost: 1, LatencyP90Ms: 100},
		{ID: 2, Name: "expensive-low-multiplier", Status: "active", CurrentPriority: priorityNeutral, RateMultiplier: 0.1, SuccessfulRequests: 5, TotalTokens: 1_000_000, AccountCost: 10, LatencyP90Ms: 100},
	}, now, 5)
	byID := make(map[int64]Recommendation, len(result))
	for _, item := range result {
		byID[item.ID] = item
	}
	if byID[1].Score <= byID[2].Score || byID[1].RecommendedPriority >= byID[2].RecommendedPriority {
		t.Fatalf("measured cost did not dominate multiplier anchor: cheap=%+v expensive=%+v", byID[1], byID[2])
	}
}

func TestScoreAccountsExcludesLowEvidenceFromPeerNormalization(t *testing.T) {
	now := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	result := scoreAccounts([]AccountMetrics{
		{
			ID: 1, Name: "mature-cheap", Status: "active", CurrentPriority: 50,
			SuccessfulRequests: 5, TotalTokens: 1_000_000, ActualCost: 1,
			LatencyP90Ms: 1000,
		},
		{
			ID: 2, Name: "mature-expensive", Status: "active", CurrentPriority: 50,
			SuccessfulRequests: 5, TotalTokens: 1_000_000, ActualCost: 2,
			LatencyP90Ms: 1000,
		},
		{
			ID: 3, Name: "new-cheapest", Status: "active", CurrentPriority: 50,
			SuccessfulRequests: 1, TotalTokens: 1_000_000, ActualCost: 0.001,
			LatencyP90Ms: 1000,
		},
	}, now, 5)
	byID := make(map[int64]Recommendation, len(result))
	for _, item := range result {
		byID[item.ID] = item
	}
	if byID[1].CostScore != 100 || byID[2].CostScore != 0 {
		t.Fatalf("mature peer scores were affected by low-evidence account: cheap=%v expensive=%v", byID[1].CostScore, byID[2].CostScore)
	}
	if byID[3].CostScore != 50 || byID[3].RecommendedPriority != 50 {
		t.Fatalf("low-evidence account should remain neutral: %+v", byID[3])
	}
}

func TestNormalizeLowerBetter(t *testing.T) {
	got := normalizeLowerBetter([]float64{10, 20, 30, 0}, []bool{true, true, true, false})
	want := []float64{100, 50, 0, 50}
	for i := range want {
		if math.Abs(got[i]-want[i]) > 1e-9 {
			t.Fatalf("scores = %v, want %v", got, want)
		}
	}
}

func TestPriorityForScoreUsesBands(t *testing.T) {
	cases := []struct {
		score float64
		want  int
	}{
		{90, priorityBest}, {70, priorityGood}, {55, priorityNeutral}, {40, priorityDegraded}, {10, priorityPoor},
	}
	for _, item := range cases {
		if got := priorityForScore(item.score); got != item.want {
			t.Errorf("priorityForScore(%v)=%d, want %d", item.score, got, item.want)
		}
	}
}

func TestNormalizedPriorityUsesSafeBounds(t *testing.T) {
	cases := []struct {
		value int
		want  int
	}{
		{value: -1, want: priorityNeutral},
		{value: 0, want: priorityNeutral},
		{value: 10, want: 10},
		{value: 10000, want: 10000},
		{value: 10001, want: priorityUnavailable},
	}
	for _, item := range cases {
		if got := normalizedPriority(item.value); got != item.want {
			t.Errorf("normalizedPriority(%d)=%d, want %d", item.value, got, item.want)
		}
	}
}

func TestEffectiveAvailabilityClampsSoftRateLimitWeight(t *testing.T) {
	if got := effectiveAvailability(AccountMetrics{SuccessfulRequests: 5, RecoveredRateLimitWeight: -3}); got != 1 {
		t.Fatalf("negative soft weight reduced availability to %v", got)
	}
	if got := effectiveAvailability(AccountMetrics{SuccessfulRequests: 5, RecoveredRateLimitWeight: 100}); got < 0 || got > 1 {
		t.Fatalf("availability escaped bounds: %v", got)
	}
}
