package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	responseLimit       = 1024 * 1024
	maxConcurrentChecks = 6
	templateUsageRatio  = "sub2api_usage"
)

type Syncer struct {
	config            *Config
	source            ChannelSource
	client            *http.Client
	store             StateStore
	state             *State
	logger            *log.Logger
	upstreamClientsMu sync.Mutex
	upstreamClients   map[string]*http.Client
}

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

type upstreamToday struct {
	Cost       float64 `json:"cost"`
	ActualCost float64 `json:"actual_cost"`
}

type upstreamResponse struct {
	Usage *struct {
		Today *upstreamToday `json:"today"`
	} `json:"usage"`
}

type groupUpdate struct {
	RateMultiplier  float64 `json:"rate_multiplier"`
	DailyLimitUSD   float64 `json:"daily_limit_usd"`
	WeeklyLimitUSD  float64 `json:"weekly_limit_usd"`
	MonthlyLimitUSD float64 `json:"monthly_limit_usd"`
}

type accountUpdate struct {
	RateMultiplier float64 `json:"rate_multiplier"`
}

type skipError string

func (e skipError) Error() string {
	return string(e)
}

func NewSyncer(config *Config, source ChannelSource, client *http.Client, store StateStore, state *State, logger *log.Logger) *Syncer {
	return &Syncer{
		config:          config,
		source:          source,
		client:          client,
		store:           store,
		state:           state,
		logger:          logger,
		upstreamClients: make(map[string]*http.Client),
	}
}

func (s *Syncer) RunOnce(ctx context.Context, now time.Time) error {
	startedAt := time.Now()
	if source, ok := s.source.(adminAPIKeySource); ok {
		adminAPIKey, err := source.AdminAPIKey(ctx)
		if err != nil {
			return err
		}
		s.config.AdminAPIKey = adminAPIKey
	}
	if s.config.AdminAPIKey == "" {
		s.logger.Printf("Admin API Key 尚未配置，本轮等待；配置后将自动开始同步")
		return nil
	}

	s.logger.Printf("开始自动发现并同步价格")
	channels, err := s.source.List(ctx)
	if err != nil {
		return fmt.Errorf("自动发现渠道: %w", err)
	}
	report := newSyncReport(s.syncTarget(), channels)
	handledGroups := map[int64]bool{}
	if s.syncTarget() == "group" {
		handledGroups = s.syncSingleAccountGroups(ctx, channels, report)
		var usageErr error
		handledGroups, usageErr = s.syncGroupsFromUsage(ctx, channels, now, handledGroups, report)
		if usageErr != nil {
			s.logger.Printf("分组历史成本校准失败，本轮回退到上游探测: %v", usageErr)
		}
	}
	stats := s.checkChannels(ctx, channels, now, handledGroups, report)

	if err := s.store.Save(s.state); err != nil {
		return err
	}
	s.logger.Printf(
		"同步检查完成: 可用绑定=%d 已检查=%d 检查正常=%d 暂不自动=%d 失败=%d 耗时=%s",
		len(channels),
		stats.checked,
		stats.normal,
		stats.skipped,
		stats.failed,
		time.Since(startedAt).Round(time.Millisecond),
	)
	s.logSyncReport(report)
	return nil
}

func (s *Syncer) checkChannels(ctx context.Context, channels []Channel, now time.Time, handledGroups map[int64]bool, report *syncReport) syncStats {
	target := s.syncTarget()
	groupBindings := make(map[int64]int, len(channels))
	accountBindings := make(map[int64]int, len(channels))
	for groupID, binding := range buildGroupBindings(channels) {
		groupBindings[groupID] = len(binding.accounts)
	}
	for i := range channels {
		accountBindings[channels[i].AccountID]++
	}

	stats := syncStats{}
	duplicateLogged := make(map[int64]bool)
	seenAccounts := make(map[int64]bool)
	skippedAccounts := make(map[int64]bool)
	// 全量缓冲避免调度阶段因已完成任务回传结果而阻塞。
	results := make(chan channelCheckResult, len(channels))
	semaphore := make(chan struct{}, maxConcurrentChecks)
	for i := range channels {
		channel := &channels[i]
		if target == "group" && handledGroups[channel.Group.ID] {
			continue
		}
		if target == "account" && len(s.config.SyncHosts) > 0 {
			_, host, err := s.config.factorForBaseURL(channel.BaseURL)
			if err != nil {
				stats.failed++
				report.markChannel(channel, reportStatusFailed)
				s.logger.Printf("[%s] 同步失败: %v", channelLabel(channel), err)
				continue
			}
			if _, allowed := s.config.SyncHosts[host]; !allowed {
				if !skippedAccounts[channel.AccountID] {
					skippedAccounts[channel.AccountID] = true
					stats.skipped++
					report.markChannel(channel, reportStatusSkipped)
					s.logger.Printf(
						"[%s] 暂不自动: 账户模式白名单未包含上游主机 %s",
						channelLabel(channel),
						host,
					)
				}
				continue
			}
		}
		if target == "group" && groupBindings[channel.Group.ID] > 1 {
			if !duplicateLogged[channel.Group.ID] {
				duplicateLogged[channel.Group.ID] = true
				stats.skipped++
				report.markGroup(channel.Group.ID, reportStatusSkipped)
				s.logger.Printf(
					"[%s] 暂不自动: 分组同时绑定了 %d 个可用账号，无法安全确定上游价格",
					channel.Group.Name,
					groupBindings[channel.Group.ID],
				)
			}
			continue
		}
		if target == "account" {
			if seenAccounts[channel.AccountID] {
				continue
			}
			seenAccounts[channel.AccountID] = true
			if accountBindings[channel.AccountID] > 1 && !duplicateLogged[channel.AccountID] {
				duplicateLogged[channel.AccountID] = true
				s.logger.Printf(
					"[%s] 账号绑定了 %d 个可用分组，账号模式仅检查一次",
					channel.AccountName,
					accountBindings[channel.AccountID],
				)
			}
		}

		stateKey := channelStateKeyForTarget(channel, target)
		identity := channelIdentityForTarget(channel, target)
		ruleState, exists := s.state.Rules[stateKey]
		if !exists || ruleState.Identity != identity {
			ruleState = &RuleState{Identity: identity}
			s.state.Rules[stateKey] = ruleState
			s.logger.Printf("[%s] 新渠道或上游凭据已变化，将重新识别模板并建立基线", channelLabel(channel))
		}

		stats.checked++
		semaphore <- struct{}{}
		go func(channel *Channel, ruleState *RuleState) {
			defer func() { <-semaphore }()
			results <- channelCheckResult{channel: channel, err: s.syncChannel(ctx, channel, ruleState, now, report)}
		}(channel, ruleState)
	}
	return s.collectChannelResults(results, stats, report)
}

type groupBinding struct {
	group    sub2APIGroup
	accounts map[int64]Channel
}

func buildGroupBindings(channels []Channel) map[int64]*groupBinding {
	bindings := make(map[int64]*groupBinding, len(channels))
	for i := range channels {
		channel := channels[i]
		binding := bindings[channel.Group.ID]
		if binding == nil {
			binding = &groupBinding{
				group:    channel.Group,
				accounts: make(map[int64]Channel),
			}
			bindings[channel.Group.ID] = binding
		}
		binding.accounts[channel.AccountID] = channel
	}
	return bindings
}

func (s *Syncer) syncSingleAccountGroups(ctx context.Context, channels []Channel, report *syncReport) map[int64]bool {
	bindings := buildGroupBindings(channels)
	handled := make(map[int64]bool, len(bindings))
	for groupID, binding := range bindings {
		if len(binding.accounts) != 1 {
			continue
		}
		var account Channel
		for _, candidate := range binding.accounts {
			account = candidate
		}
		name := binding.group.Name
		accountRate := account.AccountRateMultiplier
		if accountRate <= 0 || math.IsNaN(accountRate) || math.IsInf(accountRate, 0) {
			s.logger.Printf(
				"[%s] 单账号倍率无效: 账户 %s(%d) 倍率 %.8f，回退到上游探测",
				name,
				account.AccountName,
				account.AccountID,
				accountRate,
			)
			continue
		}
		if s.suspiciousAccountRate(&account) {
			s.logger.Printf(
				"[%s] 单账号倍率仍为 1.0000 且配置了上游折扣，暂不继承账户倍率，回退到上游探测",
				name,
			)
			continue
		}

		targetRate := round4(accountRate)
		currentRate := binding.group.RateMultiplier
		if almostEqual(currentRate, targetRate) {
			report.markGroup(groupID, reportStatusStable)
			s.logger.Printf(
				"[%s] 单账号直接使用账户倍率: 账户 %s(%d) 倍率 %.4f，分组已是 %.4f",
				name,
				account.AccountName,
				account.AccountID,
				accountRate,
				currentRate,
			)
			handled[groupID] = true
			continue
		}
		if s.config.DryRun {
			report.markGroup(groupID, reportStatusPreview)
			s.logger.Printf(
				"[%s] dry-run 单账号继承账户倍率: 账户 %s(%d) %.4f，分组 %.4f -> %.4f",
				name,
				account.AccountName,
				account.AccountID,
				accountRate,
				currentRate,
				targetRate,
			)
			handled[groupID] = true
			continue
		}
		if err := s.updateGroup(ctx, &binding.group, targetRate); err != nil {
			report.markGroup(groupID, reportStatusFailed)
			s.logger.Printf(
				"[%s] 单账号继承账户倍率更新失败，保持分组倍率 %.4f: %v",
				name,
				currentRate,
				err,
			)
			handled[groupID] = true
			continue
		}
		report.updateGroupRate(groupID, targetRate)
		report.markGroup(groupID, reportStatusUpdated)
		s.logger.Printf(
			"[%s] 已按单账号倍率更新分组: 账户 %s(%d) %.4f，分组 %.4f -> %.4f",
			name,
			account.AccountName,
			account.AccountID,
			accountRate,
			currentRate,
			targetRate,
		)
		handled[groupID] = true
	}
	return handled
}

func (s *Syncer) syncGroupsFromUsage(ctx context.Context, channels []Channel, now time.Time, initialHandled map[int64]bool, report *syncReport) (map[int64]bool, error) {
	handled := initialHandled
	if handled == nil {
		handled = make(map[int64]bool)
	}
	if _, ok := s.source.(groupUsageWindowSource); !ok {
		if _, ok := s.source.(groupUsageSource); !ok {
			return handled, nil
		}
	}
	windows := adaptiveHistoryWindows(s.config.HistoryWindow)
	usageByWindow, err := s.loadGroupUsageByWindows(ctx, now, windows)
	if err != nil {
		return handled, err
	}
	if len(usageByWindow) == 0 {
		return handled, nil
	}
	// A group is evaluated independently so busy groups can use a short,
	// responsive window while quiet groups keep accumulating evidence.

	bindings := buildGroupBindings(channels)
	groups := make(map[int64]*sub2APIGroup, len(bindings))
	singleAccountGroups := make(map[int64]bool, len(bindings))
	suspicious := make(map[int64]bool, len(channels))
	for groupID, binding := range bindings {
		group := binding.group
		groups[groupID] = &group
		if len(binding.accounts) == 1 {
			singleAccountGroups[groupID] = true
		}
		for _, channel := range binding.accounts {
			if s.suspiciousAccountRate(&channel) {
				suspicious[groupID] = true
			}
		}
	}

	for groupID := range groups {
		if handled[groupID] || singleAccountGroups[groupID] {
			continue
		}
		choice, reason, ok := chooseAdaptiveUsage(groupID, usageByWindow, windows)
		if !ok {
			report.markGroup(groupID, reportStatusSkipped)
			report.setGroupEvidence(groupID, "", reason)
			group := groups[groupID]
			s.logger.Printf(
				"[%s] 历史成本校准跳过: %s，保持当前倍率 %.4f",
				group.Name,
				reason,
				group.RateMultiplier,
			)
			continue
		}
		row := choice.Stats
		group := groups[groupID]
		report.setGroupEvidence(groupID, formatHistoryWindow(choice.Window), fmt.Sprintf("请求=%d 标准=%.4f", row.Requests, row.StandardCost))
		if row.StandardCost < s.config.MinHistoryCostUSD {
			report.markGroup(row.GroupID, reportStatusSkipped)
			s.logger.Printf(
				"[%s] 历史成本校准跳过: 最近 %s 标准费用 %.6f 小于阈值 %.6f",
				group.Name,
				formatHistoryWindow(choice.Window),
				row.StandardCost,
				s.config.MinHistoryCostUSD,
			)
			continue
		}
		if suspicious[row.GroupID] {
			report.markGroup(row.GroupID, reportStatusSkipped)
			s.logger.Printf(
				"[%s] 历史成本校准跳过: 绑定了上游系数非 1 的账号，但账号倍率仍为 1.0000；请先手动确认账号倍率",
				group.Name,
			)
			// Do not fall back to upstream probing for this group: the unknown
			// account multiplier would make that result unsafe as a group rate.
			handled[row.GroupID] = true
			continue
		}
		if row.AccountCost < 0 || math.IsNaN(row.AccountCost) || math.IsInf(row.AccountCost, 0) ||
			math.IsNaN(row.StandardCost) || math.IsInf(row.StandardCost, 0) {
			report.markGroup(row.GroupID, reportStatusSkipped)
			s.logger.Printf("[%s] 历史成本校准跳过: 历史成本包含无效数值", group.Name)
			handled[row.GroupID] = true
			continue
		}
		targetRate := round4(row.AccountCost / row.StandardCost)
		if targetRate <= 0 || math.IsNaN(targetRate) || math.IsInf(targetRate, 0) {
			report.markGroup(row.GroupID, reportStatusSkipped)
			s.logger.Printf("[%s] 历史成本校准跳过: 计算出的分组倍率无效 %.8f", group.Name, targetRate)
			handled[row.GroupID] = true
			continue
		}

		currentRate := group.RateMultiplier
		if !rateChangeSignificant(currentRate, targetRate) {
			report.markGroup(row.GroupID, reportStatusStable)
			s.logger.Printf(
				"[%s] 历史成本校准稳定: 窗口=%s 请求=%d 标准=%.6f 账号成本=%.6f 倍率=%.4f",
				group.Name,
				formatHistoryWindow(choice.Window),
				row.Requests,
				row.StandardCost,
				row.AccountCost,
				targetRate,
			)
			handled[row.GroupID] = true
			continue
		}
		if s.config.DryRun {
			report.markGroup(row.GroupID, reportStatusPreview)
			s.logger.Printf(
				"[%s] dry-run 历史成本校准: 窗口=%s 请求=%d 标准=%.6f 账号成本=%.6f，当前 %.4f -> %.4f",
				group.Name,
				formatHistoryWindow(choice.Window),
				row.Requests,
				row.StandardCost,
				row.AccountCost,
				currentRate,
				targetRate,
			)
			handled[row.GroupID] = true
			continue
		}
		if err := s.updateGroup(ctx, group, targetRate); err != nil {
			report.markGroup(row.GroupID, reportStatusFailed)
			s.logger.Printf("[%s] 历史成本校准更新失败，保持当前倍率 %.4f: %v", group.Name, currentRate, err)
			handled[row.GroupID] = true
			continue
		}
		report.updateGroupRate(row.GroupID, targetRate)
		report.markGroup(row.GroupID, reportStatusUpdated)
		s.logger.Printf(
			"[%s] 已按历史成本更新分组: 窗口=%s 请求=%d 标准=%.6f 账号成本=%.6f，当前 %.4f -> %.4f",
			group.Name,
			formatHistoryWindow(choice.Window),
			row.Requests,
			row.StandardCost,
			row.AccountCost,
			currentRate,
			targetRate,
		)
		handled[row.GroupID] = true
	}
	return handled, nil
}

func (s *Syncer) suspiciousAccountRate(channel *Channel) bool {
	if channel == nil || s.config == nil || !almostEqual(channel.AccountRateMultiplier, 1) {
		return false
	}
	factor, _, err := s.config.factorForBaseURL(channel.BaseURL)
	return err == nil && !almostEqual(factor, 1)
}

func rateChangeSignificant(current, target float64) bool {
	if almostEqual(current, target) {
		return false
	}
	absChange := math.Abs(target - current)
	relativeChange := absChange / math.Max(math.Abs(current), 0.0001)
	return absChange >= 0.005 || relativeChange >= 0.01
}

func (s *Syncer) collectChannelResults(results <-chan channelCheckResult, stats syncStats, report *syncReport) syncStats {
	for range stats.checked {
		result := <-results
		if result.err != nil {
			var skip skipError
			if errors.As(result.err, &skip) {
				stats.skipped++
				report.markChannel(result.channel, reportStatusSkipped)
				s.logger.Printf("[%s] 暂不自动: %v", channelLabel(result.channel), result.err)
				continue
			}
			stats.failed++
			report.markChannel(result.channel, reportStatusFailed)
			s.logger.Printf("[%s] 同步失败: %v", channelLabel(result.channel), result.err)
			continue
		}
		stats.normal++
		report.markChannel(result.channel, reportStatusChecked)
	}
	return stats
}

func (s *Syncer) syncChannel(ctx context.Context, channel *Channel, ruleState *RuleState, now time.Time, report *syncReport) error {
	name := channelLabel(channel)
	factor, host, err := s.config.factorForBaseURL(channel.BaseURL)
	if err != nil {
		return err
	}
	// Account rates should prefer a trusted upstream price when one is
	// available. Probe the direct NewAPI-style source before reusing a stored
	// usage template, but keep the usage state untouched if the probe fails.
	if s.syncTarget() == "account" && ruleState.Template == templateUsageRatio {
		directState := *ruleState
		matched, directErr := s.applyTemplate(ctx, channel, &directState, now, templateNewAPIRatio)
		if matched && directErr == nil {
			*ruleState = directState
			ruleState.Template = templateNewAPIRatio
			s.logger.Printf("[%s] 已自动识别价格模板: %s（上游 %s）", name, templateNewAPIRatio, host)
			return s.applyCandidate(ctx, channel, ruleState, factor, report)
		}
	}
	if ruleState.Template != "" {
		matched, err := s.applyTemplate(ctx, channel, ruleState, now, ruleState.Template)
		if matched {
			if err != nil {
				return err
			}
			return s.applyCandidate(ctx, channel, ruleState, factor, report)
		}
		if err != nil {
			return fmt.Errorf("已识别模板 %s 请求失败: %w", ruleState.Template, err)
		}
		s.logger.Printf("[%s] 原价格模板已不匹配，重新自动识别", name)
		ruleState.resetTemplate()
	}

	var probeErrors []string
	templates := [2]string{templateUsageRatio, templateNewAPIRatio}
	if s.syncTarget() == "account" {
		templates = [2]string{templateNewAPIRatio, templateUsageRatio}
	}
	for _, template := range templates {
		matched, err := s.applyTemplate(ctx, channel, ruleState, now, template)
		if matched {
			ruleState.Template = template
			s.logger.Printf("[%s] 已自动识别价格模板: %s（上游 %s）", name, template, host)
			if err != nil {
				return err
			}
			return s.applyCandidate(ctx, channel, ruleState, factor, report)
		}
		if err != nil {
			probeErrors = append(probeErrors, template+": "+err.Error())
		}
	}

	reason := "未匹配 sub2api_usage 或 newapi_pricing 模板，保持当前手动倍率"
	if len(probeErrors) > 0 {
		reason += "（" + strings.Join(probeErrors, "；") + "）"
	}
	return skipError(reason)
}

func (s *Syncer) applyTemplate(ctx context.Context, channel *Channel, state *RuleState, now time.Time, template string) (bool, error) {
	switch template {
	case templateUsageRatio:
		return s.applyUsageTemplate(ctx, channel, state, now)
	case templateNewAPIRatio:
		return s.applyNewAPITemplate(ctx, channel, state)
	default:
		return false, fmt.Errorf("未知价格模板 %q", template)
	}
}

func (s *Syncer) applyUsageTemplate(ctx context.Context, channel *Channel, state *RuleState, now time.Time) (bool, error) {
	var payload upstreamResponse
	status, err := s.fetchUpstreamJSON(ctx, channel, "/v1/usage", "days=1", &payload)
	if err != nil {
		return false, err
	}
	if status == http.StatusNotFound || status == http.StatusMethodNotAllowed {
		return false, nil
	}
	if status < 200 || status >= 300 {
		return false, fmt.Errorf("上游返回 HTTP %d", status)
	}
	if payload.Usage == nil || payload.Usage.Today == nil {
		return false, nil
	}
	if err := validateUsage(payload.Usage.Today); err != nil {
		state.resetUsage()
		return true, err
	}
	s.observeUsage(channelLabel(channel), state, *payload.Usage.Today, s.localRate(channel), now)
	return true, nil
}

func (s *Syncer) applyCandidate(ctx context.Context, channel *Channel, state *RuleState, factor float64, report *syncReport) error {
	if state.CandidateCount < s.config.Confirmations {
		return nil
	}

	finalRate := round4(state.CandidateUpstreamRate * factor)
	if finalRate <= 0 || math.IsNaN(finalRate) || math.IsInf(finalRate, 0) {
		return fmt.Errorf("最终倍率无效: %.8f", finalRate)
	}
	target := s.syncTarget()
	currentRate := channel.Group.RateMultiplier
	if target == "account" {
		currentRate = channel.AccountRateMultiplier
	}
	if almostEqual(currentRate, finalRate) {
		report.markChannel(channel, reportStatusStable)
		s.logger.Printf(
			"[%s] 本地倍率与本轮检测一致: 上游 %.4f × 本地系数 %.4f = %.4f",
			channelLabel(channel),
			state.CandidateUpstreamRate,
			factor,
			finalRate,
		)
		return nil
	}
	if s.config.DryRun {
		report.markChannel(channel, reportStatusPreview)
		s.logger.Printf(
			"[%s] dry-run: 上游 %.4f × 本地系数 %.4f = %.4f；当前本地 %.4f，不执行更新",
			channelLabel(channel),
			state.CandidateUpstreamRate,
			factor,
			finalRate,
			currentRate,
		)
		return nil
	}

	if target == "account" {
		if err := s.updateAccount(ctx, channel.AccountID, finalRate); err != nil {
			return err
		}
		report.updateAccountRate(channel.AccountID, finalRate)
		report.markAccount(channel.AccountID, reportStatusUpdated)
		s.logger.Printf(
			"[%s] 已更新账号 %s(%d) 上游倍率: %.4f × 本地系数 %.4f = %.4f",
			channelLabel(channel),
			channel.AccountName,
			channel.AccountID,
			state.CandidateUpstreamRate,
			factor,
			finalRate,
		)
	} else {
		if err := s.updateGroup(ctx, &channel.Group, finalRate); err != nil {
			return err
		}
		report.updateGroupRate(channel.Group.ID, finalRate)
		report.markGroup(channel.Group.ID, reportStatusUpdated)
		s.logger.Printf(
			"[%s] 已更新分组 %s(%d): 上游 %.4f × 本地系数 %.4f = %.4f",
			channelLabel(channel),
			channel.Group.Name,
			channel.Group.ID,
			state.CandidateUpstreamRate,
			factor,
			finalRate,
		)
	}
	return nil
}

func (s *Syncer) syncTarget() string {
	if s == nil || s.config == nil || s.config.SyncTarget == "" {
		return defaultSyncTarget
	}
	return s.config.SyncTarget
}

func (s *Syncer) localRate(channel *Channel) float64 {
	if s.syncTarget() == "account" {
		return channel.AccountRateMultiplier
	}
	return channel.Group.RateMultiplier
}

func (s *Syncer) usageBootstrapAllowed(localRate float64) bool {
	return s.syncTarget() == "account" &&
		s.config != nil &&
		s.config.UsageBootstrap &&
		almostEqual(localRate, 1)
}

func (s *Syncer) cumulativeUsageRate(current upstreamToday) (float64, bool) {
	minimumCost := 0.01
	if s.config != nil && s.config.MinHistoryCostUSD > 0 {
		minimumCost = s.config.MinHistoryCostUSD
	}
	if current.Cost < minimumCost || current.ActualCost <= 0 {
		return 0, false
	}
	rate := round4(current.ActualCost / current.Cost)
	if rate <= 0 || math.IsNaN(rate) || math.IsInf(rate, 0) {
		return 0, false
	}
	return rate, true
}

func (s *Syncer) observeUsage(name string, state *RuleState, current upstreamToday, localRate float64, now time.Time) {
	day := now.Format("2006-01-02")
	if !state.HasBaseline {
		state.resetCandidate()
		setBaseline(state, day, current)
		if s.usageBootstrapAllowed(localRate) {
			if upstreamRate, ok := s.cumulativeUsageRate(current); ok {
				s.observeRate(name, state, upstreamRate)
				s.logger.Printf(
					"[%s] 已建立上游用量基线，并从累计用量估算倍率 %.4f；等待再次确认",
					name,
					upstreamRate,
				)
				return
			}
		}
		s.logger.Printf("[%s] 已建立上游用量基线，当前本地使用倍率 %.4f，等待下一段新用量", name, localRate)
		return
	}
	if state.Day != day {
		state.resetCandidate()
		setBaseline(state, day, current)
		s.logger.Printf("[%s] 日期已变化，已重置用量基线；当前本地使用倍率 %.4f", name, localRate)
		return
	}
	if current.Cost < state.Cost || current.ActualCost < state.ActualCost {
		state.resetCandidate()
		setBaseline(state, day, current)
		s.logger.Printf("[%s] 上游累计值下降，已重置基线并停止使用旧候选倍率；当前本地使用倍率 %.4f", name, localRate)
		return
	}

	deltaCost := current.Cost - state.Cost
	deltaActual := current.ActualCost - state.ActualCost
	setBaseline(state, day, current)
	if deltaCost <= 1e-12 {
		if s.usageBootstrapAllowed(localRate) {
			if upstreamRate, ok := s.cumulativeUsageRate(current); ok {
				s.observeRate(name, state, upstreamRate)
				s.logger.Printf(
					"[%s] 无新增用量，使用累计用量估算上游倍率 %.4f（确认 %d/%d）",
					name,
					upstreamRate,
					state.CandidateCount,
					s.config.Confirmations,
				)
				return
			}
		}
		if state.CandidateCount == 0 {
			s.logger.Printf("[%s] 检查成功: 无新增用量，当前本地使用倍率 %.4f，等待可计算价格", name, localRate)
		} else if state.CandidateCount < s.config.Confirmations {
			s.logger.Printf(
				"[%s] 检查成功: 无新增用量，当前本地使用倍率 %.4f，候选上游倍率 %.4f（确认 %d/%d）",
				name,
				localRate,
				state.CandidateUpstreamRate,
				state.CandidateCount,
				s.config.Confirmations,
			)
		}
		return
	}
	if deltaActual <= 0 {
		state.resetCandidate()
		s.logger.Printf("[%s] 新用量无法计算出正倍率，本轮跳过；当前本地使用倍率 %.4f", name, localRate)
		return
	}

	upstreamRate := round4(deltaActual / deltaCost)
	if upstreamRate <= 0 || math.IsNaN(upstreamRate) || math.IsInf(upstreamRate, 0) {
		state.resetCandidate()
		s.logger.Printf("[%s] 上游倍率无效，本轮跳过；当前本地使用倍率 %.4f", name, localRate)
		return
	}
	s.observeRate(name, state, upstreamRate)
}

func (s *Syncer) observeRate(name string, state *RuleState, upstreamRate float64) {
	state.CandidateCount = min(state.CandidateCount, s.config.Confirmations)
	if almostEqual(state.CandidateUpstreamRate, upstreamRate) {
		if state.CandidateCount < s.config.Confirmations {
			state.CandidateCount++
		}
	} else {
		state.CandidateUpstreamRate = upstreamRate
		state.CandidateCount = 1
	}
	s.logger.Printf("[%s] 检测到上游倍率 %.4f（确认 %d/%d）", name, upstreamRate, state.CandidateCount, s.config.Confirmations)
}

func setBaseline(state *RuleState, day string, current upstreamToday) {
	state.Day = day
	state.Cost = current.Cost
	state.ActualCost = current.ActualCost
	state.HasBaseline = true
}

func validateUsage(today *upstreamToday) error {
	if today.Cost < 0 || today.ActualCost < 0 ||
		math.IsNaN(today.Cost) || math.IsNaN(today.ActualCost) ||
		math.IsInf(today.Cost, 0) || math.IsInf(today.ActualCost, 0) {
		return fmt.Errorf("上游 usage.today 包含无效数值")
	}
	return nil
}

func (s *Syncer) fetchUpstreamJSON(ctx context.Context, channel *Channel, path, rawQuery string, target any) (int, error) {
	return s.fetchUpstreamJSONWithLimit(ctx, channel, path, rawQuery, responseLimit, target)
}

func (s *Syncer) fetchUpstreamJSONWithLimit(ctx context.Context, channel *Channel, path, rawQuery string, limit int64, target any) (int, error) {
	endpoint, err := upstreamEndpoint(channel.BaseURL, path, rawQuery)
	if err != nil {
		return 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return 0, fmt.Errorf("创建上游请求: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+channel.APIKey)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "sub2api-rate-sync/0.2.0")

	proxyURLs, err := s.upstreamProxyURLs(channel)
	if err != nil {
		return 0, fmt.Errorf("配置上游代理: %w", err)
	}
	for index, proxyURL := range proxyURLs {
		client, err := s.upstreamClientForProxy(proxyURL)
		if err != nil {
			return 0, fmt.Errorf("配置上游代理: %w", err)
		}
		resp, err := client.Do(req.Clone(ctx))
		if err != nil {
			if index+1 >= len(proxyURLs) || !isRetryableNetworkError(err) || ctx.Err() != nil {
				return 0, fmt.Errorf("请求上游（代理 %s）: %w", proxyLabel(proxyURL), err)
			}
			if s.logger != nil {
				s.logger.Printf(
					"[%s] 上游请求网络失败，切换代理 %s -> %s: %v",
					channelLabel(channel),
					proxyLabel(proxyURL),
					proxyLabel(proxyURLs[index+1]),
					err,
				)
			}
			continue
		}

		if index > 0 && s.logger != nil {
			s.logger.Printf("[%s] 备用代理 %s 请求成功", channelLabel(channel), proxyLabel(proxyURL))
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			resp.Body.Close()
			return resp.StatusCode, nil
		}
		decodeErr := json.NewDecoder(io.LimitReader(resp.Body, limit)).Decode(target)
		resp.Body.Close()
		if decodeErr != nil {
			return resp.StatusCode, fmt.Errorf("解析上游响应: %w", decodeErr)
		}
		return resp.StatusCode, nil
	}
	return 0, fmt.Errorf("请求上游失败: 没有可用代理")
}

func (s *Syncer) upstreamClient(channel *Channel) (*http.Client, error) {
	proxyURLs, err := s.upstreamProxyURLs(channel)
	if err != nil {
		return nil, err
	}
	return s.upstreamClientForProxy(proxyURLs[0])
}

func (s *Syncer) upstreamProxyURLs(channel *Channel) ([]string, error) {
	channelProxyURL := ""
	if channel != nil {
		channelProxyURL = strings.TrimSpace(channel.ProxyURL)
	}
	globalProxyURL := ""
	if s.config != nil {
		globalProxyURL = strings.TrimSpace(s.config.ProxyURL)
	}

	proxyURLs := make([]string, 0, 2)
	if channelProxyURL != "" {
		proxyURLs = append(proxyURLs, channelProxyURL)
		if globalProxyURL != "" {
			proxyURLs = append(proxyURLs, globalProxyURL)
		}
	} else if globalProxyURL != "" {
		proxyURLs = append(proxyURLs, globalProxyURL)
	} else if s.config == nil || len(s.config.ProxyFallbackURLs) == 0 {
		proxyURLs = append(proxyURLs, "")
	}
	if s.config != nil {
		proxyURLs = append(proxyURLs, s.config.ProxyFallbackURLs...)
	}

	result := make([]string, 0, len(proxyURLs))
	seen := make(map[string]struct{}, len(proxyURLs))
	for _, proxyURL := range proxyURLs {
		proxyURL = strings.TrimSpace(proxyURL)
		if proxyURL != "" {
			if err := validateProxyURL(proxyURL, "proxy_url"); err != nil {
				return nil, err
			}
			proxyURL = strings.TrimRight(proxyURL, "/")
		}
		if _, exists := seen[proxyURL]; exists {
			continue
		}
		seen[proxyURL] = struct{}{}
		result = append(result, proxyURL)
	}
	if len(result) == 0 {
		result = append(result, "")
	}
	return result, nil
}

func (s *Syncer) upstreamClientForProxy(proxyURL string) (*http.Client, error) {
	baseClient := s.client
	if baseClient == nil {
		baseClient = http.DefaultClient
	}
	if proxyURL == "" {
		return baseClient, nil
	}

	parsed, err := url.Parse(proxyURL)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return nil, fmt.Errorf("代理 URL 无效")
	}
	cacheKey := parsed.String()

	s.upstreamClientsMu.Lock()
	defer s.upstreamClientsMu.Unlock()
	if s.upstreamClients == nil {
		s.upstreamClients = make(map[string]*http.Client)
	}
	if client, ok := s.upstreamClients[cacheKey]; ok {
		return client, nil
	}

	transport := http.DefaultTransport.(*http.Transport).Clone()
	if baseTransport, ok := baseClient.Transport.(*http.Transport); ok && baseTransport != nil {
		transport = baseTransport.Clone()
	}
	transport.Proxy = http.ProxyURL(parsed)
	client := &http.Client{
		Transport:     transport,
		Timeout:       baseClient.Timeout,
		CheckRedirect: baseClient.CheckRedirect,
		Jar:           baseClient.Jar,
	}
	s.upstreamClients[cacheKey] = client
	return client, nil
}

func isRetryableNetworkError(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, io.EOF) {
		return true
	}
	var netErr net.Error
	return errors.As(err, &netErr)
}

func proxyLabel(proxyURL string) string {
	proxyURL = strings.TrimSpace(proxyURL)
	if proxyURL == "" {
		return "直连"
	}
	parsed, err := url.Parse(proxyURL)
	if err == nil && parsed.Host != "" {
		return parsed.Scheme + "://" + parsed.Host
	}
	return proxyURL
}

func upstreamEndpoint(baseURL, path, rawQuery string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return "", fmt.Errorf("账号 base_url 必须是有效的 http/https URL")
	}
	return (&url.URL{Scheme: parsed.Scheme, Host: parsed.Host, Path: path, RawQuery: rawQuery}).String(), nil
}

func (s *Syncer) updateGroup(ctx context.Context, group *sub2APIGroup, rate float64) error {
	payload := groupUpdate{
		RateMultiplier:  rate,
		DailyLimitUSD:   limitForUpdate(group.DailyLimitUSD),
		WeeklyLimitUSD:  limitForUpdate(group.WeeklyLimitUSD),
		MonthlyLimitUSD: limitForUpdate(group.MonthlyLimitUSD),
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("编码分组更新请求: %w", err)
	}

	endpoint := s.config.Sub2APIURL + "/api/v1/admin/groups/" + strconv.FormatInt(group.ID, 10)
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("创建更新分组请求: %w", err)
	}
	req.Header.Set("x-api-key", s.config.AdminAPIKey)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "sub2api-rate-sync/0.2.0")
	req.Header.Set("Content-Type", "application/json")

	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("更新 Sub2API 分组: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("更新 Sub2API 分组返回 HTTP %d", resp.StatusCode)
	}

	var envelope struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, responseLimit)).Decode(&envelope); err != nil {
		return fmt.Errorf("解析 Sub2API 更新响应: %w", err)
	}
	if envelope.Code != 0 {
		return fmt.Errorf("更新 Sub2API 分组失败: code=%d message=%s", envelope.Code, envelope.Message)
	}
	return nil
}

func (s *Syncer) updateAccount(ctx context.Context, accountID int64, rate float64) error {
	payload := accountUpdate{RateMultiplier: rate}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("编码账号更新请求: %w", err)
	}

	endpoint := s.config.Sub2APIURL + "/api/v1/admin/accounts/" + strconv.FormatInt(accountID, 10)
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("创建账号更新请求: %w", err)
	}
	req.Header.Set("x-api-key", s.config.AdminAPIKey)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "sub2api-rate-sync/0.3.0")
	req.Header.Set("Content-Type", "application/json")

	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("更新 Sub2API 账号: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("更新 Sub2API 账号返回 HTTP %d", resp.StatusCode)
	}

	var envelope struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, responseLimit)).Decode(&envelope); err != nil {
		return fmt.Errorf("解析 Sub2API 账号更新响应: %w", err)
	}
	if envelope.Code != 0 {
		return fmt.Errorf("更新 Sub2API 账号失败: code=%d message=%s", envelope.Code, envelope.Message)
	}
	return nil
}

func channelStateKey(channel *Channel) string {
	return fmt.Sprintf("account:%d/group:%d", channel.AccountID, channel.Group.ID)
}

func channelStateKeyForTarget(channel *Channel, target string) string {
	if target == "account" {
		return fmt.Sprintf("account:%d", channel.AccountID)
	}
	return channelStateKey(channel)
}

func channelIdentity(channel *Channel) string {
	return channelIdentityForTarget(channel, "group")
}

func channelIdentityForTarget(channel *Channel, target string) string {
	digest := sha256.Sum256([]byte(channel.APIKey))
	identity := fmt.Sprintf("%d|%s|%s", channel.AccountID, strings.TrimRight(strings.ToLower(channel.BaseURL), "/"), hex.EncodeToString(digest[:8]))
	if target != "account" {
		identity = fmt.Sprintf("%s|%d", identity, channel.Group.ID)
	}
	if proxyURL := strings.TrimSpace(channel.ProxyURL); proxyURL != "" {
		proxyDigest := sha256.Sum256([]byte(proxyURL))
		identity += "|" + hex.EncodeToString(proxyDigest[:4])
	}
	return identity
}

func channelLabel(channel *Channel) string {
	account := strings.TrimSpace(channel.AccountName)
	group := strings.TrimSpace(channel.Group.Name)
	if account == group || account == "" {
		return group
	}
	return account + " → " + group
}

func limitForUpdate(value *float64) float64 {
	if value == nil {
		return -1
	}
	return *value
}

func round4(value float64) float64 {
	return math.Round(value*10000) / 10000
}

func almostEqual(left, right float64) bool {
	return math.Abs(left-right) < 0.00005
}
