package main

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

// scoreAccounts 将三个维度统一到 0..100，分数越高越值得优先路由。
// 成本占 60%，速度占 25%，可用性占 15%。
func scoreAccounts(accounts []AccountMetrics, now time.Time, minSamples int) []Recommendation {
	if minSamples < 1 {
		minSamples = defaultMinSamples
	}
	costValues := make([]float64, len(accounts))
	speedValues := make([]float64, len(accounts))
	costValid := make([]bool, len(accounts))
	speedValid := make([]bool, len(accounts))
	peerCostValid := make([]bool, len(accounts))
	peerSpeedValid := make([]bool, len(accounts))
	for index, account := range accounts {
		evidence := account.SuccessfulRequests + account.TerminalFailures
		hardExcluded, _ := accountHardExcluded(account, now)
		peerEligible := evidence >= int64(minSamples) && !hardExcluded
		observedCost := account.AccountCost
		if observedCost <= 0 || !validScore(observedCost) {
			observedCost = account.ActualCost
		}
		if account.TotalTokens > 0 && observedCost > 0 && validScore(observedCost) {
			costValues[index] = observedCost * 1_000_000 / float64(account.TotalTokens)
			costValid[index] = validScore(costValues[index]) && costValues[index] > 0
			peerCostValid[index] = costValid[index] && peerEligible
		}
		speedValues[index] = account.LatencyP90Ms
		if speedValues[index] <= 0 || !validScore(speedValues[index]) {
			speedValues[index] = account.FirstTokenP90Ms
		}
		speedValid[index] = speedValues[index] > 0 && validScore(speedValues[index])
		peerSpeedValid[index] = speedValid[index] && peerEligible
	}
	costScores := normalizeLowerBetter(costValues, peerCostValid)
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
		rawScore := costWeight*costScores[index] +
			speedWeight*speedScores[index] +
			availabilityWeight*availabilityScore
		score := rawScore*confidence + 50*(1-confidence)

		hardExcluded, hardReason := accountHardExcluded(account, now)
		recommended := account.CurrentPriority
		reason := "证据不足，保持当前优先级"
		if recommended < 0 {
			recommended = priorityNeutral
		}
		if hardExcluded {
			recommended = priorityUnavailable
			reason = hardReason
		} else if evidence >= int64(minSamples) {
			recommended = priorityForScore(score)
			reason = fmt.Sprintf("成本 %.1f%%、速度 %.1f%%、可用性 %.1f%%", costScores[index], speedScores[index], availabilityScore)
		}
		if account.RecoveredRateLimited > 0 && !hardExcluded {
			reason += fmt.Sprintf("；恢复性 429 %d 次按软惩罚计入", account.RecoveredRateLimited)
		}

		recommendation := Recommendation{
			ID:                       account.ID,
			Name:                     account.Name,
			Platform:                 account.Platform,
			Status:                   account.Status,
			CurrentPriority:          account.CurrentPriority,
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
			SpeedScore:               speedScores[index],
			AvailabilityScore:        availabilityScore,
			Score:                    score,
			HardExcluded:             hardExcluded,
			Reason:                   reason,
		}
		if costValid[index] {
			recommendation.CostPerMillionTokens = costValues[index]
		}
		result = append(result, recommendation)
	}
	sort.SliceStable(result, func(i, j int) bool {
		if result[i].RecommendedPriority != result[j].RecommendedPriority {
			return result[i].RecommendedPriority < result[j].RecommendedPriority
		}
		if result[i].Score != result[j].Score {
			return result[i].Score > result[j].Score
		}
		if strings.TrimSpace(result[i].Name) != strings.TrimSpace(result[j].Name) {
			return strings.TrimSpace(result[i].Name) < strings.TrimSpace(result[j].Name)
		}
		return result[i].ID < result[j].ID
	})
	return result
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
	switch {
	case score >= 80:
		return priorityBest
	case score >= 65:
		return priorityGood
	case score >= 50:
		return priorityNeutral
	case score >= 35:
		return priorityDegraded
	default:
		return priorityPoor
	}
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
	minValue, maxValue := validValues[0], validValues[0]
	for _, value := range validValues[1:] {
		if value < minValue {
			minValue = value
		}
		if value > maxValue {
			maxValue = value
		}
	}
	if maxValue-minValue < 1e-9 {
		return result
	}
	for index, value := range values {
		if index >= len(valid) || !valid[index] || !validScore(value) {
			continue
		}
		result[index] = clamp(100*(maxValue-value)/(maxValue-minValue), 0, 100)
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
