package main

import (
	"context"
	"fmt"
)

func (s *Syncer) publishDynamicGroup(
	ctx context.Context,
	group *sub2APIGroup,
	state *DynamicGroupState,
	event bool,
	suspicious bool,
	reason string,
	summary dynamicUsageSummary,
	report *syncReport,
) {
	fast, slow, ok := dynamicRates(state)
	if !ok {
		skipDynamicGroup(report, group, state, "记忆状态无效，等待重新初始化")
		s.logger.Printf("[%s] 动态成本跳过: 记忆状态无效，保持 %.4f", group.Name, group.RateMultiplier)
		return
	}
	rawTarget := dynamicRawTarget(fast, slow)
	if !validPositiveRate(rawTarget) {
		skipDynamicGroup(report, group, state, "计算结果无效")
		return
	}
	target := dynamicTarget(group, state, rawTarget, event)
	detail := fmt.Sprintf("新增=$%.4f F=%.4f S=%.4f 目标=%.4f", summary.standardCost, fast, slow, target)
	report.setGroupEvidence(group.ID, reason, detail)
	if suspicious {
		clearDynamicPending(state)
		report.markGroup(group.ID, reportStatusSkipped)
		s.logger.Printf("[%s] 动态成本暂停: 账号倍率仍是待确认的 1.0000；F=%.4f S=%.4f", group.Name, fast, slow)
		return
	}
	if !event && !state.HasPendingTarget {
		report.markGroup(group.ID, reportStatusStable)
		s.logger.Printf("[%s] 动态成本冻结: 无新增用量，F=%.4f S=%.4f 分组=%.4f", group.Name, fast, slow, group.RateMultiplier)
		return
	}
	if !state.HasPendingTarget || !dynamicGroupRateChangeSignificant(group.RateMultiplier, target) {
		clearDynamicPending(state)
		report.markGroup(group.ID, reportStatusStable)
		s.logger.Printf(
			"[%s] 动态成本稳定: %s 请求=%d 标准=%.6f F=%.4f S=%.4f raw=%.4f 分组=%.4f",
			group.Name, reason, summary.requests, summary.standardCost, fast, slow, rawTarget, group.RateMultiplier,
		)
		return
	}
	if s.config.DryRun {
		clearDynamicPending(state)
		report.markGroup(group.ID, reportStatusPreview)
		s.logger.Printf(
			"[%s] dry-run 动态成本: %s 请求=%d 标准=%.6f F=%.4f S=%.4f raw=%.4f，当前 %.4f -> %.4f",
			group.Name, reason, summary.requests, summary.standardCost, fast, slow, rawTarget, group.RateMultiplier, target,
		)
		return
	}
	s.publishDynamicChange(ctx, group, state, target, fast, slow, rawTarget, reason, summary, report)
}

func dynamicRates(state *DynamicGroupState) (float64, float64, bool) {
	if state == nil {
		return 0, 0, false
	}
	fast, fastOK := dynamicMemoryRate(state.Fast, state.LastAccountRates)
	slow, slowOK := dynamicMemoryRate(state.Slow, state.LastAccountRates)
	return fast, slow, fastOK && slowOK
}

func skipDynamicGroup(report *syncReport, group *sub2APIGroup, state *DynamicGroupState, detail string) {
	clearDynamicPending(state)
	report.markGroup(group.ID, reportStatusSkipped)
	report.setGroupEvidence(group.ID, "动态", detail)
}

func dynamicTarget(group *sub2APIGroup, state *DynamicGroupState, rawTarget float64, event bool) float64 {
	target := group.RateMultiplier
	if state.HasPendingTarget {
		target = state.PendingTarget
	}
	if event {
		target = dynamicPublishedTarget(group.RateMultiplier, rawTarget)
		state.PendingTarget = target
		state.HasPendingTarget = dynamicGroupRateChangeSignificant(group.RateMultiplier, target)
	}
	return target
}

func (s *Syncer) publishDynamicChange(
	ctx context.Context,
	group *sub2APIGroup,
	state *DynamicGroupState,
	target, fast, slow, rawTarget float64,
	reason string,
	summary dynamicUsageSummary,
	report *syncReport,
) {
	if err := s.updateGroup(ctx, group, target); err != nil {
		report.markGroup(group.ID, reportStatusFailed)
		s.logger.Printf("[%s] 动态成本更新失败，稍后重试 %.4f: %v", group.Name, target, err)
		return
	}
	clearDynamicPending(state)
	report.updateGroupRate(group.ID, target)
	report.markGroup(group.ID, reportStatusUpdated)
	s.logger.Printf(
		"[%s] 已按动态成本更新分组: %s 请求=%d 标准=%.6f F=%.4f S=%.4f raw=%.4f，当前 %.4f -> %.4f",
		group.Name, reason, summary.requests, summary.standardCost, fast, slow, rawTarget, group.RateMultiplier, target,
	)
}
