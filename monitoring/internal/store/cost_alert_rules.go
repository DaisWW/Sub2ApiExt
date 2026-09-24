package store

import (
	"fmt"
	"sort"
	"time"

	"github.com/DaisWW/Sub2ApiExt/monitoring/internal/config"
	"github.com/DaisWW/Sub2ApiExt/monitoring/internal/model"
)

func evaluateCostUsageGroup(group costUsageGroup, policy config.CostAlertConfig, start, end time.Time) []model.CostAlertEvent {
	base := buildCostAlertBaseEvent(group, start, end)
	userLabel := model.FormatIdentity(group.userName, group.userEmail, group.userKey, "用户")
	events := evaluateImmediateCostRules(group, policy, base, userLabel)
	if !hasWindowEvidence(group, policy) {
		return withCostAlertKeys(events)
	}
	events = append(events, evaluateBaselineCostRules(group, policy, base, userLabel)...)
	return withCostAlertKeys(events)
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
		MaxRequestCost:       group.maxRequestCost,
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

func evaluateImmediateCostRules(group costUsageGroup, policy config.CostAlertConfig, base model.CostAlertEvent, userLabel string) []model.CostAlertEvent {
	events := make([]model.CostAlertEvent, 0, 2)
	if policy.SingleRequestCost > 0 && group.maxRequestCost >= policy.SingleRequestCost {
		events = append(events, singleRequestCostAlert(group, policy, base, userLabel))
	}
	if multiplierSpike(group, policy) {
		events = append(events, multiplierCostAlert(group, base, userLabel))
	}
	return events
}

func singleRequestCostAlert(group costUsageGroup, policy config.CostAlertConfig, base model.CostAlertEvent, userLabel string) model.CostAlertEvent {
	base.Kind = model.CostAlertSingleRequest
	base.Title = "单条请求成本过高"
	base.Message = fmt.Sprintf(
		"用户 %s 的 %s 在 %s 出现单条成本 %.4f，已超过配置上限 %.4f。",
		userLabel, group.model, group.channelName, group.maxRequestCost, policy.SingleRequestCost,
	)
	base.Severity = "critical"
	return base
}

func multiplierCostAlert(group costUsageGroup, base model.CostAlertEvent, userLabel string) model.CostAlertEvent {
	base.Kind = model.CostAlertMultiplier
	base.Title = "实际倍率异常"
	base.Message = fmt.Sprintf(
		"用户 %s 的 %s 在 %s 最近窗口实际倍率 %.2fx，历史 %.2fx，最高单条倍率 %.2fx。",
		userLabel, group.model, group.channelName, base.CurrentMultiplier,
		base.BaselineMultiplier, group.maxMultiplier,
	)
	base.Severity = "critical"
	return base
}

func hasWindowEvidence(group costUsageGroup, policy config.CostAlertConfig) bool {
	return group.current.Requests >= int64(policy.MinRequests) &&
		group.current.ActualCost >= policy.MinCost &&
		group.current.TotalTokens() >= policy.MinTokens
}

func evaluateBaselineCostRules(group costUsageGroup, policy config.CostAlertConfig, base model.CostAlertEvent, userLabel string) []model.CostAlertEvent {
	if !baselineReady(group, policy) {
		return nil
	}
	events := make([]model.CostAlertEvent, 0, 2)
	if cacheDegraded(group.current, group.baseline, policy) {
		events = append(events, cacheCostAlert(group, base, userLabel))
	}
	if unitCostSpike(group.current, group.baseline, policy) {
		events = append(events, unitCostAlert(group, policy, base, userLabel))
	}
	return events
}

func baselineReady(group costUsageGroup, policy config.CostAlertConfig) bool {
	return group.baseline.Requests >= int64(policy.MinRequests) &&
		group.baseline.TotalTokens() >= policy.MinTokens
}

func cacheCostAlert(group costUsageGroup, base model.CostAlertEvent, userLabel string) model.CostAlertEvent {
	base.Kind = model.CostAlertCacheDegraded
	base.Title = "疑似缓存失效"
	base.Message = fmt.Sprintf(
		"用户 %s 的 %s 在 %s 最近窗口缓存命中率 %.1f%%（历史 %.1f%%），单位成本 %.4f（历史 %.4f）。",
		userLabel, group.model, group.channelName, base.CurrentCacheHitRate,
		base.BaselineCacheHitRate, base.CurrentUnitCost, base.BaselineUnitCost,
	)
	return base
}

func unitCostAlert(group costUsageGroup, policy config.CostAlertConfig, base model.CostAlertEvent, userLabel string) model.CostAlertEvent {
	base.Kind = model.CostAlertUnitCost
	base.Title = "每百万 Tokens 成本异常"
	base.Message = fmt.Sprintf(
		"用户 %s 的 %s 在 %s 最近窗口单位成本 %.4f，历史基线 %.4f，当前成本 %.4f，最高单条成本 %.4f。",
		userLabel, group.model, group.channelName, base.CurrentUnitCost,
		base.BaselineUnitCost, base.CurrentCost, base.MaxRequestCost,
	)
	if base.BaselineUnitCost > 0 && base.CurrentUnitCost >= base.BaselineUnitCost*policy.UnitCostRatio*1.5 {
		base.Severity = "critical"
	}
	return base
}

func withCostAlertKeys(events []model.CostAlertEvent) []model.CostAlertEvent {
	for index := range events {
		events[index].AlertKey = events[index].Kind + "|" + events[index].TargetKey
	}
	return events
}

func cacheDegraded(current, baseline costUsageMetrics, policy config.CostAlertConfig) bool {
	if current.CacheableTokens() < policy.MinTokens || baseline.CacheableTokens() < policy.MinTokens {
		return false
	}
	if baseline.CacheHitRate()/100 < policy.CacheBaselineMin || current.CacheHitRate()/100 > policy.CacheCurrentMax {
		return false
	}
	return current.UnitCost() > 0 && baseline.UnitCost() > 0 &&
		current.UnitCost() >= baseline.UnitCost()*policy.CacheCostRatio
}

func unitCostSpike(current, baseline costUsageMetrics, policy config.CostAlertConfig) bool {
	return current.TotalTokens() >= policy.MinTokens && baseline.TotalTokens() >= policy.MinTokens &&
		current.UnitCost() > 0 && baseline.UnitCost() > 0 &&
		current.UnitCost() >= baseline.UnitCost()*policy.UnitCostRatio
}

func multiplierSpike(group costUsageGroup, policy config.CostAlertConfig) bool {
	current := group.current.Multiplier()
	baseline := group.baseline.Multiplier()
	if group.current.BaseCost < policy.MinBaseCost || current <= 0 {
		return false
	}
	if baseline > 0 {
		threshold := baseline * policy.MultiplierRatio
		return current >= threshold || group.maxMultiplier >= threshold
	}
	return current >= policy.MultiplierRatio || group.highMultiplierRequests >= 2
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
	if item.dayStart.IsZero() || (item.windowCost < policy.MinCost && item.dailyCost < policy.DailyBudget) {
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
