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
	factor, host, err := s.config.factorForBaseURL(channel.BaseURL)
	if err != nil {
		return err
	}
	handled, directErr := s.tryDirectAccountTemplate(ctx, channel, ruleState, now, factor, report, host)
	if handled {
		return directErr
	}
	return s.tryUsageAccountTemplate(ctx, channel, ruleState, now, factor, report, host, directErr)
}

// tryDirectAccountTemplate 优先使用实时计费接口，失败时保留原有状态供请求成本计算回退。
func (s *Syncer) tryDirectAccountTemplate(
	ctx context.Context,
	channel *Channel,
	state *RuleState,
	now time.Time,
	factor float64,
	report *syncReport,
	host string,
) (bool, error) {
	wasDirectTemplate := state.Template == templateNewAPIRatio
	directState := *state
	matched, err := s.applyTemplate(ctx, channel, &directState, now, templateNewAPIRatio)
	if !matched || err != nil {
		// 直接倍率本轮不可用时，不能让旧直接候选跨过失败周期继续累计确认。
		// usage 的基线和候选则留给回退路径继续使用。
		if wasDirectTemplate {
			*state = directState
			state.resetCandidate()
		} else if matched && err != nil && state.Template != templateUsageRatio {
			*state = directState
			state.Template = templateNewAPIRatio
			state.resetCandidate()
		}
		return false, err
	}
	*state = directState
	state.Template = templateNewAPIRatio
	if !wasDirectTemplate {
		s.logger.Printf("[%s] 已自动识别价格模板: %s（上游 %s）", channelLabel(channel), templateNewAPIRatio, host)
	}
	return true, s.applyCandidate(ctx, channel, state, factor, report)
}

func (s *Syncer) tryUsageAccountTemplate(
	ctx context.Context,
	channel *Channel,
	state *RuleState,
	now time.Time,
	factor float64,
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
		state.Template = templateUsageRatio
		state.PriceKey = ""
		if !wasUsageTemplate {
			s.logger.Printf("[%s] 已自动识别价格模板: %s（上游 %s）", channelLabel(channel), templateUsageRatio, host)
		}
		if directErr != nil {
			s.logger.Printf("[%s] 上游直接倍率不可用，改用请求成本计算: %v", channelLabel(channel), directErr)
		}
		return s.applyCandidate(ctx, channel, state, factor, report)
	}
	if err != nil {
		return fmt.Errorf("已识别模板 %s 请求失败: %w", templateUsageRatio, err)
	}
	if directErr != nil {
		return directErr
	}
	return skipError("未匹配 sub2api_usage 或 newapi_pricing 模板，保持当前手动倍率")
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
	if s.syncTarget() != "account" {
		return skipError("分组倍率由账户倍率继承或统计校准")
	}
	if state.CandidateCount < s.config.Confirmations {
		return nil
	}
	finalRate, err := candidateFinalRate(state, factor)
	if err != nil {
		return err
	}
	currentRate := channel.AccountRateMultiplier
	if almostEqual(currentRate, finalRate) {
		report.markChannel(channel, reportStatusStable)
		s.logger.Printf(
			"[%s] 本地倍率与本轮检测一致: 上游 %.4f × 本地系数 %.4f = %.4f",
			channelLabel(channel), state.CandidateUpstreamRate, factor, finalRate,
		)
		return nil
	}
	if s.config.DryRun {
		report.markChannel(channel, reportStatusPreview)
		s.logger.Printf(
			"[%s] dry-run: 上游 %.4f × 本地系数 %.4f = %.4f；当前本地 %.4f，不执行更新",
			channelLabel(channel), state.CandidateUpstreamRate, factor, finalRate, currentRate,
		)
		return nil
	}
	return s.publishAccountCandidate(ctx, channel, state, factor, finalRate, report)
}

func candidateFinalRate(state *RuleState, factor float64) (float64, error) {
	rate := round4(state.CandidateUpstreamRate * factor)
	if !validPositiveRate(rate) {
		return 0, fmt.Errorf("最终倍率无效: %.8f", rate)
	}
	return rate, nil
}

func (s *Syncer) publishAccountCandidate(ctx context.Context, channel *Channel, state *RuleState, factor, finalRate float64, report *syncReport) error {
	if err := s.updateAccount(ctx, channel.AccountID, finalRate); err != nil {
		return err
	}
	report.updateAccountRate(channel.AccountID, finalRate)
	report.markAccount(channel.AccountID, reportStatusUpdated)
	s.logger.Printf(
		"[%s] 已更新账号 %s(%d) 上游倍率: %.4f × 本地系数 %.4f = %.4f",
		channelLabel(channel), channel.AccountName, channel.AccountID,
		state.CandidateUpstreamRate, factor, finalRate,
	)
	return nil
}
