package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/DaisWW/Sub2ApiExt/monitoring/internal/model"
)

var ErrInvalidUsagePeriod = errors.New("invalid usage period")

type usagePeriod struct {
	key      string
	label    string
	bucket   string
	duration time.Duration
}

type usageBounds struct {
	period string
	label  string
	bucket string
	start  time.Time
	end    time.Time
}

func (s *Store) UsageRanking(ctx context.Context, period string, limit int) (model.UsageRanking, error) {
	if limit <= 0 || limit > 50 {
		limit = 10
	}
	bounds, err := s.usageBounds(ctx, period)
	if err != nil {
		return model.UsageRanking{}, err
	}
	result := newUsageRanking(bounds)
	if err := s.loadUsageOverview(ctx, &result, bounds); err != nil {
		return model.UsageRanking{}, err
	}
	if err := s.loadUsageRanks(ctx, &result, bounds, limit); err != nil {
		return model.UsageRanking{}, err
	}
	return result, nil
}

func newUsageRanking(bounds usageBounds) model.UsageRanking {
	return model.UsageRanking{
		GeneratedAt: time.Now().UTC(),
		Period:      bounds.period,
		PeriodLabel: bounds.label,
		Bucket:      bounds.bucket,
		StartAt:     bounds.start,
		EndAt:       bounds.end,
		Timeline:    []model.UsageBucket{},
		Accounts:    []model.UsageRankItem{},
		Groups:      []model.UsageRankItem{},
		Models:      []model.UsageRankItem{},
	}
}

func (s *Store) loadUsageOverview(ctx context.Context, result *model.UsageRanking, bounds usageBounds) error {
	if err := s.loadUsageSummary(ctx, &result.Summary, bounds); err != nil {
		return fmt.Errorf("load usage summary: %w", err)
	}
	timeline, err := s.loadUsageTimeline(ctx, bounds)
	if err != nil {
		return fmt.Errorf("load usage timeline: %w", err)
	}
	result.Timeline = timeline
	return nil
}

func (s *Store) loadUsageRanks(ctx context.Context, result *model.UsageRanking, bounds usageBounds, limit int) error {
	totalTokens := result.Summary.TotalTokens
	totalCost := result.Summary.TotalCost
	var err error
	result.Accounts, result.DimensionMeta.Accounts, err = s.loadAccountUsageRanks(ctx, bounds, limit, totalTokens, totalCost)
	if err != nil {
		return fmt.Errorf("load account usage ranks: %w", err)
	}
	result.Groups, result.DimensionMeta.Groups, err = s.loadGroupUsageRanks(ctx, bounds, limit, totalTokens, totalCost)
	if err != nil {
		return fmt.Errorf("load group usage ranks: %w", err)
	}
	result.Models, result.DimensionMeta.Models, err = s.loadModelUsageRanks(ctx, bounds, limit, totalTokens, totalCost)
	if err != nil {
		return fmt.Errorf("load model usage ranks: %w", err)
	}
	return nil
}

func parseUsagePeriod(value string) (usagePeriod, error) {
	key := strings.ToLower(strings.TrimSpace(value))
	if key == "" {
		key = "24h"
	}
	switch key {
	case "1h":
		return usagePeriod{key: key, label: "最近 1 小时", bucket: "minute", duration: time.Hour}, nil
	case "24h":
		return usagePeriod{key: key, label: "最近 24 小时", bucket: "hour", duration: 24 * time.Hour}, nil
	case "today":
		return usagePeriod{key: key, label: "今天", bucket: "hour"}, nil
	case "yesterday":
		return usagePeriod{key: key, label: "昨天", bucket: "hour"}, nil
	case "7d":
		return usagePeriod{key: key, label: "最近 1 周", bucket: "day", duration: 7 * 24 * time.Hour}, nil
	case "15d":
		return usagePeriod{key: key, label: "最近半个月", bucket: "day", duration: 15 * 24 * time.Hour}, nil
	case "30d":
		return usagePeriod{key: key, label: "最近 1 个月", bucket: "day", duration: 30 * 24 * time.Hour}, nil
	default:
		return usagePeriod{}, ErrInvalidUsagePeriod
	}
}

func (s *Store) usageBounds(ctx context.Context, value string) (usageBounds, error) {
	period, err := parseUsagePeriod(value)
	if err != nil {
		return usageBounds{}, err
	}
	var now, todayStart, yesterdayStart time.Time
	if err := s.db.QueryRowContext(ctx, `SELECT NOW(), CURRENT_DATE::timestamptz, (CURRENT_DATE - 1)::timestamptz`).Scan(&now, &todayStart, &yesterdayStart); err != nil {
		return usageBounds{}, err
	}
	return resolveUsageBounds(period, now, todayStart, yesterdayStart), nil
}

func resolveUsageBounds(period usagePeriod, now, todayStart, yesterdayStart time.Time) usageBounds {
	now = now.UTC()
	todayStart = todayStart.UTC()
	yesterdayStart = yesterdayStart.UTC()
	start := now.Add(-period.duration)
	end := now
	if period.key == "today" {
		start = todayStart
	}
	if period.key == "yesterday" {
		start = yesterdayStart
		end = todayStart
	}
	return usageBounds{
		period: period.key, label: period.label, bucket: period.bucket,
		start: start, end: end,
	}
}

const usageSummaryQuery = `
SELECT COUNT(*)::bigint,
       COALESCE(SUM(COALESCE(ul.input_tokens, 0)::bigint +
                    COALESCE(ul.output_tokens, 0)::bigint +
                    COALESCE(ul.cache_creation_tokens, 0)::bigint +
                    COALESCE(ul.cache_read_tokens, 0)::bigint), 0)::bigint,
       COALESCE(SUM(ul.input_tokens), 0)::bigint,
       COALESCE(SUM(ul.output_tokens), 0)::bigint,
       COALESCE(SUM(ul.cache_creation_tokens), 0)::bigint,
       COALESCE(SUM(ul.cache_read_tokens), 0)::bigint,
       COALESCE(SUM(COALESCE(ul.total_cost, ul.actual_cost, 0)), 0)::double precision,
       COALESCE(SUM(COALESCE(ul.input_cost, 0)), 0)::double precision,
       COALESCE(SUM(COALESCE(ul.output_cost, 0)), 0)::double precision,
       COALESCE(SUM(COALESCE(ul.cache_creation_cost, 0)), 0)::double precision,
       COALESCE(SUM(COALESCE(ul.cache_read_cost, 0)), 0)::double precision,
       COALESCE(SUM(COALESCE(ul.actual_cost, ul.total_cost, 0)), 0)::double precision,
       COUNT(DISTINCT ul.account_id)::bigint,
       COUNT(DISTINCT ul.group_id)::bigint
 FROM usage_logs ul
 WHERE ul.created_at >= $1 AND ul.created_at < $2 AND ul.actual_cost > 0`

func (s *Store) loadUsageSummary(ctx context.Context, summary *model.UsageSummary, bounds usageBounds) error {
	var cacheCreate, cacheRead int64
	var baseCost, actualCost float64
	if err := s.db.QueryRowContext(ctx, usageSummaryQuery, bounds.start, bounds.end).Scan(
		&summary.Requests, &summary.TotalTokens, &summary.InputTokens, &summary.OutputTokens,
		&cacheCreate, &cacheRead, &baseCost, &summary.InputCost, &summary.OutputCost,
		&summary.CacheCreationCost, &summary.CacheReadCost, &actualCost,
		&summary.Accounts, &summary.Groups,
	); err != nil {
		return err
	}
	summary.CacheTokens = cacheCreate + cacheRead
	summary.CacheRead = cacheRead
	summary.BaseCost = baseCost
	summary.TotalCost = actualCost
	summary.TokenCost, summary.NonTokenCost, summary.EffectiveRateMultiplier = usageCostBreakdown(
		summary.BaseCost, summary.TotalCost, summary.InputCost, summary.OutputCost,
		summary.CacheCreationCost, summary.CacheReadCost,
	)
	summary.CostPerMillionTokens = costPerMillionTokens(summary.TotalCost, summary.TotalTokens)
	return nil
}

const usageTimelineQuery = `
	WITH buckets AS (
	    SELECT generate_series(
	        date_trunc('%s', $1::timestamptz),
	        date_trunc('%s', ($2::timestamptz - INTERVAL '1 microsecond')),
	        INTERVAL '%s'
	    ) AS start_at
	), usage AS (
	    SELECT date_trunc('%s', ul.created_at) AS start_at,
	           COALESCE(NULLIF(BTRIM(c.name), ''), '未归属渠道') AS channel_name,
	           COUNT(*)::bigint AS requests,
	           COALESCE(SUM(COALESCE(ul.input_tokens, 0)::bigint +
	                        COALESCE(ul.output_tokens, 0)::bigint +
	                        COALESCE(ul.cache_creation_tokens, 0)::bigint +
	                        COALESCE(ul.cache_read_tokens, 0)::bigint), 0)::bigint AS total_tokens,
	           COALESCE(SUM(ul.input_tokens), 0)::bigint AS input_tokens,
	           COALESCE(SUM(ul.output_tokens), 0)::bigint AS output_tokens,
	           COALESCE(SUM(ul.cache_creation_tokens), 0)::bigint AS cache_creation_tokens,
	           COALESCE(SUM(ul.cache_read_tokens), 0)::bigint AS cache_read_tokens,
	           COALESCE(SUM(COALESCE(ul.total_cost, ul.actual_cost, 0)), 0)::double precision AS base_cost,
	           COALESCE(SUM(COALESCE(ul.input_cost, 0)), 0)::double precision AS input_cost,
	           COALESCE(SUM(COALESCE(ul.output_cost, 0)), 0)::double precision AS output_cost,
	           COALESCE(SUM(COALESCE(ul.cache_creation_cost, 0)), 0)::double precision AS cache_creation_cost,
	           COALESCE(SUM(COALESCE(ul.cache_read_cost, 0)), 0)::double precision AS cache_read_cost,
	           COALESCE(SUM(COALESCE(ul.actual_cost, ul.total_cost, 0)), 0)::double precision AS total_cost
	    FROM usage_logs ul
	    LEFT JOIN channels c ON c.id = ul.channel_id
	    WHERE ul.created_at >= $1 AND ul.created_at < $2 AND ul.actual_cost > 0
	    GROUP BY 1, 2
	)
SELECT buckets.start_at,
       COALESCE(usage.channel_name, ''),
       COALESCE(usage.requests, 0),
       COALESCE(usage.total_tokens, 0),
       COALESCE(usage.input_tokens, 0),
       COALESCE(usage.output_tokens, 0),
       COALESCE(usage.cache_creation_tokens, 0),
       COALESCE(usage.cache_read_tokens, 0),
       COALESCE(usage.base_cost, 0),
       COALESCE(usage.input_cost, 0),
       COALESCE(usage.output_cost, 0),
       COALESCE(usage.cache_creation_cost, 0),
       COALESCE(usage.cache_read_cost, 0),
       COALESCE(usage.total_cost, 0)
FROM buckets
LEFT JOIN usage USING (start_at)
ORDER BY buckets.start_at, usage.channel_name`

func (s *Store) loadUsageTimeline(ctx context.Context, bounds usageBounds) ([]model.UsageBucket, error) {
	step := "1 hour"
	if bounds.bucket == "minute" {
		step = "1 minute"
	}
	if bounds.bucket == "day" {
		step = "1 day"
	}
	query := fmt.Sprintf(usageTimelineQuery, bounds.bucket, bounds.bucket, step, bounds.bucket)
	rows, err := s.db.QueryContext(ctx, query, bounds.start, bounds.end)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]model.UsageBucket, 0)
	for rows.Next() {
		var startAt time.Time
		var channel model.UsageChannelBucket
		if err := rows.Scan(
			&startAt, &channel.Name, &channel.Requests, &channel.TotalTokens,
			&channel.InputTokens, &channel.OutputTokens, &channel.CacheCreationTokens,
			&channel.CacheRead, &channel.BaseCost, &channel.InputCost, &channel.OutputCost,
			&channel.CacheCreationCost, &channel.CacheReadCost, &channel.TotalCost,
		); err != nil {
			return nil, err
		}
		startAt = startAt.UTC()
		channel.TokenCost, channel.NonTokenCost, channel.EffectiveRateMultiplier = usageCostBreakdown(
			channel.BaseCost, channel.TotalCost, channel.InputCost, channel.OutputCost,
			channel.CacheCreationCost, channel.CacheReadCost,
		)
		if len(items) == 0 || !items[len(items)-1].StartAt.Equal(startAt) {
			items = append(items, model.UsageBucket{StartAt: startAt, Channels: []model.UsageChannelBucket{}})
		}
		item := &items[len(items)-1]
		item.Requests += channel.Requests
		item.TotalTokens += channel.TotalTokens
		item.InputTokens += channel.InputTokens
		item.OutputTokens += channel.OutputTokens
		item.CacheCreationTokens += channel.CacheCreationTokens
		item.CacheRead += channel.CacheRead
		item.BaseCost += channel.BaseCost
		item.TotalCost += channel.TotalCost
		item.InputCost += channel.InputCost
		item.OutputCost += channel.OutputCost
		item.CacheCreationCost += channel.CacheCreationCost
		item.CacheReadCost += channel.CacheReadCost
		if channel.Name == "" {
			continue
		}
		channel.CostPerMillionTokens = costPerMillionTokens(channel.TotalCost, channel.TotalTokens)
		item.Channels = append(item.Channels, channel)
	}
	for index := range items {
		items[index].TokenCost, items[index].NonTokenCost, items[index].EffectiveRateMultiplier = usageCostBreakdown(
			items[index].BaseCost, items[index].TotalCost, items[index].InputCost,
			items[index].OutputCost, items[index].CacheCreationCost, items[index].CacheReadCost,
		)
		items[index].CostPerMillionTokens = costPerMillionTokens(items[index].TotalCost, items[index].TotalTokens)
	}
	return items, rows.Err()
}

func costPerMillionTokens(totalCost float64, totalTokens int64) float64 {
	if totalTokens <= 0 {
		return 0
	}
	return totalCost * 1_000_000 / float64(totalTokens)
}

func usageCostBreakdown(baseCost, actualCost, inputCost, outputCost, cacheCreationCost, cacheReadCost float64) (tokenCost, nonTokenCost, effectiveRateMultiplier float64) {
	tokenCost = inputCost + outputCost + cacheCreationCost + cacheReadCost
	if tokenCost < 0 {
		tokenCost = 0
	}
	nonTokenCost = baseCost - tokenCost
	if nonTokenCost < 0 {
		nonTokenCost = 0
	}
	if baseCost > 0 {
		effectiveRateMultiplier = actualCost / baseCost
	}
	return tokenCost, nonTokenCost, effectiveRateMultiplier
}
