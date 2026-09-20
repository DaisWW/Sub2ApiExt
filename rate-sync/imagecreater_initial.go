package main

import (
	"fmt"
)

const historicalImageCreaterBaseKey = "https://image.qzcy3.top/api/v1"

// 历史分组状态只在当前千纸账号集合上重建，账号改名不影响识别。
func isImageCreaterInitialAccount(channel Channel) bool {
	base, err := accountBaseKey(channel.BaseURL)
	if err != nil || base != historicalImageCreaterBaseKey {
		return false
	}
	switch channel.AccountID {
	case 76, 77, 78:
		return true
	default:
		return false
	}
}

func (s *Syncer) initializeImageCreaterRates(channels []Channel) error {
	if s.state.ImageCreaterInitialRatesApplied || s.config.DryRun || s.syncTarget() == "account" {
		return nil
	}
	binding := buildGroupBindings(channels)[55]
	if binding == nil || len(binding.accounts) != 3 {
		return nil
	}
	ids := []int64{76, 77, 78}
	for _, id := range ids {
		if !isImageCreaterInitialAccount(binding.accounts[id]) {
			return nil
		}
	}
	previous := s.state.DynamicGroups[55]
	if !usableDynamicGroupState(previous) {
		return nil
	}
	rates := make(map[int64]float64, len(ids))
	for _, id := range ids {
		rate := binding.accounts[id].AccountRateMultiplier
		if !validPositiveRate(rate) || almostEqual(rate, 1) {
			return nil // 等待手动维护的账户倍率就绪后再初始化分组。
		}
		rates[id] = rate
	}
	fast, fastOK := dynamicMemoryRate(previous.Fast, rates)
	slow, slowOK := dynamicMemoryRate(previous.Slow, rates)
	if !fastOK || !slowOK {
		return fmt.Errorf("千纸分组的临时成本记忆无效")
	}
	// 保留路由权重和水位；旧日志的 1.0 倍率不再作为初始成本估计。
	rebased := *previous
	rebased.Fast.AccountCost = fast * rebased.Fast.Denominator
	rebased.Slow.AccountCost = slow * rebased.Slow.Denominator
	rebased.LastAccountRates = rates
	rebased.PendingTarget = 0.25
	rebased.HasPendingTarget = true
	s.state.DynamicGroups[55] = &rebased
	s.state.ImageCreaterInitialRatesApplied = true
	if err := s.store.Save(s.state); err != nil {
		s.state.ImageCreaterInitialRatesApplied = false
		s.state.DynamicGroups[55] = previous
		return fmt.Errorf("保存千纸临时初值状态: %w", err)
	}
	return nil
}
