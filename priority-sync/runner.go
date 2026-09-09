package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"sort"
	"time"
)

type metricsSource interface {
	LoadAccountMetrics(context.Context, time.Time, time.Duration) ([]AccountMetrics, error)
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
	accounts, err := r.source.LoadAccountMetrics(ctx, now, r.config.Window)
	if err != nil {
		return err
	}
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
	if err := saveState(r.config.StateFile, r.state); err != nil {
		return err
	}
	report := PriorityReport{
		GeneratedAt: now,
		WindowStart: now.Add(-r.config.Window),
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
		"changed", changed,
		"pending", pending,
		"dry_run", r.config.DryRun,
		"window", r.config.Window.String(),
	)
	return nil
}

func hasPendingChanges(recommendations []Recommendation) bool {
	for _, recommendation := range recommendations {
		if recommendation.RecommendedPriority != recommendation.CurrentPriority {
			return true
		}
	}
	return false
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
			// The account is no longer eligible for the read-only snapshot (for
			// example it was disabled or deleted); discard the stale lease so it
			// cannot block all other accounts forever.
			r.state.Exploration = nil
			return
		}
		for index := range recommendations {
			recommendation := &recommendations[index]
			if recommendation.ID != exploration.AccountID {
				continue
			}
			recommendation.Exploration = true
			evidence := account.SuccessfulRequests + account.TerminalFailures
			hardExcluded, hardReason := accountHardExcluded(account, now)
			switch {
			case hardExcluded:
				recommendation.RecommendedPriority = priorityUnavailable
				recommendation.Reason = hardReason + "；结束探索"
				recommendation.applyImmediately = true
				recommendation.explorationEnd = true
			case evidence >= int64(r.config.MinSamples):
				targetPriority := priorityForScore(recommendation.Score)
				recommendation.RecommendedPriority = targetPriority
				recommendation.Reason += "；探索样本已足够，进入正式评分确认"
				recommendation.explorationEnd = rampPriority(recommendation.CurrentPriority, targetPriority) == targetPriority
			case exploration.StartedAt != nil && !now.Before(exploration.StartedAt.Add(explorationDuration)):
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
		evidence := account.SuccessfulRequests + account.TerminalFailures
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
		evidence := account.SuccessfulRequests + account.TerminalFailures
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
	seen := make(map[int64]struct{}, len(recommendations))
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
		seen[recommendation.ID] = struct{}{}
		state := r.state.Accounts[recommendation.ID]
		evidence := recommendation.SuccessfulRequests + recommendation.TerminalFailures
		recommendation.NextPriority = recommendation.RecommendedPriority
		if !recommendation.applyImmediately {
			recommendation.NextPriority = rampPriority(recommendation.CurrentPriority, recommendation.RecommendedPriority)
		}
		if activeExplorationID != 0 && recommendation.ID != activeExplorationID && !recommendation.applyImmediately &&
			evidence < int64(r.config.MinSamples) && recommendation.RecommendedPriority < recommendation.CurrentPriority {
			if recommendation.RecommendedPriority != recommendation.CurrentPriority {
				pending++
			}
			recommendation.ApplyStatus = "deferred-exploration"
			continue
		}
		if recommendation.explorationStart {
			if !r.applyPriorityUpdate(ctx, recommendation, apiKey, recommendation.RecommendedPriority, now, &state) {
				pending++
				continue
			}
			r.state.Accounts[recommendation.ID] = state
			recommendation.ApplyStatus = "exploration-started"
			state.LastExploredAt = timePtr(now)
			r.state.Accounts[recommendation.ID] = state
			r.state.Exploration = &explorationState{
				AccountID:        recommendation.ID,
				OriginalPriority: recommendation.CurrentPriority,
				StartedAt:        timePtr(now),
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
			r.state.Accounts[recommendation.ID] = state
			if isActiveExploration && recommendation.explorationEnd {
				r.state.Exploration = nil
				activeExplorationID = 0
				recommendation.ApplyStatus = "exploration-ended"
			} else if recommendation.ApplyStatus == "" {
				recommendation.ApplyStatus = "unchanged"
			}
			continue
		}
		if recommendation.applyImmediately {
			if !r.applyPriorityUpdate(ctx, recommendation, apiKey, recommendation.RecommendedPriority, now, &state) {
				pending++
				continue
			}
			r.state.Accounts[recommendation.ID] = state
			if isActiveExploration && recommendation.explorationEnd {
				r.state.Exploration = nil
				activeExplorationID = 0
				recommendation.ApplyStatus = "exploration-ended"
			} else {
				recommendation.ApplyStatus = "updated"
			}
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
		r.state.Accounts[recommendation.ID] = state
		if isActiveExploration && recommendation.explorationEnd {
			r.state.Exploration = nil
			activeExplorationID = 0
			recommendation.ApplyStatus = "exploration-ended"
		} else {
			recommendation.ApplyStatus = "updated"
		}
		changed++
	}
	for id := range r.state.Accounts {
		if _, exists := seen[id]; !exists && (r.state.Exploration == nil || id != r.state.Exploration.AccountID) {
			delete(r.state.Accounts, id)
		}
	}
	return changed, pending
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
		recommendation.ApplyStatus = "failed"
		r.logger.Error("账户优先级写入失败", "account", accountLabel(recommendation.Name, recommendation.ID), "error", err)
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

// rampPriority limits normal formal-band changes to one 20-point step. A
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
