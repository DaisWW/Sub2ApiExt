package store

import (
	"fmt"
	"sort"
	"time"

	"github.com/DaisWW/Sub2ApiExt/monitoring/internal/config"
	"github.com/DaisWW/Sub2ApiExt/monitoring/internal/model"
)

func aggregateCostUsageGroups(groups []costUsageGroup) []costUsageGroup {
	byKey := make(map[string]costUsageGroup, len(groups))
	for _, group := range groups {
		key := costUserTargetKey(group.userKey, group.apiKeyID)
		aggregate, exists := byKey[key]
		if !exists {
			aggregate = group
			aggregate.model = ""
			aggregate.channelID = 0
			aggregate.channelName = ""
			aggregate.accountID = 0
			aggregate.accountName = ""
			aggregate.current = costUsageMetrics{}
			aggregate.baseline = costUsageMetrics{}
		}
		aggregate.current = addCostUsageMetrics(aggregate.current, group.current)
		aggregate.baseline = addCostUsageMetrics(aggregate.baseline, group.baseline)
		byKey[key] = aggregate
	}
	keys := make([]string, 0, len(byKey))
	for key := range byKey {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	result := make([]costUsageGroup, 0, len(keys))
	for _, key := range keys {
		result = append(result, byKey[key])
	}
	return result
}

func addCostUsageMetrics(left, right costUsageMetrics) costUsageMetrics {
	return costUsageMetrics{
		Requests:            left.Requests + right.Requests,
		InputTokens:         left.InputTokens + right.InputTokens,
		OutputTokens:        left.OutputTokens + right.OutputTokens,
		CacheCreationTokens: left.CacheCreationTokens + right.CacheCreationTokens,
		CacheReadTokens:     left.CacheReadTokens + right.CacheReadTokens,
		BaseCost:            left.BaseCost + right.BaseCost,
		ActualCost:          left.ActualCost + right.ActualCost,
		InputCost:           left.InputCost + right.InputCost,
		OutputCost:          left.OutputCost + right.OutputCost,
		CacheCreationCost:   left.CacheCreationCost + right.CacheCreationCost,
		CacheReadCost:       left.CacheReadCost + right.CacheReadCost,
	}
}

func evaluateTotalCostAlert(group costUsageGroup, policy config.CostAlertConfig, start, end time.Time) (model.CostAlertEvent, bool) {
	if group.current.Requests < int64(policy.MinRequests) ||
		group.current.TotalTokens() < policy.MinTokens ||
		group.current.ActualCost < policy.TotalMinCost {
		return model.CostAlertEvent{}, false
	}
	event := buildCostAlertBaseEvent(group, start, end)
	target := costUserTargetKey(group.userKey, group.apiKeyID)
	event.Kind = model.CostAlertTotalCost
	event.TargetKey = target
	event.AlertKey = model.CostAlertTotalCost + "|" + target
	event.Title = "总费用持续偏高"
	event.Message = fmt.Sprintf(
		"用户 %s 在最近窗口累计费用 %.4f，已达到告警门槛 %.4f；本窗口请求 %d 次、Tokens %d。",
		model.FormatIdentity(group.userName, group.userEmail, group.userKey, "用户"),
		group.current.ActualCost, policy.TotalMinCost, group.current.Requests, group.current.TotalTokens(),
	)
	event.Severity = "warning"
	return event, true
}

func buildCostAlertBaseEvent(group costUsageGroup, start, end time.Time) model.CostAlertEvent {
	current := group.current
	baseline := group.baseline
	return model.CostAlertEvent{
		TargetKey:            costTargetKey(group.userKey, group.apiKeyID, group.model, group.channelID, group.accountID),
		UserKey:              group.userKey,
		UserName:             group.userName,
		UserEmail:            group.userEmail,
		APIKeyID:             group.apiKeyID,
		APIKeyName:           group.apiKeyName,
		Model:                group.model,
		ChannelName:          group.channelName,
		AccountName:          group.accountName,
		Requests:             current.Requests,
		TotalTokens:          current.TotalTokens(),
		InputTokens:          current.InputTokens,
		OutputTokens:         current.OutputTokens,
		CacheCreationTokens:  current.CacheCreationTokens,
		CacheReadTokens:      current.CacheReadTokens,
		CurrentCost:          current.ActualCost,
		BaselineCost:         baseline.ActualCost,
		InputCost:            current.InputCost,
		OutputCost:           current.OutputCost,
		CacheCreationCost:    current.CacheCreationCost,
		CacheReadCost:        current.CacheReadCost,
		CurrentUnitCost:      current.UnitCost(),
		BaselineUnitCost:     baseline.UnitCost(),
		CurrentCacheHitRate:  current.CacheHitRate(),
		BaselineCacheHitRate: baseline.CacheHitRate(),
		CurrentMultiplier:    current.Multiplier(),
		BaselineMultiplier:   baseline.Multiplier(),
		WindowStart:          start,
		WindowEnd:            end,
		CreatedAt:            end,
		Severity:             "warning",
	}
}

func evaluateBudgetBurn(usage map[string]costDailyUsage, policy config.CostAlertConfig, now time.Time) []model.CostAlertEvent {
	if policy.DailyBudget <= 0 {
		return nil
	}
	keys := make([]string, 0, len(usage))
	for key := range usage {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	events := make([]model.CostAlertEvent, 0, len(keys))
	for _, key := range keys {
		if event, ok := budgetBurnAlert(usage[key], policy, now); ok {
			events = append(events, event)
		}
	}
	return events
}

func budgetBurnAlert(item costDailyUsage, policy config.CostAlertConfig, now time.Time) (model.CostAlertEvent, bool) {
	if item.dayStart.IsZero() {
		return model.CostAlertEvent{}, false
	}
	elapsed := now.Sub(item.dayStart)
	if elapsed < 0 {
		elapsed = 0
	}
	if elapsed > 24*time.Hour {
		elapsed = 24 * time.Hour
	}
	windowHours := policy.Window.Hours()
	if windowHours <= 0 {
		return model.CostAlertEvent{}, false
	}
	remaining := 24*time.Hour - elapsed
	burnRate := item.windowCost / windowHours
	allowedRate := policy.DailyBudget / 24
	projected := item.dailyCost + burnRate*remaining.Hours()
	if item.dailyCost < policy.DailyBudget*0.8 && burnRate < allowedRate*policy.BurnRatio && projected < policy.DailyBudget {
		return model.CostAlertEvent{}, false
	}
	severity := "warning"
	if item.dailyCost >= policy.DailyBudget || projected >= policy.DailyBudget {
		severity = "critical"
	}
	userLabel := model.FormatIdentity(item.userName, item.userEmail, item.userKey, "用户")
	target := costUserTargetKey(item.userKey, item.apiKeyID)
	return model.CostAlertEvent{
		AlertKey:  model.CostAlertBudgetBurn + "|" + target,
		Kind:      model.CostAlertBudgetBurn,
		Severity:  severity,
		TargetKey: target,
		Title:     "消费速度过高",
		Message: fmt.Sprintf(
			"用户 %s 今日已消费 %.4f，最近窗口消费 %.4f，按当前速度预计今日消费 %.4f，预算 %.4f。",
			userLabel, item.dailyCost, item.windowCost, projected, policy.DailyBudget,
		),
		UserKey:       item.userKey,
		UserName:      item.userName,
		UserEmail:     item.userEmail,
		APIKeyID:      item.apiKeyID,
		APIKeyName:    item.apiKeyName,
		Requests:      item.windowRequests,
		TotalTokens:   item.windowTokens,
		CurrentCost:   item.windowCost,
		DailyCost:     item.dailyCost,
		ProjectedCost: projected,
		WindowStart:   now.Add(-policy.Window),
		WindowEnd:     now,
		CreatedAt:     now,
	}, true
}

func costUserTargetKey(userKey string, apiKeyID int64) string {
	return fmt.Sprintf("user:%s|key:%d", userKey, apiKeyID)
}

func costTargetKey(userKey string, apiKeyID int64, modelName string, channelID, accountID int64) string {
	return fmt.Sprintf("%s|model:%s|channel:%d|account:%d", costUserTargetKey(userKey, apiKeyID), modelName, channelID, accountID)
}
