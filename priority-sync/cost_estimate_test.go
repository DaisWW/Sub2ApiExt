package main

import (
	"math"
	"testing"
)

func comparableCostAccount(id int64, rate, cache float64, requests, tokens, continuous int64) AccountMetrics {
	input := tokens * 9 / 10
	read := int64(float64(input) * cache)
	output := tokens - input
	snapshot := &MetricSnapshot{
		SuccessfulRequests: requests, PricedRequests: requests, PricedTokens: tokens, TotalTokens: tokens,
		InputTokens: input - read, OutputTokens: output, CacheReadTokens: read,
		ContinuousPricedRequests: continuous, HasOutputCost: true,
		BaseOutputCost: float64(output) * 20 / 1_000_000,
	}
	snapshot.BaseCost = float64(input-read)*10/1_000_000 + float64(read)/1_000_000 + snapshot.BaseOutputCost
	snapshot.AccountCost = snapshot.BaseCost * rate
	return AccountMetrics{
		ID: id, Platform: "openai", Status: "active", RateMultiplier: rate, CurrentPriority: priorityNeutral,
		DecisionWindowsAvailable: true, GroupDataAvailable: true, GroupPriorities: map[int64]int{30: 1}, Window2h: snapshot,
		Pools: []PoolMetrics{{Platform: "openai", RequestedModel: "gpt-6.1-sol", UpstreamModel: "gpt-6.1-sol",
			UpstreamEndpoint: "responses", GroupID: 30, GroupPriority: 1, GroupDataAvailable: true,
			Window2h: snapshot, Window7d: snapshot}},
	}
}

func comparableRecoveryAccounts() []AccountMetrics {
	accounts := []AccountMetrics{
		comparableCostAccount(1, 0.1, 0, 40, 2_000_000, 39),
		comparableCostAccount(2, 1, 0, 40, 2_000_000, 39),
		comparableCostAccount(3, 2, 0, 40, 2_000_000, 39),
	}
	// An input-only model workload with a base price of 10/M.
	for index := range accounts {
		snapshot := accounts[index].Window2h
		snapshot.InputTokens, snapshot.OutputTokens = snapshot.PricedTokens, 0
		snapshot.BaseCost, snapshot.BaseOutputCost = 20, 0
		snapshot.AccountCost = 20 * accounts[index].RateMultiplier
	}
	accounts[0].Window2h, accounts[0].Pools[0].Window2h = nil, nil
	accounts[0].CurrentPriority = priorityUnavailable
	accounts[1].CurrentPriority, accounts[2].CurrentPriority = priorityBest, priorityPoor
	return accounts
}

func TestEstimatedCostUsesMultiplierAtEqualCacheRate(t *testing.T) {
	accounts := []AccountMetrics{
		comparableCostAccount(1, 0.1, 0.9, 100, 5_000_000, 90),
		comparableCostAccount(2, 0.2, 0.9, 100, 5_000_000, 90),
	}
	result := scoreAccounts(accounts, nowForTest(), 5)
	cheap, expensive := testRecommendationByID(t, result, 1), testRecommendationByID(t, result, 2)
	if math.Abs(expensive.CostPerMillionTokens/cheap.CostPerMillionTokens-2) > 1e-9 || cheap.RecommendedPriority >= expensive.RecommendedPriority {
		t.Fatalf("equal-cache cost did not follow multiplier: cheap=%+v expensive=%+v", cheap, expensive)
	}
}

func TestMatureCacheEvidenceCanReverseMultiplierOrder(t *testing.T) {
	accounts := []AccountMetrics{
		comparableCostAccount(1, 0.18, 0.7, 100, 5_000_000, 90),
		comparableCostAccount(2, 0.25, 0.95, 100, 5_000_000, 90),
	}
	result := scoreAccounts(accounts, nowForTest(), 5)
	lowRate, cached := testRecommendationByID(t, result, 1), testRecommendationByID(t, result, 2)
	if lowRate.CostEvidenceWeight != 1 || cached.CostEvidenceWeight != 1 || cached.RecommendedPriority >= lowRate.RecommendedPriority {
		t.Fatalf("mature cache savings lost to multiplier prior: lowRate=%+v cached=%+v", lowRate, cached)
	}
}

func TestHugeColdRequestHasLowWeightButRepeatedColdCostsRemainReal(t *testing.T) {
	accounts := []AccountMetrics{
		comparableCostAccount(1, 0.1, 0, 1, 20_000_000, 0),
		comparableCostAccount(2, 0.2, 0.95, 100, 5_000_000, 90),
		comparableCostAccount(3, 0.25, 0.95, 100, 5_000_000, 90),
	}
	result := scoreAccounts(accounts, nowForTest(), 5)
	cold := testRecommendationByID(t, result, 1)
	if cold.CostEvidenceWeight != 0.025 || cold.AnchorPriority != priorityBest || cold.ObservedCostPerMillion <= cold.CostPerMillionTokens {
		t.Fatalf("one huge cold request overrode the comparable prior: %+v", cold)
	}
	accounts[0] = comparableCostAccount(1, 0.1, 0, 100, 20_000_000, 0)
	result = scoreAccounts(accounts, nowForTest(), 5)
	cold = testRecommendationByID(t, result, 1)
	if cold.CostEvidenceWeight != 1 || cold.RecommendedPriority != priorityPoor {
		t.Fatalf("repeated cold traffic received permanent protection: %+v", cold)
	}
}

func TestCostEvidenceWeightAccountsForContinuity(t *testing.T) {
	cold := &MetricSnapshot{PricedRequests: 40, PricedTokens: 2_000_000}
	warm := *cold
	warm.ContinuousPricedRequests = 39
	if costEvidenceWeight(cold) >= costEvidenceWeight(&warm) || costEvidenceWeight(&warm) != 1 {
		t.Fatalf("continuity was ignored: cold=%v warm=%v", costEvidenceWeight(cold), costEvidenceWeight(&warm))
	}
}

func TestSmallQualifiedColdSampleDoesNotRaiseStablePeerPrior(t *testing.T) {
	accounts := []AccountMetrics{
		comparableCostAccount(1, 0.1, 0, 5, 500_000, 0),
		comparableCostAccount(2, 0.2, 0.95, 100, 5_000_000, 90),
	}
	result := scoreAccounts(accounts, nowForTest(), 5)
	cold := testRecommendationByID(t, result, 1)
	if cold.AnchorPriority != priorityBest || !cold.recoveryNeeded || cold.RecoveryPeerCount != 1 {
		t.Fatalf("a short cold sample contaminated the stable peer prior: %+v", cold)
	}
}

func TestEstimatedCostUsesCommonInputOutputWorkloadAndCurrentRate(t *testing.T) {
	accounts := []AccountMetrics{
		comparableCostAccount(1, 0.1, 0, 100, 5_000_000, 90),
		comparableCostAccount(2, 0.15, 0, 100, 5_000_000, 90),
	}
	// Account 1's historical rate was 2 and its workload was output-heavy.
	one := accounts[0].Window2h
	one.InputTokens, one.OutputTokens = 500_000, 4_500_000
	one.BaseOutputCost, one.BaseCost, one.AccountCost = 90, 95, 190
	result := scoreAccounts(accounts, nowForTest(), 5)
	cheap, expensive := testRecommendationByID(t, result, 1), testRecommendationByID(t, result, 2)
	if cheap.ObservedCostPerMillion <= expensive.ObservedCostPerMillion || cheap.RecommendedPriority >= expensive.RecommendedPriority ||
		math.Abs(expensive.CostPerMillionTokens/cheap.CostPerMillionTokens-1.5) > 1e-9 {
		t.Fatalf("historical rate/workload distorted comparison: cheap=%+v expensive=%+v", cheap, expensive)
	}
}

func TestCostPriorRequiresSameModelGroupTierEndpointAndContext(t *testing.T) {
	tests := []struct {
		name   string
		change func(*AccountMetrics)
	}{
		{"model", func(a *AccountMetrics) { a.Pools[0].RequestedModel = "other" }},
		{"upstream", func(a *AccountMetrics) { a.Pools[0].UpstreamModel = "other" }},
		{"group", func(a *AccountMetrics) { a.Pools[0].GroupID = 31; a.GroupPriorities = map[int64]int{31: 1} }},
		{"tier", func(a *AccountMetrics) { a.Pools[0].GroupPriority = 2; a.GroupPriorities[30] = 2 }},
		{"stale membership", func(a *AccountMetrics) { a.GroupPriorities = nil }},
		{"endpoint", func(a *AccountMetrics) { a.Pools[0].UpstreamEndpoint = "chat" }},
		{"context", func(a *AccountMetrics) { a.Pools[0].LongContext = true }},
		{"unknown model", func(a *AccountMetrics) { a.Pools[0].RequestedModel = "unknown" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			accounts := comparableRecoveryAccounts()
			test.change(&accounts[0])
			candidate := testRecommendationByID(t, scoreAccounts(accounts, nowForTest(), 5), 1)
			if candidate.CostEstimateSource != "" || candidate.recoveryNeeded || candidate.RecommendedPriority != priorityUnavailable {
				t.Fatalf("incompatible evidence created a recovery anchor: %+v", candidate)
			}
		})
	}
}

func TestCostPriorRejectsUnpricedAndUnqualifiedHistory(t *testing.T) {
	accounts := comparableRecoveryAccounts()
	for index := 1; index < len(accounts); index++ {
		accounts[index].Pools[0].Window2h.PricedRequests = 0
	}
	if got := estimateAccountCosts(accounts, nowForTest())[0]; got.Valid {
		t.Fatalf("unpriced or old history entered the cost prior: %+v", got)
	}
}

func TestRecentPoolSnapshotFallsBackFromIncompleteTwoHourCosts(t *testing.T) {
	account := comparableCostAccount(1, 0.1, 0.9, 100, 5_000_000, 90)
	fast := *account.Window2h
	fast.PricedRequests, fast.PricedTokens = 20, 1_000_000
	account.Pools[0].Window30m = &fast
	account.Pools[0].Window2h.HasOutputCost = false
	window, snapshot := recentPoolSnapshot(account.Pools[0])
	if window != "30m" || snapshot != &fast {
		t.Fatalf("incomplete long-window costs hid valid recent evidence: window=%s snapshot=%+v", window, snapshot)
	}
}

func TestEstimatedCostDeadbandRetainsIncumbentUntilSavingsAreMaterial(t *testing.T) {
	accounts := []AccountMetrics{
		comparableCostAccount(1, 0.205, 0.9, 100, 5_000_000, 90),
		comparableCostAccount(2, 0.2, 0.9, 100, 5_000_000, 90),
	}
	accounts[0].CurrentPriority, accounts[1].CurrentPriority = priorityBest, priorityPoor
	result := scoreAccounts(accounts, nowForTest(), 5)
	if testRecommendationByID(t, result, 1).RecommendedPriority != priorityBest || testRecommendationByID(t, result, 2).RecommendedPriority != priorityPoor {
		t.Fatalf("marginal savings reversed the incumbent: %+v", result)
	}
	accounts[0].RateMultiplier = 0.25
	result = scoreAccounts(accounts, nowForTest(), 5)
	if testRecommendationByID(t, result, 2).RecommendedPriority != priorityBest {
		t.Fatalf("material savings could not change the order: %+v", result)
	}
}

func TestDisconnectedModelsRankWithinTheirOwnCompetition(t *testing.T) {
	accounts := []AccountMetrics{
		comparableCostAccount(1, 0.1, 0.9, 100, 5_000_000, 90),
		comparableCostAccount(2, 0.2, 0.9, 100, 5_000_000, 90),
		comparableCostAccount(3, 0.1, 0.9, 100, 5_000_000, 90),
		comparableCostAccount(4, 0.2, 0.9, 100, 5_000_000, 90),
	}
	for index := 2; index < len(accounts); index++ {
		accounts[index].Pools[0].RequestedModel, accounts[index].Pools[0].UpstreamModel = "expensive-model", "expensive-model"
		accounts[index].Window2h.BaseCost *= 100
		accounts[index].Window2h.BaseOutputCost *= 100
		accounts[index].Window2h.AccountCost *= 100
	}
	result := scoreAccounts(accounts, nowForTest(), 5)
	if testRecommendationByID(t, result, 1).RecommendedPriority != priorityBest || testRecommendationByID(t, result, 3).RecommendedPriority != priorityBest ||
		testRecommendationByID(t, result, 2).RecommendedPriority != priorityPoor || testRecommendationByID(t, result, 4).RecommendedPriority != priorityPoor {
		t.Fatalf("unrelated model prices changed the candidate ordering: %+v", result)
	}
}
