package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"time"
)

type metricsSource interface {
	LoadAccountMetrics(context.Context, time.Time, time.Duration) ([]AccountMetrics, error)
	AdminAPIKey(context.Context) (string, error)
}

type Runner struct {
	config Config
	source metricsSource
	admin  *adminAPI
	state  *syncState
	logger *slog.Logger
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
		config: config,
		source: source,
		admin:  newAdminAPI(config.Sub2APIURL, client),
		state:  state,
		logger: logger,
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

func (r *Runner) applyRecommendations(ctx context.Context, recommendations []Recommendation, apiKey string, now time.Time) (changed, pending int) {
	seen := make(map[int64]struct{}, len(recommendations))
	for _, recommendation := range recommendations {
		seen[recommendation.ID] = struct{}{}
		state := r.state.Accounts[recommendation.ID]
		if recommendation.RecommendedPriority == recommendation.CurrentPriority {
			state.CandidatePriority = 0
			state.CandidateCount = 0
			r.state.Accounts[recommendation.ID] = state
			continue
		}
		if state.CandidatePriority == recommendation.RecommendedPriority {
			state.CandidateCount++
		} else {
			state.CandidatePriority = recommendation.RecommendedPriority
			state.CandidateCount = 1
		}
		if state.CandidateCount < r.config.Confirmations {
			pending++
			r.state.Accounts[recommendation.ID] = state
			continue
		}
		if state.LastAppliedAt != nil && now.Sub(*state.LastAppliedAt) < r.config.ChangeCooldown {
			pending++
			r.state.Accounts[recommendation.ID] = state
			continue
		}
		if r.config.DryRun {
			r.logger.Info("dry-run 建议更新账户优先级",
				"account_id", recommendation.ID,
				"from", recommendation.CurrentPriority,
				"to", recommendation.RecommendedPriority,
				"score", roundScore(recommendation.Score),
			)
			pending++
			r.state.Accounts[recommendation.ID] = state
			continue
		}
		if apiKey == "" {
			pending++
			r.logger.Warn("缺少 Admin API Key，跳过账户优先级写入", "account_id", recommendation.ID)
			r.state.Accounts[recommendation.ID] = state
			continue
		}
		if err := r.admin.updatePriority(ctx, apiKey, recommendation.ID, recommendation.RecommendedPriority); err != nil {
			pending++
			r.logger.Error("账户优先级写入失败", "account_id", recommendation.ID, "error", err)
			r.state.Accounts[recommendation.ID] = state
			continue
		}
		state.LastAppliedAt = timePtr(now)
		state.LastApplied = recommendation.RecommendedPriority
		state.CandidatePriority = 0
		state.CandidateCount = 0
		r.state.Accounts[recommendation.ID] = state
		changed++
	}
	for id := range r.state.Accounts {
		if _, exists := seen[id]; !exists {
			delete(r.state.Accounts, id)
		}
	}
	return changed, pending
}

func roundScore(value float64) float64 {
	return float64(int(value*100+0.5)) / 100
}

func timePtr(value time.Time) *time.Time {
	value = value.UTC()
	return &value
}
