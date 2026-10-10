package main

import (
	"fmt"
	"time"
)

type reliabilityAssessment struct {
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
	if fast := account.Window30m; fast != nil && fast.SuccessfulRequests+fast.TerminalFailures >= 50 {
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
	samples := maxInt64(snapshot.SuccessfulRequests, 0) + maxInt64(snapshot.TerminalFailures, 0)
	evidence := reliabilityAssessment{window: window, samples: samples, action: "cost"}
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
	// Existing writes also provide a migration watermark for older state files.
	rollbackBaseline := latestStateTime(state.LastReliabilityRollbackAt, state.LastAppliedAt)
	if lease := r.state.Exploration; lease != nil && lease.AccountID == account.ID {
		rollbackBaseline = latestStateTime(rollbackBaseline, lease.RecoveryLastFailureAt, lease.StartedAt)
	}
	unhandled := evidence.lastFailure != nil && (rollbackBaseline == nil || evidence.lastFailure.After(*rollbackBaseline))
	if samples >= 50 && evidence.failureRate > 0.05 {
		evidence.highSince = state.ReliabilityHighSince
		if evidence.highSince == nil {
			evidence.highSince = timePtr(now)
		}
		evidence.action = "observe"
		interval := r.config.Interval
		if interval <= 0 {
			interval = defaultInterval
		}
		if unhandled && now.Sub(*evidence.highSince) >= interval {
			evidence.action = "rollback"
		}
	} else if samples >= 50 && evidence.failureRate >= 0.02 {
		evidence.action = "slow"
	} else if fresh {
		evidence.action = "pause"
	}
	trailing := maxInt64(snapshot.TrailingTerminalFailures, 0)
	if account.Window2h != nil {
		trailing = maxInt64(trailing, account.Window2h.TrailingTerminalFailures)
	}
	if trailing >= 3 && unhandled {
		evidence.action = "rollback"
	}
	return evidence
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
		recommendation.reliabilityLastFailureAt = evidence.lastFailure
		recommendation.reliabilityLastSuccessAt = evidence.lastSuccess
		recommendation.reliabilityHighSince = evidence.highSince
		if recommendation.HardExcluded {
			continue
		}
		state := r.state.Accounts[recommendation.ID]
		switch evidence.action {
		case "rollback":
			target := max(recommendation.CurrentPriority, min(priorityPoor, recommendation.CurrentPriority+priorityRampStep))
			if state.RecoveryOriginalPriority > recommendation.CurrentPriority && !hasReliableCost(recommendation) {
				target = state.RecoveryOriginalPriority
			}
			active := r.state.Exploration != nil && r.state.Exploration.AccountID == recommendation.ID
			if active {
				target = max(target, r.state.Exploration.OriginalPriority)
			}
			recommendation.RecommendedPriority = target
			recommendation.Recovery = true
			recommendation.reliabilityRollback = true
			recommendation.applyImmediately = true
			recommendation.explorationStart = false
			recommendation.explorationEnd = active
			recommendation.recoveryOutcome = recoveryOutcomeUnreliable
			recommendation.Reason = fmt.Sprintf("连续至少 3 次失败或 %s 足够样本下持续失败率 %.1f%%，退回一步到 %d 并观察", evidence.window, evidence.failureRate*100, target)
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
				step := 5
				if recommendation.CurrentPriority > priorityPoor {
					step = priorityRampStep
				}
				recommendation.RecommendedPriority = max(recommendation.RecommendedPriority, recommendation.CurrentPriority-step)
			}
			recommendation.Reason += "；失败率 2%～5%，减慢提升并观察兜底成本和延迟"
		}
		if state.RecoveryRetryAt != nil && now.Before(*state.RecoveryRetryAt) && recommendation.RecommendedPriority < recommendation.CurrentPriority {
			recommendation.RecommendedPriority = recommendation.CurrentPriority
			recommendation.explorationStart = false
			recommendation.recoveryNeeded = false
			recommendation.PromotionFrozen = true
			recommendation.Reason += "；退避观察期间暂停提升"
		}
		if recommendation.recoveryOutcome == recoveryOutcomeSuccess && state.RecoveryFailures > 0 &&
			(evidence.samples < 50 || evidence.failureRate >= 0.02 ||
				(state.LastReliabilityRollbackAt != nil && now.Sub(*state.LastReliabilityRollbackAt) < decisionWindow)) {
			recommendation.recoveryOutcome = recoveryOutcomeProgress
		}
	}
	sortRecommendations(recommendations)
}

// Commit evidence only after an unchanged decision or successful Admin write.
// A rejected rollback must remain retryable with the same input evidence.
func recordReliabilityObservation(state *accountState, recommendation *Recommendation, now time.Time) {
	state.LastReliabilityFailureAt = cloneTimePtr(latestStateTime(state.LastReliabilityFailureAt, recommendation.reliabilityLastFailureAt))
	state.ReliabilityHighSince = cloneTimePtr(recommendation.reliabilityHighSince)
	if recommendation.reliabilityRollback {
		state.LastReliabilityRollbackAt = cloneTimePtr(recommendation.reliabilityLastFailureAt)
	} else if state.RecoveryFailures > 0 && state.LastReliabilityRollbackAt != nil &&
		now.Sub(*state.LastReliabilityRollbackAt) >= decisionWindow && recommendation.ReliabilitySamples >= 50 &&
		recommendation.AccountFailureRate < 0.02 && recommendation.reliabilityLastSuccessAt != nil &&
		recommendation.reliabilityLastSuccessAt.After(*state.LastReliabilityRollbackAt) {
		state.RecoveryFailures = 0
	}
}
