package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
)

type groupUpdate struct {
	RateMultiplier  float64 `json:"rate_multiplier"`
	DailyLimitUSD   float64 `json:"daily_limit_usd"`
	WeeklyLimitUSD  float64 `json:"weekly_limit_usd"`
	MonthlyLimitUSD float64 `json:"monthly_limit_usd"`
}

type accountUpdate struct {
	RateMultiplier float64 `json:"rate_multiplier"`
}

type adminUpdateResponse struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type adminUpdateSpec struct {
	resource     string
	label        string
	encodeAction string
	createAction string
	decodeAction string
	userAgent    string
}

func (s *Syncer) updateGroup(ctx context.Context, group *sub2APIGroup, rate float64) error {
	payload := groupUpdate{
		RateMultiplier:  rate,
		DailyLimitUSD:   limitForUpdate(group.DailyLimitUSD),
		WeeklyLimitUSD:  limitForUpdate(group.WeeklyLimitUSD),
		MonthlyLimitUSD: limitForUpdate(group.MonthlyLimitUSD),
	}
	return s.updateAdminResource(ctx, groupAdminUpdateSpec(), group.ID, payload)
}

func (s *Syncer) updateAccount(ctx context.Context, accountID int64, rate float64) error {
	return s.updateAdminResource(ctx, accountAdminUpdateSpec(), accountID, accountUpdate{RateMultiplier: rate})
}

func groupAdminUpdateSpec() adminUpdateSpec {
	return adminUpdateSpec{
		resource:     "groups",
		label:        "分组",
		encodeAction: "编码分组更新请求",
		createAction: "创建更新分组请求",
		decodeAction: "解析 Sub2API 更新响应",
		userAgent:    "sub2api-rate-sync/0.2.0",
	}
}

func accountAdminUpdateSpec() adminUpdateSpec {
	return adminUpdateSpec{
		resource:     "accounts",
		label:        "账号",
		encodeAction: "编码账号更新请求",
		createAction: "创建账号更新请求",
		decodeAction: "解析 Sub2API 账号更新响应",
		userAgent:    "sub2api-rate-sync/0.3.0",
	}
}

func (s *Syncer) updateAdminResource(ctx context.Context, spec adminUpdateSpec, id int64, payload any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("%s: %w", spec.encodeAction, err)
	}
	endpoint := s.config.Sub2APIURL + "/api/v1/admin/" + spec.resource + "/" + strconv.FormatInt(id, 10)
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("%s: %w", spec.createAction, err)
	}
	setAdminHeaders(req, s.config.AdminAPIKey, spec.userAgent)
	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("更新 Sub2API %s: %w", spec.label, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("更新 Sub2API %s返回 HTTP %d", spec.label, resp.StatusCode)
	}
	var envelope adminUpdateResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, responseLimit)).Decode(&envelope); err != nil {
		return fmt.Errorf("%s: %w", spec.decodeAction, err)
	}
	if envelope.Code != 0 {
		return fmt.Errorf("更新 Sub2API %s失败: code=%d message=%s", spec.label, envelope.Code, envelope.Message)
	}
	return nil
}

func setAdminHeaders(req *http.Request, apiKey, userAgent string) {
	req.Header.Set("x-api-key", apiKey)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Content-Type", "application/json")
}

func limitForUpdate(value *float64) float64 {
	if value == nil {
		return -1
	}
	return *value
}
