package store

import (
	"context"

	"github.com/DaisWW/Sub2ApiExt/monitoring/internal/model"
)

func (s *Store) loadGroupUsageRanks(ctx context.Context, bounds usageBounds, limit int, totalTokens int64) ([]model.UsageRankItem, error) {
	const query = `
WITH aggregated AS (
    SELECT ul.group_id,
           COALESCE(NULLIF(g.name, ''), '分组 #' || ul.group_id::text) AS name,
           COALESCE(g.platform, 'unknown') AS platform,
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
    GROUP BY ul.group_id, g.name, g.platform
), ranked AS (
    SELECT aggregated.*,
           ROW_NUMBER() OVER (ORDER BY total_tokens DESC, total_cost DESC, group_id) AS token_rank,
           ROW_NUMBER() OVER (ORDER BY total_cost DESC, total_tokens DESC, group_id) AS cost_rank
      FROM aggregated
)
SELECT group_id, name, platform, requests, total_tokens, total_cost
  FROM ranked
 WHERE token_rank <= $3 OR cost_rank <= $3
 ORDER BY token_rank, cost_rank`
	return s.loadDimensionRanks(ctx, query, model.KindGroup, bounds, limit, totalTokens)
}

func (s *Store) loadModelUsageRanks(ctx context.Context, bounds usageBounds, limit int, totalTokens int64) ([]model.UsageRankItem, error) {
	const query = `
WITH aggregated AS (
    SELECT COALESCE(NULLIF(ul.model, ''), 'unknown') AS name,
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
    GROUP BY COALESCE(NULLIF(ul.model, ''), 'unknown')
), ranked AS (
    SELECT aggregated.*,
           ROW_NUMBER() OVER (ORDER BY total_tokens DESC, total_cost DESC, name) AS token_rank,
           ROW_NUMBER() OVER (ORDER BY total_cost DESC, total_tokens DESC, name) AS cost_rank
      FROM aggregated
)
SELECT name, requests, total_tokens, total_cost
  FROM ranked
 WHERE token_rank <= $3 OR cost_rank <= $3
 ORDER BY token_rank, cost_rank`
	rows, err := s.db.QueryContext(ctx, query, bounds.start, bounds.end, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]model.UsageRankItem, 0)
	for rows.Next() {
		var name string
		var item model.UsageRankItem
		if err := rows.Scan(&name, &item.Requests, &item.TotalTokens, &item.TotalCost); err != nil {
			return nil, err
		}
		item.Kind, item.Key, item.Name = "model", "model:"+name, name
		items = append(items, item)
	}
	applyUsageShares(items, totalTokens)
	return items, rows.Err()
}

func (s *Store) loadDimensionRanks(ctx context.Context, query, kind string, bounds usageBounds, limit int, totalTokens int64) ([]model.UsageRankItem, error) {
	rows, err := s.db.QueryContext(ctx, query, bounds.start, bounds.end, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]model.UsageRankItem, 0)
	for rows.Next() {
		var id int64
		var name, platform string
		var item model.UsageRankItem
		if err := rows.Scan(&id, &name, &platform, &item.Requests, &item.TotalTokens, &item.TotalCost); err != nil {
			return nil, err
		}
		item.Kind, item.ID, item.Key, item.Name, item.Platform = kind, &id, model.TargetKey(kind, id), name, platform
		items = append(items, item)
	}
	applyUsageShares(items, totalTokens)
	return items, rows.Err()
}

func applyUsageShares(items []model.UsageRankItem, totalTokens int64) {
	if totalTokens <= 0 {
		return
	}
	for index := range items {
		items[index].SharePercent = float64(items[index].TotalTokens) * 100 / float64(totalTokens)
	}
}
