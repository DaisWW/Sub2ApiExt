package main

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

type costEstimate struct {
	Cost, Reference, RankingValue, Weight float64
	HistoryWeight                         float64
	PeerCount, PoolCount, Component       int
	Valid                                 bool
	Reevaluate                            bool
}

// Only successful history in the same model, endpoint, group and tier proves
// eligibility. A multiplier alone never invents model support for an account.
func estimateAccountCosts(accounts []AccountMetrics, now time.Time) []costEstimate {
	type participant struct {
		index int
		pool  PoolMetrics
	}
	pools := make(map[string][]participant)
	parents := make([]int, len(accounts))
	result := make([]costEstimate, len(accounts))
	weights := make([]float64, len(accounts))
	for index, account := range accounts {
		parents[index] = index
		result[index].Component = -1
		if excluded, _ := accountHardExcluded(account, now); excluded || account.RateMultiplier <= 0 || !validScore(account.RateMultiplier) {
			continue
		}
		for _, pool := range account.Pools {
			if !eligibleCostPool(account, pool) {
				continue
			}
			key := poolIdentity(pool)
			pools[key] = append(pools[key], participant{index, pool})
		}
	}
	find := func(index int) int {
		for parents[index] != index {
			index = parents[index]
		}
		return index
	}
	for _, participants := range pools {
		if len(participants) < 2 {
			continue
		}
		var peers []*MetricSnapshot
		var maturePeers []*MetricSnapshot
		for _, item := range participants {
			name, snapshot := recentPoolSnapshot(item.pool)
			requests, tokens := int64(mainWindowMinPricedRequests), mainWindowMinPricedTokens
			if name == "30m" {
				requests, tokens = fastWindowMinPricedRequests, fastWindowMinPricedTokens
			}
			if !snapshotMeetsCostEvidence(snapshot, requests, tokens) || !usableBaseCost(snapshot) {
				continue
			}
			peers = append(peers, snapshot)
			if costEvidenceWeight(snapshot) >= 0.5 {
				maturePeers = append(maturePeers, snapshot)
			}
		}
		// A barely qualified cold sample must not raise the shared prior when
		// there is already a more stable peer in this exact pool.
		if len(maturePeers) > 0 {
			peers = maturePeers
		}
		var inputTokens, totalTokens int64
		for _, snapshot := range peers {
			inputTokens += snapshot.InputTokens + snapshot.CacheCreationTokens + snapshot.CacheReadTokens
			totalTokens += snapshot.PricedTokens
		}
		if len(peers) < recoveryMinimumPeers || totalTokens <= 0 {
			continue
		}
		inputShare := clamp01(float64(inputTokens) / float64(totalTokens))
		baseCosts := make([]float64, 0, len(peers))
		for _, snapshot := range peers {
			if cost := baseCostForMix(snapshot, inputShare); cost > 0 {
				baseCosts = append(baseCosts, cost)
			}
		}
		if len(baseCosts) < recoveryMinimumPeers {
			continue
		}
		reference := percentile(baseCosts, 0.5)
		bestRecentCost := math.Inf(1)
		for _, item := range participants {
			_, snapshot := recentPoolSnapshot(item.pool)
			if costEvidenceWeight(snapshot) >= 0.5 {
				if base := baseCostForMix(snapshot, inputShare); base > 0 {
					bestRecentCost = math.Min(bestRecentCost, base*accounts[item.index].RateMultiplier)
				}
			}
		}
		for _, item := range participants {
			_, snapshot := recentPoolSnapshot(item.pool)
			weight, observed := costEvidenceWeight(snapshot), baseCostForMix(snapshot, inputShare)
			if observed <= 0 {
				weight = 0
				observed = reference
			}
			history, historyWeight := historicalPoolCost(item.pool, inputShare, now)
			historyWeight *= 1 - weight
			cost := accounts[item.index].RateMultiplier * ((1-weight-historyWeight)*reference + weight*observed + historyWeight*history)
			if cost <= 0 || !validScore(cost) {
				continue
			}
			// Use a common workload within each pool; divide out its reference
			// price so different model prices cannot decide the global order.
			trafficWeight := float64(totalTokens)
			estimate := &result[item.index]
			estimate.Cost += trafficWeight * cost
			estimate.Reference += trafficWeight * reference * accounts[item.index].RateMultiplier
			estimate.RankingValue += trafficWeight * cost / reference
			estimate.Weight += trafficWeight * weight
			estimate.HistoryWeight += trafficWeight * historyWeight
			if historyWeight >= 0.15 && !math.IsInf(bestRecentCost, 1) && history*accounts[item.index].RateMultiplier < bestRecentCost*(1-minimumCostAdvantage) {
				estimate.Reevaluate = true
			}
			estimate.PeerCount = max(estimate.PeerCount, len(baseCosts))
			estimate.PoolCount++
			weights[item.index] += trafficWeight
			parents[find(item.index)] = find(participants[0].index)
		}
	}
	for index := range result {
		if weights[index] > 0 {
			result[index].Cost /= weights[index]
			result[index].Reference /= weights[index]
			result[index].RankingValue /= weights[index]
			result[index].Weight /= weights[index]
			result[index].HistoryWeight /= weights[index]
			result[index].Component = find(index)
			result[index].Valid = true
		}
	}
	return result
}

// History is a decaying prior, never proof of fresh trial success. Mature
// recent costs replace it completely, including repeated real cold costs.
func historicalPoolCost(pool PoolMetrics, inputShare float64, now time.Time) (float64, float64) {
	for _, candidate := range []struct {
		snapshot *MetricSnapshot
		cap      float64
	}{{pool.Window24h, 0.85}, {pool.Window7d, 0.5}} {
		snapshot := candidate.snapshot
		quality := costEvidenceWeight(snapshot)
		if quality < 0.5 || snapshot.LastPricedSuccessAt == nil {
			continue
		}
		age := now.Sub(*snapshot.LastPricedSuccessAt)
		cost := baseCostForMix(snapshot, inputShare)
		if age < 0 || age > trafficWindow || cost <= 0 {
			continue
		}
		return cost, math.Min(quality, candidate.cap) * math.Exp2(-age.Hours()/24)
	}
	return 0, 0
}

// Direct costs have no common model-price normalization. Compare only the
// same observed pool set; old schemas retain a platform-local fallback.
func directCompetitionKey(account AccountMetrics) string {
	keys := make([]string, 0, len(account.Pools))
	for _, pool := range account.Pools {
		if eligibleCostPool(account, pool) {
			keys = append(keys, poolIdentity(pool))
		}
	}
	if len(keys) == 0 && (account.GroupDataAvailable || len(account.Pools) > 0) {
		return fmt.Sprintf("account:%d", account.ID)
	}
	sort.Strings(keys)
	return normalizePoolPart(account.Platform) + ":" + strings.Join(keys, "|")
}

func eligibleCostPool(account AccountMetrics, pool PoolMetrics) bool {
	requested, upstream := normalizePoolPart(pool.RequestedModel), normalizePoolPart(pool.UpstreamModel)
	tier, exists := account.GroupPriorities[pool.GroupID]
	if pool.GroupID <= 0 || !exists || tier != pool.GroupPriority || poolPlatform(pool) != normalizePoolPart(account.Platform) ||
		requested == "" || requested == "unknown" || upstream == "" || upstream == "unknown" {
		return false
	}
	for _, snapshot := range []*MetricSnapshot{pool.Window30m, pool.Window2h, pool.Window24h, pool.Window7d} {
		if snapshot != nil && snapshot.PricedRequests > 0 {
			return true
		}
	}
	return false
}

func recentPoolSnapshot(pool PoolMetrics) (string, *MetricSnapshot) {
	fastValid, mainValid := usableBaseCost(pool.Window30m), usableBaseCost(pool.Window2h)
	if fastValid && (!mainValid || costEvidenceWeight(pool.Window30m) > costEvidenceWeight(pool.Window2h)) {
		return "30m", pool.Window30m
	}
	if mainValid {
		return "2h", pool.Window2h
	}
	return "", nil
}

func costEvidenceWeight(snapshot *MetricSnapshot) float64 {
	if snapshot == nil || snapshot.PricedRequests <= 0 || snapshot.PricedTokens <= 0 {
		return 0
	}
	requests := float64(snapshot.PricedRequests)
	// Repeated cold requests remain real costs. After 100 requests the
	// continuity discount expires, even if every request was intermittent.
	continuity := clamp01(math.Max(float64(snapshot.ContinuousPricedRequests)/20, requests/100))
	return clamp01(math.Min(math.Min(requests/40, float64(snapshot.PricedTokens)/2_000_000), 0.35+0.65*continuity))
}

func usableBaseCost(snapshot *MetricSnapshot) bool {
	return snapshot != nil && snapshot.PricedTokens > 0 && snapshot.BaseCost > 0 && validScore(snapshot.BaseCost) &&
		snapshot.HasOutputCost && snapshot.BaseOutputCost >= 0 && validScore(snapshot.BaseOutputCost) &&
		snapshot.BaseOutputCost <= snapshot.BaseCost*(1+1e-9) &&
		snapshot.InputTokens >= 0 && snapshot.OutputTokens >= 0 && snapshot.CacheCreationTokens >= 0 && snapshot.CacheReadTokens >= 0
}

// Normalize the input/output workload, retaining each account's own cache
// benefits inside its input cost. Base costs already remove each log's rate.
func baseCostForMix(snapshot *MetricSnapshot, inputShare float64) float64 {
	if !usableBaseCost(snapshot) {
		return 0
	}
	input := snapshot.InputTokens + snapshot.CacheCreationTokens + snapshot.CacheReadTokens
	if (inputShare > 0 && input <= 0) || (inputShare < 1 && snapshot.OutputTokens <= 0) {
		return 0
	}
	var cost float64
	if inputShare > 0 {
		cost += inputShare * math.Max(0, snapshot.BaseCost-snapshot.BaseOutputCost) * 1_000_000 / float64(input)
	}
	if inputShare < 1 {
		cost += (1 - inputShare) * snapshot.BaseOutputCost * 1_000_000 / float64(snapshot.OutputTokens)
	}
	return cost
}

// Within a 5% cost band, retain the incumbent order to avoid cooling caches
// for marginal savings. Ties at the same current priority remain ties.
func rankEstimatedCosts(values []float64, valid []bool, accounts []AccountMetrics, estimates []costEstimate) []float64 {
	components := make(map[string][]int)
	for index := range values {
		if valid[index] {
			key := "direct:" + directCompetitionKey(accounts[index])
			if estimates[index].Valid {
				key = fmt.Sprintf("estimate:%d", estimates[index].Component)
			}
			components[key] = append(components[key], index)
		}
	}
	result := make([]float64, len(values))
	for index := range result {
		result[index] = 50
	}
	for _, indexes := range components {
		estimated := estimates[indexes[0]].Valid
		sort.Slice(indexes, func(i, j int) bool { return values[indexes[i]] < values[indexes[j]] })
		adjusted, included := make([]float64, len(indexes)), make([]bool, len(indexes))
		for start := 0; start < len(indexes); {
			end := start + 1
			if estimated {
				for end < len(indexes) && values[indexes[end]] <= values[indexes[start]]*(1+minimumCostAdvantage) {
					end++
				}
				sort.Slice(indexes[start:end], func(i, j int) bool {
					left, right := accounts[indexes[start+i]], accounts[indexes[start+j]]
					if left.CurrentPriority != right.CurrentPriority {
						return left.CurrentPriority < right.CurrentPriority
					}
					return left.ID < right.ID
				})
			}
			for position := start; position < end; position++ {
				adjusted[position], included[position] = values[indexes[position]], true
				if estimated {
					adjusted[position] = float64(position + 1)
					if position > start && accounts[indexes[position]].CurrentPriority == accounts[indexes[position-1]].CurrentPriority {
						adjusted[position] = adjusted[position-1]
					}
				}
			}
			start = end
		}
		scores := rankLowerBetter(adjusted, included)
		for position, index := range indexes {
			result[index] = scores[position]
		}
	}
	return result
}
