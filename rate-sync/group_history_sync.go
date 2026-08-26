package main

import (
	"context"
	"fmt"
	"math"
	"time"
)

type historicalGroup struct {
	group           *sub2APIGroup
	singleAccount   bool
	suspiciousRates bool
}

func (s *Syncer) syncGroupsFromUsage(
	ctx context.Context,
	channels []Channel,
	now time.Time,
	initialHandled map[int64]bool,
	report *syncReport,
) (map[int64]bool, error) {
	if source, ok := s.source.(groupUsageIncrementalSource); ok {
		return s.syncDynamicGroups(ctx, source, channels, now, initialHandled, report)
	}
	return s.syncGroupsFromHistory(ctx, channels, now, initialHandled, report)
}

func (s *Syncer) syncGroupsFromHistory(
	ctx context.Context,
	channels []Channel,
	now time.Time,
	initialHandled map[int64]bool,
	report *syncReport,
) (map[int64]bool, error) {
	handled := ensureHandledGroups(initialHandled)
	if !supportsHistoricalUsage(s.source) {
		return handled, nil
	}
	windows := adaptiveHistoryWindows(s.config.HistoryWindow)
	usageByWindow, err := s.loadGroupUsageByWindows(ctx, now, windows)
	if err != nil {
		return handled, err
	}
	if len(usageByWindow) == 0 {
		return handled, nil
	}

	groups := collectHistoricalGroups(s, channels)
	for groupID, historical := range groups {
		if handled[groupID] || historical.singleAccount {
			continue
		}
		s.syncHistoricalGroup(ctx, groupID, historical, usageByWindow, windows, handled, report)
	}
	return handled, nil
}

func ensureHandledGroups(groups map[int64]bool) map[int64]bool {
	if groups != nil {
		return groups
	}
	return make(map[int64]bool)
}

func supportsHistoricalUsage(source ChannelSource) bool {
	if _, ok := source.(groupUsageWindowSource); ok {
		return true
	}
	_, ok := source.(groupUsageSource)
	return ok
}

func collectHistoricalGroups(s *Syncer, channels []Channel) map[int64]historicalGroup {
	bindings := buildGroupBindings(channels)
	groups := make(map[int64]historicalGroup, len(bindings))
	for groupID, binding := range bindings {
		historical := historicalGroup{
			group:         &binding.group,
			singleAccount: len(binding.accounts) == 1,
		}
		for _, channel := range binding.accounts {
			if s.suspiciousAccountRate(&channel) {
				historical.suspiciousRates = true
				break
			}
		}
		groups[groupID] = historical
	}
	return groups
}

func (s *Syncer) syncHistoricalGroup(
	ctx context.Context,
	groupID int64,
	historical historicalGroup,
	usageByWindow map[time.Duration]map[int64]GroupUsageStats,
	windows []time.Duration,
	handled map[int64]bool,
	report *syncReport,
) {
	choice, reason, ok := chooseAdaptiveUsage(groupID, usageByWindow, windows)
	if !ok {
		report.markGroup(groupID, reportStatusSkipped)
		report.setGroupEvidence(groupID, "", reason)
		s.logger.Printf(
			"[%s] 历史成本校准跳过: %s，保持当前倍率 %.4f",
			historical.group.Name,
			reason,
			historical.group.RateMultiplier,
		)
		return
	}

	row := choice.Stats
	report.setGroupEvidence(groupID, formatHistoryWindow(choice.Window), fmt.Sprintf("请求=%d 标准=%.4f", row.Requests, row.StandardCost))
	if row.StandardCost < s.config.MinHistoryCostUSD {
		report.markGroup(groupID, reportStatusSkipped)
		s.logger.Printf(
			"[%s] 历史成本校准跳过: 最近 %s 标准费用 %.6f 小于阈值 %.6f",
			historical.group.Name,
			formatHistoryWindow(choice.Window),
			row.StandardCost,
			s.config.MinHistoryCostUSD,
		)
		return
	}
	if historical.suspiciousRates {
		report.markGroup(groupID, reportStatusSkipped)
		s.logger.Printf(
			"[%s] 历史成本校准跳过: 绑定了上游系数非 1 的账号，但账号倍率仍为 1.0000；请先手动确认账号倍率",
			historical.group.Name,
		)
		handled[groupID] = true
		return
	}

	targetRate, ok := historicalTargetRate(row)
	if !ok {
		report.markGroup(groupID, reportStatusSkipped)
		s.logger.Printf("[%s] 历史成本校准跳过: 历史成本包含无效数值", historical.group.Name)
		handled[groupID] = true
		return
	}
	if targetRate <= 0 || math.IsNaN(targetRate) || math.IsInf(targetRate, 0) {
		report.markGroup(groupID, reportStatusSkipped)
		s.logger.Printf("[%s] 历史成本校准跳过: 计算出的分组倍率无效 %.8f", historical.group.Name, targetRate)
		handled[groupID] = true
		return
	}

	s.publishHistoricalRate(ctx, groupID, historical.group, choice, targetRate, handled, report)
}

func historicalTargetRate(row GroupUsageStats) (float64, bool) {
	if row.AccountCost < 0 || math.IsNaN(row.AccountCost) || math.IsInf(row.AccountCost, 0) ||
		math.IsNaN(row.StandardCost) || math.IsInf(row.StandardCost, 0) {
		return 0, false
	}
	return round4(row.AccountCost / row.StandardCost), true
}

func (s *Syncer) publishHistoricalRate(
	ctx context.Context,
	groupID int64,
	group *sub2APIGroup,
	choice adaptiveUsageChoice,
	targetRate float64,
	handled map[int64]bool,
	report *syncReport,
) {
	currentRate := group.RateMultiplier
	row := choice.Stats
	window := formatHistoryWindow(choice.Window)
	if !rateChangeSignificant(currentRate, targetRate) {
		report.markGroup(groupID, reportStatusStable)
		s.logger.Printf(
			"[%s] 历史成本校准稳定: 窗口=%s 请求=%d 标准=%.6f 账号成本=%.6f 倍率=%.4f",
			group.Name, window, row.Requests, row.StandardCost, row.AccountCost, targetRate,
		)
		handled[groupID] = true
		return
	}
	if s.config.DryRun {
		report.markGroup(groupID, reportStatusPreview)
		s.logger.Printf(
			"[%s] dry-run 历史成本校准: 窗口=%s 请求=%d 标准=%.6f 账号成本=%.6f，当前 %.4f -> %.4f",
			group.Name, window, row.Requests, row.StandardCost, row.AccountCost, currentRate, targetRate,
		)
		handled[groupID] = true
		return
	}
	if err := s.updateGroup(ctx, group, targetRate); err != nil {
		report.markGroup(groupID, reportStatusFailed)
		s.logger.Printf("[%s] 历史成本校准更新失败，保持当前倍率 %.4f: %v", group.Name, currentRate, err)
		handled[groupID] = true
		return
	}
	report.updateGroupRate(groupID, targetRate)
	report.markGroup(groupID, reportStatusUpdated)
	s.logger.Printf(
		"[%s] 已按历史成本更新分组: 窗口=%s 请求=%d 标准=%.6f 账号成本=%.6f，当前 %.4f -> %.4f",
		group.Name, window, row.Requests, row.StandardCost, row.AccountCost, currentRate, targetRate,
	)
	handled[groupID] = true
}
