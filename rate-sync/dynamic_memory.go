package main

import "math"

func clearDynamicPending(state *DynamicGroupState) {
	if state == nil {
		return
	}
	state.PendingTarget = 0
	state.HasPendingTarget = false
}

func newDynamicGroupState() *DynamicGroupState {
	return &DynamicGroupState{
		Fast:             DynamicCostMemory{AccountBase: make(map[int64]float64)},
		Slow:             DynamicCostMemory{AccountBase: make(map[int64]float64)},
		LastAccountRates: make(map[int64]float64),
	}
}

func seedDynamicGroupState(rows []GroupUsageAccountStats, lastUsageID int64) *DynamicGroupState {
	if !dynamicBootstrapSufficient(rows) {
		return nil
	}
	state := newDynamicGroupState()
	state.Initialized = true
	state.LastUsageID = lastUsageID
	summary := summarizeDynamicUsage(rows)
	for _, row := range rows {
		if !finiteNonNegative(row.BaseCost) || !validPositiveRate(row.CurrentAccountRate) {
			return nil
		}
		state.LastAccountRates[row.AccountID] = row.CurrentAccountRate
		state.Fast.AccountBase[row.AccountID] += row.BaseCost * dynamicFastBudgetUSD / summary.standardCost
		state.Slow.AccountBase[row.AccountID] += row.BaseCost * dynamicSlowBudgetUSD / summary.standardCost
	}
	state.Fast.Denominator = dynamicFastBudgetUSD
	state.Slow.Denominator = dynamicSlowBudgetUSD
	if _, ok := dynamicMemoryRate(state.Fast, state.LastAccountRates); !ok {
		return nil
	}
	if _, ok := dynamicMemoryRate(state.Slow, state.LastAccountRates); !ok {
		return nil
	}
	return state
}

func updateDynamicMemory(memory *DynamicCostMemory, budget float64, rows []GroupUsageAccountStats) {
	if memory.AccountBase == nil {
		memory.AccountBase = make(map[int64]float64)
	}
	summary := summarizeDynamicUsage(rows)
	if summary.standardCost <= 0 || !finiteNonNegative(summary.standardCost) {
		return
	}
	decay := math.Exp(-summary.standardCost / budget)
	memory.Denominator = decay*memory.Denominator + summary.standardCost
	for accountID, value := range memory.AccountBase {
		value *= decay
		if value < 1e-12 {
			delete(memory.AccountBase, accountID)
		} else {
			memory.AccountBase[accountID] = value
		}
	}
	for _, row := range rows {
		if finiteNonNegative(row.BaseCost) {
			memory.AccountBase[row.AccountID] += row.BaseCost
		}
	}
}

func dynamicMemoryRate(memory DynamicCostMemory, rates map[int64]float64) (float64, bool) {
	if memory.Denominator <= 0 || !finiteNonNegative(memory.Denominator) {
		return 0, false
	}
	numerator := 0.0
	for accountID, base := range memory.AccountBase {
		rate, ok := rates[accountID]
		if !ok || !finiteNonNegative(base) || !validPositiveRate(rate) {
			return 0, false
		}
		numerator += base * rate
	}
	value := numerator / memory.Denominator
	return value, validPositiveRate(value)
}

func dynamicRawTarget(fast, slow float64) float64 {
	difference := fast - slow
	weight := 0.0
	if difference >= 0 {
		weight = clampFloat((difference-0.002)/0.010, 0, 1)
	} else {
		weight = 0.7 * clampFloat((-difference-0.006)/0.020, 0, 1)
	}
	return slow + weight*difference
}

func dynamicPublishedTarget(current, rawTarget float64) float64 {
	change := clampFloat(rawTarget-current, -dynamicFallStepLimit, dynamicRiseStepLimit)
	return round4(current + change)
}

func dynamicGroupRateChangeSignificant(current, target float64) bool {
	if !validPositiveRate(target) || almostEqual(current, target) {
		return false
	}
	threshold := math.Max(dynamicAbsoluteDeadband, math.Abs(current)*dynamicRelativeDeadband)
	return math.Abs(target-current)+1e-12 >= threshold
}

func usableDynamicGroupState(state *DynamicGroupState) bool {
	if state == nil || !state.Initialized || state.LastUsageID < 0 || state.LastAccountRates == nil {
		return false
	}
	_, fastOK := dynamicMemoryRate(state.Fast, state.LastAccountRates)
	_, slowOK := dynamicMemoryRate(state.Slow, state.LastAccountRates)
	return fastOK && slowOK
}

func relevantAccountRatesChanged(state *DynamicGroupState, rates map[int64]float64) bool {
	for accountID := range state.Fast.AccountBase {
		if accountRateChanged(state.LastAccountRates, rates, accountID) {
			return true
		}
	}
	for accountID := range state.Slow.AccountBase {
		if accountRateChanged(state.LastAccountRates, rates, accountID) {
			return true
		}
	}
	return false
}

func accountRateChanged(previous, current map[int64]float64, accountID int64) bool {
	oldRate, oldOK := previous[accountID]
	newRate, newOK := current[accountID]
	return !oldOK || !newOK || !almostEqual(oldRate, newRate)
}

func overlayBindingRates(rates map[int64]float64, binding *groupBinding) {
	if binding == nil {
		return
	}
	for accountID, channel := range binding.accounts {
		if validPositiveRate(channel.AccountRateMultiplier) {
			rates[accountID] = channel.AccountRateMultiplier
		}
	}
}

func cloneAccountRates(source map[int64]float64) map[int64]float64 {
	result := make(map[int64]float64, len(source))
	for accountID, rate := range source {
		result[accountID] = rate
	}
	return result
}

func groupDynamicUsage(rows []GroupUsageAccountStats) map[int64][]GroupUsageAccountStats {
	result := make(map[int64][]GroupUsageAccountStats)
	for _, row := range rows {
		result[row.GroupID] = append(result[row.GroupID], row)
	}
	return result
}

func summarizeDynamicUsage(rows []GroupUsageAccountStats) dynamicUsageSummary {
	var result dynamicUsageSummary
	for _, row := range rows {
		result.requests += row.Requests
		result.standardCost += row.StandardCost
	}
	return result
}

func dynamicBootstrapSufficient(rows []GroupUsageAccountStats) bool {
	if len(rows) == 0 {
		return false
	}
	summary := summarizeDynamicUsage(rows)
	if summary.requests < adaptiveMinRequests ||
		summary.standardCost < adaptiveMinStandardCostUSD ||
		!finiteNonNegative(summary.standardCost) {
		return false
	}
	for _, row := range rows {
		if !finiteNonNegative(row.StandardCost) || !finiteNonNegative(row.BaseCost) || !validPositiveRate(row.CurrentAccountRate) {
			return false
		}
	}
	return true
}

func validPositiveRate(value float64) bool {
	return value > 0 && !math.IsNaN(value) && !math.IsInf(value, 0)
}

func clampFloat(value, minimum, maximum float64) float64 {
	return math.Min(maximum, math.Max(minimum, value))
}
