package main

import (
	"context"
	"fmt"
	"net/http"
	"strings"
)

const templateSub2APIBilling = "sub2api_billing"

// sub2APIKeyBillingResponse is the static account multiplier resolved by Sub2API.
// effective_rate_multiplier may include a temporary peak coefficient and must not
// be persisted as the account's base multiplier.
type sub2APIKeyBillingResponse struct {
	Object                 string   `json:"object"`
	ResolvedRateMultiplier *float64 `json:"resolved_rate_multiplier"`
}

func (s *Syncer) applySub2APIBillingTemplate(ctx context.Context, channel *Channel, state *RuleState) (bool, error) {
	var payload sub2APIKeyBillingResponse
	status, err := s.fetchUpstreamJSON(ctx, channel, "/v1/sub2api/billing", "", &payload)
	if err != nil {
		return false, err
	}
	if status == http.StatusNotFound || status == http.StatusMethodNotAllowed ||
		status == http.StatusUnauthorized || status == http.StatusForbidden {
		return false, nil
	}
	if status < 200 || status >= 300 {
		return false, fmt.Errorf("上游返回 HTTP %d", status)
	}
	if object := strings.TrimSpace(payload.Object); object != "" && object != "sub2api.key_billing" {
		state.resetCandidate()
		return true, fmt.Errorf("Sub2API billing 返回对象 %q 无法识别", object)
	}
	if payload.ResolvedRateMultiplier == nil {
		state.resetCandidate()
		return true, fmt.Errorf("Sub2API billing 缺少 resolved_rate_multiplier")
	}
	upstreamRate := round4(*payload.ResolvedRateMultiplier)
	if !validPositiveRate(upstreamRate) {
		state.resetCandidate()
		return true, fmt.Errorf("Sub2API billing resolved_rate_multiplier 无效")
	}
	s.observeRate(channelLabel(channel), state, upstreamRate)
	s.logger.Printf("[%s] 已读取 Sub2API 自动探测倍率 %.4f", channelLabel(channel), upstreamRate)
	return true, nil
}
