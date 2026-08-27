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
	var err error
	result.Accounts, err = s.loadAccountUsageRanks(ctx, bounds, limit, totalTokens)
	if err != nil {
		return fmt.Errorf("load account usage ranks: %w", err)
	}
	result.Groups, err = s.loadGroupUsageRanks(ctx, bounds, limit, totalTokens)
	if err != nil {
		return fmt.Errorf("load group usage ranks: %w", err)
	}
	result.Models, err = s.loadModelUsageRanks(ctx, bounds, limit, totalTokens)
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
		return usagePeriod{key: key, label: "最近 1 小时", bucket: "hour", duration: time.Hour}, nil
	case "24h":
		return usagePeriod{key: key, label: "最近 24 小时", bucket: "hour", duration: 24 * time.Hour}, nil
	case "today":
		return usagePeriod{key: key, label: "今天", bucket: "hour"}, nil
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
	var now, todayStart time.Time
	if err := s.db.QueryRowContext(ctx, `SELECT NOW(), CURRENT_DATE::timestamptz`).Scan(&now, &todayStart); err != nil {
		return usageBounds{}, err
	}
	now = now.UTC()
	start := now.Add(-period.duration)
	if period.key == "today" {
		start = todayStart.UTC()
	}
	return usageBounds{
		period: period.key, label: period.label, bucket: period.bucket,
		start: start, end: now,
	}, nil
}

func (s *Store) loadUsageSummary(ctx context.Context, summary *model.UsageSummary, bounds usageBounds) error {
	const query = `
SELECT COUNT(*)::bigint,
       COALESCE(SUM(ul.input_tokens::bigint + ul.output_tokens::bigint + ul.cache_creation_tokens::bigint + ul.cache_read_tokens::bigint), 0)::bigint,
       COALESCE(SUM(ul.input_tokens), 0)::bigint,
       COALESCE(SUM(ul.output_tokens), 0)::bigint,
       COALESCE(SUM(ul.cache_creation_tokens), 0)::bigint,
       COALESCE(SUM(ul.cache_read_tokens), 0)::bigint,
       COALESCE(SUM(COALESCE(ul.actual_cost, ul.total_cost, 0)), 0)::double precision,
       COUNT(DISTINCT ul.account_id)::bigint,
       COUNT(DISTINCT ul.group_id)::bigint
 FROM usage_logs ul
 JOIN accounts a ON a.id = ul.account_id
                 AND a.deleted_at IS NULL
                 AND LOWER(TRIM(a.status)) = 'active'
                 AND a.schedulable = TRUE
 JOIN groups g ON g.id = ul.group_id
              AND g.deleted_at IS NULL
              AND LOWER(TRIM(g.status)) = 'active'
 WHERE ul.created_at >= $1 AND ul.created_at < $2 AND ul.actual_cost > 0`
	var cacheCreate, cacheRead int64
	if err := s.db.QueryRowContext(ctx, query, bounds.start, bounds.end).Scan(
		&summary.Requests, &summary.TotalTokens, &summary.InputTokens, &summary.OutputTokens,
		&cacheCreate, &cacheRead, &summary.TotalCost, &summary.Accounts, &summary.Groups,
	); err != nil {
		return err
	}
	summary.CacheTokens = cacheCreate + cacheRead
	summary.CacheRead = cacheRead
	return nil
}

func (s *Store) loadUsageTimeline(ctx context.Context, bounds usageBounds) ([]model.UsageBucket, error) {
	step := "1 hour"
	if bounds.bucket == "day" {
		step = "1 day"
	}
	query := fmt.Sprintf(`
	WITH buckets AS (
	    SELECT generate_series(
	        date_trunc('%s', $1::timestamptz),
	        date_trunc('%s', ($2::timestamptz - INTERVAL '1 microsecond')),
	        INTERVAL '%s'
	    ) AS start_at
	), usage AS (
	    SELECT date_trunc('%s', ul.created_at) AS start_at,
	           COUNT(*)::bigint AS requests,
	           COALESCE(SUM(ul.input_tokens::bigint + ul.output_tokens::bigint + ul.cache_creation_tokens::bigint + ul.cache_read_tokens::bigint), 0)::bigint AS total_tokens,
	           COALESCE(SUM(COALESCE(ul.actual_cost, ul.total_cost, 0)), 0)::double precision AS total_cost
	    FROM usage_logs ul
	    JOIN accounts a ON a.id = ul.account_id
	                     AND a.deleted_at IS NULL
	                     AND LOWER(TRIM(a.status)) = 'active'
	                     AND a.schedulable = TRUE
	    JOIN groups g ON g.id = ul.group_id
	                 AND g.deleted_at IS NULL
	                 AND LOWER(TRIM(g.status)) = 'active'
	    WHERE ul.created_at >= $1 AND ul.created_at < $2 AND ul.actual_cost > 0
	    GROUP BY 1
	)
SELECT buckets.start_at, COALESCE(usage.requests, 0), COALESCE(usage.total_tokens, 0), COALESCE(usage.total_cost, 0)
FROM buckets
LEFT JOIN usage USING (start_at)
ORDER BY buckets.start_at`, bounds.bucket, bounds.bucket, step, bounds.bucket)
	rows, err := s.db.QueryContext(ctx, query, bounds.start, bounds.end)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]model.UsageBucket, 0)
	for rows.Next() {
		var item model.UsageBucket
		if err := rows.Scan(&item.StartAt, &item.Requests, &item.TotalTokens, &item.TotalCost); err != nil {
			return nil, err
		}
		item.StartAt = item.StartAt.UTC()
		items = append(items, item)
	}
	return items, rows.Err()
}
