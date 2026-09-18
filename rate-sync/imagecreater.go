package main

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

const (
	templateImageCreaterBalance       = "imagecreater_balance"
	imageCreaterConfirmationsRequired = 2
)

type imageCreaterBalance struct {
	Balance       *float64 `json:"balance"`
	TodayCost     *float64 `json:"todayCost"`
	TodayRequests *int64   `json:"todayRequests"`
	Unit          string   `json:"unit"`
}

type imageCreaterBalanceValues struct {
	Balance       float64
	TodayCost     float64
	TodayRequests int64
}

type imageCreaterChannelGroup struct {
	key      string
	channels []*Channel
}

func (s *Syncer) syncImageCreaterAccounts(ctx context.Context, channels []Channel, now time.Time, report *syncReport) (map[int64]bool, syncStats) {
	handled := make(map[int64]bool)
	stats := syncStats{}
	usageSource, ok := s.source.(accountUsageIncrementalSource)
	if s.syncTarget() != "account" || !ok {
		return handled, stats
	}
	if s.state.ImageCreaterHosts == nil {
		s.state.ImageCreaterHosts = make(map[string]*ImageCreaterHostState)
	}

	for _, group := range imageCreaterChannelGroups(channels) {
		matched, results := s.syncImageCreaterGroup(ctx, usageSource, group, now, report)
		if !matched {
			continue
		}
		for _, channel := range group.channels {
			handled[channel.AccountID] = true
			stats.checked++
			err := results[channel.AccountID]
			if err == nil {
				stats.normal++
				report.markChannel(channel, reportStatusChecked)
				continue
			}
			var skipped skipError
			if errors.As(err, &skipped) {
				stats.skipped++
				report.markChannel(channel, reportStatusSkipped)
				s.logger.Printf("[%s] 暂不自动: %v", channelLabel(channel), err)
				continue
			}
			stats.failed++
			report.markChannel(channel, reportStatusFailed)
			s.logger.Printf("[%s] 同步失败: %v", channelLabel(channel), err)
		}
	}
	return handled, stats
}

func imageCreaterChannelGroups(channels []Channel) []imageCreaterChannelGroup {
	groups := make([]imageCreaterChannelGroup, 0)
	indexes := make(map[string]int)
	seenAccounts := make(map[int64]bool)
	for index := range channels {
		channel := &channels[index]
		if seenAccounts[channel.AccountID] {
			continue
		}
		key, err := imageCreaterBaseKey(channel.BaseURL)
		if err != nil {
			continue
		}
		seenAccounts[channel.AccountID] = true
		groupIndex, exists := indexes[key]
		if !exists {
			groupIndex = len(groups)
			indexes[key] = groupIndex
			groups = append(groups, imageCreaterChannelGroup{key: key})
		}
		groups[groupIndex].channels = append(groups[groupIndex].channels, channel)
	}
	return groups
}

func imageCreaterBaseKey(baseURL string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return "", fmt.Errorf("账号 base_url 必须是有效的 http/https URL")
	}
	parsed.Scheme = strings.ToLower(parsed.Scheme)
	parsed.Host = strings.ToLower(parsed.Host)
	parsed.User = nil
	parsed.Path = strings.TrimRight(parsed.Path, "/")
	parsed.RawPath = ""
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return parsed.String(), nil
}

func (s *Syncer) syncImageCreaterGroup(
	ctx context.Context,
	usageSource accountUsageIncrementalSource,
	group imageCreaterChannelGroup,
	now time.Time,
	report *syncReport,
) (bool, map[int64]error) {
	current, matched, probeErr := s.probeImageCreaterGroup(ctx, group)
	if !matched {
		if s.knownImageCreaterGroup(group) {
			s.resetImageCreaterGroup(group)
		}
		return false, nil
	}
	rules, discounts, results := s.prepareImageCreaterRules(group, report)
	for _, preparationErr := range results {
		if preparationErr != nil {
			resetImageCreaterCandidates(rules)
			setImageCreaterGroupError(results, group, preparationErr)
			return true, results
		}
	}
	if probeErr != nil {
		resetImageCreaterCandidates(rules)
		var skipped skipError
		if errors.As(probeErr, &skipped) {
			delete(s.state.ImageCreaterHosts, group.key)
		}
		setImageCreaterGroupError(results, group, probeErr)
		return true, results
	}

	throughID, err := usageSource.LatestAccountUsageID(ctx)
	if err != nil {
		resetImageCreaterCandidates(rules)
		setImageCreaterGroupError(results, group, err)
		return true, results
	}
	identity := imageCreaterGroupIdentity(group.channels)
	hostState := s.state.ImageCreaterHosts[group.key]
	day := now.Format("2006-01-02")
	if hostState == nil || hostState.Identity != identity || !hostState.Initialized {
		hostState = &ImageCreaterHostState{Identity: identity}
		s.state.ImageCreaterHosts[group.key] = hostState
		setImageCreaterBaseline(hostState, current, throughID, day)
		resetImageCreaterCandidates(rules)
		s.logger.Printf("[%s] 已建立千纸共享余额基线，等待可核对的新请求", group.key)
		return true, results
	}
	if hostState.Day != day || throughID < hostState.LastUsageID || imageCreaterCountersRegressed(hostState, current) {
		setImageCreaterBaseline(hostState, current, throughID, day)
		resetImageCreaterCandidates(rules)
		s.logger.Printf("[%s] 千纸日期、计数或水位已变化，已重置共享余额基线", group.key)
		return true, results
	}

	usage, err := usageSource.ListAccountUsageSince(ctx, imageCreaterAccountIDs(group.channels), hostState.LastUsageID, throughID)
	if err != nil {
		resetImageCreaterCandidates(rules)
		setImageCreaterGroupError(results, group, err)
		return true, results
	}
	deltaRequests := current.TodayRequests - hostState.TodayRequests
	deltaCost := current.TodayCost - hostState.TodayCost
	deltaBalance := hostState.Balance - current.Balance
	setImageCreaterBaseline(hostState, current, throughID, day)

	accountID, baseCost, reason := evaluateImageCreaterWindow(deltaRequests, deltaCost, deltaBalance, usage)
	if reason != "" {
		resetImageCreaterCandidates(rules)
		err := skipError(reason)
		setImageCreaterGroupError(results, group, err)
		return true, results
	}
	if accountID != 0 {
		state := rules[accountID]
		if state == nil {
			resetImageCreaterCandidates(rules)
			err := skipError("本地增量用量归属到了当前千纸渠道集合之外的账号")
			setImageCreaterGroupError(results, group, err)
			return true, results
		}
		upstreamRate := round4(deltaCost / baseCost)
		if !validPositiveRate(upstreamRate) {
			resetImageCreaterCandidates(rules)
			err := skipError("千纸余额增量计算出的上游倍率无效")
			setImageCreaterGroupError(results, group, err)
			return true, results
		}
		observeImageCreaterRate(state, upstreamRate)
		s.logger.Printf("[%s] 千纸余额对账得到账号 %d 上游倍率 %.4f（确认 %d/%d）",
			group.key, accountID, upstreamRate, state.CandidateCount, imageCreaterConfirmationsRequired)
	}

	for _, channel := range group.channels {
		state := rules[channel.AccountID]
		if state == nil {
			continue
		}
		if state.CandidateCount > 0 {
			report.setAccountUpstreamRate(channel.AccountID, state.CandidateUpstreamRate)
			if expected, err := candidateFinalRate(state, discounts[channel.AccountID]); err == nil {
				report.setAccountExpectedRate(channel.AccountID, expected)
			}
		}
		if state.CandidateCount < imageCreaterConfirmationsRequired {
			continue
		}
		if err := s.applyCandidate(ctx, channel, state, discounts[channel.AccountID], report); err != nil {
			results[channel.AccountID] = err
			continue
		}
		if !s.config.DryRun {
			state.resetCandidate()
		}
	}
	return true, results
}

func (s *Syncer) prepareImageCreaterRules(group imageCreaterChannelGroup, report *syncReport) (map[int64]*RuleState, map[int64]float64, map[int64]error) {
	rules := make(map[int64]*RuleState, len(group.channels))
	discounts := make(map[int64]float64, len(group.channels))
	results := make(map[int64]error, len(group.channels))
	for _, channel := range group.channels {
		discount, host, err := s.config.rechargeDiscountForBaseURL(channel.BaseURL)
		if err != nil {
			results[channel.AccountID] = err
			continue
		}
		discounts[channel.AccountID] = discount
		report.setAccountRechargeDiscount(channel.AccountID, discount)
		report.setAccountSource(channel.AccountID, reportAccountSourceBalance)
		state := s.ruleStateFor(channel, "account")
		if state.Template != templateImageCreaterBalance {
			state.resetCandidate()
			state.PriceKey = ""
			state.Template = templateImageCreaterBalance
			s.logger.Printf("[%s] 已自动识别价格模板: %s（上游 %s）", channelLabel(channel), templateImageCreaterBalance, host)
		}
		rules[channel.AccountID] = state
	}
	return rules, discounts, results
}

func (s *Syncer) probeImageCreaterGroup(ctx context.Context, group imageCreaterChannelGroup) (imageCreaterBalanceValues, bool, error) {
	var reference imageCreaterBalanceValues
	matchedCount := 0
	var firstErr error
	for _, channel := range group.channels {
		current, matched, err := s.fetchImageCreaterBalance(ctx, channel)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if !matched {
			continue
		}
		matchedCount++
		if matchedCount == 1 {
			reference = current
			continue
		}
		if !sameImageCreaterBalance(reference, current) {
			return imageCreaterBalanceValues{}, true, skipError("同一千纸上游的渠道 Key 返回了不同余额汇总，无法安全归属成本")
		}
	}
	if matchedCount == 0 {
		if firstErr != nil && s.knownImageCreaterGroup(group) {
			return imageCreaterBalanceValues{}, true, firstErr
		}
		return imageCreaterBalanceValues{}, false, nil
	}
	if matchedCount != len(group.channels) {
		if firstErr != nil {
			return imageCreaterBalanceValues{}, true, fmt.Errorf("读取千纸共享余额: %w", firstErr)
		}
		return imageCreaterBalanceValues{}, true, skipError("同一千纸上游并非所有渠道 Key 都能读取共享余额，无法安全归属成本")
	}
	return reference, true, nil
}

func (s *Syncer) knownImageCreaterGroup(group imageCreaterChannelGroup) bool {
	if s.state.ImageCreaterHosts[group.key] != nil {
		return true
	}
	for _, channel := range group.channels {
		state := s.state.Rules[channelStateKeyForTarget(channel, "account")]
		if state != nil && state.Template == templateImageCreaterBalance {
			return true
		}
	}
	return false
}

func (s *Syncer) resetImageCreaterGroup(group imageCreaterChannelGroup) {
	delete(s.state.ImageCreaterHosts, group.key)
	for _, channel := range group.channels {
		state := s.state.Rules[channelStateKeyForTarget(channel, "account")]
		if state != nil && state.Template == templateImageCreaterBalance {
			state.resetTemplate()
		}
	}
}

func (s *Syncer) fetchImageCreaterBalance(ctx context.Context, channel *Channel) (imageCreaterBalanceValues, bool, error) {
	var payload imageCreaterBalance
	status, err := s.fetchUpstreamBasePathJSON(ctx, channel, "/user/balance", "", &payload)
	if err != nil {
		return imageCreaterBalanceValues{}, false, err
	}
	if status == http.StatusUnauthorized || status == http.StatusForbidden || status == http.StatusNotFound || status == http.StatusMethodNotAllowed {
		return imageCreaterBalanceValues{}, false, nil
	}
	if status < 200 || status >= 300 {
		return imageCreaterBalanceValues{}, false, fmt.Errorf("千纸余额接口返回 HTTP %d", status)
	}
	if payload.Balance == nil || payload.TodayCost == nil || payload.TodayRequests == nil || !strings.EqualFold(strings.TrimSpace(payload.Unit), "USD") {
		return imageCreaterBalanceValues{}, false, nil
	}
	values := imageCreaterBalanceValues{
		Balance:       *payload.Balance,
		TodayCost:     *payload.TodayCost,
		TodayRequests: *payload.TodayRequests,
	}
	if math.IsNaN(values.Balance) || math.IsInf(values.Balance, 0) ||
		values.TodayCost < 0 || math.IsNaN(values.TodayCost) || math.IsInf(values.TodayCost, 0) ||
		values.TodayRequests < 0 {
		return imageCreaterBalanceValues{}, true, fmt.Errorf("千纸余额接口包含无效数值")
	}
	return values, true, nil
}

func evaluateImageCreaterWindow(deltaRequests int64, deltaCost, deltaBalance float64, usage []AccountUsageStats) (int64, float64, string) {
	if deltaRequests < 0 || deltaCost < -1e-12 {
		return 0, 0, "千纸今日计数发生回退，已重置余额基线"
	}
	// 只容忍微美元级金额舍入差，同时限制低成本窗口的相对误差。
	moneyDifference := math.Abs(deltaBalance - deltaCost)
	moneyTolerance := math.Min(5e-6, math.Abs(deltaCost)*0.001) + 1e-9
	if moneyDifference > moneyTolerance {
		return 0, 0, fmt.Sprintf("千纸余额变化与今日成本增量不一致（余额变化 %.6f USD，成本增量 %.6f USD，差额 %.9f USD），可能发生了充值、调账或跨日", deltaBalance, deltaCost, moneyDifference)
	}
	var localRequests int64
	var positiveAccountID int64
	var positiveBaseCost float64
	positiveAccounts := 0
	for _, item := range usage {
		if item.Requests < 0 || item.BaseCost < 0 || math.IsNaN(item.BaseCost) || math.IsInf(item.BaseCost, 0) {
			return 0, 0, "本地账号增量用量包含无效数值"
		}
		localRequests += item.Requests
		if item.BaseCost > 1e-12 {
			positiveAccounts++
			positiveAccountID = item.AccountID
			positiveBaseCost = item.BaseCost
		}
	}
	if deltaRequests != localRequests {
		return 0, 0, fmt.Sprintf("千纸新增请求数 %d 与本地新增请求数 %d 不一致，可能存在外部请求", deltaRequests, localRequests)
	}
	if deltaRequests == 0 && math.Abs(deltaCost) <= 1e-12 {
		return 0, 0, ""
	}
	if deltaCost <= 1e-12 {
		return 0, 0, "千纸新增请求尚未产生可核对的正成本"
	}
	if positiveAccounts != 1 {
		return 0, 0, "同一余额窗口有多个账号产生标准成本，无法把上游成本准确归属到单个账号"
	}
	return positiveAccountID, positiveBaseCost, ""
}

func observeImageCreaterRate(state *RuleState, upstreamRate float64) {
	if state.CandidateCount > 0 && almostEqual(state.CandidateUpstreamRate, upstreamRate) {
		state.CandidateCount++
		if state.CandidateCount > imageCreaterConfirmationsRequired {
			state.CandidateCount = imageCreaterConfirmationsRequired
		}
		return
	}
	state.CandidateUpstreamRate = upstreamRate
	state.CandidateCount = 1
}

func setImageCreaterBaseline(state *ImageCreaterHostState, current imageCreaterBalanceValues, usageID int64, day string) {
	state.Day = day
	state.Balance = current.Balance
	state.TodayCost = current.TodayCost
	state.TodayRequests = current.TodayRequests
	state.LastUsageID = usageID
	state.Initialized = true
}

func imageCreaterCountersRegressed(state *ImageCreaterHostState, current imageCreaterBalanceValues) bool {
	return current.TodayCost < state.TodayCost-1e-12 || current.TodayRequests < state.TodayRequests
}

func resetImageCreaterCandidates(rules map[int64]*RuleState) {
	for _, state := range rules {
		if state != nil {
			state.resetCandidate()
		}
	}
}

func setImageCreaterGroupError(results map[int64]error, group imageCreaterChannelGroup, err error) {
	for _, channel := range group.channels {
		if results[channel.AccountID] == nil {
			results[channel.AccountID] = err
		}
	}
}

func sameImageCreaterBalance(left, right imageCreaterBalanceValues) bool {
	return left.TodayRequests == right.TodayRequests &&
		sameMoney(left.Balance, right.Balance) &&
		sameMoney(left.TodayCost, right.TodayCost)
}

func sameMoney(left, right float64) bool {
	difference := math.Abs(left - right)
	return difference <= 1e-9 || difference <= math.Max(math.Abs(left), math.Abs(right))*1e-9
}

func imageCreaterAccountIDs(channels []*Channel) []int64 {
	ids := make([]int64, 0, len(channels))
	for _, channel := range channels {
		ids = append(ids, channel.AccountID)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}

func imageCreaterGroupIdentity(channels []*Channel) string {
	identities := make([]string, 0, len(channels))
	for _, channel := range channels {
		identities = append(identities, channelIdentityForTarget(channel, "account"))
	}
	sort.Strings(identities)
	digest := sha256.Sum256([]byte(strings.Join(identities, "\n")))
	return fmt.Sprintf("%x", digest[:16])
}
