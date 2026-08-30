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
		s.syncSingleAccountGroup(ctx, groupID, binding, account, report)
		handled[groupID] = true
	}
	return handled
}

// syncSingleAccountGroup 处理单账号分组；无论账户倍率是否有效，本轮都不进入上游探测。
func (s *Syncer) syncSingleAccountGroup(
	ctx context.Context,
	groupID int64,
	binding *groupBinding,
	account Channel,
	report *syncReport,
) {
	groupName := binding.group.Name
	accountRate := account.AccountRateMultiplier
	if !validPositiveRate(accountRate) {
		report.markGroup(groupID, reportStatusSkipped)
		report.setGroupEvidence(groupID, "", "等待账户倍率同步")
		s.logger.Printf(
			"[%s] 单账号分组跳过: 账户 %s(%d) 倍率 %.8f 无效，等待账户 worker 同步",
			groupName,
			account.AccountName,
			account.AccountID,
			accountRate,
		)
		return
	}

	targetRate := round4(accountRate)
	if !validPositiveRate(targetRate) {
		report.markGroup(groupID, reportStatusSkipped)
		report.setGroupEvidence(groupID, "", "账户倍率四舍五入后无效")
		s.logger.Printf(
			"[%s] 单账号分组跳过: 账户 %s(%d) 倍率 %.8f 四舍五入后无效，等待账户 worker 同步",
			groupName,
			account.AccountName,
			account.AccountID,
			accountRate,
		)
		return
	}
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
		return
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
		return
	}
	if err := s.updateGroup(ctx, &binding.group, targetRate); err != nil {
		report.markGroup(groupID, reportStatusFailed)
		s.logger.Printf(
			"[%s] 单账号继承账户倍率更新失败，保持分组倍率 %.4f: %v",
			groupName,
			currentRate,
			err,
		)
		return
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
}
