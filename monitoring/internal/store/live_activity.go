package store

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"time"

	"github.com/DaisWW/Sub2ApiExt/monitoring/internal/model"
)

const (
	liveActivityWindow      = 5 * time.Minute
	liveActivityDetailLimit = 20
)

const liveActivityQuery = `
WITH bounds AS (
    SELECT NOW() - ($1::bigint * INTERVAL '1 second') AS start_at,
           NOW() AS end_at
), activity AS MATERIALIZED (
    SELECT ul.user_id,
           ul.channel_id,
           ul.account_id,
           COALESCE(
               NULLIF(BTRIM(c.name), ''),
               CASE
                   WHEN ul.channel_id IS NULL THEN '未归属渠道'
                   ELSE '渠道 #' || ul.channel_id::text
               END
           ) AS channel_name,
           COALESCE(NULLIF(BTRIM(a.name), ''), '账户 #' || ul.account_id::text) AS account_name,
           ul.created_at
    FROM usage_logs ul
    CROSS JOIN bounds
    JOIN accounts a ON a.id = ul.account_id
                   AND a.deleted_at IS NULL
                   AND LOWER(TRIM(a.status)) = 'active'
                   AND a.schedulable = TRUE
    LEFT JOIN groups g ON g.id = ul.group_id
                      AND g.deleted_at IS NULL
                      AND LOWER(TRIM(g.status)) = 'active'
    LEFT JOIN channels c ON c.id = ul.channel_id
    WHERE ul.created_at >= bounds.start_at
      AND ul.created_at < bounds.end_at
      AND ul.actual_cost > 0
      AND (ul.group_id IS NULL OR g.id IS NOT NULL)
), activity_rows AS (
    SELECT 0 AS kind_order,
           'summary' AS kind,
           NULL::bigint AS channel_id,
           ''::text AS channel_name,
           NULL::bigint AS account_id,
           ''::text AS account_name,
           COUNT(*)::bigint AS requests,
           COUNT(DISTINCT user_id)::bigint AS active_users,
           COUNT(DISTINCT COALESCE(channel_id::text, 'unknown'))::bigint AS related_count,
           COUNT(DISTINCT account_id)::bigint AS secondary_count
    FROM activity
    UNION ALL
    SELECT 1,
           'channel',
           channel_id,
           channel_name,
           NULL::bigint,
           ''::text,
           COUNT(*)::bigint,
           COUNT(DISTINCT user_id)::bigint,
           COUNT(DISTINCT account_id)::bigint,
           0::bigint
    FROM activity
    GROUP BY channel_id, channel_name
    UNION ALL
    SELECT 2,
           'account',
           NULL::bigint,
           ''::text,
           account_id,
           account_name,
           COUNT(*)::bigint,
           COUNT(DISTINCT user_id)::bigint,
           COUNT(DISTINCT COALESCE(channel_id::text, 'unknown'))::bigint,
           0::bigint
    FROM activity
    GROUP BY account_id, account_name
    UNION ALL
    SELECT 3,
           'route',
           channel_id,
           channel_name,
           account_id,
           account_name,
           COUNT(*)::bigint,
           COUNT(DISTINCT user_id)::bigint,
           0::bigint,
           0::bigint
    FROM activity
    GROUP BY channel_id, channel_name, account_id, account_name
), ranked_rows AS (
    SELECT activity_rows.*,
           ROW_NUMBER() OVER (
               PARTITION BY activity_rows.kind
               ORDER BY activity_rows.active_users DESC,
                        activity_rows.requests DESC,
                        activity_rows.channel_name,
                        activity_rows.account_name,
                        activity_rows.channel_id,
                        activity_rows.account_id
           ) AS detail_rank
    FROM activity_rows
)
SELECT ranked_rows.kind_order,
       ranked_rows.kind,
       ranked_rows.channel_id,
       ranked_rows.channel_name,
       ranked_rows.account_id,
       ranked_rows.account_name,
       ranked_rows.requests,
       ranked_rows.active_users,
       ranked_rows.related_count,
       ranked_rows.secondary_count,
       bounds.start_at,
       bounds.end_at
FROM ranked_rows
CROSS JOIN bounds
WHERE ranked_rows.kind = 'summary'
   OR ranked_rows.detail_rank <= $2
ORDER BY ranked_rows.kind_order,
         ranked_rows.channel_name,
         ranked_rows.account_name,
         ranked_rows.channel_id,
         ranked_rows.account_id`

type liveActivityRow struct {
	kindOrder, requests, activeUsers, relatedCount, secondaryCount int64
	kind, channelName, accountName                                 string
	channelID, accountID                                           sql.NullInt64
	windowStart, generatedAt                                       time.Time
}

func (s *Store) LiveActivity(ctx context.Context) (model.LiveActivity, error) {
	rows, err := s.db.QueryContext(ctx, liveActivityQuery,
		int64(liveActivityWindow/time.Second), liveActivityDetailLimit)
	if err != nil {
		return model.LiveActivity{}, fmt.Errorf("load live activity: %w", err)
	}
	defer rows.Close()

	activity := model.LiveActivity{
		WindowSeconds: int(liveActivityWindow / time.Second),
		Channels:      []model.LiveActivityChannel{},
		Accounts:      []model.LiveActivityAccount{},
		Routes:        []model.LiveActivityRoute{},
	}
	initialized := false
	for rows.Next() {
		var row liveActivityRow
		if err := rows.Scan(
			&row.kindOrder, &row.kind, &row.channelID, &row.channelName, &row.accountID, &row.accountName,
			&row.requests, &row.activeUsers, &row.relatedCount, &row.secondaryCount,
			&row.windowStart, &row.generatedAt,
		); err != nil {
			return model.LiveActivity{}, fmt.Errorf("scan live activity: %w", err)
		}
		if !initialized {
			activity.WindowStart = row.windowStart.UTC()
			activity.GeneratedAt = row.generatedAt.UTC()
			initialized = true
		}
		switch row.kind {
		case "summary":
			activity.Summary = model.LiveActivitySummary{
				ActiveUsers: row.activeUsers,
				Requests:    row.requests,
				Channels:    row.relatedCount,
				Accounts:    row.secondaryCount,
			}
		case "channel":
			activity.Channels = append(activity.Channels, model.LiveActivityChannel{
				ID:          nullableID(row.channelID),
				Name:        row.channelName,
				ActiveUsers: row.activeUsers,
				Requests:    row.requests,
				Accounts:    row.relatedCount,
			})
		case "account":
			activity.Accounts = append(activity.Accounts, model.LiveActivityAccount{
				ID:          nullableID(row.accountID),
				Name:        row.accountName,
				ActiveUsers: row.activeUsers,
				Requests:    row.requests,
				Channels:    row.relatedCount,
			})
		case "route":
			activity.Routes = append(activity.Routes, model.LiveActivityRoute{
				ChannelID:   nullableID(row.channelID),
				ChannelName: row.channelName,
				AccountID:   nullableID(row.accountID),
				AccountName: row.accountName,
				ActiveUsers: row.activeUsers,
				Requests:    row.requests,
			})
		}
	}
	if err := rows.Err(); err != nil {
		return model.LiveActivity{}, fmt.Errorf("read live activity: %w", err)
	}
	if !initialized {
		now := time.Now().UTC()
		activity.GeneratedAt = now
		activity.WindowStart = now.Add(-liveActivityWindow)
	}

	sortLiveActivity(&activity)
	return activity, nil
}

func nullableID(value sql.NullInt64) *int64 {
	if !value.Valid {
		return nil
	}
	result := value.Int64
	return &result
}

func sortLiveActivity(activity *model.LiveActivity) {
	sort.SliceStable(activity.Channels, func(left, right int) bool {
		return activityRankLess(
			activity.Channels[left].ActiveUsers, activity.Channels[left].Requests, activity.Channels[left].Name,
			activity.Channels[right].ActiveUsers, activity.Channels[right].Requests, activity.Channels[right].Name,
		)
	})
	sort.SliceStable(activity.Accounts, func(left, right int) bool {
		return activityRankLess(
			activity.Accounts[left].ActiveUsers, activity.Accounts[left].Requests, activity.Accounts[left].Name,
			activity.Accounts[right].ActiveUsers, activity.Accounts[right].Requests, activity.Accounts[right].Name,
		)
	})
	sort.SliceStable(activity.Routes, func(left, right int) bool {
		leftName := activity.Routes[left].ChannelName + "\x00" + activity.Routes[left].AccountName
		rightName := activity.Routes[right].ChannelName + "\x00" + activity.Routes[right].AccountName
		return activityRankLess(
			activity.Routes[left].ActiveUsers, activity.Routes[left].Requests, leftName,
			activity.Routes[right].ActiveUsers, activity.Routes[right].Requests, rightName,
		)
	})
	if len(activity.Channels) > liveActivityDetailLimit {
		activity.Channels = activity.Channels[:liveActivityDetailLimit]
	}
	if len(activity.Accounts) > liveActivityDetailLimit {
		activity.Accounts = activity.Accounts[:liveActivityDetailLimit]
	}
	if len(activity.Routes) > liveActivityDetailLimit {
		activity.Routes = activity.Routes[:liveActivityDetailLimit]
	}
}

func activityRankLess(leftUsers, leftRequests int64, leftName string, rightUsers, rightRequests int64, rightName string) bool {
	if leftUsers != rightUsers {
		return leftUsers > rightUsers
	}
	if leftRequests != rightRequests {
		return leftRequests > rightRequests
	}
	return leftName < rightName
}
