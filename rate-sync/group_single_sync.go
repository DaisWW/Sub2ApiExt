package main

import "context"

func (s *Syncer) syncSingleAccountGroups(ctx context.Context, channels []Channel, report *syncReport) map[int64]bool {
	bindings := buildGroupBindings(channels)
	handled := make(map[int64]bool, len(bindings))
	for groupID, binding := range bindings {
		account, ok := binding.onlyAccount()
		if !ok {
			continue
		}
		if s.syncSingleAccountGroup(ctx, groupID, binding, account, report) {
			handled[groupID] = true
		}
	}
	return handled
}

// syncSingleAccountGroup 返回 true 表示本轮已明确处理，不应再回退到上游探测。
func (s *Syncer) syncSingleAccountGroup(
	ctx context.Context,
	groupID int64,
	binding *groupBinding,
	account Channel,
	report *syncReport,
) bool {
	groupName := binding.group.Name
	accountRate := account.AccountRateMultiplier
	if !validPositiveRate(accountRate) {
		s.logger.Printf(
			"[%s] 单账号倍率无效: 账户 %s(%d) 倍率 %.8f，回退到上游探测",
			groupName,
			account.AccountName,
			account.AccountID,
			accountRate,
		)
		return false
	}
	if s.suspiciousAccountRate(&account) {
		s.logger.Printf(
			"[%s] 单账号倍率仍为 1.0000 且配置了上游折扣，暂不继承账户倍率，回退到上游探测",
			groupName,
		)
		return false
	}

	targetRate := round4(accountRate)
	currentRate := binding.group.RateMultiplier
	if almostEqual(currentRate, targetRate) {
		report.markGroup(groupID, reportStatusStable)
		s.logger.Printf(
			"[%s] 单账号直接使用账户倍率: 账户 %s(%d) 倍率 %.4f，分组已是 %.4f",
			groupName,
			account.AccountName,
			account.AccountID,
			accountRate,
			currentRate,
		)
		return true
	}
	if s.config.DryRun {
		report.markGroup(groupID, reportStatusPreview)
		s.logger.Printf(
			"[%s] dry-run 单账号继承账户倍率: 账户 %s(%d) %.4f，分组 %.4f -> %.4f",
			groupName,
			account.AccountName,
			account.AccountID,
			accountRate,
			currentRate,
			targetRate,
		)
		return true
	}
	if err := s.updateGroup(ctx, &binding.group, targetRate); err != nil {
		report.markGroup(groupID, reportStatusFailed)
		s.logger.Printf(
			"[%s] 单账号继承账户倍率更新失败，保持分组倍率 %.4f: %v",
			groupName,
			currentRate,
			err,
		)
		return true
	}

	report.updateGroupRate(groupID, targetRate)
	report.markGroup(groupID, reportStatusUpdated)
	s.logger.Printf(
		"[%s] 已按单账号倍率更新分组: 账户 %s(%d) %.4f，分组 %.4f -> %.4f",
		groupName,
		account.AccountName,
		account.AccountID,
		accountRate,
		currentRate,
		targetRate,
	)
	return true
}
