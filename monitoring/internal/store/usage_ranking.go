package store

import (
	"context"
	"database/sql"

	"github.com/DaisWW/Sub2ApiExt/monitoring/internal/model"
)

const accountUsageRankQuery = `
WITH aggregated AS (
    SELECT ul.account_id AS entity_id,
           COALESCE(NULLIF(a.name, ''),
                    CASE WHEN ul.account_id IS NULL THEN '未知账户' ELSE '账户 #' || ul.account_id::text END) AS name,
           ''::text AS context,
           COALESCE(a.platform, 'unknown') AS platform,
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
           COALESCE(SUM(COALESCE(ul.actual_cost, ul.total_cost, 0)), 0)::double precision AS actual_cost
      FROM usage_logs ul
      LEFT JOIN accounts a ON a.id = ul.account_id
     WHERE ul.created_at >= $1 AND ul.created_at < $2 AND ul.actual_cost > 0
     GROUP BY ul.account_id, a.name, a.platform
), ranked AS (
    SELECT aggregated.*,
           COUNT(*) OVER () AS total_items,
           SUM(requests) OVER () AS total_requests,
           SUM(total_tokens) OVER () AS all_tokens,
           SUM(actual_cost) OVER () AS all_actual_cost,
           ROW_NUMBER() OVER (ORDER BY total_tokens DESC, actual_cost DESC, entity_id) AS token_rank,
           ROW_NUMBER() OVER (ORDER BY actual_cost DESC, total_tokens DESC, entity_id) AS cost_rank,
           ROW_NUMBER() OVER (
               ORDER BY CASE WHEN total_tokens > 0 THEN actual_cost * 1000000 / total_tokens ELSE 0 END DESC,
                        actual_cost DESC, entity_id
           ) AS unit_cost_rank,
           ROW_NUMBER() OVER (
               ORDER BY input_tokens + cache_read_tokens DESC, cache_read_tokens DESC, entity_id
           ) AS cache_context_rank
      FROM aggregated
)
SELECT entity_id, CASE WHEN entity_id IS NULL THEN 'account:unknown' END AS entity_key, name, context, platform,
       requests, total_tokens, input_tokens, output_tokens, cache_creation_tokens, cache_read_tokens,
       base_cost, input_cost, output_cost, cache_creation_cost, cache_read_cost, actual_cost,
       total_items, total_requests, all_tokens, all_actual_cost
  FROM ranked
 WHERE token_rank <= $3 OR cost_rank <= $3 OR unit_cost_rank <= $3 OR cache_context_rank <= $3
 ORDER BY LEAST(token_rank, cost_rank, unit_cost_rank, cache_context_rank),
          token_rank, cost_rank, unit_cost_rank, cache_context_rank`

func (s *Store) loadAccountUsageRanks(ctx context.Context, bounds usageBounds, limit int, totalTokens int64, totalCost float64) ([]model.UsageRankItem, model.UsageDimensionMeta, error) {
	return s.loadDimensionRanks(ctx, accountUsageRankQuery, model.KindAccount, bounds, limit, totalTokens, totalCost)
}

const groupUsageRankQuery = `
WITH aggregated AS (
    SELECT ul.group_id AS entity_id,
           COALESCE(NULLIF(g.name, ''),
                    CASE WHEN ul.group_id IS NULL THEN '未分组' ELSE '分组 #' || ul.group_id::text END) AS name,
           ''::text AS context,
           COALESCE(g.platform, 'unknown') AS platform,
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
           COALESCE(SUM(COALESCE(ul.actual_cost, ul.total_cost, 0)), 0)::double precision AS actual_cost
      FROM usage_logs ul
      LEFT JOIN groups g ON g.id = ul.group_id
     WHERE ul.created_at >= $1 AND ul.created_at < $2 AND ul.actual_cost > 0
     GROUP BY ul.group_id, g.name, g.platform
), ranked AS (
    SELECT aggregated.*,
           COUNT(*) OVER () AS total_items,
           SUM(requests) OVER () AS total_requests,
           SUM(total_tokens) OVER () AS all_tokens,
           SUM(actual_cost) OVER () AS all_actual_cost,
           ROW_NUMBER() OVER (ORDER BY total_tokens DESC, actual_cost DESC, entity_id) AS token_rank,
           ROW_NUMBER() OVER (ORDER BY actual_cost DESC, total_tokens DESC, entity_id) AS cost_rank,
           ROW_NUMBER() OVER (
               ORDER BY CASE WHEN total_tokens > 0 THEN actual_cost * 1000000 / total_tokens ELSE 0 END DESC,
                        actual_cost DESC, entity_id
           ) AS unit_cost_rank,
           ROW_NUMBER() OVER (
               ORDER BY input_tokens + cache_read_tokens DESC, cache_read_tokens DESC, entity_id
           ) AS cache_context_rank
      FROM aggregated
)
SELECT entity_id, CASE WHEN entity_id IS NULL THEN 'group:unassigned' END AS entity_key, name, context, platform,
       requests, total_tokens, input_tokens, output_tokens, cache_creation_tokens, cache_read_tokens,
       base_cost, input_cost, output_cost, cache_creation_cost, cache_read_cost, actual_cost,
       total_items, total_requests, all_tokens, all_actual_cost
  FROM ranked
 WHERE token_rank <= $3 OR cost_rank <= $3 OR unit_cost_rank <= $3 OR cache_context_rank <= $3
 ORDER BY LEAST(token_rank, cost_rank, unit_cost_rank, cache_context_rank),
          token_rank, cost_rank, unit_cost_rank, cache_context_rank`

func (s *Store) loadGroupUsageRanks(ctx context.Context, bounds usageBounds, limit int, totalTokens int64, totalCost float64) ([]model.UsageRankItem, model.UsageDimensionMeta, error) {
	return s.loadDimensionRanks(ctx, groupUsageRankQuery, model.KindGroup, bounds, limit, totalTokens, totalCost)
}

const modelUsageRankQuery = `
WITH aggregated AS (
    SELECT NULL::bigint AS entity_id,
           'model:' || COALESCE(NULLIF(ul.model, ''), 'unknown') AS entity_key,
           COALESCE(NULLIF(ul.model, ''), 'unknown') AS name,
           ''::text AS context,
           ''::text AS platform,
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
           COALESCE(SUM(COALESCE(ul.actual_cost, ul.total_cost, 0)), 0)::double precision AS actual_cost
      FROM usage_logs ul
      WHERE ul.created_at >= $1 AND ul.created_at < $2 AND ul.actual_cost > 0
      GROUP BY COALESCE(NULLIF(ul.model, ''), 'unknown')
), ranked AS (
    SELECT aggregated.*,
           COUNT(*) OVER () AS total_items,
           SUM(requests) OVER () AS total_requests,
           SUM(total_tokens) OVER () AS all_tokens,
           SUM(actual_cost) OVER () AS all_actual_cost,
           ROW_NUMBER() OVER (ORDER BY total_tokens DESC, actual_cost DESC, entity_key) AS token_rank,
           ROW_NUMBER() OVER (ORDER BY actual_cost DESC, total_tokens DESC, entity_key) AS cost_rank,
           ROW_NUMBER() OVER (
               ORDER BY CASE WHEN total_tokens > 0 THEN actual_cost * 1000000 / total_tokens ELSE 0 END DESC,
                        actual_cost DESC, entity_key
           ) AS unit_cost_rank
      FROM aggregated
)
SELECT entity_id, entity_key, name, context, platform,
       requests, total_tokens, input_tokens, output_tokens, cache_creation_tokens, cache_read_tokens,
       base_cost, input_cost, output_cost, cache_creation_cost, cache_read_cost, actual_cost,
       total_items, total_requests, all_tokens, all_actual_cost
  FROM ranked
 WHERE token_rank <= $3 OR cost_rank <= $3 OR unit_cost_rank <= $3
  ORDER BY LEAST(token_rank, cost_rank, unit_cost_rank), token_rank, cost_rank, unit_cost_rank`

func (s *Store) loadModelUsageRanks(ctx context.Context, bounds usageBounds, limit int, totalTokens int64, totalCost float64) ([]model.UsageRankItem, model.UsageDimensionMeta, error) {
	return s.loadDimensionRanks(ctx, modelUsageRankQuery, model.KindModel, bounds, limit, totalTokens, totalCost)
}

func (s *Store) loadDimensionRanks(ctx context.Context, query, kind string, bounds usageBounds, limit int, totalTokens int64, totalCost float64) ([]model.UsageRankItem, model.UsageDimensionMeta, error) {
	rows, err := s.db.QueryContext(ctx, query, bounds.start, bounds.end, limit)
	if err != nil {
		return nil, model.UsageDimensionMeta{}, err
	}
	defer rows.Close()
	items := make([]model.UsageRankItem, 0)
	meta := model.UsageDimensionMeta{}
	for rows.Next() {
		var id sql.NullInt64
		var entityKey sql.NullString
		var baseCost, actualCost float64
		var totalItems, totalRequests, allTokens int64
		var allActualCost float64
		var item model.UsageRankItem
		if err := rows.Scan(
			&id, &entityKey, &item.Name, &item.Context, &item.Platform,
			&item.Requests, &item.TotalTokens, &item.InputTokens, &item.OutputTokens,
			&item.CacheCreationTokens, &item.CacheRead, &baseCost, &item.InputCost,
			&item.OutputCost, &item.CacheCreationCost, &item.CacheReadCost, &actualCost,
			&totalItems, &totalRequests, &allTokens, &allActualCost,
		); err != nil {
			return nil, model.UsageDimensionMeta{}, err
		}
		item.Kind = kind
		item.BaseCost = baseCost
		item.TotalCost = actualCost
		item.TokenCost, item.NonTokenCost, item.EffectiveRateMultiplier = usageCostBreakdown(
			item.BaseCost, item.TotalCost, item.InputCost, item.OutputCost,
			item.CacheCreationCost, item.CacheReadCost,
		)
		if id.Valid {
			idValue := id.Int64
			item.ID = &idValue
			item.Key = model.TargetKey(kind, idValue)
		} else {
			item.Key = entityKey.String
		}
		if meta.TotalItems == 0 {
			meta.TotalItems = totalItems
			meta.OmittedRequests = totalRequests
			meta.OmittedTokens = allTokens
			meta.OmittedCost = allActualCost
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, model.UsageDimensionMeta{}, err
	}
	meta.ReturnedItems = int64(len(items))
	meta.OmittedItems = meta.TotalItems - meta.ReturnedItems
	if meta.OmittedItems < 0 {
		meta.OmittedItems = 0
	}
	var returnedRequests, returnedTokens int64
	var returnedCost float64
	for _, item := range items {
		returnedRequests += item.Requests
		returnedTokens += item.TotalTokens
		returnedCost += item.TotalCost
	}
	meta.OmittedRequests -= returnedRequests
	meta.OmittedTokens -= returnedTokens
	meta.OmittedCost -= returnedCost
	if meta.OmittedRequests < 0 {
		meta.OmittedRequests = 0
	}
	if meta.OmittedTokens < 0 {
		meta.OmittedTokens = 0
	}
	if meta.OmittedCost < 0 {
		meta.OmittedCost = 0
	}
	applyUsageShares(items, totalTokens, totalCost)
	return items, meta, nil
}

func applyUsageShares(items []model.UsageRankItem, totalTokens int64, totalCost float64) {
	for index := range items {
		items[index].CacheHitRate = cacheHitRate(items[index].InputTokens, items[index].CacheRead)
		items[index].CostPerMillionTokens = costPerMillionTokens(items[index].TotalCost, items[index].TotalTokens)
		if totalTokens > 0 {
			items[index].SharePercent = float64(items[index].TotalTokens) * 100 / float64(totalTokens)
		}
		if totalCost > 0 {
			items[index].CostSharePercent = items[index].TotalCost * 100 / totalCost
		}
	}
}

func cacheHitRate(inputTokens, cacheReadTokens int64) float64 {
	cacheableInput := inputTokens + cacheReadTokens
	if cacheableInput <= 0 {
		return 0
	}
	return float64(cacheReadTokens) * 100 / float64(cacheableInput)
}
