package main

import (
	"math"
	"strings"
	"testing"
	"time"
)

func scoreQualifiedAccounts(accounts []AccountMetrics, now time.Time, minSamples int) []Recommendation {
	return scoreAccounts(withQualifiedTwoHourCosts(accounts), now, minSamples)
}

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

func TestScoreAccountsRanksOnlyByDirectCost(t *testing.T) {
	now := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
	result := scoreQualifiedAccounts([]AccountMetrics{
		{
			ID: 1, Name: "cheap-but-slow", Status: "active", CurrentPriority: priorityNeutral,
			SuccessfulRequests: 20, TerminalFailures: 20, RecoveredRateLimitWeight: 10,
			TotalTokens: 1_000_000, AccountCost: 1, LatencyP90Ms: 100_000,
		},
		{
			ID: 2, Name: "middle", Status: "active", CurrentPriority: priorityNeutral,
			SuccessfulRequests: 100, TotalTokens: 1_000_000, AccountCost: 2, LatencyP90Ms: 1_000,
		},
		{
			ID: 3, Name: "expensive-but-fast", Status: "active", CurrentPriority: priorityNeutral,
			SuccessfulRequests: 100, TotalTokens: 1_000_000, AccountCost: 3, LatencyP90Ms: 1,
		},
	}, now, 5)

	byID := make(map[int64]Recommendation, len(result))
	for _, recommendation := range result {
		byID[recommendation.ID] = recommendation
	}
	if byID[1].CostPerMillionTokens != 1 || byID[2].CostPerMillionTokens != 2 || byID[3].CostPerMillionTokens != 3 {
		t.Fatalf("direct costs were modified: cheap=%+v middle=%+v expensive=%+v", byID[1], byID[2], byID[3])
	}
	if !(byID[1].RecommendedPriority < byID[2].RecommendedPriority && byID[2].RecommendedPriority < byID[3].RecommendedPriority) {
		t.Fatalf("priorities do not follow direct cost: cheap=%+v middle=%+v expensive=%+v", byID[1], byID[2], byID[3])
	}
}

func TestScoreAccountsKeepsCurrentPriorityWithoutCost(t *testing.T) {
	result := scoreAccounts([]AccountMetrics{{
		ID: 1, Name: "no-cost", Status: "active", CurrentPriority: 37,
		SuccessfulRequests: 100, TotalTokens: 1_000_000,
	}}, time.Now().UTC(), 5)
	if len(result) != 1 || result[0].RecommendedPriority != 37 {
		t.Fatalf("account without cost evidence changed priority: %+v", result)
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
		t.Fatalf("unqualified cost changed priority: %d", result[0].RecommendedPriority)
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

func TestScoreAccountsKeepsHighPriorityAccountWithoutCost(t *testing.T) {
	now := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	result := scoreAccounts([]AccountMetrics{{
		ID: 1, Name: "stale-high-priority", Status: "active", CurrentPriority: priorityBest,
	}}, now, 5)
	if result[0].AnchorPriority != 0 || result[0].RecommendedPriority != priorityBest {
		t.Fatalf("account without cost did not keep its priority: %+v", result[0])
	}
}

func TestScoreAccountsIgnoresMultiplierWithoutCost(t *testing.T) {
	now := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	result := scoreQualifiedAccounts([]AccountMetrics{
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
	if cold.AnchorPriority != 0 || cold.RecommendedPriority != priorityPoor {
		t.Fatalf("multiplier changed account without cost: %+v", cold)
	}
	if cold.Confidence != 0 {
		t.Fatalf("cold account confidence = %v", cold.Confidence)
	}
}

func TestScoreAccountsUsesMeasuredCostOverMultiplierAnchor(t *testing.T) {
	now := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	result := scoreQualifiedAccounts([]AccountMetrics{
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

func TestScoreAccountsExcludesUnqualifiedLowSampleCostFromRanking(t *testing.T) {
	now := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	result := scoreQualifiedAccounts([]AccountMetrics{
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
	if byID[3].CostPerMillionTokens != 0 || byID[3].CostScore != 50 || byID[1].CostScore != 100 || byID[2].CostScore != 0 {
		t.Fatalf("unqualified cost entered ranking: new=%v cheap=%v expensive=%v", byID[3].CostScore, byID[1].CostScore, byID[2].CostScore)
	}
	if byID[3].RecommendedPriority != priorityNeutral || byID[1].RecommendedPriority >= byID[2].RecommendedPriority {
		t.Fatalf("qualified costs or unqualified priority are wrong: %+v", byID)
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

func TestPriorityForScoreUsesContinuousRange(t *testing.T) {
	cases := []struct {
		score float64
		want  int
	}{
		{100, priorityBest}, {90, 18}, {70, 34}, {55, 46}, {50, priorityNeutral}, {40, 58}, {10, 82}, {0, priorityPoor},
	}
	for _, item := range cases {
		if got := priorityForScore(item.score); got != item.want {
			t.Errorf("priorityForScore(%v)=%d, want %d", item.score, got, item.want)
		}
	}
}

func TestColdAnchorUsesContinuousMultiplierRange(t *testing.T) {
	cheap := coldAnchorPriority(100)
	mid := coldAnchorPriority(75)
	expensive := coldAnchorPriority(50)
	if !(cheap < mid && mid < expensive) {
		t.Fatalf("multiplier anchors are not ordered: cheap=%d mid=%d expensive=%d", cheap, mid, expensive)
	}
	if cheap != coldAnchorFloor || mid != 40 || expensive != priorityNeutral {
		t.Fatalf("unexpected multiplier anchors: cheap=%d expensive=%d", cheap, expensive)
	}
}

func TestScoreAccountsDoesNotUseColdPriceAnchors(t *testing.T) {
	now := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	result := scoreQualifiedAccounts([]AccountMetrics{
		{ID: 1, Name: "cheap", Status: "active", CurrentPriority: priorityPoor, RateMultiplier: 0.05},
		{ID: 2, Name: "mid-cheap", Status: "active", CurrentPriority: priorityPoor, RateMultiplier: 0.10},
		{ID: 3, Name: "mid-expensive", Status: "active", CurrentPriority: priorityPoor, RateMultiplier: 0.15},
		{ID: 4, Name: "expensive", Status: "active", CurrentPriority: priorityPoor, RateMultiplier: 0.20},
	}, now, 5)
	priorities := make(map[int64]int, len(result))
	for _, item := range result {
		priorities[item.ID] = item.RecommendedPriority
	}
	for id, priority := range priorities {
		if priority != priorityPoor {
			t.Fatalf("multiplier changed account %d without cost: %+v", id, priorities)
		}
	}
}

func TestScoreAccountsSeparatesMeasuredScores(t *testing.T) {
	now := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	result := scoreQualifiedAccounts([]AccountMetrics{
		{ID: 1, Name: "cheap", Status: "active", CurrentPriority: priorityNeutral, SuccessfulRequests: 5, TotalTokens: 1_000_000, AccountCost: 1, LatencyP90Ms: 100},
		{ID: 2, Name: "mid-cheap", Status: "active", CurrentPriority: priorityNeutral, SuccessfulRequests: 5, TotalTokens: 1_000_000, AccountCost: 2, LatencyP90Ms: 100},
		{ID: 3, Name: "mid-expensive", Status: "active", CurrentPriority: priorityNeutral, SuccessfulRequests: 5, TotalTokens: 1_000_000, AccountCost: 3, LatencyP90Ms: 100},
		{ID: 4, Name: "expensive", Status: "active", CurrentPriority: priorityNeutral, SuccessfulRequests: 5, TotalTokens: 1_000_000, AccountCost: 4, LatencyP90Ms: 100},
	}, now, 5)
	priorities := make(map[int64]int, len(result))
	for _, item := range result {
		priorities[item.ID] = item.RecommendedPriority
	}
	if !(priorities[1] < priorities[2] && priorities[2] < priorities[3] && priorities[3] < priorities[4]) {
		t.Fatalf("measured priorities are not ordered: %+v", priorities)
	}
}

func TestPriorityForScoreKeepsExplorationSlotReserved(t *testing.T) {
	if got := priorityForScore(87.5); got == priorityExplore {
		t.Fatalf("formal score reused exploration priority %d", priorityExplore)
	}
	if !(priorityForScore(90) < priorityForScore(87.5)) {
		t.Fatalf("score ordering was lost around exploration slot")
	}
}

func TestPriorityMappingsStayOrderedAndInRange(t *testing.T) {
	previousScored := priorityForScore(0)
	previousAnchor := coldAnchorPriority(0)
	for score := 0; score <= 100; score++ {
		scored := priorityForScore(float64(score))
		anchor := coldAnchorPriority(float64(score))
		if scored < priorityBest || scored > priorityPoor || scored == priorityExplore {
			t.Fatalf("score %d mapped to invalid formal priority %d", score, scored)
		}
		if anchor < coldAnchorFloor || anchor > priorityPoor || anchor == priorityExplore {
			t.Fatalf("score %d mapped to invalid cold anchor %d", score, anchor)
		}
		if scored > previousScored || anchor > previousAnchor {
			t.Fatalf("mapping reversed at score %d: scored=%d previous=%d anchor=%d previousAnchor=%d", score, scored, previousScored, anchor, previousAnchor)
		}
		previousScored = scored
		previousAnchor = anchor
	}
	if got := priorityForScore(50); got != priorityNeutral {
		t.Fatalf("neutral score mapped to %d, want %d", got, priorityNeutral)
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

func TestScoreAccountsDoesNotAddFailureOrCachePenalties(t *testing.T) {
	now := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	accounts := []AccountMetrics{
		{ID: 1, Name: "stable", Status: "active", CurrentPriority: priorityNeutral,
			SuccessfulRequests: 20, TotalTokens: 1_000_000, InputTokens: 1_000_000, AccountCost: 1},
		{ID: 2, Name: "fails-with-cache", Status: "active", CurrentPriority: priorityNeutral,
			SuccessfulRequests: 20, TerminalFailures: 50, RecoveredRateLimitWeight: 20,
			TotalTokens: 1_000_000, InputTokens: 100_000, CacheReadTokens: 900_000, AccountCost: 1},
	}
	result := scoreQualifiedAccounts(accounts, now, 5)
	byID := make(map[int64]Recommendation, len(result))
	for _, item := range result {
		byID[item.ID] = item
	}
	if byID[2].CostPerMillionTokens != byID[1].CostPerMillionTokens || byID[2].RecommendedPriority != byID[1].RecommendedPriority {
		t.Fatalf("legacy signals changed equal direct costs: stable=%+v failed=%+v", byID[1], byID[2])
	}
	if !byID[2].CacheHitRateKnown || math.Abs(byID[2].CacheHitRate-0.9) > 1e-9 {
		t.Fatalf("cache metric was not retained for observation: %+v", byID[2])
	}
}

func TestCostDominatesSpeedWhenDifferenceExceedsFivePercent(t *testing.T) {
	now := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	result := scoreQualifiedAccounts([]AccountMetrics{
		{ID: 1, Name: "cheap-slow", Status: "active", CurrentPriority: priorityNeutral,
			SuccessfulRequests: 10, TotalTokens: 20_000_000, AccountCost: 1, LatencyP90Ms: 20_000},
		{ID: 2, Name: "expensive-fast", Status: "active", CurrentPriority: priorityNeutral,
			SuccessfulRequests: 10, TotalTokens: 20_000_000, AccountCost: 2, LatencyP90Ms: 10},
	}, now, 5)
	byID := make(map[int64]Recommendation, len(result))
	for _, item := range result {
		byID[item.ID] = item
	}
	if byID[1].RecommendedPriority >= byID[2].RecommendedPriority {
		t.Fatalf("speed reversed material cost advantage: cheap=%+v expensive=%+v", byID[1], byID[2])
	}
}

func TestPoolStandardizationUsesPeerTokenMix(t *testing.T) {
	aggregate := poolAggregate{
		InputTokens: 900, OutputTokens: 100, TotalTokens: 1000,
	}
	pool := PoolMetrics{
		TotalTokens: 1000, InputTokens: 1000, AccountCost: 0.002,
		InputCost: 0.001, OutputCost: 0.001,
	}
	got := standardizedPoolCost(pool, aggregate)
	if got <= 0 || got >= 2.1 {
		t.Fatalf("unexpected standardized pool cost: %v", got)
	}
}

func TestPoolIdentityNormalizesModelFallbacks(t *testing.T) {
	got := poolIdentity(PoolMetrics{Platform: " OpenAI ", RequestedModel: " GPT-5 ", Model: "Actual"})
	if got != "openai:gpt-5:actual" {
		t.Fatalf("pool identity = %q", got)
	}
	if got := poolIdentity(PoolMetrics{Key: " OpenAI:GPT-5:Actual "}); got != "openai:gpt-5:actual" {
		t.Fatalf("explicit pool identity = %q", got)
	}
	if got := poolIdentity(PoolMetrics{Platform: " OpenAI ", RequestedModel: " GPT-5 ", UpstreamModel: "Actual", UpstreamEndpoint: " /v1/Responses "}); got != "openai:gpt-5:actual:/v1/responses" {
		t.Fatalf("endpoint-aware pool identity = %q", got)
	}
	if got := poolIdentity(PoolMetrics{Platform: "openai", RequestedModel: "gpt-5", UpstreamModel: "actual", GroupID: 7, GroupPriority: 2, LongContext: true}); got != "openai:group=7:tier=2:gpt-5:actual:long-context" {
		t.Fatalf("group- and context-aware pool identity = %q", got)
	}
}

func TestScoreAccountsRanksDirectCostAcrossGroupsAndTiers(t *testing.T) {
	now := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
	pool := func(groupID int64, groupPriority int, cost float64) PoolMetrics {
		return PoolMetrics{
			Platform: "openai", RequestedModel: "gpt-test", UpstreamModel: "gpt-test",
			GroupID: groupID, GroupPriority: groupPriority, SuccessfulRequests: 20,
			TotalTokens: 1_000_000, InputTokens: 1_000_000, AccountCost: cost, InputCost: cost,
		}
	}
	tests := []struct {
		name   string
		first  AccountMetrics
		second AccountMetrics
	}{
		{
			name: "different groups",
			first: AccountMetrics{ID: 1, Name: "cheap", Platform: "openai", Status: "active", CurrentPriority: 90, SuccessfulRequests: 20, TotalTokens: 1_000_000, AccountCost: 1,
				GroupPriorities: map[int64]int{10: 0}, Pools: []PoolMetrics{pool(10, 0, 1)}},
			second: AccountMetrics{ID: 2, Name: "expensive", Platform: "openai", Status: "active", CurrentPriority: 10, SuccessfulRequests: 20, TotalTokens: 1_000_000, AccountCost: 10,
				GroupPriorities: map[int64]int{20: 0}, Pools: []PoolMetrics{pool(20, 0, 10)}},
		},
		{
			name: "different group tiers",
			first: AccountMetrics{ID: 1, Name: "cheap", Platform: "openai", Status: "active", CurrentPriority: 90, SuccessfulRequests: 20, TotalTokens: 1_000_000, AccountCost: 1,
				GroupPriorities: map[int64]int{10: 0}, Pools: []PoolMetrics{pool(10, 0, 1)}},
			second: AccountMetrics{ID: 2, Name: "expensive", Platform: "openai", Status: "active", CurrentPriority: 10, SuccessfulRequests: 20, TotalTokens: 1_000_000, AccountCost: 10,
				GroupPriorities: map[int64]int{10: 1}, Pools: []PoolMetrics{pool(10, 1, 10)}},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result := scoreQualifiedAccounts([]AccountMetrics{test.first, test.second}, now, 5)
			byID := make(map[int64]Recommendation, len(result))
			for _, recommendation := range result {
				byID[recommendation.ID] = recommendation
			}
			if byID[test.first.ID].RecommendedPriority >= byID[test.second.ID].RecommendedPriority {
				t.Fatalf("group or tier overrode direct cost: cheap=%+v expensive=%+v", byID[test.first.ID], byID[test.second.ID])
			}
		})
	}
}

func TestScoreAccountsUsesSameGroupTierCompetition(t *testing.T) {
	now := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
	pool := func(cost float64) PoolMetrics {
		return PoolMetrics{
			Platform: "openai", RequestedModel: "gpt-test", UpstreamModel: "gpt-test",
			GroupID: 10, GroupPriority: 3, SuccessfulRequests: 20,
			TotalTokens: 1_000_000, InputTokens: 1_000_000, AccountCost: cost, InputCost: cost,
		}
	}
	result := scoreQualifiedAccounts([]AccountMetrics{
		{ID: 1, Name: "cheap", Platform: "openai", Status: "active", CurrentPriority: 90, SuccessfulRequests: 20, TotalTokens: 1_000_000, AccountCost: 1,
			GroupPriorities: map[int64]int{10: 3}, Pools: []PoolMetrics{pool(1)}},
		{ID: 2, Name: "expensive", Platform: "openai", Status: "active", CurrentPriority: 10, SuccessfulRequests: 20, TotalTokens: 1_000_000, AccountCost: 10,
			GroupPriorities: map[int64]int{10: 3}, Pools: []PoolMetrics{pool(10)}},
	}, now, 5)
	byID := make(map[int64]Recommendation, len(result))
	for _, recommendation := range result {
		byID[recommendation.ID] = recommendation
	}
	if byID[1].RecommendedPriority >= byID[2].RecommendedPriority {
		t.Fatalf("same-tier cheaper account did not win: cheap=%+v expensive=%+v", byID[1], byID[2])
	}
	if byID[1].PoolCount != 0 || byID[2].PoolCount != 0 {
		t.Fatalf("legacy pools affected direct-cost report: cheap=%+v expensive=%+v", byID[1], byID[2])
	}
}

func TestScoreAccountsNormalizesIndependentCompetitionDomainsSeparately(t *testing.T) {
	now := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
	account := func(id, groupID int64, cost float64) AccountMetrics {
		return AccountMetrics{
			ID: id, Platform: "openai", Status: "active", CurrentPriority: 50, RateMultiplier: 1, SuccessfulRequests: 20,
			TotalTokens: 100_000_000, AccountCost: cost,
			GroupPriorities: map[int64]int{groupID: 0},
			Pools: []PoolMetrics{{
				Platform: "openai", RequestedModel: "gpt", UpstreamModel: "gpt", GroupID: groupID,
				SuccessfulRequests: 20, TotalTokens: 100_000_000, InputTokens: 100_000_000,
				AccountCost: cost, InputCost: cost, RateMultiplier: 1,
			}},
		}
	}
	result := scoreQualifiedAccounts([]AccountMetrics{
		account(1, 10, 1), account(2, 10, 2),
		account(3, 20, 100), account(4, 20, 200),
	}, now, 5)
	byID := make(map[int64]Recommendation, len(result))
	for _, recommendation := range result {
		byID[recommendation.ID] = recommendation
	}
	if !(byID[1].RecommendedPriority < byID[2].RecommendedPriority && byID[2].RecommendedPriority < byID[3].RecommendedPriority && byID[3].RecommendedPriority < byID[4].RecommendedPriority) {
		t.Fatalf("priorities were not globally ordered by direct cost: %+v", byID)
	}
}

func TestAggregatePoolsKeepsLongContextCostsSeparate(t *testing.T) {
	now := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
	account := AccountMetrics{
		ID: 1, Platform: "openai", Status: "active", SuccessfulRequests: 20,
		GroupPriorities: map[int64]int{10: 0},
		Pools: []PoolMetrics{
			{Platform: "openai", RequestedModel: "gpt", UpstreamModel: "gpt", GroupID: 10, TotalTokens: 1_000_000, InputTokens: 1_000_000, AccountCost: 1},
			{Platform: "openai", RequestedModel: "gpt", UpstreamModel: "gpt", GroupID: 10, LongContext: true, TotalTokens: 1_000_000, InputTokens: 1_000_000, AccountCost: 10},
		},
	}
	pools := aggregatePools([]AccountMetrics{account}, now, 5)
	if len(pools) != 2 {
		t.Fatalf("long-context and regular costs were merged: %+v", pools)
	}
	regular := pools["openai:group=10:tier=0:gpt:gpt"]
	longContext := pools["openai:group=10:tier=0:gpt:gpt:long-context"]
	if regular.CostsByAccount[1] != 1 || longContext.CostsByAccount[1] != 10 {
		t.Fatalf("context costs crossed pools: regular=%+v long=%+v", regular, longContext)
	}
}

func TestGroupedPoolsExcludeUnmappedHistoricalTraffic(t *testing.T) {
	now := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
	account := AccountMetrics{
		ID: 1, Platform: "openai", Status: "active", CurrentPriority: 50, SuccessfulRequests: 20,
		GroupPriorities: map[int64]int{10: 0},
		Pools: []PoolMetrics{
			{Platform: "openai", RequestedModel: "gpt", UpstreamModel: "gpt", GroupID: 10, TotalTokens: 1_000_000, InputTokens: 1_000_000, AccountCost: 1},
			{Key: "openai:legacy", Platform: "openai", TotalTokens: 100_000_000, InputTokens: 100_000_000, AccountCost: 1000},
		},
	}
	peer := AccountMetrics{
		ID: 2, Platform: "openai", Status: "active", CurrentPriority: 50, SuccessfulRequests: 20,
		GroupPriorities: map[int64]int{10: 0},
		Pools:           []PoolMetrics{{Platform: "openai", RequestedModel: "gpt", UpstreamModel: "gpt", GroupID: 10, TotalTokens: 1_000_000, InputTokens: 1_000_000, AccountCost: 2}},
	}
	observed, _, _, _, count, _ := accountRiskCost(account, aggregatePools([]AccountMetrics{account, peer}, now, 5))
	if math.Abs(observed-1) > 1e-9 || count != 1 {
		t.Fatalf("unmapped traffic affected grouped cost: observed=%v pools=%d", observed, count)
	}
}

func TestGroupAwareSourceUsesAccountWideDirectCost(t *testing.T) {
	now := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
	result := scoreQualifiedAccounts([]AccountMetrics{{
		ID: 1, Platform: "openai", Status: "active", CurrentPriority: 37,
		SuccessfulRequests: 20, TotalTokens: 1_000_000, AccountCost: 1,
		GroupDataAvailable: true, GroupPriorities: map[int64]int{10: 0},
	}}, now, 5)
	if len(result) != 1 || result[0].RecommendedPriority != priorityBest || result[0].CostPerMillionTokens != 1 {
		t.Fatalf("group metadata overrode account-wide direct cost: %+v", result)
	}
}

func TestGroupedPoolsIgnoreUnmappedCacheEvidence(t *testing.T) {
	now := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
	accounts := []AccountMetrics{
		{
			ID: 1, Platform: "openai", Status: "active", CurrentPriority: 50, SuccessfulRequests: 20,
			GroupDataAvailable: true, GroupPriorities: map[int64]int{10: 0},
			Pools: []PoolMetrics{
				{Platform: "openai", RequestedModel: "gpt", UpstreamModel: "gpt", GroupID: 10, GroupDataAvailable: true, TotalTokens: 1_000_000, OutputTokens: 1_000_000, AccountCost: 1},
				{Key: "openai:legacy", Platform: "openai", GroupDataAvailable: true, TotalTokens: 10_000_000, CacheReadTokens: 10_000_000, AccountCost: 1},
			},
		},
		{
			ID: 2, Platform: "openai", Status: "active", CurrentPriority: 50, SuccessfulRequests: 20,
			GroupDataAvailable: true, GroupPriorities: map[int64]int{10: 0},
			Pools: []PoolMetrics{{Platform: "openai", RequestedModel: "gpt", UpstreamModel: "gpt", GroupID: 10, GroupDataAvailable: true, TotalTokens: 1_000_000, OutputTokens: 1_000_000, AccountCost: 2}},
		},
	}
	result := scoreQualifiedAccounts(accounts, now, 5)
	for _, recommendation := range result {
		if recommendation.ID == 1 && (recommendation.CacheHitRateKnown || recommendation.CacheHitRate != 0) {
			t.Fatalf("unmapped cache evidence affected grouped result: %+v", recommendation)
		}
	}
}

func TestCacheHitRateUsesCacheableInput(t *testing.T) {
	if got := cacheHitRate(100, 0, 50); math.Abs(got-1.0/3.0) > 1e-9 {
		t.Fatalf("cache hit rate = %v, want %v", got, 1.0/3.0)
	}
	if got := cacheHitRate(0, 0, 10); got != 1 {
		t.Fatalf("cache-only hit rate = %v, want 1", got)
	}
	if got := cacheHitRate(100, 50, 50); math.Abs(got-0.25) > 1e-9 {
		t.Fatalf("cache creation was omitted from hit-rate denominator: %v", got)
	}
	if got := cacheHitRate(-1, -1, -1); got != 0 {
		t.Fatalf("negative cache inputs were not clamped: %v", got)
	}
}

func TestKnownZeroCacheReadCostProducesMissCost(t *testing.T) {
	known := PoolMetrics{
		TotalTokens: 1_000_000, InputTokens: 500_000, CacheReadTokens: 500_000,
		AccountCost: 1, CacheReadCost: 0, HasCacheReadCost: true,
	}
	if got := poolMissCostPerMillion(known); math.Abs(got-2) > 1e-9 {
		t.Fatalf("known zero cache-read cost produced miss cost %v, want 2", got)
	}
	known.HasCacheReadCost = false
	if got := poolMissCostPerMillion(known); math.Abs(got-1) > 1e-9 {
		t.Fatalf("unknown cache-read cost did not fall back to blended cost: %v", got)
	}
}

func TestAccountCacheHitRateFallsBackToSixHourWindow(t *testing.T) {
	account := AccountMetrics{
		Window24h: &MetricSnapshot{TotalTokens: 100, OutputTokens: 100, AccountCost: 1},
		Window6h:  &MetricSnapshot{TotalTokens: 100, InputTokens: 10, CacheReadTokens: 90, AccountCost: 1},
		Window7d:  &MetricSnapshot{TotalTokens: 100, InputTokens: 100, AccountCost: 1},
	}
	_, _, hitRate, _ := accountWindowRiskCost(account)
	if math.Abs(hitRate-0.9) > 1e-9 {
		t.Fatalf("cache hit rate = %v, want 0.9 from 6h window", hitRate)
	}
}

func TestAccountRiskCostOnlyUsesCompatiblePlatformPools(t *testing.T) {
	accounts := []AccountMetrics{
		{ID: 1, Platform: "openai", Pools: []PoolMetrics{{
			Key: "openai:gpt", Platform: "openai", TotalTokens: 1_000_000,
			InputTokens: 1_000_000, AccountCost: 1,
		}}},
		{ID: 2, Platform: "anthropic", Pools: []PoolMetrics{{
			Key: "anthropic:claude", Platform: "anthropic", TotalTokens: 1_000_000,
			InputTokens: 1_000_000, AccountCost: 100,
		}}},
	}
	observed, _, _, _, _, _ := accountRiskCost(accounts[0], aggregatePools(accounts, time.Now().UTC(), 1))
	if math.Abs(observed-1) > 1e-9 {
		t.Fatalf("cross-platform pool changed observed cost to %v", observed)
	}
}

func TestAccountRiskCostRenormalizesMissingEvidenceAndCache(t *testing.T) {
	account := AccountMetrics{ID: 1, Platform: "openai", Pools: []PoolMetrics{{
		Key: "openai:known", Platform: "openai", TotalTokens: 1_000_000,
		InputTokens: 500_000, CacheReadTokens: 500_000, AccountCost: 1,
	}}}
	accounts := []AccountMetrics{
		account,
		{ID: 2, Platform: "openai", Pools: []PoolMetrics{{
			Key: "openai:unknown-cost", Platform: "openai", TotalTokens: 1_000_000,
			InputTokens: 1_000_000,
		}}},
	}
	observed, _, hitRate, _, _, _ := accountRiskCost(account, aggregatePools(accounts, time.Now().UTC(), 1))
	if math.Abs(observed-1) > 1e-9 {
		t.Fatalf("missing pool evidence diluted observed cost to %v", observed)
	}
	if math.Abs(hitRate-0.5) > 1e-9 {
		t.Fatalf("missing pool cache evidence diluted hit rate to %v", hitRate)
	}
}

func TestSpeedMetricCombinesAndRenormalizesLatencyEvidence(t *testing.T) {
	if got := speedMetric(1000, 100); math.Abs(got-730) > 1e-9 {
		t.Fatalf("combined speed metric = %v, want 730", got)
	}
	if got := speedMetric(1000, 0); got != 1000 {
		t.Fatalf("duration-only speed metric = %v, want 1000", got)
	}
	if got := speedMetric(0, 100); got != 100 {
		t.Fatalf("first-token-only speed metric = %v, want 100", got)
	}
}

func TestNormalizeLowerBetterClipsOutliersToP10P90(t *testing.T) {
	got := normalizeLowerBetter([]float64{1, 2, 3, 4, 100}, []bool{true, true, true, true, true})
	if got[0] != 100 || got[4] != 0 {
		t.Fatalf("outliers were not clipped to percentile bounds: %v", got)
	}
	if got[2] <= 90 || got[2] >= 100 {
		t.Fatalf("central value was distorted by the outlier: %v", got)
	}
}

func TestScoreAccountsIgnoresSuccessRateForPriority(t *testing.T) {
	now := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	result := scoreQualifiedAccounts([]AccountMetrics{
		{ID: 1, Name: "unstable", Status: "active", CurrentPriority: priorityPoor,
			SuccessfulRequests: 19, TerminalFailures: 2, TotalTokens: 20_000_000, AccountCost: 1},
		{ID: 2, Name: "peer", Status: "active", CurrentPriority: priorityPoor,
			SuccessfulRequests: 20, TotalTokens: 20_000_000, AccountCost: 2},
	}, now, 5)
	for _, item := range result {
		if item.ID == 1 && item.RecommendedPriority != priorityBest {
			t.Fatalf("success rate changed cost-only priority: %+v", item)
		}
	}
}

func TestScoreAccountsTrailingFailuresDoNotOverrideCost(t *testing.T) {
	now := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	result := scoreQualifiedAccounts([]AccountMetrics{{
		ID: 1, Name: "failing", Status: "active", CurrentPriority: priorityBest,
		SuccessfulRequests: 10, TerminalFailures: 3, TrailingTerminalFailures: 3,
		TotalTokens: 10_000_000, AccountCost: 1,
	}}, now, 5)
	if len(result) != 1 || result[0].RecommendedPriority != priorityBest || result[0].applyImmediately {
		t.Fatalf("trailing failures changed cost-only priority: %+v", result)
	}
}

func TestCostDominanceForbidsSpeedReversalAtSixPercentGap(t *testing.T) {
	now := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	result := scoreQualifiedAccounts([]AccountMetrics{
		{ID: 1, Name: "cheap-slow", Status: "active", CurrentPriority: priorityNeutral,
			SuccessfulRequests: 10, TotalTokens: 20_000_000, AccountCost: 1, LatencyP90Ms: 100_000},
		{ID: 2, Name: "slightly-expensive-fast", Status: "active", CurrentPriority: priorityNeutral,
			SuccessfulRequests: 10, TotalTokens: 20_000_000, AccountCost: 1.06, LatencyP90Ms: 1},
	}, now, 5)
	byID := make(map[int64]Recommendation, len(result))
	for _, item := range result {
		byID[item.ID] = item
	}
	if byID[1].RecommendedPriority >= byID[2].RecommendedPriority {
		t.Fatalf("speed reversed a six-percent cost advantage: cheap=%+v expensive=%+v", byID[1], byID[2])
	}
}

func TestSortRecommendationsUsesStableIDAfterCostAndLatency(t *testing.T) {
	recommendations := []Recommendation{
		{ID: 2, Name: "aaa", RecommendedPriority: priorityNeutral, CostPerMillionTokens: 1, LatencyP90Ms: 100},
		{ID: 1, Name: "zzz", RecommendedPriority: priorityNeutral, CostPerMillionTokens: 1, LatencyP90Ms: 100},
	}
	sortRecommendations(recommendations)
	if recommendations[0].ID != 1 || recommendations[1].ID != 2 {
		t.Fatalf("recommendations were name-sorted instead of ID-sorted: %+v", recommendations)
	}
}

func TestSortRecommendationsIgnoresSpeedForEqualCosts(t *testing.T) {
	recommendations := []Recommendation{
		{ID: 1, RecommendedPriority: priorityNeutral, CostPerMillionTokens: 1, LatencyP90Ms: 1000},
		{ID: 2, RecommendedPriority: priorityNeutral, CostPerMillionTokens: 1, FirstTokenP90Ms: 100},
	}
	sortRecommendations(recommendations)
	if recommendations[0].ID != 1 || recommendations[1].ID != 2 {
		t.Fatalf("speed changed equal-cost ordering: %+v", recommendations)
	}
}

func TestAggregatePoolsExcludesColdAndHardExcludedAccountsFromPeerStatistics(t *testing.T) {
	now := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	reset := now.Add(time.Minute)
	pool := func(cost float64) PoolMetrics {
		return PoolMetrics{
			Key:            "openai:gpt-test",
			TotalTokens:    1_000_000,
			InputTokens:    1_000_000,
			AccountCost:    cost,
			InputCost:      cost,
			RateMultiplier: 1,
		}
	}
	accounts := []AccountMetrics{
		{ID: 1, Status: "active", SuccessfulRequests: 5, Pools: []PoolMetrics{pool(1)}},
		{ID: 2, Status: "active", SuccessfulRequests: 5, Pools: []PoolMetrics{pool(3)}},
		{ID: 3, Status: "active", SuccessfulRequests: 1, Pools: []PoolMetrics{pool(0.001)}},
		{ID: 4, Status: "active", SuccessfulRequests: 5, RateLimitResetAt: &reset, Pools: []PoolMetrics{pool(100)}},
	}
	pools := aggregatePools(accounts, now, 5)
	aggregate := pools["openai:gpt-test"]
	peerCosts := mapValues(aggregate.PeerCostsByAccount)
	if got := percentile(peerCosts, 0.5); got != 2 {
		t.Fatalf("peer median=%v, want 2 from mature accounts only: %+v", got, aggregate.PeerCostsByAccount)
	}
	if got := percentile(aggregate.PeerRates, 0.5); got != 1 {
		t.Fatalf("peer multiplier median=%v, want 1: %v", got, aggregate.PeerRates)
	}
	if got := percentile(mapValues(aggregate.PeerMissCostsByAccount), 0.75); got != 2.5 {
		t.Fatalf("peer fallback P75=%v, want 2.5 from mature accounts only: %+v", got, aggregate.PeerMissCostsByAccount)
	}
	for _, excluded := range []int64{3, 4} {
		if _, ok := aggregate.PeerCostsByAccount[excluded]; ok {
			t.Fatalf("excluded account %d entered peer costs: %+v", excluded, aggregate.PeerCostsByAccount)
		}
	}
}

func TestAccountRiskCostFallbackIgnoresHardExcludedAccounts(t *testing.T) {
	now := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	reset := now.Add(time.Minute)
	pool := func(cost float64) PoolMetrics {
		return PoolMetrics{
			Key: "openai:gpt-test", Platform: "openai", TotalTokens: 1_000_000,
			InputTokens: 1_000_000, AccountCost: cost, InputCost: cost,
			RateMultiplier: 1,
		}
	}
	target := AccountMetrics{
		ID: 1, Platform: "openai", SuccessfulRequests: 5,
		Pools: []PoolMetrics{pool(1)},
	}
	hardExcluded := AccountMetrics{
		ID: 2, Platform: "openai", SuccessfulRequests: 5,
		RateLimitResetAt: &reset, Pools: []PoolMetrics{pool(100)},
	}
	_, fallback, _, _, _, _ := accountRiskCost(target, aggregatePools([]AccountMetrics{target, hardExcluded}, now, 5))
	if math.Abs(fallback-1) > 1e-9 {
		t.Fatalf("hard-excluded account polluted fallback miss cost: %v, want 1", fallback)
	}
}

func TestScoreAccountsNeverReversesCostOrderWithManyAccounts(t *testing.T) {
	accounts := make([]AccountMetrics, 81)
	for index := range accounts {
		accounts[index] = AccountMetrics{
			ID: int64(index + 1), Status: "active", CurrentPriority: priorityNeutral,
			SuccessfulRequests: 1, TotalTokens: 1_000_000, AccountCost: float64(index + 1),
		}
	}
	result := scoreQualifiedAccounts(accounts, time.Now().UTC(), 5)
	byID := make(map[int64]Recommendation, len(result))
	for _, recommendation := range result {
		byID[recommendation.ID] = recommendation
	}
	for id := int64(2); id <= int64(len(accounts)); id++ {
		if byID[id-1].RecommendedPriority > byID[id].RecommendedPriority {
			t.Fatalf("cost order reversed between %d and %d: %d > %d", id-1, id, byID[id-1].RecommendedPriority, byID[id].RecommendedPriority)
		}
	}
}

func TestScoreAccountsDoesNotGateCostOnRecentFailure(t *testing.T) {
	now := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)
	result := scoreAccounts([]AccountMetrics{
		{
			ID: 1, Status: "active", CurrentPriority: priorityNeutral, DecisionWindowsAvailable: true,
			Window30m: &MetricSnapshot{SuccessfulRequests: 20, PricedRequests: 20, PricedTokens: 1_000_000, TerminalFailures: 1, TotalTokens: 1_000_000, AccountCost: 0.1},
		},
		{
			ID: 2, Status: "active", CurrentPriority: priorityNeutral, DecisionWindowsAvailable: true,
			Window30m: &MetricSnapshot{SuccessfulRequests: 20, PricedRequests: 20, PricedTokens: 1_000_000, TotalTokens: 1_000_000, AccountCost: 0.2},
		},
	}, now, 5)
	byID := make(map[int64]Recommendation, len(result))
	for _, item := range result {
		byID[item.ID] = item
	}
	cheap := byID[1]
	if cheap.ScoringWindow != "30m" || cheap.RecommendedPriority != priorityBest || strings.Contains(cheap.Reason, "成功率") {
		t.Fatalf("recent failure changed cost-only priority: %+v", cheap)
	}
}

func TestScoreAccountsDoesNotGateCostOnFailureHistory(t *testing.T) {
	now := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
	result := scoreAccounts([]AccountMetrics{
		{
			ID: 1, Status: "active", CurrentPriority: priorityPoor, DecisionWindowsAvailable: true,
			Window30m: &MetricSnapshot{SuccessfulRequests: 80, PricedRequests: 20, PricedTokens: 1_000_000, TerminalFailures: 20, TotalTokens: 1_000_000, AccountCost: 0.1},
		},
		{
			ID: 2, Status: "active", CurrentPriority: priorityPoor, DecisionWindowsAvailable: true,
			Window30m: &MetricSnapshot{SuccessfulRequests: 20, PricedRequests: 20, PricedTokens: 1_000_000, TotalTokens: 1_000_000, AccountCost: 0.2},
		},
	}, now, 5)
	byID := make(map[int64]Recommendation, len(result))
	for _, recommendation := range result {
		byID[recommendation.ID] = recommendation
	}
	unstable := byID[1]
	if unstable.RecommendedPriority != priorityBest || strings.Contains(unstable.Reason, "成功率") {
		t.Fatalf("failure history changed cost-only priority: %+v", unstable)
	}
}

func TestScoreAccountsNeverImmediatelyDegradesTrailingFailures(t *testing.T) {
	now := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)
	base := AccountMetrics{
		ID: 1, Status: "active", CurrentPriority: priorityBest,
		Window7d: &MetricSnapshot{
			SuccessfulRequests: 636, TerminalFailures: 3,
			TotalTokens: 100_000_000, AccountCost: 10,
		},
	}
	withoutStreak := scoreAccounts([]AccountMetrics{base}, now, 5)[0]
	if withoutStreak.applyImmediately || strings.Contains(withoutStreak.Reason, "连续 3 次") {
		t.Fatalf("cumulative failures were treated as a trailing streak: %+v", withoutStreak)
	}
	base.Window7d.TrailingTerminalFailures = 3
	withStreak := scoreAccounts([]AccountMetrics{base}, now, 5)[0]
	if withStreak.applyImmediately || withStreak.RecommendedPriority != withoutStreak.RecommendedPriority {
		t.Fatalf("trailing failure streak changed cost-only priority: %+v", withStreak)
	}
}

func TestScoreAccountsDoesNotUseConfiguredAggregateCost(t *testing.T) {
	now := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	result := scoreAccounts([]AccountMetrics{{
		ID: 1, Name: "aggregate", Status: "active", CurrentPriority: priorityNeutral,
		SuccessfulRequests: 20, TotalTokens: 10_000_000, AccountCost: 18,
	}}, now, 5)
	if len(result) != 1 || result[0].CostPerMillionTokens != 0 || result[0].RecommendedPriority != priorityNeutral {
		t.Fatalf("configured aggregate entered formal scoring: %+v", result)
	}
}

func TestScoreAccountsDoesNotMixRawP75IntoStandardizedPoolCost(t *testing.T) {
	now := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	result := scoreQualifiedAccounts([]AccountMetrics{{
		ID: 1, Status: "active", CurrentPriority: priorityNeutral, SuccessfulRequests: 20, TotalTokens: 1_000_000, AccountCost: 1,
		Pools: []PoolMetrics{{
			Key: "pool", TotalTokens: 1_000_000, AccountCost: 1,
			Window24h: &MetricSnapshot{TotalTokens: 1_000_000, AccountCost: 1, CostP75PerMillion: 2},
			Window6h:  &MetricSnapshot{TotalTokens: 1_000_000, AccountCost: 1},
		}},
	}}, now, 5)
	if len(result) != 1 || math.Abs(result[0].ObservedCostPerMillion-1) > 1e-9 {
		t.Fatalf("raw request P75 biased standardized pool cost: %+v", result)
	}
}

func TestScoreAccountsDoesNotFallBackWhenDecisionSnapshotsAreMissing(t *testing.T) {
	now := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	result := scoreAccounts([]AccountMetrics{{
		ID: 1, Name: "missing-fast", Status: "active", CurrentPriority: priorityNeutral,
		SuccessfulRequests: 5, TotalTokens: 1_000_000, AccountCost: 1,
		Window24h: &MetricSnapshot{TotalTokens: 1_000_000, AccountCost: 1},
	}}, now, 5)
	if len(result) != 1 || result[0].CostPerMillionTokens != 0 || result[0].RecommendedPriority != priorityNeutral {
		t.Fatalf("missing decision windows fell back to configured or 24h cost: %+v", result)
	}
}

func TestScoreAccountsUsesQualifiedThirtyMinuteCost(t *testing.T) {
	result := scoreAccounts([]AccountMetrics{{
		ID: 1, Status: "active", CurrentPriority: priorityNeutral, DecisionWindowsAvailable: true,
		Window30m: &MetricSnapshot{SuccessfulRequests: 20, PricedRequests: 20, PricedTokens: 1_000_000, TotalTokens: 1_000_000, AccountCost: 1},
		Window2h:  &MetricSnapshot{SuccessfulRequests: 50, PricedRequests: 50, PricedTokens: 5_000_000, TotalTokens: 5_000_000, AccountCost: 10},
		Window24h: &MetricSnapshot{SuccessfulRequests: 100, PricedRequests: 100, PricedTokens: 10_000_000, TotalTokens: 10_000_000, AccountCost: 100},
	}}, time.Now().UTC(), 5)
	if len(result) != 1 || result[0].ScoringWindow != "30m" || result[0].CostPerMillionTokens != 1 {
		t.Fatalf("qualified 30m cost was not selected: %+v", result)
	}
	if result[0].ScoringPricedRequests != 20 || result[0].ScoringTokens != 1_000_000 {
		t.Fatalf("30m billing evidence was not reported: %+v", result[0])
	}
}

func TestScoreAccountsFallsBackToQualifiedTwoHourCost(t *testing.T) {
	result := scoreAccounts([]AccountMetrics{{
		ID: 1, Status: "active", CurrentPriority: priorityNeutral, DecisionWindowsAvailable: true,
		Window30m: &MetricSnapshot{SuccessfulRequests: 50, PricedRequests: 19, PricedTokens: 2_000_000, TotalTokens: 2_000_000, AccountCost: 1},
		Window2h:  &MetricSnapshot{SuccessfulRequests: 5, PricedRequests: 5, PricedTokens: 500_000, TotalTokens: 500_000, AccountCost: 1},
	}}, time.Now().UTC(), 5)
	if len(result) != 1 || result[0].ScoringWindow != "2h" || result[0].CostPerMillionTokens != 2 {
		t.Fatalf("qualified 2h fallback was not selected: %+v", result)
	}
}

func TestScoreAccountsNeverUsesObservationWindowsForFormalCost(t *testing.T) {
	result := scoreAccounts([]AccountMetrics{{
		ID: 1, Status: "active", CurrentPriority: 37, DecisionWindowsAvailable: true,
		Window30m: &MetricSnapshot{PricedRequests: 20, PricedTokens: 999_999, TotalTokens: 999_999, AccountCost: 1},
		Window2h:  &MetricSnapshot{PricedRequests: 4, PricedTokens: 5_000_000, TotalTokens: 5_000_000, AccountCost: 1},
		Window24h: &MetricSnapshot{PricedRequests: 100, PricedTokens: 10_000_000, TotalTokens: 10_000_000, AccountCost: 1},
		Window7d:  &MetricSnapshot{PricedRequests: 1000, PricedTokens: 100_000_000, TotalTokens: 100_000_000, AccountCost: 1},
	}}, time.Now().UTC(), 5)
	if len(result) != 1 || result[0].CostPerMillionTokens != 0 || result[0].ScoringWindow != "" || result[0].RecommendedPriority != 37 {
		t.Fatalf("observation-only cost changed formal priority: %+v", result)
	}
	if result[0].Confidence != 0 || len(result[0].CostWindows) != 4 {
		t.Fatalf("formal confidence or window observations are wrong: %+v", result[0])
	}
	for _, window := range result[0].CostWindows {
		if (window.Window == "24h" || window.Window == "7d") && window.FormalEligible {
			t.Fatalf("observation window became formally eligible: %+v", window)
		}
	}
}

func TestScoreAccountsUsesOnlyPricedTokensAsCostDenominator(t *testing.T) {
	result := scoreAccounts([]AccountMetrics{{
		ID: 1, Status: "active", CurrentPriority: priorityNeutral, DecisionWindowsAvailable: true,
		Window30m: &MetricSnapshot{
			SuccessfulRequests: 100, PricedRequests: 20, PricedTokens: 1_000_000,
			TotalTokens: 10_000_000, AccountCost: 2,
		},
	}}, time.Now().UTC(), 5)
	if len(result) != 1 || result[0].CostPerMillionTokens != 2 {
		t.Fatalf("unpriced tokens diluted formal cost: %+v", result)
	}
}

func TestScoreAccountsDoesNotRequireRelativeCostAdvantage(t *testing.T) {
	now := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	result := scoreQualifiedAccounts([]AccountMetrics{
		{ID: 1, Status: "active", CurrentPriority: priorityPoor, SuccessfulRequests: 20, TotalTokens: 100_000_000, AccountCost: 100},
		{ID: 2, Status: "active", CurrentPriority: priorityPoor, SuccessfulRequests: 20, TotalTokens: 100_000_000, AccountCost: 120},
	}, now, 5)
	byID := make(map[int64]Recommendation, len(result))
	for _, item := range result {
		byID[item.ID] = item
	}
	if byID[1].CostAdvantage != 0 || byID[2].CostAdvantage != 0 || byID[1].RecommendedPriority >= byID[2].RecommendedPriority {
		t.Fatalf("relative cost gate affected direct ranking: cheap=%+v expensive=%+v", byID[1], byID[2])
	}
}

func TestRankLowerBetterUsesGlobalCostOrder(t *testing.T) {
	got := rankLowerBetter([]float64{10, 20, 100, 200}, []bool{true, true, true, true})
	if !(got[0] > got[1] && got[1] > got[2] && got[2] > got[3]) {
		t.Fatalf("global rank is not monotonic: %v", got)
	}
}

func TestScoreAccountsIgnoresInvalidCostAndTokenValues(t *testing.T) {
	now := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	result := scoreAccounts([]AccountMetrics{
		{ID: 1, Status: "active", CurrentPriority: priorityNeutral, SuccessfulRequests: 5, TotalTokens: -1, AccountCost: math.NaN(), ActualCost: math.Inf(1)},
		{ID: 2, Status: "active", CurrentPriority: priorityNeutral, SuccessfulRequests: 5, TotalTokens: 1_000_000, AccountCost: 1},
	}, now, 5)
	if len(result) != 2 {
		t.Fatalf("got %d recommendations", len(result))
	}
	for _, item := range result {
		if !validScore(item.Score) {
			t.Fatalf("invalid score: %+v", item)
		}
		if item.ID == 1 && item.CostPerMillionTokens != 0 {
			t.Fatalf("invalid account cost was retained: %+v", item)
		}
	}
}
