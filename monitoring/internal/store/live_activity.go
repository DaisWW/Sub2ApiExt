package store

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/DaisWW/Sub2ApiExt/monitoring/internal/model"
	"github.com/redis/go-redis/v9"
)

const liveActivityWindow = 5 * time.Minute

const liveActivityQuery = `
WITH bounds AS (
    SELECT NOW() - ($1::bigint * INTERVAL '1 second') AS start_at,
           NOW() AS end_at
), active_targets AS MATERIALIZED (
    SELECT target_key
    FROM monitoring_targets
    WHERE active = TRUE
      AND LOWER(TRIM(source_status)) = 'active'
      AND kind IN ('account', 'group')
), activity AS MATERIALIZED (
    SELECT ul.user_id,
           ul.account_id,
           ul.group_id
    FROM usage_logs ul
    CROSS JOIN bounds
    JOIN accounts a ON a.id = ul.account_id
                   AND a.deleted_at IS NULL
                   AND LOWER(TRIM(a.status)) = 'active'
                   AND a.schedulable = TRUE
    LEFT JOIN groups g ON g.id = ul.group_id
                      AND g.deleted_at IS NULL
                      AND LOWER(TRIM(g.status)) = 'active'
    WHERE ul.created_at >= bounds.start_at
      AND ul.created_at < bounds.end_at
      AND ul.actual_cost > 0
      AND (ul.group_id IS NULL OR g.id IS NOT NULL)
), target_counts AS (
    SELECT 'account:' || activity.account_id::text AS target_key,
           COUNT(DISTINCT activity.user_id)::bigint AS active_users,
           COUNT(*)::bigint AS requests
    FROM activity
    JOIN active_targets targets
      ON targets.target_key = 'account:' || activity.account_id::text
    GROUP BY activity.account_id
    UNION ALL
    SELECT 'group:' || activity.group_id::text AS target_key,
           COUNT(DISTINCT activity.user_id)::bigint AS active_users,
           COUNT(*)::bigint AS requests
    FROM activity
    JOIN active_targets targets
      ON targets.target_key = 'group:' || activity.group_id::text
    WHERE activity.group_id IS NOT NULL
    GROUP BY activity.group_id
)
SELECT targets.target_key,
       COALESCE(target_counts.active_users, 0)::bigint,
       COALESCE(target_counts.requests, 0)::bigint,
       bounds.start_at,
       bounds.end_at
FROM active_targets targets
CROSS JOIN bounds
LEFT JOIN target_counts ON target_counts.target_key = targets.target_key
ORDER BY targets.target_key`

type liveActivityRow struct {
	targetKey   string
	activeUsers int64
	requests    int64
	windowStart time.Time
	generatedAt time.Time
}

func (s *Store) LiveActivity(ctx context.Context) (model.LiveActivity, error) {
	rows, err := s.db.QueryContext(ctx, liveActivityQuery, int64(liveActivityWindow/time.Second))
	if err != nil {
		return model.LiveActivity{}, fmt.Errorf("load live activity: %w", err)
	}
	defer rows.Close()

	activity := model.LiveActivity{
		WindowSeconds: int(liveActivityWindow / time.Second),
		Targets:       []model.LiveActivityTarget{},
	}
	for rows.Next() {
		var row liveActivityRow
		if err := rows.Scan(&row.targetKey, &row.activeUsers, &row.requests, &row.windowStart, &row.generatedAt); err != nil {
			return model.LiveActivity{}, fmt.Errorf("scan live activity: %w", err)
		}
		activity.Targets = append(activity.Targets, model.LiveActivityTarget{
			TargetKey:   row.targetKey,
			ActiveUsers: row.activeUsers,
			Requests:    row.requests,
		})
		if activity.GeneratedAt.IsZero() {
			activity.WindowStart = row.windowStart.UTC()
			activity.GeneratedAt = row.generatedAt.UTC()
		}
	}
	if err := rows.Err(); err != nil {
		return model.LiveActivity{}, fmt.Errorf("read live activity: %w", err)
	}
	if activity.GeneratedAt.IsZero() {
		now := time.Now().UTC()
		activity.GeneratedAt = now
		activity.WindowStart = now.Add(-liveActivityWindow)
	}
	if err := s.addCurrentConcurrency(ctx, &activity); err == nil {
		activity.ConcurrencyOK = true
	}
	return activity, nil
}

func (s *Store) addCurrentConcurrency(ctx context.Context, activity *model.LiveActivity) error {
	if s.redis == nil {
		return fmt.Errorf("Redis is not configured")
	}
	ttl := s.concurrencySlotTTL
	if ttl <= 0 {
		ttl = 30 * time.Minute
	}
	now, err := s.redis.Time(ctx).Result()
	if err != nil {
		return fmt.Errorf("read Redis time: %w", err)
	}
	groupAccounts, err := s.loadActiveGroupAccounts(ctx)
	if err != nil {
		return err
	}

	accountIDs := make([]int64, 0, len(activity.Targets))
	accountIDSet := make(map[int64]struct{}, len(activity.Targets))
	accountIndex := make(map[int64]int, len(activity.Targets))
	addAccountID := func(id int64) {
		if _, exists := accountIDSet[id]; exists {
			return
		}
		accountIDSet[id] = struct{}{}
		accountIDs = append(accountIDs, id)
	}
	for index := range activity.Targets {
		if accountID, ok := targetEntityID(activity.Targets[index].TargetKey, model.KindAccount); ok {
			addAccountID(accountID)
			accountIndex[accountID] = index
			continue
		}
		if groupID, ok := targetEntityID(activity.Targets[index].TargetKey, model.KindGroup); ok {
			for _, accountID := range groupAccounts[groupID] {
				addAccountID(accountID)
			}
		}
	}
	if len(accountIDs) == 0 {
		return nil
	}

	regularCutoff := strconv.FormatInt(now.Add(-ttl).Unix(), 10)
	liveCutoff := strconv.FormatInt(now.Add(-time.Minute).Unix(), 10)
	pipe := s.redis.Pipeline()
	type commands struct {
		regular *redis.IntCmd
		live    *redis.IntCmd
	}
	counts := make(map[int64]commands, len(accountIDs))
	for _, accountID := range accountIDs {
		regularKey := "concurrency:account:" + strconv.FormatInt(accountID, 10)
		liveKey := "concurrency:live:account:" + strconv.FormatInt(accountID, 10)
		counts[accountID] = commands{
			regular: pipe.ZCount(ctx, regularKey, "("+regularCutoff, "+inf"),
			live:    pipe.ZCount(ctx, liveKey, "("+liveCutoff, "+inf"),
		}
	}
	if _, err := pipe.Exec(ctx); err != nil {
		return fmt.Errorf("read current concurrency: %w", err)
	}

	accountCounts := make(map[int64]int64, len(counts))
	for accountID, cmd := range counts {
		count := cmd.regular.Val() + cmd.live.Val()
		accountCounts[accountID] = count
		if index, exists := accountIndex[accountID]; exists {
			activity.Targets[index].CurrentConcurrency = count
		}
	}
	for index := range activity.Targets {
		groupID, ok := targetEntityID(activity.Targets[index].TargetKey, model.KindGroup)
		if !ok {
			continue
		}
		for _, accountID := range groupAccounts[groupID] {
			activity.Targets[index].CurrentConcurrency += accountCounts[accountID]
		}
	}
	return nil
}

func (s *Store) loadActiveGroupAccounts(ctx context.Context) (map[int64][]int64, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT ag.group_id, ag.account_id
FROM account_groups ag
JOIN accounts a ON a.id = ag.account_id
               AND a.deleted_at IS NULL
               AND LOWER(TRIM(a.status)) = 'active'
               AND a.schedulable = TRUE
JOIN groups g ON g.id = ag.group_id
             AND g.deleted_at IS NULL
             AND LOWER(TRIM(g.status)) = 'active'
ORDER BY ag.group_id, ag.account_id`)
	if err != nil {
		return nil, fmt.Errorf("load group accounts for concurrency: %w", err)
	}
	defer rows.Close()

	result := make(map[int64][]int64)
	for rows.Next() {
		var groupID, accountID int64
		if err := rows.Scan(&groupID, &accountID); err != nil {
			return nil, fmt.Errorf("scan group account for concurrency: %w", err)
		}
		result[groupID] = append(result[groupID], accountID)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read group accounts for concurrency: %w", err)
	}
	return result, nil
}

func targetEntityID(targetKey, kind string) (int64, bool) {
	prefix := kind + ":"
	if !strings.HasPrefix(targetKey, prefix) {
		return 0, false
	}
	id, err := strconv.ParseInt(strings.TrimPrefix(targetKey, prefix), 10, 64)
	return id, err == nil && id > 0
}
