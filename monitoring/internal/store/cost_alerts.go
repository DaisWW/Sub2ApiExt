package store

import (
	"context"
	"fmt"
	"time"

	"github.com/DaisWW/Sub2ApiExt/monitoring/internal/config"
	"github.com/DaisWW/Sub2ApiExt/monitoring/internal/model"
)

type costAlertBounds struct {
	now           time.Time
	currentStart  time.Time
	baselineStart time.Time
	baselineEnd   time.Time
	dayStart      time.Time
}

func newCostAlertBounds(now time.Time, policy config.CostAlertConfig) costAlertBounds {
	currentStart := now.Add(-policy.Window)
	localNow := now.In(time.Local)
	dayStart := time.Date(localNow.Year(), localNow.Month(), localNow.Day(), 0, 0, 0, 0, time.Local).UTC()
	baselineEnd := dayStart
	if currentStart.Before(baselineEnd) {
		baselineEnd = currentStart
	}
	return costAlertBounds{
		now:           now,
		currentStart:  currentStart,
		baselineStart: now.Add(-policy.Baseline),
		baselineEnd:   baselineEnd,
		dayStart:      dayStart,
	}
}

func (s *Store) AnalyzeCostAlerts(ctx context.Context, policy config.CostAlertConfig) ([]model.CostAlertEvent, error) {
	if !policy.Enabled {
		return nil, nil
	}
	bounds := newCostAlertBounds(time.Now().UTC(), policy)
	groups, err := s.loadCostUsageGroups(ctx, bounds, policy)
	if err != nil {
		return nil, fmt.Errorf("load cost usage: %w", err)
	}
	daily, err := s.loadCostDailyUsage(ctx, bounds)
	if err != nil {
		return nil, fmt.Errorf("load daily cost: %w", err)
	}
	candidates := buildCostAlertCandidates(groups, daily, policy, bounds)
	return s.persistCostAlertCandidates(ctx, candidates, bounds.now, policy)
}

func buildCostAlertCandidates(groups []costUsageGroup, daily map[string]costDailyUsage, policy config.CostAlertConfig, bounds costAlertBounds) []model.CostAlertEvent {
	candidates := make([]model.CostAlertEvent, 0, len(groups))
	for _, group := range groups {
		candidates = append(candidates, evaluateCostUsageGroup(group, policy, bounds.currentStart, bounds.now)...)
	}
	candidates = append(candidates, evaluateBudgetBurn(daily, policy, bounds.now)...)
	addDailyCosts(candidates, daily)
	return candidates
}

func addDailyCosts(candidates []model.CostAlertEvent, daily map[string]costDailyUsage) {
	for index := range candidates {
		key := costUserTargetKey(candidates[index].UserKey, candidates[index].APIKeyID)
		if item, ok := daily[key]; ok {
			candidates[index].DailyCost = item.dailyCost
		}
	}
}
