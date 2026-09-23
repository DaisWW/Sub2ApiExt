package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/DaisWW/Sub2ApiExt/monitoring/internal/config"
	"github.com/DaisWW/Sub2ApiExt/monitoring/internal/model"
)

// costUsageQuery reads a bounded baseline window and aggregates the current
// window in the same pass. The query only returns dimensions that have current
// traffic, so an inactive user does not create work or alerts.
const costUsageQuery = `
WITH usage AS (
    SELECT ul.created_at,
           COALESCE(NULLIF(BTRIM(ul.user_id::text), ''), 'unknown') AS user_key,
           COALESCE(NULLIF(BTRIM(ul.model), ''), 'unknown') AS model,
           COALESCE(ul.api_key_id, 0)::bigint AS api_key_id,
           COALESCE(ul.channel_id, 0)::bigint AS channel_id,
           COALESCE(NULLIF(BTRIM(c.name), ''), '未归属渠道') AS channel_name,
           COALESCE(ul.account_id, 0)::bigint AS account_id,
           COALESCE(NULLIF(BTRIM(a.name), ''), '未归属账户') AS account_name,
           GREATEST(COALESCE(ul.input_tokens, 0), 0)::bigint AS input_tokens,
           GREATEST(COALESCE(ul.output_tokens, 0), 0)::bigint AS output_tokens,
           GREATEST(COALESCE(ul.cache_creation_tokens, 0), 0)::bigint AS cache_creation_tokens,
           GREATEST(COALESCE(ul.cache_read_tokens, 0), 0)::bigint AS cache_read_tokens,
           GREATEST(COALESCE(ul.total_cost, 0), 0)::double precision AS base_cost,
           GREATEST(COALESCE(ul.actual_cost, 0), 0)::double precision AS actual_cost,
           GREATEST(COALESCE(ul.input_cost, 0), 0)::double precision AS input_cost,
           GREATEST(COALESCE(ul.output_cost, 0), 0)::double precision AS output_cost,
           GREATEST(COALESCE(ul.cache_creation_cost, 0), 0)::double precision AS cache_creation_cost,
           GREATEST(COALESCE(ul.cache_read_cost, 0), 0)::double precision AS cache_read_cost
    FROM usage_logs ul
    LEFT JOIN channels c ON c.id = ul.channel_id
    LEFT JOIN accounts a ON a.id = ul.account_id
    WHERE ul.created_at >= $3
      AND ul.created_at < $2
      AND ul.actual_cost > 0
)
SELECT user_key,
       model,
       api_key_id,
       channel_id,
       MAX(channel_name) AS channel_name,
       account_id,
       MAX(account_name) AS account_name,
       COUNT(*) FILTER (WHERE created_at >= $1)::bigint AS current_requests,
       COALESCE(SUM(input_tokens) FILTER (WHERE created_at >= $1), 0)::bigint AS current_input_tokens,
       COALESCE(SUM(output_tokens) FILTER (WHERE created_at >= $1), 0)::bigint AS current_output_tokens,
       COALESCE(SUM(cache_creation_tokens) FILTER (WHERE created_at >= $1), 0)::bigint AS current_cache_creation_tokens,
       COALESCE(SUM(cache_read_tokens) FILTER (WHERE created_at >= $1), 0)::bigint AS current_cache_read_tokens,
       COALESCE(SUM(base_cost) FILTER (WHERE created_at >= $1), 0)::double precision AS current_base_cost,
       COALESCE(SUM(actual_cost) FILTER (WHERE created_at >= $1), 0)::double precision AS current_actual_cost,
       COALESCE(SUM(input_cost) FILTER (WHERE created_at >= $1), 0)::double precision AS current_input_cost,
       COALESCE(SUM(output_cost) FILTER (WHERE created_at >= $1), 0)::double precision AS current_output_cost,
       COALESCE(SUM(cache_creation_cost) FILTER (WHERE created_at >= $1), 0)::double precision AS current_cache_creation_cost,
       COALESCE(SUM(cache_read_cost) FILTER (WHERE created_at >= $1), 0)::double precision AS current_cache_read_cost,
       COALESCE(MAX(actual_cost) FILTER (WHERE created_at >= $1), 0)::double precision AS current_max_request_cost,
       COALESCE(MAX(CASE WHEN base_cost > 0 AND base_cost >= $5 THEN actual_cost / base_cost END)
                FILTER (WHERE created_at >= $1), 0)::double precision AS current_max_multiplier,
       COUNT(*) FILTER (WHERE created_at >= $1
                         AND base_cost > 0
                         AND base_cost >= $5
                         AND actual_cost / base_cost >= $4)::bigint AS current_high_multiplier_requests,
       COUNT(*) FILTER (WHERE created_at < $1)::bigint AS baseline_requests,
       COALESCE(SUM(input_tokens) FILTER (WHERE created_at < $1), 0)::bigint AS baseline_input_tokens,
       COALESCE(SUM(output_tokens) FILTER (WHERE created_at < $1), 0)::bigint AS baseline_output_tokens,
       COALESCE(SUM(cache_creation_tokens) FILTER (WHERE created_at < $1), 0)::bigint AS baseline_cache_creation_tokens,
       COALESCE(SUM(cache_read_tokens) FILTER (WHERE created_at < $1), 0)::bigint AS baseline_cache_read_tokens,
       COALESCE(SUM(base_cost) FILTER (WHERE created_at < $1), 0)::double precision AS baseline_base_cost,
       COALESCE(SUM(actual_cost) FILTER (WHERE created_at < $1), 0)::double precision AS baseline_actual_cost
FROM usage
GROUP BY user_key, model, api_key_id, channel_id, account_id
HAVING COUNT(*) FILTER (WHERE created_at >= $1) > 0
ORDER BY user_key, model, api_key_id, channel_id, account_id`

const costDailyQuery = `
WITH bounds AS (
    SELECT $1::timestamptz AS window_start,
           $2::timestamptz AS end_at,
           $3::timestamptz AS day_start
), usage AS (
    SELECT COALESCE(NULLIF(BTRIM(ul.user_id::text), ''), 'unknown') AS user_key,
           COALESCE(ul.api_key_id, 0)::bigint AS api_key_id,
           ul.created_at,
           GREATEST(COALESCE(ul.actual_cost, 0), 0)::double precision AS actual_cost,
           (GREATEST(COALESCE(ul.input_tokens, 0), 0)::bigint +
            GREATEST(COALESCE(ul.output_tokens, 0), 0)::bigint +
            GREATEST(COALESCE(ul.cache_creation_tokens, 0), 0)::bigint +
            GREATEST(COALESCE(ul.cache_read_tokens, 0), 0)::bigint) AS total_tokens
    FROM usage_logs ul
    CROSS JOIN bounds
    WHERE ul.created_at >= LEAST(bounds.window_start, bounds.day_start)
      AND ul.created_at < bounds.end_at
      AND ul.actual_cost > 0
)
SELECT usage.user_key,
       usage.api_key_id,
       bounds.day_start,
       COUNT(*) FILTER (WHERE usage.created_at >= GREATEST(bounds.window_start, bounds.day_start))::bigint AS window_requests,
       COALESCE(SUM(usage.total_tokens) FILTER (WHERE usage.created_at >= GREATEST(bounds.window_start, bounds.day_start)), 0)::bigint AS window_tokens,
       COALESCE(SUM(usage.actual_cost) FILTER (WHERE usage.created_at >= GREATEST(bounds.window_start, bounds.day_start)), 0)::double precision AS window_cost,
       COALESCE(SUM(usage.actual_cost) FILTER (WHERE usage.created_at >= bounds.day_start), 0)::double precision AS daily_cost
FROM usage
CROSS JOIN bounds
GROUP BY usage.user_key, usage.api_key_id, bounds.day_start
ORDER BY usage.user_key, usage.api_key_id`

type costUsageGroup struct {
	userKey                string
	model                  string
	apiKeyID               int64
	channelID              int64
	channelName            string
	accountID              int64
	accountName            string
	current                costUsageMetrics
	baseline               costUsageMetrics
	maxRequestCost         float64
	maxMultiplier          float64
	highMultiplierRequests int64
}

type costUsageMetrics struct {
	Requests            int64
	InputTokens         int64
	OutputTokens        int64
	CacheCreationTokens int64
	CacheReadTokens     int64
	BaseCost            float64
	ActualCost          float64
	InputCost           float64
	OutputCost          float64
	CacheCreationCost   float64
	CacheReadCost       float64
}

func (m costUsageMetrics) TotalTokens() int64 {
	return m.InputTokens + m.OutputTokens + m.CacheCreationTokens + m.CacheReadTokens
}

func (m costUsageMetrics) CacheableTokens() int64 {
	// input_tokens is the uncached portion of the prompt. Cache creation and
	// cache reads are both part of the cacheable context, so omitting creation
	// tokens would make a cache miss look artificially healthy.
	return m.InputTokens + m.CacheCreationTokens + m.CacheReadTokens
}

func (m costUsageMetrics) CacheHitRate() float64 {
	cacheable := m.CacheableTokens()
	if cacheable <= 0 {
		return 0
	}
	return float64(m.CacheReadTokens) * 100 / float64(cacheable)
}

func (m costUsageMetrics) UnitCost() float64 {
	return costPerMillionTokens(m.ActualCost, m.TotalTokens())
}

func (m costUsageMetrics) Multiplier() float64 {
	if m.BaseCost <= 0 {
		return 0
	}
	return m.ActualCost / m.BaseCost
}

type costDailyUsage struct {
	userKey        string
	apiKeyID       int64
	dayStart       time.Time
	windowRequests int64
	windowTokens   int64
	windowCost     float64
	dailyCost      float64
}

func (s *Store) AnalyzeCostAlerts(ctx context.Context, policy config.CostAlertConfig) ([]model.CostAlertEvent, error) {
	if !policy.Enabled {
		return nil, nil
	}
	now := time.Now().UTC()
	currentStart := now.Add(-policy.Window)
	baselineStart := now.Add(-policy.Baseline)
	groups, err := s.loadCostUsageGroups(ctx, currentStart, now, baselineStart, policy.MultiplierRatio, policy.MinBaseCost)
	if err != nil {
		return nil, fmt.Errorf("load cost usage: %w", err)
	}
	daily := map[string]costDailyUsage{}
	if policy.DailyBudget > 0 {
		localNow := now.In(time.Local)
		dayStart := time.Date(localNow.Year(), localNow.Month(), localNow.Day(), 0, 0, 0, 0, time.Local)
		daily, err = s.loadCostDailyUsage(ctx, currentStart, now, dayStart)
		if err != nil {
			return nil, fmt.Errorf("load daily cost: %w", err)
		}
	}

	candidates := make([]model.CostAlertEvent, 0)
	for _, group := range groups {
		candidates = append(candidates, evaluateCostUsageGroup(group, policy, currentStart, now)...)
	}
	candidates = append(candidates, evaluateBudgetBurn(daily, policy, now)...)
	if len(candidates) == 0 {
		return nil, nil
	}
	return s.persistCostAlertCandidates(ctx, candidates, now, policy.Cooldown)
}

func (s *Store) loadCostUsageGroups(ctx context.Context, currentStart, currentEnd, baselineStart time.Time, multiplierThreshold, minBaseCost float64) ([]costUsageGroup, error) {
	rows, err := s.db.QueryContext(ctx, costUsageQuery, currentStart, currentEnd, baselineStart, multiplierThreshold, minBaseCost)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	groups := make([]costUsageGroup, 0)
	for rows.Next() {
		var group costUsageGroup
		var current, baseline costUsageMetrics
		if err := rows.Scan(
			&group.userKey, &group.model, &group.apiKeyID, &group.channelID, &group.channelName,
			&group.accountID, &group.accountName,
			&current.Requests, &current.InputTokens, &current.OutputTokens,
			&current.CacheCreationTokens, &current.CacheReadTokens,
			&current.BaseCost, &current.ActualCost, &current.InputCost,
			&current.OutputCost, &current.CacheCreationCost, &current.CacheReadCost,
			&group.maxRequestCost, &group.maxMultiplier, &group.highMultiplierRequests,
			&baseline.Requests, &baseline.InputTokens, &baseline.OutputTokens,
			&baseline.CacheCreationTokens, &baseline.CacheReadTokens,
			&baseline.BaseCost, &baseline.ActualCost,
		); err != nil {
			return nil, err
		}
		group.current = current
		group.baseline = baseline
		groups = append(groups, group)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return groups, nil
}

func (s *Store) loadCostDailyUsage(ctx context.Context, windowStart, end, dayStart time.Time) (map[string]costDailyUsage, error) {
	rows, err := s.db.QueryContext(ctx, costDailyQuery, windowStart, end, dayStart)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	usage := make(map[string]costDailyUsage)
	for rows.Next() {
		var item costDailyUsage
		if err := rows.Scan(
			&item.userKey, &item.apiKeyID, &item.dayStart, &item.windowRequests, &item.windowTokens,
			&item.windowCost, &item.dailyCost,
		); err != nil {
			return nil, err
		}
		// Keep API keys separate. A user can have several keys, and assigning by
		// user alone would silently overwrite whichever row was read first.
		usage[costUserTargetKey(item.userKey, item.apiKeyID)] = item
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return usage, nil
}

func evaluateCostUsageGroup(group costUsageGroup, policy config.CostAlertConfig, start, end time.Time) []model.CostAlertEvent {
	current := group.current
	baseline := group.baseline
	currentTokens := current.TotalTokens()
	baselineTokens := baseline.TotalTokens()
	baseEvent := model.CostAlertEvent{
		TargetKey:            costTargetKey(group.userKey, group.apiKeyID, group.model, group.channelID, group.accountID),
		UserKey:              group.userKey,
		APIKeyID:             group.apiKeyID,
		Model:                group.model,
		ChannelName:          group.channelName,
		AccountName:          group.accountName,
		Requests:             current.Requests,
		TotalTokens:          currentTokens,
		MaxRequestCost:       group.maxRequestCost,
		InputTokens:          current.InputTokens,
		OutputTokens:         current.OutputTokens,
		CacheCreationTokens:  current.CacheCreationTokens,
		CacheReadTokens:      current.CacheReadTokens,
		CurrentCost:          current.ActualCost,
		BaselineCost:         baseline.ActualCost,
		InputCost:            current.InputCost,
		OutputCost:           current.OutputCost,
		CacheCreationCost:    current.CacheCreationCost,
		CacheReadCost:        current.CacheReadCost,
		CurrentUnitCost:      current.UnitCost(),
		BaselineUnitCost:     baseline.UnitCost(),
		CurrentCacheHitRate:  current.CacheHitRate(),
		BaselineCacheHitRate: baseline.CacheHitRate(),
		CurrentMultiplier:    current.Multiplier(),
		BaselineMultiplier:   baseline.Multiplier(),
		WindowStart:          start,
		WindowEnd:            end,
		CreatedAt:            end,
		Severity:             "warning",
	}
	baseEvent.AlertKey = baseEvent.TargetKey
	events := make([]model.CostAlertEvent, 0, 4)
	if policy.SingleRequestCost > 0 && group.maxRequestCost >= policy.SingleRequestCost {
		event := baseEvent
		event.Kind = model.CostAlertSingleRequest
		event.Title = "单条请求成本过高"
		event.Message = fmt.Sprintf(
			"用户 %s 的 %s 在 %s 出现单条成本 %.4f，已超过配置上限 %.4f。",
			group.userKey, group.model, group.channelName, group.maxRequestCost, policy.SingleRequestCost,
		)
		event.Severity = "critical"
		events = append(events, event)
	}
	if multiplierSpike(group, policy) {
		event := baseEvent
		event.Kind = model.CostAlertMultiplier
		event.Title = "实际倍率异常"
		event.Message = fmt.Sprintf(
			"用户 %s 的 %s 在 %s 最近窗口实际倍率 %.2fx，历史 %.2fx，最高单条倍率 %.2fx。",
			group.userKey, group.model, group.channelName, event.CurrentMultiplier,
			event.BaselineMultiplier, group.maxMultiplier,
		)
		event.Severity = "critical"
		events = append(events, event)
	}
	if current.Requests < int64(policy.MinRequests) || current.ActualCost < policy.MinCost || currentTokens < policy.MinTokens {
		for index := range events {
			events[index].AlertKey = events[index].Kind + "|" + events[index].TargetKey
		}
		return events
	}
	baselineReady := baseline.Requests >= int64(policy.MinRequests) && baselineTokens >= policy.MinTokens
	if baselineReady && cacheDegraded(current, baseline, policy) {
		event := baseEvent
		event.Kind = model.CostAlertCacheDegraded
		event.Title = "疑似缓存失效"
		event.Message = fmt.Sprintf(
			"用户 %s 的 %s 在 %s 最近窗口缓存命中率 %.1f%%（历史 %.1f%%），单位成本 %.4f（历史 %.4f）。",
			group.userKey, group.model, group.channelName, event.CurrentCacheHitRate,
			event.BaselineCacheHitRate, event.CurrentUnitCost, event.BaselineUnitCost,
		)
		events = append(events, event)
	}
	if baselineReady && unitCostSpike(current, baseline, policy) {
		event := baseEvent
		event.Kind = model.CostAlertUnitCost
		event.Title = "每百万 Tokens 成本异常"
		event.Message = fmt.Sprintf(
			"用户 %s 的 %s 在 %s 最近窗口单位成本 %.4f，历史基线 %.4f，当前成本 %.4f，最高单条成本 %.4f。",
			group.userKey, group.model, group.channelName, event.CurrentUnitCost,
			event.BaselineUnitCost, event.CurrentCost, event.MaxRequestCost,
		)
		if event.BaselineUnitCost > 0 && event.CurrentUnitCost >= event.BaselineUnitCost*policy.UnitCostRatio*1.5 {
			event.Severity = "critical"
		}
		events = append(events, event)
	}
	for index := range events {
		events[index].AlertKey = events[index].Kind + "|" + events[index].TargetKey
	}
	return events
}

func cacheDegraded(current, baseline costUsageMetrics, policy config.CostAlertConfig) bool {
	if current.CacheableTokens() < policy.MinTokens || baseline.CacheableTokens() < policy.MinTokens {
		return false
	}
	if baseline.CacheHitRate()/100 < policy.CacheBaselineMin || current.CacheHitRate()/100 > policy.CacheCurrentMax {
		return false
	}
	return current.UnitCost() > 0 && baseline.UnitCost() > 0 &&
		current.UnitCost() >= baseline.UnitCost()*policy.CacheCostRatio
}

func unitCostSpike(current, baseline costUsageMetrics, policy config.CostAlertConfig) bool {
	return current.TotalTokens() >= policy.MinTokens && baseline.TotalTokens() >= policy.MinTokens &&
		current.UnitCost() > 0 && baseline.UnitCost() > 0 &&
		current.UnitCost() >= baseline.UnitCost()*policy.UnitCostRatio
}

func multiplierSpike(group costUsageGroup, policy config.CostAlertConfig) bool {
	current := group.current.Multiplier()
	baseline := group.baseline.Multiplier()
	if group.current.BaseCost < policy.MinBaseCost || current <= 0 {
		return false
	}
	if baseline > 0 {
		threshold := baseline * policy.MultiplierRatio
		return current >= threshold || group.maxMultiplier >= threshold
	}
	return current >= policy.MultiplierRatio || group.highMultiplierRequests >= 2
}

func evaluateBudgetBurn(usage map[string]costDailyUsage, policy config.CostAlertConfig, now time.Time) []model.CostAlertEvent {
	if policy.DailyBudget <= 0 {
		return nil
	}
	events := make([]model.CostAlertEvent, 0)
	for userKey, item := range usage {
		if item.dayStart.IsZero() || (item.windowCost < policy.MinCost && item.dailyCost < policy.DailyBudget) {
			continue
		}
		elapsed := now.Sub(item.dayStart)
		if elapsed < 0 {
			elapsed = 0
		}
		if elapsed > 24*time.Hour {
			elapsed = 24 * time.Hour
		}
		remaining := 24*time.Hour - elapsed
		windowHours := policy.Window.Hours()
		if windowHours <= 0 {
			continue
		}
		burnRate := item.windowCost / windowHours
		allowedRate := policy.DailyBudget / 24
		projected := item.dailyCost + burnRate*remaining.Hours()
		if item.dailyCost < policy.DailyBudget*0.8 && burnRate < allowedRate*policy.BurnRatio && projected < policy.DailyBudget {
			continue
		}
		severity := "warning"
		if item.dailyCost >= policy.DailyBudget || projected >= policy.DailyBudget {
			severity = "critical"
		}
		target := costUserTargetKey(userKey, item.apiKeyID)
		events = append(events, model.CostAlertEvent{
			AlertKey:  model.CostAlertBudgetBurn + "|" + target,
			Kind:      model.CostAlertBudgetBurn,
			Severity:  severity,
			TargetKey: target,
			Title:     "消费速度过高",
			Message: fmt.Sprintf(
				"用户 %s 今日已消费 %.4f，最近窗口消费 %.4f，按当前速度预计今日消费 %.4f，预算 %.4f。",
				userKey, item.dailyCost, item.windowCost, projected, policy.DailyBudget,
			),
			UserKey:       userKey,
			APIKeyID:      item.apiKeyID,
			Requests:      item.windowRequests,
			TotalTokens:   item.windowTokens,
			CurrentCost:   item.windowCost,
			DailyCost:     item.dailyCost,
			ProjectedCost: projected,
			WindowStart:   now.Add(-policy.Window),
			WindowEnd:     now,
			CreatedAt:     now,
		})
	}
	return events
}

func costUserTargetKey(userKey string, apiKeyID int64) string {
	return fmt.Sprintf("user:%s|key:%d", userKey, apiKeyID)
}

func costTargetKey(userKey string, apiKeyID int64, modelName string, channelID, accountID int64) string {
	return fmt.Sprintf("%s|model:%s|channel:%d|account:%d", costUserTargetKey(userKey, apiKeyID), modelName, channelID, accountID)
}

func (s *Store) persistCostAlertCandidates(ctx context.Context, candidates []model.CostAlertEvent, now time.Time, cooldown time.Duration) ([]model.CostAlertEvent, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	newAlerts := make([]model.CostAlertEvent, 0, len(candidates))
	for _, candidate := range candidates {
		var lastAlerted sql.NullTime
		err := tx.QueryRowContext(ctx, `
SELECT last_alerted_at
FROM monitoring_cost_alert_states
WHERE alert_key = $1
FOR UPDATE`, candidate.AlertKey).Scan(&lastAlerted)
		if err != nil && err != sql.ErrNoRows {
			return nil, err
		}
		shouldNotify := err == sql.ErrNoRows || !lastAlerted.Valid || now.Sub(lastAlerted.Time) >= cooldown
		if err == sql.ErrNoRows {
			_, err = tx.ExecContext(ctx, `
INSERT INTO monitoring_cost_alert_states (alert_key, last_alerted_at)
VALUES ($1, CASE WHEN $2 THEN $3::timestamptz ELSE NULL END)`, candidate.AlertKey, shouldNotify, now)
		} else {
			if shouldNotify {
				_, err = tx.ExecContext(ctx, `
UPDATE monitoring_cost_alert_states
SET last_alerted_at = $2, updated_at = NOW()
WHERE alert_key = $1`, candidate.AlertKey, now)
			} else {
				_, err = tx.ExecContext(ctx, `
UPDATE monitoring_cost_alert_states
SET updated_at = NOW()
WHERE alert_key = $1`, candidate.AlertKey)
			}
		}
		if err != nil {
			return nil, err
		}
		if !shouldNotify {
			continue
		}
		var id int64
		if err := tx.QueryRowContext(ctx, `
INSERT INTO monitoring_cost_alerts
 (alert_key, kind, severity, target_key, title, message)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING id`, candidate.AlertKey, candidate.Kind, candidate.Severity,
			candidate.TargetKey, candidate.Title, candidate.Message).Scan(&id); err != nil {
			return nil, err
		}
		candidate.ID = id
		candidate.CreatedAt = now
		newAlerts = append(newAlerts, candidate)
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return newAlerts, nil
}

// DiscardCostAlertNotifications discards rows created for a failed email send.
// The analysis transaction records the cooldown before the external SMTP call;
// keeping that timestamp makes retries bounded by the configured cooldown,
// while removing the row avoids presenting an unsent email as a delivered
// alert in a future history view.
func (s *Store) DiscardCostAlertNotifications(ctx context.Context, events []model.CostAlertEvent) error {
	if len(events) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, event := range events {
		if event.AlertKey == "" {
			continue
		}
		if event.ID > 0 {
			if _, err := tx.ExecContext(ctx, `
DELETE FROM monitoring_cost_alerts
WHERE id = $1 AND alert_key = $2`, event.ID, event.AlertKey); err != nil {
				return err
			}
		}
		if event.CreatedAt.IsZero() {
			if _, err := tx.ExecContext(ctx, `
UPDATE monitoring_cost_alert_states
SET updated_at = NOW()
WHERE alert_key = $1`, event.AlertKey); err != nil {
				return err
			}
			continue
		}
		if _, err := tx.ExecContext(ctx, `
UPDATE monitoring_cost_alert_states
SET updated_at = NOW()
WHERE alert_key = $1
  AND last_alerted_at IS NOT DISTINCT FROM $2::timestamptz`, event.AlertKey, event.CreatedAt); err != nil {
			return err
		}
	}
	return tx.Commit()
}
