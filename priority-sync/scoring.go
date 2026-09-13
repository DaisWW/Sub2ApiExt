package main

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

// scoreAccounts 将账户在各模型池的实际风险成本汇总为全局排序。
// 成本是目标；速度只在成本接近时提供很小的区分，硬故障直接排除。
func scoreAccounts(accounts []AccountMetrics, now time.Time, minSamples int) []Recommendation {
	if minSamples < 1 {
		minSamples = defaultMinSamples
	}
	pools := aggregatePools(accounts, now, minSamples)
	costValues := make([]float64, len(accounts))
	observedValues := make([]float64, len(accounts))
	fallbackValues := make([]float64, len(accounts))
	cacheHitRates := make([]float64, len(accounts))
	multiplierValues := make([]float64, len(accounts))
	speedValues := make([]float64, len(accounts))
	costValid := make([]bool, len(accounts))
	costAdvantages := make([]float64, len(accounts))
	costAdvantageKnown := make([]bool, len(accounts))
	multiplierValid := make([]bool, len(accounts))
	speedValid := make([]bool, len(accounts))
	peerCostValid := make([]bool, len(accounts))
	multiplierEligible := make([]bool, len(accounts))
	peerSpeedValid := make([]bool, len(accounts))
	for index, account := range accounts {
		evidence := account.SuccessfulRequests + account.TerminalFailures
		hardExcluded, _ := accountHardExcluded(account, now)
		peerEligible := evidence >= int64(minSamples) && !hardExcluded
		observed, fallback, hitRate, tokens := accountRiskCost(account, pools)
		observedValues[index] = observed
		fallbackValues[index] = fallback
		cacheHitRates[index] = hitRate
		if tokens > 0 && observed > 0 && validScore(observed) {
			lambda := float64(tokens) / float64(tokens+shrinkTokens)
			anchor := multiplierAnchorCost(account, pools)
			if anchor > 0 {
				observed = lambda*observed + (1-lambda)*anchor
			}
			failureRate := finalFailureRate(account)
			softRate := recovered429Rate(account)
			costValues[index] = observed + failureRate*maxFloat(fallback-observed, 0) + 0.02*softRate*observed
			costValid[index] = validScore(costValues[index]) && costValues[index] > 0
			peerCostValid[index] = costValid[index] && peerEligible
		}
		multiplierValues[index] = account.RateMultiplier
		multiplierValid[index] = multiplierValues[index] > 0 && validScore(multiplierValues[index])
		multiplierEligible[index] = multiplierValid[index] && !hardExcluded
		speedValues[index] = speedMetric(account.LatencyP90Ms, account.FirstTokenP90Ms)
		speedValid[index] = speedValues[index] > 0 && validScore(speedValues[index])
		peerSpeedValid[index] = speedValid[index] && peerEligible
	}
	costAdvantages, costAdvantageKnown = computeCostAdvantages(accounts, pools, costValues, costValid, now, minSamples)
	costScores := normalizeLowerBetter(costValues, peerCostValid)
	multiplierScores := normalizeLowerBetter(multiplierValues, multiplierEligible)
	speedScores := normalizeLowerBetter(speedValues, peerSpeedValid)

	result := make([]Recommendation, 0, len(accounts))
	for index, account := range accounts {
		// Recovered 429 belongs to the same final successful request and must not
		// increase the evidence count a second time. Its delay is already applied
		// through RecoveredRateLimitWeight in effectiveAvailability.
		evidence := account.SuccessfulRequests + account.TerminalFailures
		availability := effectiveAvailability(account)
		availabilityScore := availability * 100
		if evidence == 0 {
			availabilityScore = 50
		}
		confidence := math.Min(1, float64(evidence)/float64(minSamples))
		rawScore := costWeight*costScores[index] + speedWeight*speedScores[index]
		score := rawScore*confidence + 50*(1-confidence)

		hardExcluded, hardReason := accountHardExcluded(account, now)
		currentPriority := normalizedPriority(account.CurrentPriority)
		recommended := currentPriority
		reason := "证据不足，保持当前优先级"
		anchorPriority := coldAnchorPriority(multiplierScores[index])
		applyImmediately := hardExcluded
		if hardExcluded {
			recommended = priorityUnavailable
			reason = hardReason
		} else if account.TerminalFailures >= 3 {
			// The aggregate query does not expose an attempt sequence, so three
			// terminal outcomes in the window are the conservative proxy for a
			// trailing failure streak and trigger an immediate move out of the
			// preferred band.
			recommended = maxPriority(currentPriority, priorityDegraded)
			reason = "窗口内最终失败达到 3 次（连续失败代理），立即降级"
			applyImmediately = true
		} else if evidence >= int64(minSamples) {
			recommended = priorityForScore(score)
			reason = fmt.Sprintf("风险成本 %.4f/M、实测 %.4f/M、后备 %.4f/M、模型池 %d、缓存命中 %.1f%%", costValues[index], observedValues[index], fallbackValues[index], len(account.Pools), cacheHitRates[index]*100)
		} else if anchorPriority != currentPriority {
			recommended = anchorPriority
			reason = fmt.Sprintf("证据不足，向倍率锚点优先级 %d 缓慢靠近", anchorPriority)
		}
		if account.RecoveredRateLimited > 0 && !hardExcluded {
			reason += fmt.Sprintf("；恢复性 429 %d 次按软惩罚计入", account.RecoveredRateLimited)
		}

		recommendation := Recommendation{
			ID:                       account.ID,
			Name:                     account.Name,
			Platform:                 account.Platform,
			Status:                   account.Status,
			CurrentPriority:          currentPriority,
			RecommendedPriority:      recommended,
			LatencyP90Ms:             positiveOrZero(account.LatencyP90Ms),
			FirstTokenP90Ms:          positiveOrZero(account.FirstTokenP90Ms),
			SuccessfulRequests:       account.SuccessfulRequests,
			TerminalFailures:         account.TerminalFailures,
			RateLimitedRequests:      account.RateLimitedRequests,
			RecoveredRateLimited:     account.RecoveredRateLimited,
			RecoveredRateLimitWeight: account.RecoveredRateLimitWeight,
			Availability:             availability,
			Confidence:               confidence,
			CostScore:                costScores[index],
			MultiplierScore:          multiplierScores[index],
			SpeedScore:               speedScores[index],
			AvailabilityScore:        availabilityScore,
			Score:                    score,
			HardExcluded:             hardExcluded,
			AnchorPriority:           anchorPriority,
			Reason:                   reason,
			applyImmediately:         applyImmediately,
			costAdvantageKnown:       costAdvantageKnown[index],
		}
		if costValid[index] {
			recommendation.CostPerMillionTokens = costValues[index]
			recommendation.ObservedCostPerMillion = observedValues[index]
			recommendation.FallbackMissCostPerMillion = fallbackValues[index]
			recommendation.CostAdvantage = costAdvantages[index]
			recommendation.CacheHitRate = cacheHitRates[index]
			recommendation.PoolCount = len(account.Pools)
		}
		result = append(result, recommendation)
	}
	enforceCostDominance(result, costValues, peerCostValid)
	for index := range result {
		if result[index].HardExcluded {
			continue
		}
		account := accounts[index]
		if result[index].Confidence >= 1 && !result[index].applyImmediately {
			result[index].RecommendedPriority = priorityForScore(result[index].Score)
		}
		evidence := result[index].SuccessfulRequests + result[index].TerminalFailures
		successRate := finalSuccessRate(account)
		if successRate < 0.90 && evidence >= 20 {
			if result[index].RecommendedPriority < priorityDegraded {
				result[index].RecommendedPriority = priorityDegraded
			}
			result[index].Reason += "；最终成功率低于 90%，进入明显降级区间"
		} else if successRate < 0.95 && result[index].RecommendedPriority < result[index].CurrentPriority {
			result[index].RecommendedPriority = result[index].CurrentPriority
			result[index].Reason += "；最终成功率低于 95%，禁止提升"
		}
	}
	sortRecommendations(result)
	return result
}

type poolAggregate struct {
	Platform                  string
	InputTokens               int64
	OutputTokens              int64
	CacheCreationTokens       int64
	CacheReadTokens           int64
	TotalTokens               int64
	Window24h                 MetricSnapshot
	Window6h                  MetricSnapshot
	Window7d                  MetricSnapshot
	HasWindow24h              bool
	HasWindow6h               bool
	HasWindow7d               bool
	CostsByAccount            map[int64]float64
	MissCostsByAccount        map[int64]float64
	PeerCostsByAccount        map[int64]float64
	PeerMissCostsByAccount    map[int64]float64
	Costs24hByAccount         map[int64]float64
	MissCosts24hByAccount     map[int64]float64
	PeerCosts24hByAccount     map[int64]float64
	PeerMissCosts24hByAccount map[int64]float64
	Rates                     []float64
	PeerRates                 []float64
}

func aggregatePools(accounts []AccountMetrics, now time.Time, minSamples int) map[string]poolAggregate {
	result := make(map[string]poolAggregate)
	for _, account := range accounts {
		evidence := account.SuccessfulRequests + account.TerminalFailures
		hardExcluded, _ := accountHardExcluded(account, now)
		peerEligible := evidence >= int64(minSamples) && !hardExcluded
		for _, pool := range account.Pools {
			key := poolIdentity(pool)
			item := result[key]
			if item.Platform == "" {
				item.Platform = poolPlatform(pool)
			}
			item.InputTokens += maxInt64(pool.InputTokens, 0)
			item.OutputTokens += maxInt64(pool.OutputTokens, 0)
			item.CacheCreationTokens += maxInt64(pool.CacheCreationTokens, 0)
			item.CacheReadTokens += maxInt64(pool.CacheReadTokens, 0)
			item.TotalTokens += maxInt64(pool.TotalTokens, 0)
			if pool.Window24h != nil {
				addMetricSnapshot(&item.Window24h, *pool.Window24h)
				item.HasWindow24h = true
			}
			if pool.Window6h != nil {
				addMetricSnapshot(&item.Window6h, *pool.Window6h)
				item.HasWindow6h = true
			}
			if pool.Window7d != nil {
				addMetricSnapshot(&item.Window7d, *pool.Window7d)
				item.HasWindow7d = true
			}
			if item.CostsByAccount == nil {
				item.CostsByAccount = make(map[int64]float64)
			}
			if item.MissCostsByAccount == nil {
				item.MissCostsByAccount = make(map[int64]float64)
			}
			if item.PeerCostsByAccount == nil {
				item.PeerCostsByAccount = make(map[int64]float64)
			}
			if item.PeerMissCostsByAccount == nil {
				item.PeerMissCostsByAccount = make(map[int64]float64)
			}
			if item.Costs24hByAccount == nil {
				item.Costs24hByAccount = make(map[int64]float64)
			}
			if item.MissCosts24hByAccount == nil {
				item.MissCosts24hByAccount = make(map[int64]float64)
			}
			if item.PeerCosts24hByAccount == nil {
				item.PeerCosts24hByAccount = make(map[int64]float64)
			}
			if item.PeerMissCosts24hByAccount == nil {
				item.PeerMissCosts24hByAccount = make(map[int64]float64)
			}
			if pool.RateMultiplier > 0 && validScore(pool.RateMultiplier) {
				item.Rates = append(item.Rates, pool.RateMultiplier)
				if peerEligible {
					item.PeerRates = append(item.PeerRates, pool.RateMultiplier)
				}
			}
			result[key] = item
		}
	}
	for _, account := range accounts {
		for _, pool := range account.Pools {
			item := result[poolIdentity(pool)]
			evidence := account.SuccessfulRequests + account.TerminalFailures
			hardExcluded, _ := accountHardExcluded(account, now)
			if value := standardizedPoolCost(pool, item); value > 0 {
				item.CostsByAccount[account.ID] = value
				if evidence >= int64(minSamples) && !hardExcluded {
					item.PeerCostsByAccount[account.ID] = value
				}
				if miss := poolMissCostPerMillion(pool); miss > 0 {
					item.MissCostsByAccount[account.ID] = miss
					if evidence >= int64(minSamples) && !hardExcluded {
						item.PeerMissCostsByAccount[account.ID] = miss
					}
				}
			}
			if item.HasWindow24h {
				if value24 := poolWindowCost(pool.Window24h, item.Window24h); value24 > 0 {
					item.Costs24hByAccount[account.ID] = value24
					if evidence >= int64(minSamples) && !hardExcluded {
						item.PeerCosts24hByAccount[account.ID] = value24
					}
				}
				if miss24 := snapshotMissCost(pool.Window24h); miss24 > 0 {
					item.MissCosts24hByAccount[account.ID] = miss24
					if evidence >= int64(minSamples) && !hardExcluded {
						item.PeerMissCosts24hByAccount[account.ID] = miss24
					}
				}
			}
			result[poolIdentity(pool)] = item
		}
	}
	return result
}

func poolIdentity(pool PoolMetrics) string {
	if key := strings.TrimSpace(pool.Key); key != "" {
		return strings.ToLower(key)
	}
	platform := normalizePoolPart(pool.Platform)
	requested := normalizePoolPart(pool.RequestedModel)
	if requested == "" {
		requested = normalizePoolPart(pool.Model)
	}
	upstream := normalizePoolPart(pool.UpstreamModel)
	if upstream == "" {
		upstream = normalizePoolPart(pool.Model)
	}
	if platform == "" {
		platform = "unknown"
	}
	if requested == "" {
		requested = "unknown"
	}
	if upstream == "" {
		upstream = "unknown"
	}
	identity := platform + ":" + requested + ":" + upstream
	if endpoint := normalizePoolPart(pool.UpstreamEndpoint); endpoint != "" {
		identity += ":" + endpoint
	}
	return identity
}

func normalizePoolPart(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

func poolPlatform(pool PoolMetrics) string {
	if platform := normalizePoolPart(pool.Platform); platform != "" {
		return platform
	}
	key := normalizePoolPart(pool.Key)
	if separator := strings.IndexByte(key, ':'); separator > 0 {
		return key[:separator]
	}
	return "unknown"
}

func poolCostPerMillion(pool PoolMetrics) float64 {
	if pool.TotalTokens <= 0 {
		return 0
	}
	if pool.AccountCost > 0 && validScore(pool.AccountCost) {
		return pool.AccountCost * 1_000_000 / float64(pool.TotalTokens)
	}
	if pool.ActualCost > 0 && validScore(pool.ActualCost) {
		return pool.ActualCost * 1_000_000 / float64(pool.TotalTokens)
	}
	return 0
}

func standardizedPoolCost(pool PoolMetrics, aggregate poolAggregate) float64 {
	if pool.TotalTokens <= 0 {
		return 0
	}
	base := poolCostPerMillion(pool)
	weights := []struct {
		poolTokens int64
		allTokens  int64
		cost       float64
	}{
		{pool.InputTokens, aggregate.InputTokens, pool.InputCost},
		{pool.OutputTokens, aggregate.OutputTokens, pool.OutputCost},
		{pool.CacheCreationTokens, aggregate.CacheCreationTokens, pool.CacheCreationCost},
		{pool.CacheReadTokens, aggregate.CacheReadTokens, pool.CacheReadCost},
	}
	var result float64
	var usedWeight float64
	for _, item := range weights {
		if item.allTokens <= 0 {
			continue
		}
		weight := float64(item.allTokens) / float64(maxInt64(aggregate.TotalTokens, 1))
		unit := base
		if item.poolTokens > 0 && item.cost > 0 && validScore(item.cost) {
			unit = item.cost * 1_000_000 / float64(item.poolTokens)
		}
		result += weight * unit
		usedWeight += weight
	}
	if usedWeight <= 0 {
		return base
	}
	if usedWeight < 1 {
		result += (1 - usedWeight) * base
	}
	return result
}

func addMetricSnapshot(target *MetricSnapshot, source MetricSnapshot) {
	if target == nil {
		return
	}
	target.TotalTokens += maxInt64(source.TotalTokens, 0)
	target.InputTokens += maxInt64(source.InputTokens, 0)
	target.OutputTokens += maxInt64(source.OutputTokens, 0)
	target.CacheCreationTokens += maxInt64(source.CacheCreationTokens, 0)
	target.CacheReadTokens += maxInt64(source.CacheReadTokens, 0)
	target.AccountCost += positiveMetric(source.AccountCost)
	target.ActualCost += positiveMetric(source.ActualCost)
	target.InputCost += positiveMetric(source.InputCost)
	target.OutputCost += positiveMetric(source.OutputCost)
	target.CacheCreationCost += positiveMetric(source.CacheCreationCost)
	target.CacheReadCost += positiveMetric(source.CacheReadCost)
}

func positiveMetric(value float64) float64 {
	if value > 0 && validScore(value) {
		return value
	}
	return 0
}

func poolWindowCost(snapshot *MetricSnapshot, aggregate MetricSnapshot) float64 {
	if snapshot == nil {
		return 0
	}
	return standardizedSnapshotCost(*snapshot, aggregate)
}

func standardizedSnapshotCost(snapshot, aggregate MetricSnapshot) float64 {
	if snapshot.TotalTokens <= 0 {
		return 0
	}
	base := snapshotCostPerMillion(snapshot)
	if base <= 0 {
		return 0
	}
	weights := []struct {
		tokens int64
		all    int64
		cost   float64
	}{
		{snapshot.InputTokens, aggregate.InputTokens, snapshot.InputCost},
		{snapshot.OutputTokens, aggregate.OutputTokens, snapshot.OutputCost},
		{snapshot.CacheCreationTokens, aggregate.CacheCreationTokens, snapshot.CacheCreationCost},
		{snapshot.CacheReadTokens, aggregate.CacheReadTokens, snapshot.CacheReadCost},
	}
	var result, used float64
	for _, item := range weights {
		if item.all <= 0 {
			continue
		}
		weight := float64(item.all) / float64(maxInt64(aggregate.TotalTokens, 1))
		unit := base
		if item.tokens > 0 && item.cost > 0 && validScore(item.cost) {
			unit = item.cost * 1_000_000 / float64(item.tokens)
		}
		result += weight * unit
		used += weight
	}
	if used <= 0 {
		return base
	}
	if used < 1 {
		result += (1 - used) * base
	}
	return result
}

func snapshotCostPerMillion(snapshot MetricSnapshot) float64 {
	if snapshot.TotalTokens <= 0 {
		return 0
	}
	value := positiveMetric(snapshot.AccountCost)
	if value <= 0 {
		value = positiveMetric(snapshot.ActualCost)
	}
	if value <= 0 {
		return 0
	}
	return value * 1_000_000 / float64(snapshot.TotalTokens)
}

func snapshotMissCost(snapshot *MetricSnapshot) float64 {
	if snapshot == nil || snapshot.TotalTokens <= 0 {
		return 0
	}
	base := snapshotCostPerMillion(*snapshot)
	if base <= 0 {
		return 0
	}
	missTokens := snapshot.TotalTokens - maxInt64(snapshot.CacheReadTokens, 0)
	if missTokens <= 0 {
		return 0
	}
	if snapshot.CacheReadTokens <= 0 || snapshot.CacheReadCost <= 0 || snapshot.AccountCost <= snapshot.CacheReadCost {
		return base
	}
	return (snapshot.AccountCost - snapshot.CacheReadCost) * 1_000_000 / float64(missTokens)
}

func poolMissCostPerMillion(pool PoolMetrics) float64 {
	nonCacheTokens := pool.TotalTokens - maxInt64(pool.CacheReadTokens, 0)
	if nonCacheTokens <= 0 {
		return 0
	}
	if pool.CacheReadTokens <= 0 || pool.CacheReadCost <= 0 || pool.AccountCost <= pool.CacheReadCost {
		return poolCostPerMillion(pool)
	}
	return (pool.AccountCost - pool.CacheReadCost) * 1_000_000 / float64(nonCacheTokens)
}

func accountRiskCost(account AccountMetrics, pools map[string]poolAggregate) (observed, fallback, hitRate float64, tokens int64) {
	if len(pools) == 0 {
		return accountWindowRiskCost(account)
	}
	accountPools := make(map[string]PoolMetrics, len(account.Pools))
	for _, pool := range account.Pools {
		accountPools[poolIdentity(pool)] = pool
	}
	platforms := map[string]struct{}{}
	if platform := normalizePoolPart(account.Platform); platform != "" {
		platforms[platform] = struct{}{}
	}
	for _, pool := range account.Pools {
		platforms[poolPlatform(pool)] = struct{}{}
	}
	if len(platforms) == 0 {
		platforms["unknown"] = struct{}{}
	}
	allTokens := int64(0)
	poolCount := 0
	for _, pool := range pools {
		if _, eligible := platforms[pool.Platform]; !eligible {
			continue
		}
		allTokens += poolTrafficTokens(pool)
		poolCount++
	}
	if allTokens <= 0 || poolCount == 0 {
		return accountWindowRiskCost(account)
	}
	uniformWeight := 0.1 / float64(poolCount)
	var fallbackTotal, observedTotal, costWeightUsed, cacheTotal, cacheWeightUsed float64
	for key, aggregate := range pools {
		if _, eligible := platforms[aggregate.Platform]; !eligible {
			continue
		}
		trafficTokens := poolTrafficTokens(aggregate)
		trafficWeight := 0.9*float64(maxInt64(trafficTokens, 0))/float64(allTokens) + uniformWeight
		pool, exists := accountPools[key]
		if exists && !poolHasRecentEvidence(pool) {
			exists = false
		}
		peerCosts := mapValues(aggregate.PeerCostsByAccount)
		if len(peerCosts) == 0 {
			peerCosts = mapValues(aggregate.CostsByAccount)
		}
		value := percentile(peerCosts, 0.5)
		if exists {
			value = poolRiskCost(pool, aggregate, peerCosts)
			poolTokens := pool.TotalTokens
			if pool.Window24h != nil && pool.Window24h.TotalTokens > 0 {
				poolTokens = pool.Window24h.TotalTokens
			}
			tokens += maxInt64(poolTokens, 0)
			cacheInput, cacheRead := pool.InputTokens, pool.CacheReadTokens
			if pool.Window24h != nil {
				cacheInput, cacheRead = pool.Window24h.InputTokens, pool.Window24h.CacheReadTokens
			}
			if maxInt64(cacheInput, 0)+maxInt64(cacheRead, 0) > 0 {
				cacheTotal += cacheHitRate(cacheInput, cacheRead) * trafficWeight
				cacheWeightUsed += trafficWeight
			}
		}
		if !exists {
			// Missing pool evidence gets a multiplier-adjusted peer median. It
			// contributes to the global prior, but not to this account's sample
			// token count, so a cold account cannot look free merely by omission.
			medianRates := aggregate.PeerRates
			if len(medianRates) == 0 {
				medianRates = aggregate.Rates
			}
			medianRate := percentile(medianRates, 0.5)
			if value > 0 && account.RateMultiplier > 0 && medianRate > 0 {
				value *= account.RateMultiplier / medianRate
			}
		}
		if value <= 0 {
			continue
		}
		peerMissCosts := aggregate.PeerMissCosts24hByAccount
		if len(peerMissCosts) == 0 {
			peerMissCosts = aggregate.PeerMissCostsByAccount
		}
		fallbackPool := percentile(otherPoolCosts(peerMissCosts, account.ID), 0.75)
		if fallbackPool <= 0 {
			fallbackPool = percentile(mapValues(peerMissCosts), 0.75)
		}
		if fallbackPool <= 0 {
			fallbackPool = value
		}
		observedTotal += trafficWeight * value
		fallbackTotal += trafficWeight * fallbackPool
		costWeightUsed += trafficWeight
	}
	if costWeightUsed <= 0 {
		return accountWindowRiskCost(account)
	}
	observedTotal /= costWeightUsed
	fallbackTotal /= costWeightUsed
	if cacheWeightUsed > 0 {
		cacheTotal /= cacheWeightUsed
	}
	return observedTotal, fallbackTotal, cacheTotal, tokens
}

func poolHasRecentEvidence(pool PoolMetrics) bool {
	if pool.TotalTokens > 0 {
		return true
	}
	for _, snapshot := range []*MetricSnapshot{pool.Window24h, pool.Window6h} {
		if snapshot != nil && snapshot.TotalTokens > 0 {
			return true
		}
	}
	return false
}

func poolTrafficTokens(pool poolAggregate) int64 {
	if pool.HasWindow7d && pool.Window7d.TotalTokens > 0 {
		return maxInt64(pool.Window7d.TotalTokens, 0)
	}
	if pool.TotalTokens > 0 {
		return maxInt64(pool.TotalTokens, 0)
	}
	if pool.HasWindow24h {
		return maxInt64(pool.Window24h.TotalTokens, 0)
	}
	return 0
}

func accountWindowRiskCost(account AccountMetrics) (observed, fallback, hitRate float64, tokens int64) {
	current := MetricSnapshot{
		TotalTokens:         account.TotalTokens,
		InputTokens:         account.InputTokens,
		OutputTokens:        account.OutputTokens,
		CacheCreationTokens: account.CacheCreationTokens,
		CacheReadTokens:     account.CacheReadTokens,
		AccountCost:         account.AccountCost,
		ActualCost:          account.ActualCost,
		CostP75PerMillion:   account.CostP75PerMillion,
		InputCost:           account.InputCost,
		OutputCost:          account.OutputCost,
		CacheCreationCost:   account.CacheCreationCost,
		CacheReadCost:       account.CacheReadCost,
	}
	if current.InputTokens <= 0 && current.OutputTokens <= 0 &&
		current.CacheCreationTokens <= 0 && current.CacheReadTokens <= 0 {
		// Preserve compatibility with sources that only expose total_tokens.
		current.InputTokens = current.TotalTokens
	}
	window24 := account.Window24h
	if window24 == nil {
		window24 = &current
	}
	window6 := account.Window6h
	if window6 == nil {
		window6 = window24
	}
	c24 := snapshotCostPerMillion(*window24)
	c6 := snapshotCostPerMillion(*window6)
	if c24 <= 0 {
		c24 = c6
	}
	if c6 <= 0 {
		c6 = c24
	}
	if c24 <= 0 {
		return 0, 0, 0, 0
	}
	recent := 0.7*c24 + 0.3*c6
	p75 := positiveMetric(window24.CostP75PerMillion)
	if p75 <= 0 {
		p75 = recent
	}
	observed = 0.8*recent + 0.2*p75
	fallback = snapshotMissCost(window24)
	if fallback <= 0 {
		fallback = observed
	}
	tokens = maxInt64(window24.TotalTokens, 0)
	if tokens == 0 {
		tokens = maxInt64(account.TotalTokens, 0)
	}
	hitRate = cacheHitRate(window24.InputTokens, window24.CacheReadTokens)
	return observed, fallback, hitRate, tokens
}

func poolRiskCost(pool PoolMetrics, aggregate poolAggregate, peerCosts []float64) float64 {
	current := standardizedPoolCost(pool, aggregate)
	if pool.Window24h == nil && pool.Window6h == nil {
		if current > 0 {
			return current
		}
		return percentile(peerCosts, 0.5)
	}
	c24 := poolWindowCost(pool.Window24h, aggregate.Window24h)
	c6 := poolWindowCost(pool.Window6h, aggregate.Window6h)
	if c24 <= 0 {
		c24 = current
	}
	if c6 <= 0 {
		c6 = c24
	}
	if c24 <= 0 {
		return percentile(peerCosts, 0.5)
	}
	recent := 0.7*c24 + 0.3*c6
	p75 := 0.0
	if pool.Window24h != nil {
		p75 = positiveMetric(pool.Window24h.CostP75PerMillion)
	}
	if p75 <= 0 {
		p75 = recent
	}
	return 0.8*recent + 0.2*p75
}

func computeCostAdvantages(accounts []AccountMetrics, pools map[string]poolAggregate, costs []float64, valid []bool, now time.Time, minSamples int) ([]float64, []bool) {
	result := make([]float64, len(accounts))
	known := make([]bool, len(accounts))
	for index, account := range accounts {
		if index >= len(costs) || index >= len(valid) || !valid[index] || costs[index] <= 0 {
			continue
		}
		hardExcluded, _ := accountHardExcluded(account, now)
		if hardExcluded || account.SuccessfulRequests+account.TerminalFailures < int64(minSamples) {
			continue
		}
		peers := make([]float64, 0)
		for _, pool := range account.Pools {
			aggregate := pools[poolIdentity(pool)]
			values := aggregate.PeerCosts24hByAccount
			if len(values) == 0 {
				values = aggregate.PeerCostsByAccount
			}
			for peerID, value := range values {
				if peerID != account.ID && value > 0 && validScore(value) {
					peers = append(peers, value)
				}
			}
		}
		if len(peers) == 0 && len(pools) == 0 {
			for peerIndex, peer := range accounts {
				if peerIndex == index || peerIndex >= len(costs) || peerIndex >= len(valid) || !valid[peerIndex] || costs[peerIndex] <= 0 {
					continue
				}
				peerHardExcluded, _ := accountHardExcluded(peer, now)
				if peerHardExcluded || peer.SuccessfulRequests+peer.TerminalFailures < int64(minSamples) {
					continue
				}
				peers = append(peers, costs[peerIndex])
			}
		}
		peerMedian := percentile(peers, 0.5)
		if peerMedian > 0 && validScore(peerMedian) {
			known[index] = true
			if peerMedian > costs[index] {
				result[index] = clamp01((peerMedian - costs[index]) / peerMedian)
			}
		}
	}
	return result, known
}

func multiplierAnchorCost(account AccountMetrics, pools map[string]poolAggregate) float64 {
	if account.RateMultiplier <= 0 || !validScore(account.RateMultiplier) {
		return 0
	}
	var anchor, weight float64
	for _, pool := range account.Pools {
		aggregate := pools[poolIdentity(pool)]
		peerCosts := mapValues(aggregate.PeerCostsByAccount)
		if len(peerCosts) == 0 {
			peerCosts = mapValues(aggregate.CostsByAccount)
		}
		median := percentile(peerCosts, 0.5)
		if median <= 0 || pool.TotalTokens <= 0 {
			continue
		}
		medianRates := aggregate.PeerRates
		if len(medianRates) == 0 {
			medianRates = aggregate.Rates
		}
		medianRate := percentile(medianRates, 0.5)
		if medianRate <= 0 {
			medianRate = account.RateMultiplier
		}
		anchor += median * account.RateMultiplier / medianRate * float64(pool.TotalTokens)
		weight += float64(pool.TotalTokens)
	}
	if weight == 0 {
		return 0
	}
	return anchor / weight
}

func mapValues(values map[int64]float64) []float64 {
	result := make([]float64, 0, len(values))
	for _, value := range values {
		if value > 0 && validScore(value) {
			result = append(result, value)
		}
	}
	return result
}

func otherPoolCosts(values map[int64]float64, excluded int64) []float64 {
	result := make([]float64, 0, len(values))
	for accountID, value := range values {
		if accountID != excluded && value > 0 && validScore(value) {
			result = append(result, value)
		}
	}
	return result
}

func finalFailureRate(account AccountMetrics) float64 {
	denominator := account.SuccessfulRequests + maxInt64(account.TerminalFailures, 0)
	if denominator <= 0 {
		return 0
	}
	return clamp01(float64(maxInt64(account.TerminalFailures, 0)) / float64(denominator))
}

func recovered429Rate(account AccountMetrics) float64 {
	denominator := account.SuccessfulRequests + maxInt64(account.TerminalFailures, 0)
	if denominator <= 0 {
		return 0
	}
	return clamp01(maxFloat(account.RecoveredRateLimitWeight, 0) / float64(denominator))
}

func finalSuccessRate(account AccountMetrics) float64 {
	denominator := account.SuccessfulRequests + maxInt64(account.TerminalFailures, 0)
	if denominator <= 0 {
		return 1
	}
	return clamp01(float64(maxInt64(account.SuccessfulRequests, 0)) / float64(denominator))
}

func maxPriority(left, right int) int {
	if left > right {
		return left
	}
	return right
}

// speedMetric combines end-to-end and first-token latency. Missing components
// are omitted and the remaining weight is renormalized.
func speedMetric(durationP90Ms, firstTokenP90Ms float64) float64 {
	durationValid := durationP90Ms > 0 && validScore(durationP90Ms)
	firstTokenValid := firstTokenP90Ms > 0 && validScore(firstTokenP90Ms)
	switch {
	case durationValid && firstTokenValid:
		return durationWeight*durationP90Ms + firstTokenWeight*firstTokenP90Ms
	case durationValid:
		return durationP90Ms
	case firstTokenValid:
		return firstTokenP90Ms
	default:
		return 0
	}
}

func cacheHitRate(inputTokens, cacheReadTokens int64) float64 {
	inputTokens = maxInt64(inputTokens, 0)
	cacheReadTokens = maxInt64(cacheReadTokens, 0)
	cacheableInput := inputTokens + cacheReadTokens
	if cacheableInput <= 0 {
		return 0
	}
	return clamp01(float64(cacheReadTokens) / float64(cacheableInput))
}

func percentile(values []float64, quantile float64) float64 {
	if len(values) == 0 {
		return 0
	}
	copyValues := append([]float64(nil), values...)
	sort.Float64s(copyValues)
	quantile = clamp(quantile, 0, 1)
	position := quantile * float64(len(copyValues)-1)
	low := int(math.Floor(position))
	high := int(math.Ceil(position))
	if low == high {
		return copyValues[low]
	}
	return copyValues[low] + (copyValues[high]-copyValues[low])*(position-float64(low))
}

func enforceCostDominance(recommendations []Recommendation, costs []float64, valid []bool) {
	indices := make([]int, 0, len(recommendations))
	for index := range recommendations {
		if index < len(costs) && index < len(valid) && valid[index] && costs[index] > 0 {
			indices = append(indices, index)
		}
	}
	sort.Slice(indices, func(i, j int) bool { return costs[indices[i]] < costs[indices[j]] })
	for i := 1; i < len(indices); i++ {
		current := indices[i]
		for j := 0; j < i; j++ {
			previous := indices[j]
			if costs[current] <= costs[previous]*(1+minimumCostAdvantage) {
				continue
			}
			if recommendations[current].Score >= recommendations[previous].Score {
				recommendations[current].Score = math.Max(0, recommendations[previous].Score-0.01)
			}
		}
	}
	for index := range recommendations {
		if recommendations[index].Score < 0 || !validScore(recommendations[index].Score) {
			recommendations[index].Score = 0
		}
	}
}

func sortRecommendations(recommendations []Recommendation) {
	sort.SliceStable(recommendations, func(i, j int) bool {
		if recommendations[i].RecommendedPriority != recommendations[j].RecommendedPriority {
			return recommendations[i].RecommendedPriority < recommendations[j].RecommendedPriority
		}
		iHasCost := recommendations[i].CostPerMillionTokens > 0 && validScore(recommendations[i].CostPerMillionTokens)
		jHasCost := recommendations[j].CostPerMillionTokens > 0 && validScore(recommendations[j].CostPerMillionTokens)
		if iHasCost != jHasCost {
			return iHasCost
		}
		if iHasCost && recommendations[i].CostPerMillionTokens != recommendations[j].CostPerMillionTokens {
			return recommendations[i].CostPerMillionTokens < recommendations[j].CostPerMillionTokens
		}
		iSpeed := speedMetric(recommendations[i].LatencyP90Ms, recommendations[i].FirstTokenP90Ms)
		jSpeed := speedMetric(recommendations[j].LatencyP90Ms, recommendations[j].FirstTokenP90Ms)
		iHasSpeed := iSpeed > 0 && validScore(iSpeed)
		jHasSpeed := jSpeed > 0 && validScore(jSpeed)
		if iHasSpeed != jHasSpeed {
			return iHasSpeed
		}
		if iHasSpeed && iSpeed != jSpeed {
			return iSpeed < jSpeed
		}
		return recommendations[i].ID < recommendations[j].ID
	})
}

func effectiveAvailability(account AccountMetrics) float64 {
	if account.SuccessfulRequests <= 0 {
		return 0
	}
	denominator := float64(account.SuccessfulRequests) + float64(maxInt64(account.TerminalFailures, 0)) +
		maxFloat(account.RecoveredRateLimitWeight, 0)
	if denominator <= 0 {
		return 1
	}
	return clamp01(float64(account.SuccessfulRequests) / denominator)
}

func accountHardExcluded(account AccountMetrics, now time.Time) (bool, string) {
	if !strings.EqualFold(strings.TrimSpace(account.Status), "active") {
		return true, "账户状态不是 active，暂时降到不可用档位"
	}
	if future(account.RateLimitResetAt, now) {
		return true, "账户仍在 rate limit 冷却窗口"
	}
	if future(account.TempUnschedulableTill, now) {
		return true, "账户仍在临时不可调度窗口"
	}
	if future(account.OverloadUntil, now) {
		return true, "账户仍在过载冷却窗口"
	}
	return false, ""
}

func future(value *time.Time, now time.Time) bool {
	return value != nil && value.After(now)
}

func priorityForScore(score float64) int {
	return priorityForScoreRange(score, priorityBest, priorityPoor)
}

func coldAnchorPriority(multiplierScore float64) int {
	return priorityForScoreRange(multiplierScore, coldAnchorFloor, priorityPoor)
}

// priorityForScoreRange maps a higher score to a lower priority number while
// keeping the neutral score at the neutral priority. This preserves the
// default position when a metric has no usable peer evidence.
func priorityForScoreRange(score float64, best, worst int) int {
	if best <= 0 || best > priorityNeutral || worst < priorityNeutral {
		return priorityNeutral
	}
	score = clamp(score, 0, 100)
	var priority int
	if score >= 50 {
		priority = best + int(math.Round((100-score)*float64(priorityNeutral-best)/50))
	} else {
		priority = priorityNeutral + int(math.Round((50-score)*float64(worst-priorityNeutral)/50))
	}
	// Keep the bounded exploration slot distinct from formal score output.
	if priority == priorityExplore {
		priority++
	}
	return priority
}

// normalizedPriority preserves positive database values. A missing or invalid
// value falls back to the neutral band.
func normalizedPriority(value int) int {
	if value <= 0 {
		return priorityNeutral
	}
	if value > 10000 {
		return priorityUnavailable
	}
	return value
}

// normalizeLowerBetter 对越小越好的指标做稳健的 0..100 反向归一化。
// 没有足够有效值或所有值相同时返回中性 50，避免无依据地偏向某个账号。
func normalizeLowerBetter(values []float64, valid []bool) []float64 {
	validValues := make([]float64, 0, len(values))
	for index, value := range values {
		if index < len(valid) && valid[index] && validScore(value) {
			validValues = append(validValues, value)
		}
	}
	result := make([]float64, len(values))
	for index := range result {
		result[index] = 50
	}
	if len(validValues) < 2 {
		return result
	}
	lower, upper := percentile(validValues, 0.10), percentile(validValues, 0.90)
	if upper-lower < 1e-9 {
		return result
	}
	for index, value := range values {
		if index >= len(valid) || !valid[index] || !validScore(value) {
			continue
		}
		result[index] = 100 * (upper - clamp(value, lower, upper)) / (upper - lower)
	}
	return result
}

func positiveOrZero(value float64) float64 {
	if value > 0 && validScore(value) {
		return value
	}
	return 0
}

func clamp(value, minimum, maximum float64) float64 {
	if value < minimum {
		return minimum
	}
	if value > maximum {
		return maximum
	}
	return value
}

func clamp01(value float64) float64 {
	return clamp(value, 0, 1)
}

func maxInt64(left, right int64) int64 {
	if left > right {
		return left
	}
	return right
}

func maxFloat(left, right float64) float64 {
	if left > right {
		return left
	}
	return right
}
