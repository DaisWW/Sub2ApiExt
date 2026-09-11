package main

import (
	"context"
	"fmt"
	"net/http"
	"time"
)

// syncChannel 负责一次渠道的模板识别、价格观测和候选发布。
func (s *Syncer) syncChannel(ctx context.Context, channel *Channel, ruleState *RuleState, now time.Time, report *syncReport) error {
	if s.syncTarget() != "account" {
		return skipError("分组倍率由账户倍率继承或统计校准")
	}
	rechargeDiscount, host, err := s.config.rechargeDiscountForBaseURL(channel.BaseURL)
	if err != nil {
		return err
	}
	report.setAccountRechargeDiscount(channel.AccountID, rechargeDiscount)
	handled, directErr := s.tryDirectAccountTemplate(ctx, channel, ruleState, now, rechargeDiscount, report, host)
	if handled {
		return directErr
	}
	return s.tryUsageAccountTemplate(ctx, channel, ruleState, now, rechargeDiscount, report, host, directErr)
}

// tryDirectAccountTemplate 优先使用上游已解析的静态倍率，再读取上游价格接口。
func (s *Syncer) tryDirectAccountTemplate(
	ctx context.Context,
	channel *Channel,
	state *RuleState,
	now time.Time,
	rechargeDiscount float64,
	report *syncReport,
	host string,
) (bool, error) {
	var directErr error
	var failedDirectState *RuleState
	failedDirectTemplate := ""
	for _, template := range []string{templateSub2APIBilling, templateNewAPIRatio} {
		directState := *state
		if directState.Template != template {
			directState.resetCandidate()
			directState.PriceKey = ""
		}
		matched, err := s.applyTemplate(ctx, channel, &directState, now, template)
		if !matched {
			if err != nil {
				directErr = err
			}
			continue
		}
		if err != nil {
			directErr = err
			failedState := directState
			failedDirectState = &failedState
			failedDirectTemplate = template
			continue
		}
		report.setAccountSource(channel.AccountID, accountSourceForTemplate(template))
		wasTemplate := state.Template == template
		*state = directState
		state.Template = template
		if !wasTemplate {
			s.logger.Printf("[%s] 已自动识别价格模板: %s（上游 %s）", channelLabel(channel), template, host)
		}
		return true, s.applyCandidate(ctx, channel, state, rechargeDiscount, report)
	}

	// 直接倍率本轮不可用时，不能让旧直接候选跨过失败周期继续参与发布。
	// usage 的基线和候选则留给回退路径继续使用。
	if state.Template != templateUsageRatio {
		if failedDirectState != nil {
			*state = *failedDirectState
			state.Template = failedDirectTemplate
		}
		state.resetCandidate()
	}
	return false, directErr
}

func (s *Syncer) tryUsageAccountTemplate(
	ctx context.Context,
	channel *Channel,
	state *RuleState,
	now time.Time,
	rechargeDiscount float64,
	report *syncReport,
	host string,
	directErr error,
) error {
	usageState := *state
	wasUsageTemplate := usageState.Template == templateUsageRatio
	if !wasUsageTemplate {
		usageState.resetCandidate()
		usageState.PriceKey = ""
	}
	matched, err := s.applyTemplate(ctx, channel, &usageState, now, templateUsageRatio)
	if matched {
		*state = usageState
		if err != nil {
			return err
		}
		report.setAccountSource(channel.AccountID, reportAccountSourceUsage)
		state.Template = templateUsageRatio
		state.PriceKey = ""
		if !wasUsageTemplate {
			s.logger.Printf("[%s] 已自动识别价格模板: %s（上游 %s）", channelLabel(channel), templateUsageRatio, host)
		}
		if directErr != nil {
			s.logger.Printf("[%s] 上游直接倍率不可用，改用请求成本计算: %v", channelLabel(channel), directErr)
		}
		return s.applyCandidate(ctx, channel, state, rechargeDiscount, report)
	}
	if err != nil {
		return fmt.Errorf("已识别模板 %s 请求失败: %w", templateUsageRatio, err)
	}
	if directErr != nil {
		return directErr
	}
	return skipError("未匹配 sub2api_billing、newapi_pricing 或 sub2api_usage 模板，保持当前手动倍率")
}

func accountSourceForTemplate(template string) string {
	switch template {
	case templateSub2APIBilling:
		return reportAccountSourceProbe
	case templateNewAPIRatio:
		return reportAccountSourceUpstream
	default:
		return ""
	}
}

func (s *Syncer) applyTemplate(ctx context.Context, channel *Channel, state *RuleState, now time.Time, template string) (bool, error) {
	switch template {
	case templateUsageRatio:
		return s.applyUsageTemplate(ctx, channel, state, now)
	case templateSub2APIBilling:
		return s.applySub2APIBillingTemplate(ctx, channel, state)
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

func (s *Syncer) applyCandidate(ctx context.Context, channel *Channel, state *RuleState, rechargeDiscount float64, report *syncReport) error {
	if s.syncTarget() != "account" {
		return skipError("分组倍率由账户倍率继承或统计校准")
	}
	if state.CandidateCount <= 0 {
		return nil
	}
	finalRate, err := candidateFinalRate(state, rechargeDiscount)
	if err != nil {
		return err
	}
	report.setAccountExpectedRate(channel.AccountID, finalRate)
	currentRate := channel.AccountRateMultiplier
	if almostEqual(currentRate, finalRate) {
		report.markChannel(channel, reportStatusStable)
		s.logger.Printf(
			"[%s] 倍率稳定: 当前 %.4f；%s",
			channelLabel(channel), currentRate, rateFormula(state.CandidateUpstreamRate, rechargeDiscount, finalRate),
		)
		return nil
	}
	if s.config.DryRun {
		report.markChannel(channel, reportStatusPreview)
		s.logger.Printf(
			"[%s] 预览更新: 原 %.4f；%s，dry-run 未写回",
			channelLabel(channel), currentRate, rateFormula(state.CandidateUpstreamRate, rechargeDiscount, finalRate),
		)
		return nil
	}
	return s.publishAccountCandidate(ctx, channel, state, rechargeDiscount, currentRate, finalRate, report)
}

func candidateFinalRate(state *RuleState, rechargeDiscount float64) (float64, error) {
	rate := round4(state.CandidateUpstreamRate * rechargeDiscount)
	if !validPositiveRate(rate) {
		return 0, fmt.Errorf("最终倍率无效: %.8f", rate)
	}
	return rate, nil
}

func (s *Syncer) publishAccountCandidate(ctx context.Context, channel *Channel, state *RuleState, rechargeDiscount, previousRate, finalRate float64, report *syncReport) error {
	if err := s.updateAccount(ctx, channel.AccountID, finalRate); err != nil {
		return err
	}
	report.updateAccountRate(channel.AccountID, finalRate)
	report.markAccount(channel.AccountID, reportStatusUpdated)
	s.logger.Printf(
		"[%s] 已更新账号 %s(%d) 账户倍率: 原 %.4f -> 新 %.4f；%s",
		channelLabel(channel), channel.AccountName, channel.AccountID,
		previousRate, finalRate, rateFormula(state.CandidateUpstreamRate, rechargeDiscount, finalRate),
	)
	return nil
}

func rateFormula(upstreamRate, rechargeDiscount, expectedRate float64) string {
	return fmt.Sprintf("上游倍率 %.4f × 充值折扣 %.4f = 预期账户倍率 %.4f", upstreamRate, rechargeDiscount, expectedRate)
}
