package main

import (
	"context"
	"fmt"
	"sort"
	"time"
)

const (
	dynamicFastBudgetUSD        = 5.0
	dynamicSlowBudgetUSD        = 20.0
	dynamicBootstrapWindow      = 6 * time.Hour
	dynamicBootstrapMaxWindow   = 24 * time.Hour
	dynamicBootstrapWindowLabel = "1h/6h/24h"
	dynamicRiseStepLimit        = 0.020
	dynamicFallStepLimit        = 0.020
	dynamicAbsoluteDeadband     = 0.0002
	dynamicRelativeDeadband     = 0.0025
)

var dynamicBootstrapWindowOrder = [...]time.Duration{
	time.Hour,
	dynamicBootstrapWindow,
	dynamicBootstrapMaxWindow,
}

type dynamicBootstrapChoice struct {
	rows   []GroupUsageAccountStats
	window time.Duration
}

type dynamicUsageSummary struct {
	requests     int64
	standardCost float64
}

type dynamicGroupPlan struct {
	bindings map[int64]*groupBinding
	groupIDs []int64
}

func (s *Syncer) syncDynamicGroups(
	ctx context.Context,
	source groupUsageIncrementalSource,
	channels []Channel,
	now time.Time,
	initialHandled map[int64]bool,
	report *syncReport,
) (map[int64]bool, error) {
	handled := ensureHandledGroups(initialHandled)
	plan := buildDynamicGroupPlan(channels, handled)
	if len(plan.groupIDs) == 0 {
		return handled, nil
	}
	snapshotID, err := source.LatestGroupUsageID(ctx)
	if err != nil {
		return handled, err
	}
	watermarks, uninitialized := s.prepareDynamicStates(plan.groupIDs, snapshotID)
	choices, insufficient, err := loadDynamicBootstrap(ctx, source, now, snapshotID, uninitialized)
	if err != nil {
		return handled, err
	}
	s.reportDynamicBootstrap(ctx, plan, choices, insufficient, snapshotID, report)
	if err := s.consumeDynamicUsage(ctx, source, plan, watermarks, snapshotID, report); err != nil {
		return handled, err
	}
	return handled, nil
}

func buildDynamicGroupPlan(channels []Channel, handled map[int64]bool) dynamicGroupPlan {
	plan := dynamicGroupPlan{
		bindings: buildGroupBindings(channels),
	}
	for groupID, binding := range plan.bindings {
		if len(binding.accounts) < 2 {
			continue
		}
		plan.groupIDs = append(plan.groupIDs, groupID)
		handled[groupID] = true
	}
	sort.Slice(plan.groupIDs, func(i, j int) bool { return plan.groupIDs[i] < plan.groupIDs[j] })
	return plan
}

func (s *Syncer) prepareDynamicStates(groupIDs []int64, snapshotID int64) (map[int64]int64, []int64) {
	if s.state.DynamicGroups == nil {
		s.state.DynamicGroups = make(map[int64]*DynamicGroupState)
	}
	watermarks := make(map[int64]int64)
	uninitialized := make([]int64, 0)
	for _, groupID := range groupIDs {
		state := s.state.DynamicGroups[groupID]
		if !usableDynamicGroupState(state) || state.LastUsageID > snapshotID {
			pendingTarget, hasPendingTarget := preservedPendingTarget(state)
			state = newDynamicGroupState()
			if hasPendingTarget {
				state.PendingTarget = pendingTarget
				state.HasPendingTarget = true
			}
			s.state.DynamicGroups[groupID] = state
		}
		if state.Initialized {
			watermarks[groupID] = state.LastUsageID
			continue
		}
		uninitialized = append(uninitialized, groupID)
	}
	return watermarks, uninitialized
}

func (s *Syncer) reportDynamicBootstrap(
	ctx context.Context,
	plan dynamicGroupPlan,
	choices map[int64]dynamicBootstrapChoice,
	insufficient []int64,
	snapshotID int64,
	report *syncReport,
) {
	for _, groupID := range insufficient {
		state := s.state.DynamicGroups[groupID]
		if state != nil && state.HasPendingTarget {
			s.publishDynamicGroup(
				ctx,
				&plan.bindings[groupID].group,
				state,
				false,
				"待发布重试",
				dynamicUsageSummary{},
				report,
			)
			continue
		}
		group := plan.bindings[groupID].group
		report.markGroup(groupID, reportStatusSkipped)
		report.setGroupEvidence(groupID, "初始化", fmt.Sprintf("%s 样本不足（请求>=30 且标准费用>=5 美元），保持当前倍率", dynamicBootstrapWindowLabel))
		s.logger.Printf("[%s] 动态成本初始化等待: %s 请求或标准费用不足，保持 %.4f", group.Name, dynamicBootstrapWindowLabel, group.RateMultiplier)
	}
	groupIDs := make([]int64, 0, len(choices))
	for groupID := range choices {
		groupIDs = append(groupIDs, groupID)
	}
	sort.Slice(groupIDs, func(i, j int) bool { return groupIDs[i] < groupIDs[j] })
	for _, groupID := range groupIDs {
		choice := choices[groupID]
		previous := s.state.DynamicGroups[groupID]
		state := seedDynamicGroupState(choice.rows, snapshotID)
		group := plan.bindings[groupID].group
		if state == nil {
			if previous != nil && previous.HasPendingTarget {
				s.publishDynamicGroup(
					ctx,
					&plan.bindings[groupID].group,
					previous,
					false,
					"待发布重试",
					dynamicUsageSummary{},
					report,
				)
				continue
			}
			report.markGroup(groupID, reportStatusSkipped)
			report.setGroupEvidence(groupID, "初始化", "历史账号成本无效，保持当前倍率")
			s.logger.Printf("[%s] 动态成本初始化跳过: 历史账号成本无效，保持 %.4f", group.Name, group.RateMultiplier)
			continue
		}
		if pendingTarget, hasPendingTarget := preservedPendingTarget(previous); hasPendingTarget {
			state.PendingTarget = pendingTarget
			state.HasPendingTarget = true
		}
		overlayBindingRates(state.LastAccountRates, plan.bindings[groupID])
		s.state.DynamicGroups[groupID] = state
		s.publishDynamicGroup(
			ctx,
			&plan.bindings[groupID].group,
			state,
			true,
			fmt.Sprintf("初始化%s", formatHistoryWindow(choice.window)),
			summarizeDynamicUsage(choice.rows),
			report,
		)
	}
}

func preservedPendingTarget(state *DynamicGroupState) (float64, bool) {
	if state == nil || !state.HasPendingTarget || !validPositiveRate(state.PendingTarget) {
		return 0, false
	}
	return state.PendingTarget, true
}

func (s *Syncer) consumeDynamicUsage(
	ctx context.Context,
	source groupUsageIncrementalSource,
	plan dynamicGroupPlan,
	watermarks map[int64]int64,
	snapshotID int64,
	report *syncReport,
) error {
	if len(watermarks) == 0 {
		return nil
	}
	readyWatermarks := make(map[int64]int64, len(watermarks))
	pendingGroupIDs := make([]int64, 0)
	for groupID, watermark := range watermarks {
		state := s.state.DynamicGroups[groupID]
		if state != nil && state.HasPendingTarget {
			pendingGroupIDs = append(pendingGroupIDs, groupID)
			continue
		}
		readyWatermarks[groupID] = watermark
	}
	sort.Slice(pendingGroupIDs, func(i, j int) bool { return pendingGroupIDs[i] < pendingGroupIDs[j] })
	for _, groupID := range pendingGroupIDs {
		state := s.state.DynamicGroups[groupID]
		s.publishDynamicGroup(
			ctx,
			&plan.bindings[groupID].group,
			state,
			false,
			"待发布重试",
			dynamicUsageSummary{},
			report,
		)
	}
	if len(readyWatermarks) == 0 {
		return nil
	}
	rows, err := source.ListGroupUsageSince(ctx, readyWatermarks, snapshotID)
	if err != nil {
		return err
	}
	usageByGroup := groupDynamicUsage(rows)
	groupIDs := make([]int64, 0, len(readyWatermarks))
	for groupID := range readyWatermarks {
		groupIDs = append(groupIDs, groupID)
	}
	sort.Slice(groupIDs, func(i, j int) bool { return groupIDs[i] < groupIDs[j] })
	for _, groupID := range groupIDs {
		s.consumeDynamicGroup(ctx, plan, groupID, usageByGroup[groupID], snapshotID, report)
	}
	return nil
}

func (s *Syncer) consumeDynamicGroup(
	ctx context.Context,
	plan dynamicGroupPlan,
	groupID int64,
	rows []GroupUsageAccountStats,
	snapshotID int64,
	report *syncReport,
) {
	state := s.state.DynamicGroups[groupID]
	summary := summarizeDynamicUsage(rows)
	newUsage := summary.standardCost > 0
	if newUsage {
		updateDynamicMemory(&state.Fast, dynamicFastBudgetUSD, rows)
		updateDynamicMemory(&state.Slow, dynamicSlowBudgetUSD, rows)
	}
	newRates := cloneAccountRates(state.LastAccountRates)
	for _, row := range rows {
		if validPositiveRate(row.CurrentAccountRate) {
			newRates[row.AccountID] = row.CurrentAccountRate
		}
	}
	overlayBindingRates(newRates, plan.bindings[groupID])
	ratesChanged := relevantAccountRatesChanged(state, newRates)
	state.LastAccountRates = newRates
	state.LastUsageID = snapshotID
	reason := dynamicUsageReason(newUsage, ratesChanged)
	s.publishDynamicGroup(
		ctx,
		&plan.bindings[groupID].group,
		state,
		newUsage || ratesChanged,
		reason,
		summary,
		report,
	)
}

func dynamicUsageReason(newUsage, ratesChanged bool) string {
	if newUsage {
		return "新增请求"
	}
	if ratesChanged {
		return "账号倍率变更"
	}
	return "无新增，冻结"
}
