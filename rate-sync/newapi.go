package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strings"
)

const (
	newAPILogResponseLimit = 8 * 1024 * 1024
	newAPIConsumeLogType   = 2
	templateNewAPIRatio    = "newapi_pricing"
)

type groupRatioResponse struct {
	Success    bool               `json:"success"`
	GroupRatio map[string]float64 `json:"group_ratio"`
}

type newAPITokenLogResponse struct {
	Success bool             `json:"success"`
	Data    []newAPITokenLog `json:"data"`
}

type newAPITokenLog struct {
	ID        int64  `json:"id"`
	CreatedAt int64  `json:"created_at"`
	Type      int    `json:"type"`
	Group     string `json:"group"`
	Other     string `json:"other"`
}

type newAPILogDetails struct {
	GroupRatio *float64 `json:"group_ratio"`
}

func (s *Syncer) applyNewAPITemplate(ctx context.Context, channel *Channel, state *RuleState) (bool, error) {
	var pricing groupRatioResponse
	status, err := s.fetchUpstreamJSON(ctx, channel, "/api/pricing", "", &pricing)
	if err != nil {
		return false, err
	}
	if status == http.StatusNotFound || status == http.StatusMethodNotAllowed {
		return false, nil
	}
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		return s.applyNewAPILogRatio(ctx, channel, state)
	}
	if status < 200 || status >= 300 {
		return false, fmt.Errorf("上游返回 HTTP %d", status)
	}
	if !pricing.Success || len(pricing.GroupRatio) == 0 {
		return false, nil
	}

	priceKey, err := s.resolveNewAPIPriceKey(ctx, channel, state, pricing.GroupRatio)
	if err != nil {
		return true, err
	}
	upstreamRate := pricing.GroupRatio[priceKey]
	if upstreamRate <= 0 || math.IsNaN(upstreamRate) || math.IsInf(upstreamRate, 0) {
		state.resetCandidate()
		return true, fmt.Errorf("NewAPI group_ratio[%q] 无效", priceKey)
	}
	s.observeRate(channelLabel(channel), state, round4(upstreamRate))
	return true, nil
}

func (s *Syncer) applyNewAPILogRatio(ctx context.Context, channel *Channel, state *RuleState) (bool, error) {
	billingLog, err := s.fetchNewAPIBillingLog(ctx, channel)
	if err != nil {
		state.resetCandidate()
		return true, err
	}
	upstreamRate, err := newAPILogGroupRatio(billingLog)
	if err != nil {
		state.resetCandidate()
		return true, err
	}
	priceKey := strings.TrimSpace(billingLog.Group)
	if state.PriceKey != priceKey {
		state.resetCandidate()
	}
	state.PriceKey = priceKey
	s.logger.Printf(
		"[%s] 已读取最新计费日志: NewAPI 价格组 %s，实际计费倍率 %.4f",
		channelLabel(channel),
		priceKey,
		upstreamRate,
	)
	s.observeRate(channelLabel(channel), state, upstreamRate)
	return true, nil
}

func (s *Syncer) resolveNewAPIPriceKey(ctx context.Context, channel *Channel, state *RuleState, ratios map[string]float64) (string, error) {
	if _, exists := ratios[state.PriceKey]; state.PriceKey != "" && !exists {
		state.resetPriceKey()
	}
	billingLog, err := s.fetchNewAPIBillingLog(ctx, channel)
	if err != nil {
		state.resetCandidate()
		return "", err
	}
	refreshedKey := strings.TrimSpace(billingLog.Group)
	if _, exists := ratios[refreshedKey]; !exists {
		state.resetPriceKey()
		return "", fmt.Errorf("真实请求使用的 NewAPI 价格组 %q 不在当前价格表中", refreshedKey)
	}
	if state.PriceKey != refreshedKey {
		state.resetCandidate()
		s.logger.Printf("[%s] 已通过真实请求日志确认 NewAPI 价格组: %s", channelLabel(channel), refreshedKey)
	}
	state.PriceKey = refreshedKey
	return refreshedKey, nil
}

func (s *Syncer) fetchNewAPIBillingLog(ctx context.Context, channel *Channel) (newAPITokenLog, error) {
	var logs newAPITokenLogResponse
	status, err := s.fetchUpstreamJSONWithLimit(ctx, channel, "/api/log/token", "", newAPILogResponseLimit, &logs)
	if err != nil {
		return newAPITokenLog{}, fmt.Errorf("读取 NewAPI Key 请求日志: %w", err)
	}
	if status == http.StatusUnauthorized || status == http.StatusForbidden ||
		status == http.StatusNotFound || status == http.StatusMethodNotAllowed {
		return newAPITokenLog{}, fmt.Errorf("NewAPI 无法使用渠道 Key 读取实时计费日志（HTTP %d）", status)
	}
	if status == http.StatusTooManyRequests {
		return newAPITokenLog{}, fmt.Errorf("NewAPI 实时计费日志接口限流（HTTP 429）")
	}
	if status < 200 || status >= 300 {
		return newAPITokenLog{}, fmt.Errorf("读取 NewAPI 实时计费日志返回 HTTP %d", status)
	}
	if !logs.Success {
		return newAPITokenLog{}, fmt.Errorf("NewAPI 实时计费日志响应无效")
	}
	billingLog, err := latestNewAPIBillingLog(logs.Data)
	if err != nil {
		return newAPITokenLog{}, err
	}
	return billingLog, nil
}

func latestNewAPIBillingLog(logs []newAPITokenLog) (newAPITokenLog, error) {
	latest := -1
	for i := range logs {
		if logs[i].Type != newAPIConsumeLogType {
			continue
		}
		if latest == -1 || logs[i].CreatedAt > logs[latest].CreatedAt ||
			(logs[i].CreatedAt == logs[latest].CreatedAt && logs[i].ID > logs[latest].ID) {
			latest = i
		}
	}
	if latest == -1 {
		return newAPITokenLog{}, fmt.Errorf("NewAPI 尚无已计费的真实请求日志，等待该 Key 产生正常请求")
	}
	if strings.TrimSpace(logs[latest].Group) == "" {
		return newAPITokenLog{}, fmt.Errorf("NewAPI 最新计费日志缺少价格组，无法完成本轮实时检查")
	}
	return logs[latest], nil
}

func newAPILogGroupRatio(billingLog newAPITokenLog) (float64, error) {
	if strings.TrimSpace(billingLog.Other) == "" {
		return 0, fmt.Errorf("NewAPI 最新计费日志缺少 other.group_ratio，无法完成本轮实时检查")
	}
	var details newAPILogDetails
	if err := json.Unmarshal([]byte(billingLog.Other), &details); err != nil {
		return 0, fmt.Errorf("NewAPI 最新计费日志 other 无法解析，无法完成本轮实时检查")
	}
	if details.GroupRatio == nil {
		return 0, fmt.Errorf("NewAPI 最新计费日志缺少 other.group_ratio，无法完成本轮实时检查")
	}
	upstreamRate := round4(*details.GroupRatio)
	if upstreamRate <= 0 || math.IsNaN(upstreamRate) || math.IsInf(upstreamRate, 0) {
		return 0, fmt.Errorf("NewAPI 最新计费日志 other.group_ratio 无效，无法完成本轮实时检查")
	}
	return upstreamRate, nil
}
