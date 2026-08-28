package store

import (
	"context"
	"fmt"
	"time"

	"github.com/DaisWW/Sub2ApiExt/monitoring/internal/model"
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
           COUNT(DISTINCT activity.user_id)::bigint AS active_users
    FROM activity
    JOIN active_targets targets
      ON targets.target_key = 'account:' || activity.account_id::text
    GROUP BY activity.account_id
    UNION ALL
    SELECT 'group:' || activity.group_id::text AS target_key,
           COUNT(DISTINCT activity.user_id)::bigint AS active_users
    FROM activity
    JOIN active_targets targets
      ON targets.target_key = 'group:' || activity.group_id::text
    WHERE activity.group_id IS NOT NULL
    GROUP BY activity.group_id
)
SELECT targets.target_key,
       COALESCE(target_counts.active_users, 0)::bigint,
       bounds.start_at,
       bounds.end_at
FROM active_targets targets
CROSS JOIN bounds
LEFT JOIN target_counts ON target_counts.target_key = targets.target_key
ORDER BY targets.target_key`

type liveActivityRow struct {
	targetKey   string
	activeUsers int64
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
		if err := rows.Scan(&row.targetKey, &row.activeUsers, &row.windowStart, &row.generatedAt); err != nil {
			return model.LiveActivity{}, fmt.Errorf("scan live activity: %w", err)
		}
		activity.Targets = append(activity.Targets, model.LiveActivityTarget{
			TargetKey:   row.targetKey,
			ActiveUsers: row.activeUsers,
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
	return activity, nil
}
