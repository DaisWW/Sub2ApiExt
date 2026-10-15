package main

import (
	"fmt"
	"time"
)

type reliabilityAssessment struct {
	snapshot    *MetricSnapshot
	window      string
	samples     int64
	failureRate float64
	lastFailure *time.Time
	lastSuccess *time.Time
	highSince   *time.Time
	action      string
}

func (r *Runner) assessReliability(account AccountMetrics, now time.Time) reliabilityAssessment {
	window, snapshot := "2h", account.Window2h
	if fast := account.Window30m; fast != nil && fast.SuccessfulRequests+fast.TerminalFailures >= recoveryMinimumSamples {
		window, snapshot = "30m", fast
	} else if snapshot == nil {
		window, snapshot = "30m", account.Window30m
	}
	if snapshot == nil {
		snapshot = &MetricSnapshot{}
		if !usesDecisionWindows(account) {
			window, snapshot = "legacy", accountMetricSnapshot(account)
		}
	}
	lease := r.state.Exploration
	activeRecovery := lease != nil && lease.AccountID == account.ID && lease.RecoveryProgressive
	if activeRecovery && account.RecoveryWindow != nil {
		window, snapshot = "recovery", account.RecoveryWindow
	}
	samples := maxInt64(snapshot.SuccessfulRequests, 0) + maxInt64(snapshot.TerminalFailures, 0)
	evidence := reliabilityAssessment{snapshot: snapshot, window: window, samples: samples, action: "cost"}
	if samples > 0 {
		evidence.failureRate = float64(maxInt64(snapshot.TerminalFailures, 0)) / float64(samples)
	}
	evidence.lastFailure = latestStateTime(snapshot.LastTerminalFailureAt, snapshotLastFailure(account.Window30m), snapshotLastFailure(account.Window2h))
	evidence.lastSuccess = latestStateTime(snapshot.LastPricedSuccessAt, snapshotLastSuccess(account.Window30m), snapshotLastSuccess(account.Window2h))
	state := r.state.Accounts[account.ID]
	pauseBaseline := state.LastReliabilityFailureAt
	if lease := r.state.Exploration; lease != nil && lease.AccountID == account.ID {
		pauseBaseline = latestStateTime(pauseBaseline, lease.RecoveryLastFailureAt, lease.StartedAt)
	}
	fresh := evidence.lastFailure != nil && (pauseBaseline == nil || evidence.lastFailure.After(*pauseBaseline))
	rollbackBaseline := reliabilityRollbackBaseline(state)
	if lease := r.state.Exploration; lease != nil && lease.AccountID == account.ID {
		rollbackBaseline = latestStateTime(rollbackBaseline, lease.StartedAt)
		if !lease.RecoveryProgressive {
			rollbackBaseline = latestStateTime(rollbackBaseline, lease.RecoveryLastFailureAt)
		}
	}
	unhandled := evidence.lastFailure != nil && (rollbackBaseline == nil || evidence.lastFailure.After(*rollbackBaseline))
	trailing := maxInt64(snapshot.TrailingTerminalFailures, 0)
	if !activeRecovery && account.Window2h != nil {
		trailing = maxInt64(trailing, account.Window2h.TrailingTerminalFailures)
	}
	thresholdFailure := latestStateTime(snapshot.FifthTerminalFailureAt, account.FifthTerminalFailureAt)
	if !activeRecovery {
		thresholdFailure = latestStateTime(thresholdFailure, fifthSnapshotFailure(account.Window30m), fifthSnapshotFailure(account.Window2h))
	}
	newFailures := thresholdFailure != nil && (rollbackBaseline == nil || thresholdFailure.After(*rollbackBaseline))
	if samples >= recoveryMinimumSamples && evidence.failureRate > recoveryUnreliableFailureRate {
		evidence.highSince = state.ReliabilityHighSince
		if evidence.highSince == nil {
			evidence.highSince = timePtr(now)
		}
		evidence.action = "observe"
		interval := r.config.Interval
		if interval <= 0 {
			interval = defaultInterval
		}
		if unhandled && (state.LastReliabilityRollbackAt == nil || newFailures) &&
			now.Sub(*evidence.highSince) >= time.Duration(recoveryUnreliableObservationCycles)*interval {
			evidence.action = "rollback"
		}
	} else if samples >= recoveryMinimumSamples && evidence.failureRate >= recoveryHealthyFailureRate {
		evidence.action = "slow"
	} else if fresh && samples < recoveryMinimumSamples {
		evidence.action = "pause"
	}
	if trailing >= recoveryUnreliableConsecutiveFailures && unhandled && (newFailures || rollbackBaseline == nil) {
		evidence.action = "rollback"
	}
	return evidence
}

func fifthSnapshotFailure(snapshot *MetricSnapshot) *time.Time {
	if snapshot == nil {
		return nil
	}
	return snapshot.FifthTerminalFailureAt
}

func reliabilityRollbackBaseline(state accountState) *time.Time {
	if state.LastReliabilityRollbackAt == nil && (state.LastReliabilityFailureAt == nil || state.RecoveryFailures > 0) {
		// Migrate old reliability/recovery writes into a durable watermark once.
		return state.LastAppliedAt
	}
	return state.LastReliabilityRollbackAt
}

func recoveryFailureBaseline(state accountState) *time.Time {
	if state.LastReliabilityRollbackAt != nil {
		return state.LastReliabilityRollbackAt
	}
	// Older recovery outcomes only recorded the successful priority write.
	return state.LastAppliedAt
}

func (r *Runner) prepareReliability(accounts []AccountMetrics, recommendations []Recommendation, now time.Time) {
	byID := make(map[int64]AccountMetrics, len(accounts))
	for _, account := range accounts {
		byID[account.ID] = account
	}
	for index := range recommendations {
		recommendation := &recommendations[index]
		evidence := r.assessReliability(byID[recommendation.ID], now)
		recommendation.ReliabilityWindow, recommendation.ReliabilitySamples = evidence.window, evidence.samples
		recommendation.AccountFailureRate, recommendation.ReliabilityAction = evidence.failureRate, evidence.action
		recommendation.DeliveryWindow = evidence.window
		recommendation.SameAccountRecoveredRequests = evidence.snapshot.SameAccountRecoveredRequests
		recommendation.FallbackRecoveredRequests = evidence.snapshot.FallbackRecoveredRequests
		recommendation.UnresolvedRequests = evidence.snapshot.UnresolvedRequests
		recommendation.RecordedFallbackCost = positiveMetric(evidence.snapshot.FallbackCost)
		recommendation.FallbackLatencyP90Ms = positiveMetric(evidence.snapshot.FallbackLatencyP90Ms)
		cost := positiveMetric(evidence.snapshot.AccountCost)
		if cost == 0 {
			cost = positiveMetric(evidence.snapshot.ActualCost)
		}
		if tokens := evidence.snapshot.PricedTokens + evidence.snapshot.FallbackPricedTokens; tokens > 0 {
			recommendation.RecordedDeliveryCostPerMillion = (cost + recommendation.RecordedFallbackCost) * 1_000_000 / float64(tokens)
		}
		recommendation.reliabilityLastFailureAt = evidence.lastFailure
		recommendation.reliabilityLastSuccessAt = evidence.lastSuccess
		recommendation.reliabilityHighSince = evidence.highSince
		if evidence.action != "cost" || recommendation.HardExcluded {
			recommendation.recoveryCompleted = false
		}
		if recommendation.HardExcluded {
			continue
		}
		state := r.state.Accounts[recommendation.ID]
		switch evidence.action {
		case "rollback":
			target := max(recommendation.CurrentPriority, min(priorityPoor, recommendation.CurrentPriority+priorityRampStep))
			if state.RecoveryOriginalPriority > recommendation.CurrentPriority {
				target = state.RecoveryOriginalPriority
			}
			active := r.state.Exploration != nil && r.state.Exploration.AccountID == recommendation.ID
			if active {
				if r.state.Exploration.RecoveryProgressive {
					target = max(recommendation.CurrentPriority, r.state.Exploration.OriginalPriority)
				} else {
					target = max(target, r.state.Exploration.OriginalPriority)
				}
			}
			recommendation.RecommendedPriority = target
			recommendation.Recovery = true
			recommendation.reliabilityRollback = true
			recommendation.applyImmediately = true
			recommendation.explorationStart = false
			recommendation.explorationEnd = active
			recommendation.recoveryOutcome = recoveryOutcomeUnreliable
			recommendation.Reason = fmt.Sprintf("连续至少 %d 次失败或 %s 足够样本下持续失败率 %.1f%%（至少观察 %d 个周期），退回一步到 %d 并观察", recoveryUnreliableConsecutiveFailures, evidence.window, evidence.failureRate*100, recoveryUnreliableObservationCycles, target)
		case "pause", "observe":
			recommendation.PromotionFrozen = true
			recommendation.recoveryNeeded = false
			recommendation.explorationStart = false
			if recommendation.RecommendedPriority < recommendation.CurrentPriority {
				recommendation.RecommendedPriority = recommendation.CurrentPriority
			}
			if recommendation.explorationEnd && recommendation.recoveryOutcome != recoveryOutcomeFailure {
				recommendation.recoveryOutcome = recoveryOutcomeNoResult
			}
			recommendation.Reason += "；新软错误或高失败率待确认，暂停提升并保留进度"
		case "slow":
			if recommendation.RecommendedPriority < recommendation.CurrentPriority {
				step := reliabilitySlowRampStep
				if recommendation.Recovery {
					step = recoverySlowRampStep
				}
				if !recommendation.Recovery && recommendation.CurrentPriority > priorityPoor {
					step = priorityRampStep
				}
				recommendation.RecommendedPriority = max(recommendation.RecommendedPriority, recommendation.CurrentPriority-step)
			}
			recommendation.Reason += "；失败率 3%～8%，减慢提升并观察兜底成本和延迟"
		}
		if state.RecoveryRetryAt != nil && now.Before(*state.RecoveryRetryAt) && recommendation.RecommendedPriority < recommendation.CurrentPriority {
			recommendation.RecommendedPriority = recommendation.CurrentPriority
			recommendation.explorationStart = false
			recommendation.recoveryNeeded = false
			recommendation.PromotionFrozen = true
			recommendation.Reason += "；退避观察期间暂停提升"
		}
		recoveryFailureAt := recoveryFailureBaseline(state)
		if recommendation.recoveryOutcome == recoveryOutcomeSuccess && state.RecoveryFailures > 0 &&
			(evidence.samples < recoveryMinimumSamples || evidence.failureRate >= recoveryHealthyFailureRate ||
				(recoveryFailureAt != nil && now.Sub(*recoveryFailureAt) < decisionWindow)) {
			recommendation.recoveryOutcome = recoveryOutcomeProgress
		}
	}
	sortRecommendations(recommendations)
}

// Commit evidence only after an unchanged decision or successful Admin write.
// A rejected rollback must remain retryable with the same input evidence.
func recordReliabilityObservation(state *accountState, recommendation *Recommendation, now time.Time) {
	state.LastReliabilityRollbackAt = cloneTimePtr(reliabilityRollbackBaseline(*state))
	state.LastReliabilityFailureAt = cloneTimePtr(latestStateTime(state.LastReliabilityFailureAt, recommendation.reliabilityLastFailureAt))
	state.ReliabilityHighSince = cloneTimePtr(recommendation.reliabilityHighSince)
	recoveryFailureAt := recoveryFailureBaseline(*state)
	if recommendation.reliabilityRollback {
		rollbackAt := recommendation.reliabilityLastFailureAt
		if rollbackAt == nil {
			rollbackAt = timePtr(now)
		}
		state.LastReliabilityRollbackAt = cloneTimePtr(rollbackAt)
		// A rollback starts a new high-failure observation period. The next
		// cycle must observe the configured interval again before repeating it.
		state.ReliabilityHighSince = timePtr(now)
	} else if (state.RecoveryFailures > 0 || state.RecoveryRetryAt != nil) && recoveryFailureAt != nil &&
		now.Sub(*recoveryFailureAt) >= decisionWindow && recommendation.ReliabilitySamples >= recoveryMinimumSamples &&
		recommendation.AccountFailureRate < recoveryHealthyFailureRate && recommendation.reliabilityLastSuccessAt != nil &&
		recommendation.reliabilityLastSuccessAt.After(*recoveryFailureAt) {
		state.RecoveryFailures = 0
		state.LastRecoveryFailureAt = nil
		state.RecoveryRetryAt = nil
	}
	if recommendation.recoveryCompleted {
		state.RecoveryOriginalPriority = 0
		state.RecoveryFailures = 0
		state.LastRecoveryFailureAt = nil
		state.RecoveryRetryAt = nil
	}
}
