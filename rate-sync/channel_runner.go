package main

import (
	"context"
	"errors"
	"time"
)

const maxConcurrentChecks = 6

type channelCheckResult struct {
	channel *Channel
	err     error
}

type syncStats struct {
	checked int
	normal  int
	skipped int
	failed  int
}

// channelCheckPlan 保存本轮渠道检查的准入条件和去重状态。
type channelCheckPlan struct {
	target           string
	handledGroups    map[int64]bool
	groupAccountNums map[int64]int
	accountGroupNums map[int64]int
	loggedGroups     map[int64]bool
	loggedAccounts   map[int64]bool
	seenAccounts     map[int64]bool
}

func newChannelCheckPlan(target string, channels []Channel, handledGroups map[int64]bool) *channelCheckPlan {
	plan := &channelCheckPlan{
		target:           target,
		handledGroups:    handledGroups,
		groupAccountNums: make(map[int64]int, len(channels)),
		accountGroupNums: make(map[int64]int, len(channels)),
		loggedGroups:     make(map[int64]bool),
		loggedAccounts:   make(map[int64]bool),
		seenAccounts:     make(map[int64]bool),
	}
	for groupID, binding := range buildGroupBindings(channels) {
		plan.groupAccountNums[groupID] = len(binding.accounts)
	}
	for _, channel := range channels {
		plan.accountGroupNums[channel.AccountID]++
	}
	return plan
}

// admit 判断渠道是否应进入上游检查，并同步记录可解释的跳过原因。
func (p *channelCheckPlan) admit(s *Syncer, channel *Channel, report *syncReport, stats *syncStats) bool {
	if p.target == "group" && p.handledGroups[channel.Group.ID] {
		return false
	}
	if p.target == "account" && !p.admitAccountHost(s, channel, report, stats) {
		return false
	}
	if p.target == "group" && p.groupAccountNums[channel.Group.ID] > 1 {
		p.skipMultiAccountGroup(s, channel, report, stats)
		return false
	}
	if p.target == "account" && !p.admitUniqueAccount(s, channel) {
		return false
	}
	return true
}

func (p *channelCheckPlan) admitAccountHost(s *Syncer, channel *Channel, report *syncReport, stats *syncStats) bool {
	// 账号已由前一个有效渠道准入时，后续绑定不应再次校验或覆盖账号级折扣。
	// 首个渠道无效时不会设置 seenAccounts，后续有效渠道仍可接替检查。
	if p.seenAccounts[channel.AccountID] {
		return true
	}
	discount, _, err := s.config.rechargeDiscountForBaseURL(channel.BaseURL)
	if err != nil {
		stats.failed++
		report.markChannel(channel, reportStatusFailed)
		s.logger.Printf("[%s] 同步失败: %v", channelLabel(channel), err)
		return false
	}
	report.setAccountRechargeDiscount(channel.AccountID, discount)
	return true
}

func (p *channelCheckPlan) skipMultiAccountGroup(s *Syncer, channel *Channel, report *syncReport, stats *syncStats) {
	groupID := channel.Group.ID
	if p.loggedGroups[groupID] {
		return
	}
	p.loggedGroups[groupID] = true
	stats.skipped++
	report.markGroup(groupID, reportStatusSkipped)
	// 多账号分组不直接探测上游价格，避免把某一个账号的价格误用于整个分组。
	s.logger.Printf(
		"[%s] 暂不自动: 分组同时绑定了 %d 个可用账号，无法安全确定上游价格",
		channel.Group.Name,
		p.groupAccountNums[groupID],
	)
}

func (p *channelCheckPlan) admitUniqueAccount(s *Syncer, channel *Channel) bool {
	accountID := channel.AccountID
	if p.seenAccounts[accountID] {
		return false
	}
	p.seenAccounts[accountID] = true
	if p.accountGroupNums[accountID] > 1 && !p.loggedAccounts[accountID] {
		p.loggedAccounts[accountID] = true
		s.logger.Printf(
			"[%s] 账号绑定了 %d 个可用分组，账号模式仅检查一次",
			channel.AccountName,
			p.accountGroupNums[accountID],
		)
	}
	return true
}

func (s *Syncer) runChannelChecks(
	ctx context.Context,
	channels []Channel,
	now time.Time,
	handledGroups map[int64]bool,
	report *syncReport,
) syncStats {
	plan := newChannelCheckPlan(s.syncTarget(), channels, handledGroups)
	results := make(chan channelCheckResult, len(channels))
	semaphore := make(chan struct{}, maxConcurrentChecks)
	stats := syncStats{}
	for i := range channels {
		channel := &channels[i]
		if !plan.admit(s, channel, report, &stats) {
			continue
		}
		if !acquireCheckSlot(ctx, semaphore) {
			return s.collectCheckResults(results, stats, report)
		}
		ruleState := s.ruleStateFor(channel, plan.target)
		stats.checked++
		go func(channel *Channel, ruleState *RuleState) {
			defer func() { <-semaphore }()
			results <- channelCheckResult{channel: channel, err: s.syncChannel(ctx, channel, ruleState, now, report)}
		}(channel, ruleState)
	}
	return s.collectCheckResults(results, stats, report)
}

func acquireCheckSlot(ctx context.Context, semaphore chan<- struct{}) bool {
	if err := ctx.Err(); err != nil {
		return false
	}
	select {
	case semaphore <- struct{}{}:
		return true
	case <-ctx.Done():
		return false
	}
}

func (s *Syncer) ruleStateFor(channel *Channel, target string) *RuleState {
	if s.state.Rules == nil {
		s.state.Rules = make(map[string]*RuleState)
	}
	stateKey := channelStateKeyForTarget(channel, target)
	identity := channelIdentityForTarget(channel, target)
	ruleState, exists := s.state.Rules[stateKey]
	if exists && ruleState.Identity == identity {
		return ruleState
	}
	ruleState = &RuleState{Identity: identity}
	s.state.Rules[stateKey] = ruleState
	s.logger.Printf("[%s] 新渠道或上游凭据已变化，将重新识别模板并建立基线", channelLabel(channel))
	return ruleState
}

func (s *Syncer) collectCheckResults(results <-chan channelCheckResult, stats syncStats, report *syncReport) syncStats {
	for range stats.checked {
		result := <-results
		if result.err == nil {
			stats.normal++
			report.markChannel(result.channel, reportStatusChecked)
			continue
		}
		var skipped skipError
		if errors.As(result.err, &skipped) {
			stats.skipped++
			report.markChannel(result.channel, reportStatusSkipped)
			s.logger.Printf("[%s] 暂不自动: %v", channelLabel(result.channel), result.err)
			continue
		}
		stats.failed++
		report.markChannel(result.channel, reportStatusFailed)
		s.logger.Printf("[%s] 同步失败: %v", channelLabel(result.channel), result.err)
	}
	return stats
}
