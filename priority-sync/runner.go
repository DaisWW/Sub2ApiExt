package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"
)

type metricsSource interface {
	LoadAccountMetricsWindows(context.Context, time.Time) ([]AccountMetrics, error)
	AdminAPIKey(context.Context) (string, error)
}

type Runner struct {
	config      Config
	source      metricsSource
	admin       *adminAPI
	state       *syncState
	logger      *slog.Logger
	tableWriter io.Writer
}

func NewRunner(config Config, source metricsSource, client *http.Client, state *syncState, logger *slog.Logger) *Runner {
	if state == nil {
		state = &syncState{Accounts: make(map[int64]accountState)}
	}
	if state.Accounts == nil {
		state.Accounts = make(map[int64]accountState)
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Runner{
		config:      config,
		source:      source,
		admin:       newAdminAPI(config.Sub2APIURL, client),
		state:       state,
		logger:      logger,
		tableWriter: os.Stdout,
	}
}

func (r *Runner) RunOnce(ctx context.Context, now time.Time) error {
	if r == nil || r.source == nil {
		return fmt.Errorf("priority runner is not configured")
	}
	now = now.UTC()
	window := defaultWindow
	accounts, err := r.source.LoadAccountMetricsWindows(ctx, now)
	if err != nil {
		return err
	}
	// A dry-run is an observation mode. Work on an isolated state snapshot so
	// evaluating an existing confirmation or exploration lease cannot change
	// the next cycle in memory or create a durable state file.
	var originalState *syncState
	if r.config.DryRun {
		originalState = r.state
		r.state = cloneSyncState(r.state)
		defer func() { r.state = originalState }()
	}
	r.prepareStrategyState()
	accounts, excluded := r.filterExcludedAccounts(accounts)
	accounts = r.includeExpiredMissingRecovery(accounts, now)
	recommendations := scoreAccounts(accounts, now, r.config.MinSamples)
	r.prepareExploration(accounts, recommendations, now)
	apiKey := r.config.AdminAPIKey
	if !r.config.DryRun && hasPendingChanges(recommendations) && apiKey == "" {
		apiKey, err = r.source.AdminAPIKey(ctx)
		if err != nil {
			r.logger.Error("读取 Admin API Key 失败，本轮不写入优先级", "error", err)
		}
	}
	changed, pending := r.applyRecommendations(ctx, recommendations, apiKey, now)
	if !r.config.DryRun {
		if err := saveState(r.config.StateFile, r.state); err != nil {
			return err
		}
	}
	report := PriorityReport{
		GeneratedAt: now,
		WindowStart: now.Add(-window),
		WindowEnd:   now,
		DryRun:      r.config.DryRun,
		Accounts:    recommendations,
	}
	if err := saveReport(r.config.ReportFile, report); err != nil {
		return err
	}
	if err := writeRecommendationTable(r.tableWriter, now.Format(time.RFC3339), recommendations); err != nil {
		r.logger.Error("输出优先级账户表失败", "error", err)
	}
	r.logger.Info("优先级策略周期完成",
		"accounts", len(recommendations),
		"excluded", excluded,
		"changed", changed,
		"pending", pending,
		"dry_run", r.config.DryRun,
		"window", window.String(),
		"evaluation_weights", evaluationWeights,
		"decision_windows", decisionWindowPolicy,
		"recovery_anchor", recoveryAnchorPolicy,
	)
	return nil
}

func (r *Runner) filterExcludedAccounts(accounts []AccountMetrics) ([]AccountMetrics, int) {
	if r == nil || len(r.config.ExcludeRules) == 0 || len(accounts) == 0 {
		return accounts, 0
	}
	filtered := make([]AccountMetrics, 0, len(accounts))
	excluded := 0
	for _, account := range accounts {
		if r.config.ExcludesAccount(account) {
			if r.state != nil {
				delete(r.state.Accounts, account.ID)
				if r.state.Exploration != nil && r.state.Exploration.AccountID == account.ID {
					r.state.Exploration = nil
				}
			}
			excluded++
			continue
		}
		filtered = append(filtered, account)
	}
	return filtered, excluded
}

func (r *Runner) prepareStrategyState() {
	if r == nil || r.state == nil || r.state.Strategy == strategyVersion {
		return
	}
	for id, state := range r.state.Accounts {
		state.CandidatePriority = 0
		state.CandidateCount = 0
		state.PromotionFrozenCycles = 0
		r.state.Accounts[id] = state
	}
	r.state.Strategy = strategyVersion
}

func hasPendingChanges(recommendations []Recommendation) bool {
	for _, recommendation := range recommendations {
		if recommendation.explorationStart || recommendation.RecommendedPriority != recommendation.CurrentPriority {
			return true
		}
	}
	return false
}

func (r *Runner) includeExpiredMissingRecovery(accounts []AccountMetrics, now time.Time) []AccountMetrics {
	if r == nil || r.state == nil || r.state.Exploration == nil ||
		!r.state.Exploration.Recovery || !explorationExpired(r.state.Exploration, now) {
		return accounts
	}
	for _, account := range accounts {
		if account.ID == r.state.Exploration.AccountID {
			return accounts
		}
	}
	return append(accounts, AccountMetrics{
		ID:                       r.state.Exploration.AccountID,
		Status:                   "active",
		CurrentPriority:          priorityExplore,
		DecisionWindowsAvailable: true,
		recoveryPlaceholder:      true,
	})
}

// prepareExploration gives one low-evidence, low-priority account a bounded
// observation slot. It only prepares recommendations; durable exploration
// state is written by applyRecommendations after the Admin API succeeds.
func (r *Runner) prepareExploration(accounts []AccountMetrics, recommendations []Recommendation, now time.Time) {
	if r == nil || r.state == nil || len(recommendations) == 0 {
		return
	}
	byID := make(map[int64]AccountMetrics, len(accounts))
	for _, account := range accounts {
		byID[account.ID] = account
	}
	if exploration := r.state.Exploration; exploration != nil {
		account, exists := byID[exploration.AccountID]
		if !exists {
			// A recovery lease means priority 20 was already written. Keep it
			// until the account returns so the original priority can be restored.
			if explorationExpired(exploration, now) && !exploration.Recovery {
				r.state.Exploration = nil
			}
			return
		}
		for index := range recommendations {
			recommendation := &recommendations[index]
			if recommendation.ID != exploration.AccountID {
				continue
			}
			recommendation.Exploration = true
			if exploration.Recovery {
				r.prepareActiveRecovery(account, recommendation, exploration, now)
				break
			}
			evidence := scoringEvidence(account, r.config.MinSamples)
			hardExcluded, hardReason := accountHardExcluded(account, now)
			switch {
			case hardExcluded:
				recommendation.RecommendedPriority = priorityUnavailable
				recommendation.Reason = hardReason + "；结束探索"
				recommendation.applyImmediately = true
				recommendation.explorationEnd = true
			case !r.config.ExplorationEnabled:
				recommendation.RecommendedPriority = normalizedPriority(exploration.OriginalPriority)
				recommendation.Reason = "自动探索已关闭，恢复探索前优先级"
				recommendation.applyImmediately = true
				recommendation.explorationEnd = true
			case hasReliableCost(recommendation):
				targetPriority := recommendation.RecommendedPriority
				recommendation.Reason += "；已有成本证据，结束探索"
				recommendation.explorationEnd = rampPriority(recommendation.CurrentPriority, targetPriority) == targetPriority
			case evidence >= int64(r.config.MinSamples):
				targetPriority := recommendation.RecommendedPriority
				recommendation.Reason += "；探索样本已足够但无成本，结束探索"
				recommendation.explorationEnd = recommendation.applyImmediately ||
					rampPriority(recommendation.CurrentPriority, targetPriority) == targetPriority
			case explorationExpired(exploration, now):
				restorePriority := recommendation.AnchorPriority
				if restorePriority <= 0 {
					restorePriority = normalizedPriority(exploration.OriginalPriority)
				}
				recommendation.RecommendedPriority = restorePriority
				recommendation.Reason = fmt.Sprintf("探索超时，向默认优先级 %d 缓慢回退", restorePriority)
				recommendation.explorationEnd = rampPriority(recommendation.CurrentPriority, restorePriority) == restorePriority
			default:
				recommendation.RecommendedPriority = priorityExplore
				recommendation.Reason = "低样本账户探索中，等待更多最终结果"
				recommendation.ApplyStatus = "exploring"
			}
			break
		}
		if !containsRecommendation(recommendations, exploration.AccountID) {
			r.state.Exploration = nil
			return
		}
		sortRecommendations(recommendations)
		return
	}
	if candidate, ok := r.selectRecoveryCandidate(accounts, now); ok {
		for index := range recommendations {
			if recommendations[index].ID != candidate.Account.ID {
				continue
			}
			recommendation := &recommendations[index]
			recommendation.RecommendedPriority = priorityExplore
			recommendation.AnchorPriority = candidate.TargetPriority
			recommendation.Exploration = true
			recommendation.Recovery = true
			recommendation.RecoveryAnchorCostPerMillion = candidate.AnchorCostPerMillion
			recommendation.RecoveryPeerCount = candidate.PeerCount
			recommendation.recoveryBaselineFailures = recoveryTerminalFailures(candidate.Account)
			recommendation.explorationStart = true
			recommendation.applyImmediately = true
			recommendation.Reason = fmt.Sprintf("倍率恢复锚点 %.4f/M（同平台 %d 个样本，预测目标 %d），进入 10 分钟试跑", candidate.AnchorCostPerMillion, candidate.PeerCount, candidate.TargetPriority)
			break
		}
		sortRecommendations(recommendations)
		return
	}
	if !r.config.ExplorationEnabled {
		return
	}
	if r.config.DryRun {
		return
	}
	candidate, ok := r.selectExplorationCandidate(accounts, now)
	if !ok {
		return
	}
	for index := range recommendations {
		if recommendations[index].ID != candidate.ID {
			continue
		}
		recommendation := &recommendations[index]
		recommendation.RecommendedPriority = priorityExplore
		recommendation.Exploration = true
		recommendation.explorationStart = true
		recommendation.applyImmediately = true
		recommendation.Reason = "证据不足，进入有限探索档位"
		break
	}
	sortRecommendations(recommendations)
}

const (
	recoveryOutcomeSuccess  = "success"
	recoveryOutcomeFailure  = "failure"
	recoveryOutcomeNoResult = "no-result"
)

type recoveryCandidate struct {
	Account              AccountMetrics
	AnchorCostPerMillion float64
	PeerCount            int
	TargetPriority       int
}

func (r *Runner) prepareActiveRecovery(account AccountMetrics, recommendation *Recommendation, exploration *explorationState, now time.Time) {
	recommendation.Recovery = true
	recommendation.recoveryMissing = account.recoveryPlaceholder
	recommendation.RecoveryAnchorCostPerMillion = exploration.RecoveryAnchorCostPerMillion
	recommendation.RecoveryPeerCount = exploration.RecoveryPeerCount
	recommendation.AnchorPriority = exploration.RecoveryTargetPriority
	hardExcluded, hardReason := accountHardExcluded(account, now)
	if hardExcluded {
		recommendation.RecommendedPriority = priorityUnavailable
		recommendation.Reason = hardReason + "；结束倍率恢复试跑"
		recommendation.applyImmediately = true
		recommendation.explorationEnd = true
		recommendation.recoveryOutcome = recoveryOutcomeFailure
		return
	}
	if !explorationExpired(exploration, now) {
		recommendation.RecommendedPriority = priorityExplore
		recommendation.Reason = "倍率恢复试跑中，等待 10 分钟真实成本"
		recommendation.ApplyStatus = "recovery-testing"
		return
	}

	measuredPriority := recommendation.RecommendedPriority
	restorePriority := normalizedPriority(exploration.OriginalPriority)
	recommendation.RecommendedPriority = restorePriority
	recommendation.applyImmediately = true
	recommendation.explorationEnd = true
	if !hasReliableCost(recommendation) {
		if recoveryTerminalFailures(account) > exploration.RecoveryBaselineFailures {
			recommendation.recoveryOutcome = recoveryOutcomeFailure
			recommendation.Reason = fmt.Sprintf("倍率恢复试跑出现终态失败，恢复原优先级 %d", restorePriority)
		} else {
			recommendation.recoveryOutcome = recoveryOutcomeNoResult
			recommendation.Reason = fmt.Sprintf("倍率恢复试跑无有效成本，恢复原优先级 %d，2 小时后可重试", restorePriority)
		}
		return
	}
	if measuredPriority < restorePriority && measuredPriority < priorityNeutral {
		recommendation.recoveryOutcome = recoveryOutcomeSuccess
		recommendation.Reason = fmt.Sprintf("倍率恢复试跑成本 %.4f/M，恢复原优先级 %d，后续按正式成本重新确认", recommendation.CostPerMillionTokens, restorePriority)
		return
	}
	recommendation.recoveryOutcome = recoveryOutcomeFailure
	recommendation.Reason = fmt.Sprintf("倍率恢复试跑成本 %.4f/M 未改善目标，恢复原优先级 %d", recommendation.CostPerMillionTokens, restorePriority)
}

func (r *Runner) selectRecoveryCandidate(accounts []AccountMetrics, now time.Time) (recoveryCandidate, bool) {
	best := recoveryCandidate{}
	for _, account := range accounts {
		if !account.DecisionWindowsAvailable || directAccountCost(account).Valid {
			continue
		}
		state := r.state.Accounts[account.ID]
		if state.RecoveryRetryAt != nil && now.Before(*state.RecoveryRetryAt) {
			continue
		}
		if account.RateMultiplier <= 0 || !validScore(account.RateMultiplier) {
			continue
		}
		if hardExcluded, _ := accountHardExcluded(account, now); hardExcluded {
			continue
		}
		platform := strings.ToLower(strings.TrimSpace(account.Platform))
		if platform == "" {
			continue
		}
		normalizedPeerCosts := make([]float64, 0)
		for _, peer := range accounts {
			if peer.ID == account.ID || !strings.EqualFold(strings.TrimSpace(peer.Platform), platform) || peer.RateMultiplier <= 0 || !validScore(peer.RateMultiplier) {
				continue
			}
			if hardExcluded, _ := accountHardExcluded(peer, now); hardExcluded {
				continue
			}
			evidence := directAccountCost(peer)
			if !evidence.Valid {
				continue
			}
			normalizedPeerCosts = append(normalizedPeerCosts, evidence.CostPerMillion/peer.RateMultiplier)
		}
		if len(normalizedPeerCosts) < recoveryMinimumPeers {
			continue
		}
		anchor := percentile(normalizedPeerCosts, 0.5) * account.RateMultiplier
		if anchor <= 0 || !validScore(anchor) {
			continue
		}
		target := recoveryPriorityForCost(accounts, anchor, now)
		if target >= normalizedPriority(account.CurrentPriority) {
			continue
		}
		candidate := recoveryCandidate{Account: account, AnchorCostPerMillion: anchor, PeerCount: len(normalizedPeerCosts), TargetPriority: target}
		if best.Account.ID == 0 || candidate.AnchorCostPerMillion < best.AnchorCostPerMillion ||
			(candidate.AnchorCostPerMillion == best.AnchorCostPerMillion && candidate.Account.ID < best.Account.ID) {
			best = candidate
		}
	}
	return best, best.Account.ID != 0
}

func recoveryPriorityForCost(accounts []AccountMetrics, anchor float64, now time.Time) int {
	values := make([]float64, 0, len(accounts)+1)
	for _, account := range accounts {
		if hardExcluded, _ := accountHardExcluded(account, now); hardExcluded {
			continue
		}
		if evidence := directAccountCost(account); evidence.Valid {
			values = append(values, evidence.CostPerMillion)
		}
	}
	values = append(values, anchor)
	valid := make([]bool, len(values))
	for index := range valid {
		valid[index] = true
	}
	scores := rankLowerBetter(values, valid)
	return priorityForScore(scores[len(scores)-1])
}

func recoveryTerminalFailures(account AccountMetrics) int64 {
	if account.Window30m == nil {
		return 0
	}
	return maxInt64(account.Window30m.TerminalFailures, 0)
}

func containsRecommendation(recommendations []Recommendation, id int64) bool {
	for _, recommendation := range recommendations {
		if recommendation.ID == id {
			return true
		}
	}
	return false
}

func (r *Runner) selectExplorationCandidate(accounts []AccountMetrics, now time.Time) (AccountMetrics, bool) {
	eligible := make([]AccountMetrics, 0, len(accounts))
	for _, account := range accounts {
		if accountState, ok := r.state.Accounts[account.ID]; ok && accountState.LastExploredAt != nil && now.Sub(*accountState.LastExploredAt) < explorationDuration {
			continue
		}
		if directAccountCost(account).Valid {
			continue
		}
		evidence := scoringEvidence(account, r.config.MinSamples)
		hardExcluded, _ := accountHardExcluded(account, now)
		priority := normalizedPriority(account.CurrentPriority)
		if evidence >= int64(r.config.MinSamples) || hardExcluded || priority < priorityNeutral || priority >= priorityUnavailable {
			continue
		}
		eligible = append(eligible, account)
	}
	return selectExplorationCandidate(eligible, r.state.Exploration, r.state.ExplorationCursor, now, r.config.MinSamples)
}

// selectExplorationCandidate returns the first eligible account after the
// persisted cursor, wrapping by stable ID to give cold accounts fair turns.
func selectExplorationCandidate(accounts []AccountMetrics, active *explorationState, cursor int64, now time.Time, minSamples int) (AccountMetrics, bool) {
	if minSamples < 1 {
		minSamples = defaultMinSamples
	}
	eligible := make([]AccountMetrics, 0, len(accounts))
	for _, account := range accounts {
		if active != nil && account.ID == active.AccountID {
			continue
		}
		evidence := scoringEvidence(account, minSamples)
		hardExcluded, _ := accountHardExcluded(account, now)
		priority := normalizedPriority(account.CurrentPriority)
		if account.ID <= 0 || evidence >= int64(minSamples) || hardExcluded || priority < priorityNeutral || priority >= priorityUnavailable {
			continue
		}
		eligible = append(eligible, account)
	}
	if len(eligible) == 0 {
		return AccountMetrics{}, false
	}
	sort.SliceStable(eligible, func(i, j int) bool { return eligible[i].ID < eligible[j].ID })
	for _, account := range eligible {
		if account.ID > cursor {
			return account, true
		}
	}
	return eligible[0], true
}

func (r *Runner) applyRecommendations(ctx context.Context, recommendations []Recommendation, apiKey string, now time.Time) (changed, pending int) {
	activeExplorationID := int64(0)
	if r.state.Exploration != nil {
		activeExplorationID = r.state.Exploration.AccountID
	} else {
		for _, recommendation := range recommendations {
			if recommendation.explorationStart {
				activeExplorationID = recommendation.ID
				break
			}
		}
	}
	for index := range recommendations {
		recommendation := &recommendations[index]
		state := r.state.Accounts[recommendation.ID]
		state.LastSeenAt = timePtr(now)
		state.PromotionFrozenCycles = 0
		evidence := recommendation.SuccessfulRequests + recommendation.TerminalFailures
		recommendation.NextPriority = recommendation.RecommendedPriority
		if !recommendation.applyImmediately {
			recommendation.NextPriority = rampPriority(recommendation.CurrentPriority, recommendation.RecommendedPriority)
		}
		if activeExplorationID != 0 && recommendation.ID != activeExplorationID && !recommendation.applyImmediately && !hasReliableCost(recommendation) &&
			evidence < int64(r.config.MinSamples) && recommendation.RecommendedPriority < recommendation.CurrentPriority {
			if recommendation.RecommendedPriority != recommendation.CurrentPriority {
				pending++
			}
			recommendation.ApplyStatus = "deferred-exploration"
			r.state.Accounts[recommendation.ID] = state
			continue
		}
		if recommendation.explorationStart {
			if !r.applyPriorityUpdate(ctx, recommendation, apiKey, recommendation.RecommendedPriority, now, &state) {
				pending++
				r.state.Accounts[recommendation.ID] = state
				continue
			}
			r.state.Accounts[recommendation.ID] = state
			if recommendation.Recovery {
				recommendation.ApplyStatus = "recovery-started"
			} else {
				recommendation.ApplyStatus = "exploration-started"
			}
			state.LastExploredAt = timePtr(now)
			r.state.Accounts[recommendation.ID] = state
			r.state.Exploration = &explorationState{
				AccountID:                    recommendation.ID,
				OriginalPriority:             recommendation.CurrentPriority,
				StartedAt:                    timePtr(now),
				Recovery:                     recommendation.Recovery,
				RecoveryAnchorCostPerMillion: recommendation.RecoveryAnchorCostPerMillion,
				RecoveryPeerCount:            recommendation.RecoveryPeerCount,
				RecoveryTargetPriority:       recommendation.AnchorPriority,
				RecoveryBaselineFailures:     recommendation.recoveryBaselineFailures,
			}
			r.state.ExplorationCursor = recommendation.ID
			changed++
			activeExplorationID = recommendation.ID
			continue
		}
		isActiveExploration := recommendation.Exploration && r.state.Exploration != nil && r.state.Exploration.AccountID == recommendation.ID
		if recommendation.RecommendedPriority == recommendation.CurrentPriority {
			state.CandidatePriority = 0
			state.CandidateCount = 0
			if isActiveExploration && recommendation.explorationEnd {
				r.finishExploration(recommendation, &state, now)
				activeExplorationID = 0
				recommendation.ApplyStatus = explorationEndStatus(recommendation)
			} else if recommendation.ApplyStatus == "" {
				recommendation.ApplyStatus = "unchanged"
			}
			r.state.Accounts[recommendation.ID] = state
			continue
		}
		if recommendation.applyImmediately {
			if !r.applyPriorityUpdate(ctx, recommendation, apiKey, recommendation.RecommendedPriority, now, &state) {
				if isActiveExploration && recommendation.recoveryMissing && recommendation.ApplyStatus == "not-found" {
					r.state.Exploration = nil
					activeExplorationID = 0
					recommendation.ApplyStatus = "recovery-account-removed"
				} else {
					pending++
				}
				r.state.Accounts[recommendation.ID] = state
				continue
			}
			if isActiveExploration && recommendation.explorationEnd {
				r.finishExploration(recommendation, &state, now)
				activeExplorationID = 0
				recommendation.ApplyStatus = explorationEndStatus(recommendation)
			} else {
				recommendation.ApplyStatus = "updated"
			}
			r.state.Accounts[recommendation.ID] = state
			changed++
			continue
		}
		direction := priorityChangeDirection(recommendation.CurrentPriority, recommendation.RecommendedPriority)
		candidateDirection := priorityChangeDirection(recommendation.CurrentPriority, state.CandidatePriority)
		if state.CandidateCount > 0 && candidateDirection == direction {
			state.CandidatePriority = recommendation.RecommendedPriority
			state.CandidateCount++
		} else {
			state.CandidatePriority = recommendation.RecommendedPriority
			state.CandidateCount = 1
		}
		if state.CandidateCount < r.config.Confirmations {
			recommendation.ApplyStatus = "pending"
			pending++
			r.state.Accounts[recommendation.ID] = state
			continue
		}
		if !isActiveExploration && state.LastAppliedAt != nil && now.Sub(*state.LastAppliedAt) < r.config.ChangeCooldown {
			recommendation.ApplyStatus = "cooldown"
			pending++
			r.state.Accounts[recommendation.ID] = state
			continue
		}
		if r.config.DryRun {
			recommendation.ApplyStatus = "dry-run"
			r.logger.Info("dry-run 建议更新账户优先级",
				"account", accountLabel(recommendation.Name, recommendation.ID),
				"from", recommendation.CurrentPriority,
				"to", recommendation.NextPriority,
				"target", recommendation.RecommendedPriority,
				"score", roundScore(recommendation.Score),
			)
			pending++
			r.state.Accounts[recommendation.ID] = state
			continue
		}
		if apiKey == "" {
			recommendation.ApplyStatus = "missing-key"
			pending++
			r.logger.Warn("缺少 Admin API Key，跳过账户优先级写入", "account", accountLabel(recommendation.Name, recommendation.ID))
			r.state.Accounts[recommendation.ID] = state
			continue
		}
		if !r.applyPriorityUpdate(ctx, recommendation, apiKey, recommendation.NextPriority, now, &state) {
			pending++
			r.state.Accounts[recommendation.ID] = state
			continue
		}
		if isActiveExploration && recommendation.explorationEnd {
			r.finishExploration(recommendation, &state, now)
			activeExplorationID = 0
			recommendation.ApplyStatus = explorationEndStatus(recommendation)
		} else {
			recommendation.ApplyStatus = "updated"
		}
		r.state.Accounts[recommendation.ID] = state
		changed++
	}
	r.pruneAccountState(now)
	return changed, pending
}

// pruneAccountState keeps retry and baseline state across short snapshot gaps,
// while bounding entries for accounts that have been removed or disabled.
func (r *Runner) pruneAccountState(now time.Time) {
	if r == nil || r.state == nil || r.state.Accounts == nil {
		return
	}
	activeExplorationID := int64(0)
	if r.state.Exploration != nil {
		activeExplorationID = r.state.Exploration.AccountID
	}
	for id, state := range r.state.Accounts {
		if id == activeExplorationID && !explorationExpired(r.state.Exploration, now) {
			continue
		}
		lastSeen := state.LastSeenAt
		if lastSeen == nil {
			// Older state files predate last_seen_at. Use the latest known
			// activity as a conservative starting point; otherwise give the
			// entry one retention period to be observed again.
			lastSeen = latestStateTime(state.LastAppliedAt, state.LastExploredAt)
			if lastSeen == nil {
				state.LastSeenAt = timePtr(now)
				r.state.Accounts[id] = state
				continue
			}
		}
		if now.Before(*lastSeen) || now.Sub(*lastSeen) < stateRetentionDuration {
			continue
		}
		delete(r.state.Accounts, id)
	}
}

func explorationExpired(exploration *explorationState, now time.Time) bool {
	if exploration == nil {
		return true
	}
	startedAt := exploration.StartedAt
	if startedAt == nil {
		return true
	}
	duration := explorationDuration
	if exploration.Recovery {
		duration = recoveryDuration
	}
	return !now.Before(startedAt.Add(duration))
}

func (r *Runner) finishExploration(recommendation *Recommendation, state *accountState, now time.Time) {
	if r == nil || r.state == nil || r.state.Exploration == nil {
		return
	}
	if r.state.Exploration.Recovery && recommendation != nil && state != nil {
		switch recommendation.recoveryOutcome {
		case recoveryOutcomeSuccess:
			state.RecoveryFailures = 0
			state.RecoveryRetryAt = nil
		case recoveryOutcomeFailure:
			state.RecoveryFailures++
			state.RecoveryRetryAt = timePtr(now.Add(recoveryBackoff(state.RecoveryFailures)))
		case recoveryOutcomeNoResult:
			state.RecoveryRetryAt = timePtr(now.Add(recoveryNoResultBackoff))
		}
	}
	r.state.Exploration = nil
}

func recoveryBackoff(failures int) time.Duration {
	switch failures {
	case 1:
		return 2 * time.Hour
	case 2:
		return 6 * time.Hour
	default:
		return 24 * time.Hour
	}
}

func explorationEndStatus(recommendation *Recommendation) string {
	if recommendation != nil && recommendation.Recovery {
		return "recovery-ended"
	}
	return "exploration-ended"
}

func latestStateTime(values ...*time.Time) *time.Time {
	var latest *time.Time
	for _, value := range values {
		if value == nil || (latest != nil && !value.After(*latest)) {
			continue
		}
		latest = value
	}
	return latest
}

func hasReliableCost(recommendation *Recommendation) bool {
	return recommendation != nil && recommendation.CostPerMillionTokens > 0 && validScore(recommendation.CostPerMillionTokens)
}

func costAdvantageEvidence(recommendation *Recommendation) bool {
	return recommendation != nil && recommendation.costAdvantageKnown
}

func updatePromotionBaseline(state *accountState, recommendation *Recommendation) (shock, frozen bool) {
	if state == nil {
		return false, false
	}
	if !hasReliableCost(recommendation) {
		if state.PromotionFrozenCycles > 0 {
			frozen = true
			state.PromotionFrozenCycles--
		}
		return false, frozen
	}
	if state.HasCostBaseline && state.LastCostPerMillion > 0 {
		delta := math.Abs(recommendation.CostPerMillionTokens-state.LastCostPerMillion) / state.LastCostPerMillion
		if validScore(delta) && delta > costShockThreshold {
			shock = true
		}
	}
	if state.HasCacheBaseline && recommendation.CacheHitRateKnown && validScore(state.LastCacheHitRate) && validScore(recommendation.CacheHitRate) {
		if math.Abs(recommendation.CacheHitRate-state.LastCacheHitRate) > cacheShockThreshold {
			shock = true
		}
	}
	if shock {
		state.PromotionFrozenCycles = promotionFreezeCycles
	} else if state.PromotionFrozenCycles > 0 {
		frozen = true
		state.PromotionFrozenCycles--
	}
	state.LastCostPerMillion = recommendation.CostPerMillionTokens
	state.HasCostBaseline = true
	if recommendation.CacheHitRateKnown && validScore(recommendation.CacheHitRate) {
		state.LastCacheHitRate = clamp01(recommendation.CacheHitRate)
		state.HasCacheBaseline = true
	}
	return shock, frozen
}

func (r *Runner) applyPriorityUpdate(ctx context.Context, recommendation *Recommendation, apiKey string, priority int, now time.Time, state *accountState) bool {
	if r.config.DryRun {
		recommendation.ApplyStatus = "dry-run"
		r.logger.Info("dry-run 建议更新账户优先级",
			"account", accountLabel(recommendation.Name, recommendation.ID),
			"from", recommendation.CurrentPriority,
			"to", priority,
			"target", recommendation.RecommendedPriority,
			"score", roundScore(recommendation.Score),
		)
		return false
	}
	if apiKey == "" {
		recommendation.ApplyStatus = "missing-key"
		r.logger.Warn("缺少 Admin API Key，跳过账户优先级写入", "account", accountLabel(recommendation.Name, recommendation.ID))
		return false
	}
	if err := r.admin.updatePriority(ctx, apiKey, recommendation.ID, priority); err != nil {
		if isAdminAccountNotFound(err) {
			recommendation.ApplyStatus = "not-found"
			r.logger.Warn("账户已不存在，跳过优先级写入", "account", accountLabel(recommendation.Name, recommendation.ID), "error", err)
		} else {
			recommendation.ApplyStatus = "failed"
			r.logger.Error("账户优先级写入失败", "account", accountLabel(recommendation.Name, recommendation.ID), "error", err)
		}
		return false
	}
	state.LastAppliedAt = timePtr(now)
	state.LastApplied = priority
	state.CandidatePriority = 0
	state.CandidateCount = 0
	recommendation.ApplyStatus = "updated"
	return true
}

// priorityChangeDirection identifies whether a recommendation improves or
// degrades the current priority. Priority values are ordered ascending.
func priorityChangeDirection(current, candidate int) int {
	switch {
	case candidate < current:
		return -1
	case candidate > current:
		return 1
	default:
		return 0
	}
}

// rampPriority limits normal scored-priority changes to one 20-point step. A
// larger custom gap is capped to four writes; unavailable recovery re-enters
// at neutral for good-or-better targets, but keeps degraded/poor targets so a
// bad account is never promoted straight back into the front of the queue.
func rampPriority(current, target int) int {
	if current == target || current <= 0 || target <= 0 || target >= priorityUnavailable {
		return target
	}
	if current >= priorityUnavailable {
		if target > priorityNeutral {
			return target
		}
		return priorityNeutral
	}
	distance := target - current
	if distance < 0 {
		distance = -distance
	}
	step := priorityRampStep
	maxDistance := priorityRampStep * priorityRampMaxSteps
	if distance > maxDistance {
		step = (distance + priorityRampMaxSteps - 1) / priorityRampMaxSteps
	}
	if target > current {
		if current+step > target {
			return target
		}
		return current + step
	}
	if current-step < target {
		return target
	}
	return current - step
}

func roundScore(value float64) float64 {
	return float64(int(value*100+0.5)) / 100
}

func timePtr(value time.Time) *time.Time {
	value = value.UTC()
	return &value
}
