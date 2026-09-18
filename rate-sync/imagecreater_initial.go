package main

import (
	"context"
	"fmt"
)

// 用户确认的临时上游初值仅用于当前千纸账号，账号改名不影响一次性初始化。
func imageCreaterInitialRate(channel Channel) float64 {
	base, err := imageCreaterBaseKey(channel.BaseURL)
	if err != nil || base != "https://image.qzcy3.top/api/v1" {
		return 0
	}
	switch channel.AccountID {
	case 76, 78:
		return 0.25
	case 77:
		return 0.18
	default:
		return 0
	}
}

func (s *Syncer) initializeImageCreaterRates(ctx context.Context, channels []Channel, report *syncReport) error {
	if s.state.ImageCreaterInitialRatesApplied || s.config.DryRun {
		return nil
	}
	binding := buildGroupBindings(channels)[55]
	if binding == nil || len(binding.accounts) != 3 {
		return nil
	}
	ids := []int64{76, 77, 78}
	for _, id := range ids {
		if imageCreaterInitialRate(binding.accounts[id]) == 0 {
			return nil
		}
	}
	previous := s.state.DynamicGroups[55]
	if s.syncTarget() == "account" {
		for _, id := range ids {
			channel := binding.accounts[id]
			if !almostEqual(channel.AccountRateMultiplier, 1) {
				continue
			}
			discount, _, err := s.config.rechargeDiscountForBaseURL(channel.BaseURL)
			if err != nil {
				return err
			}
			rate := round4(imageCreaterInitialRate(channel) * discount)
			if !validPositiveRate(rate) {
				return fmt.Errorf("千纸账号 %d 的临时初值无效", id)
			}
			if err := s.updateAccount(ctx, id, rate); err != nil {
				return fmt.Errorf("预置千纸账号 %d 倍率: %w", id, err)
			}
			for i := range channels {
				if channels[i].AccountID == id {
					channels[i].AccountRateMultiplier = rate
				}
			}
			report.updateAccountRate(id, rate)
			s.logger.Printf("[%s] 已按临时上游初值 %.4f × 充值折扣 %.4f 预置账户倍率 %.4f，等待真实余额对账接管",
				channelLabel(&channel), imageCreaterInitialRate(channel), discount, rate)
		}
	} else {
		if !usableDynamicGroupState(previous) {
			return nil
		}
		rates := make(map[int64]float64, len(ids))
		for _, id := range ids {
			rate := binding.accounts[id].AccountRateMultiplier
			if !validPositiveRate(rate) || almostEqual(rate, 1) {
				return nil // 账户 worker 完成初值写入后再初始化分组。
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
	}
	s.state.ImageCreaterInitialRatesApplied = true
	if err := s.store.Save(s.state); err != nil {
		s.state.ImageCreaterInitialRatesApplied = false
		if s.syncTarget() == "group" {
			s.state.DynamicGroups[55] = previous
		}
		return fmt.Errorf("保存千纸临时初值状态: %w", err)
	}
	return nil
}
