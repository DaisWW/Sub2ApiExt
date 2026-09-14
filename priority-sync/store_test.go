package main

import (
	"math"
	"strings"
	"testing"
)

func TestPriorityMetricsQueryUsesRawEvidenceAndCooldowns(t *testing.T) {
	for _, marker := range []string{
		"FROM accounts a",
		"FROM usage_logs ul",
		"usage_candidates AS",
		"usage_rows AS MATERIALIZED",
		"DISTINCT ON (account_id, request_key)",
		"COUNT(*)::bigint AS successful_requests",
		"FROM ops_error_logs oe",
		"rate_limit_reset_at",
		"temp_unschedulable_until",
		"overload_until",
		"percentile_cont(0.90)",
		"recovered_rate_limited",
		"CASE WHEN GREATEST(COALESCE(ul.input_tokens, 0), 0)",
		"COALESCE(ul.account_stats_cost, 0) > 0",
		"COALESCE(ul.total_cost, 0) > 0",
		"NULLIF(GREATEST(COALESCE(ul.account_stats_cost, 0), 0), 0)",
		"NULLIF(GREATEST(COALESCE(ul.total_cost, 0), 0), 0)",
		"NULLIF(GREATEST(COALESCE(ul.account_rate_multiplier, 0), 0), 0)",
		"COALESCE(SUM(GREATEST(COALESCE(ul.input_cost, 0), 0)",
		"COALESCE(SUM(GREATEST(COALESCE(ul.cache_read_cost, 0), 0)",
		"client_request_id",
		"REGEXP_REPLACE",
		"LOWER(REGEXP_REPLACE",
		"created_at AS success_at",
		"latest_success AS",
		"ORDER BY account_id, request_key, created_at DESC, id DESC",
		"MAX(oe.created_at) AS error_at",
		"s.success_at >= e.error_at",
		"trailing_terminal_failures",
	} {
		if !strings.Contains(priorityMetricsQuery, marker) {
			t.Errorf("query missing %q", marker)
		}
	}
	for _, marker := range []string{"requested_model", "upstream_response_model", "upstream_model", "upstream_endpoint", "ul.group_id", "ag.priority", "long_context_billing_applied", "LEFT JOIN account_groups", "input_tokens", "cache_read_tokens", "GROUP BY ul.account_id"} {
		if !strings.Contains(priorityPoolMetricsQuery, marker) {
			t.Errorf("pool query missing %q", marker)
		}
	}
	for _, marker := range []string{"WHERE a.deleted_at IS NULL", "a.schedulable = TRUE", "LOWER(TRIM(a.status)) IN ('active', 'error')"} {
		if !strings.Contains(priorityPoolMetricsQuery, marker) {
			t.Errorf("pool query missing account eligibility filter %q", marker)
		}
	}
}

func TestPriorityPoolQueryFallsBackForMissingEnhancementColumns(t *testing.T) {
	query := priorityPoolMetricsQueryForColumns(priorityPoolColumns(
		"model", "input_tokens", "output_tokens",
	))
	for _, forbidden := range []string{
		"ul.requested_model", "ul.upstream_response_model", "ul.upstream_model", "ul.upstream_endpoint", "ul.inbound_endpoint",
		"ul.group_id", "ul.long_context_billing_applied", "account_groups ag",
		"ul.cache_creation_tokens", "ul.cache_read_tokens",
		"ul.input_cost", "ul.output_cost", "ul.cache_creation_cost", "ul.cache_read_cost",
	} {
		if strings.Contains(query, forbidden) {
			t.Errorf("fallback query still references missing column %q", forbidden)
		}
	}
	if !strings.Contains(query, "'unknown'") || !strings.Contains(query, "SUM(0)") || !strings.Contains(query, "0::bigint") || !strings.Contains(query, "FALSE") {
		t.Fatalf("fallback query did not emit safe defaults: %s", query)
	}
}

func TestPriorityMetricsCompatQueryOmitsMissingColumns(t *testing.T) {
	query := priorityMetricsQueryForColumns(
		priorityPoolColumns("id", "account_id", "created_at", "client_request_id", "input_tokens", "output_tokens"),
		map[string]bool{},
	)
	for _, forbidden := range []string{
		"ul.request_id", "ul.actual_cost", "ul.total_cost", "ul.account_stats_cost",
		"ul.cache_creation_tokens", "ul.cache_read_tokens", "ul.duration_ms", "ul.first_token_ms",
		"ul.input_cost", "ul.output_cost", "ul.cache_creation_cost", "ul.cache_read_cost",
		"FROM ops_error_logs oe",
	} {
		if strings.Contains(query, forbidden) {
			t.Errorf("compat query still references missing column/table %q", forbidden)
		}
	}
	if !strings.Contains(query, "WHERE FALSE") {
		t.Fatal("missing error columns did not disable error evidence")
	}
	if strings.Contains(query, "__") {
		t.Fatalf("compat query left placeholders: %s", query)
	}
}

func TestPriorityMetricsQueriesKeepZeroValuedUsageAsSuccessEvidence(t *testing.T) {
	compat := priorityMetricsQueryForColumns(
		priorityPoolColumns("id", "account_id", "created_at"),
		map[string]bool{},
	)
	for name, query := range map[string]string{"full": priorityMetricsQuery, "compat": compat} {
		usagePrefix, _, ok := strings.Cut(query, "), usage_rows AS MATERIALIZED")
		if !ok {
			t.Fatalf("%s query has no usage candidate boundary", name)
		}
		_, filterTail, ok := strings.Cut(usagePrefix, "AND ul.created_at >= $1 AND ul.created_at < $2")
		if !ok || strings.TrimSpace(filterTail) != "" {
			t.Fatalf("%s query filters zero-valued completed usage rows: %q", name, filterTail)
		}
	}
	pool := priorityPoolMetricsQueryForColumns(priorityPoolColumns("id", "account_id", "created_at"))
	poolPrefix, _, ok := strings.Cut(pool, "), usage_rows AS MATERIALIZED")
	if !ok {
		t.Fatal("pool query has no usage candidate boundary")
	}
	whereStart := strings.Index(poolPrefix, "WHERE ul.account_id")
	if whereStart < 0 {
		t.Fatalf("pool query candidate has no account filter: %s", poolPrefix)
	}
	whereClause := poolPrefix[whereStart:]
	if strings.Contains(whereClause, "input_tokens") || strings.Contains(whereClause, "output_tokens") ||
		strings.Contains(whereClause, "cache_creation_tokens") || strings.Contains(whereClause, "cache_read_tokens") {
		t.Fatalf("pool query filters zero-valued completed usage rows: %q", whereClause)
	}
}

func TestPriorityPoolQueryExcludesZeroTokenRowsFromCost(t *testing.T) {
	query := priorityPoolMetricsQueryForColumns(priorityPoolColumns(
		"id", "account_id", "created_at", "input_tokens", "output_tokens",
		"cache_creation_tokens", "cache_read_tokens", "account_stats_cost",
		"total_cost", "actual_cost", "input_cost", "output_cost",
		"cache_creation_cost", "cache_read_cost",
	))
	selectStart := strings.Index(query, "SELECT ul.account_id")
	if selectStart < 0 {
		t.Fatalf("pool query has no account select: %s", query)
	}
	costBlock := query[selectStart:]
	for _, required := range []string{
		"THEN GREATEST(COALESCE(ul.actual_cost, 0), 0) ELSE 0 END",
		"CASE WHEN GREATEST(COALESCE(ul.input_tokens, 0), 0) > 0 THEN GREATEST(COALESCE(ul.input_cost, 0), 0)",
		"CASE WHEN GREATEST(COALESCE(ul.cache_read_tokens, 0), 0) > 0 THEN GREATEST(COALESCE(ul.cache_read_cost, 0), 0)",
	} {
		if !strings.Contains(costBlock, required) {
			t.Fatalf("pool cost aggregation missing %q in %s", required, costBlock)
		}
	}
	if strings.Contains(costBlock, "COALESCE(SUM(GREATEST(COALESCE(ul.actual_cost, 0), 0)), 0)") {
		t.Fatal("pool query still sums actual_cost for zero-token rows")
	}
}

func TestPriorityCompatAndPoolCostFallbacksClipInvalidValues(t *testing.T) {
	compat := priorityMetricsQueryForColumns(
		priorityPoolColumns("id", "account_id", "created_at", "input_tokens", "account_stats_cost", "total_cost", "actual_cost", "account_rate_multiplier"),
		map[string]bool{},
	)
	pool := priorityPoolMetricsQueryForColumns(priorityPoolColumns(
		"id", "account_id", "created_at", "input_tokens", "account_stats_cost", "total_cost", "actual_cost", "account_rate_multiplier",
	))
	for name, query := range map[string]string{"compat": compat, "pool": pool} {
		if !strings.Contains(query, "NULLIF(GREATEST(GREATEST(COALESCE(ul.account_stats_cost") ||
			!strings.Contains(query, "NULLIF(GREATEST(GREATEST(COALESCE(ul.total_cost") {
			t.Fatalf("%s query does not clip cost fallbacks: %s", name, query)
		}
		if !strings.Contains(query, "GREATEST(COALESCE(ul.account_rate_multiplier") {
			t.Fatalf("%s query does not clip the rate multiplier: %s", name, query)
		}
		if strings.Contains(query, "__") {
			t.Fatalf("%s query left placeholders: %s", name, query)
		}
	}
}

func TestPriorityRequestKeyPrefersRequestIDThenClientID(t *testing.T) {
	query := priorityRequestKeyExpr("ul", priorityPoolColumns("id", "request_id", "client_request_id"), "usage")
	requestPos := strings.Index(query, "ul.request_id")
	clientPos := strings.Index(query, "ul.client_request_id")
	if requestPos < 0 || clientPos < 0 || requestPos > clientPos {
		t.Fatalf("request key precedence is wrong: %s", query)
	}
}

func TestPriorityErrorQueryDisablesUndeduplicableSchema(t *testing.T) {
	query := priorityErrorRequestsExpr(
		priorityPoolColumns("id", "account_id", "created_at", "status_code"),
		map[string]bool{},
	)
	if !strings.Contains(query, "WHERE FALSE") || strings.Contains(query, "FROM ops_error_logs") {
		t.Fatalf("underdeduplicable error schema was not disabled: %s", query)
	}
}

func TestPriorityErrorQueryKeepsExplicitUpstreamFailuresWithoutOwnerMetadata(t *testing.T) {
	query := priorityErrorRequestsExpr(
		priorityPoolColumns("id", "account_id", "created_at", "client_request_id", "status_code", "upstream_status_code"),
		priorityPoolColumns("client_request_id"),
	)
	for _, required := range []string{"oe.upstream_status_code = 429", "oe.upstream_status_code >= 500"} {
		if !strings.Contains(query, required) {
			t.Fatalf("explicit upstream evidence missing %q from %s", required, query)
		}
		if !strings.Contains(priorityMetricsQuery, required) {
			t.Fatalf("full query missing explicit upstream evidence %q", required)
		}
	}
}

func TestPriorityErrorQueryRejectsAmbiguousRateLimitsWithoutUpstreamEvidence(t *testing.T) {
	query := priorityErrorRequestsExpr(
		priorityPoolColumns("id", "account_id", "created_at", "client_request_id", "status_code", "error_type"),
		priorityPoolColumns("client_request_id"),
	)
	if !strings.Contains(query, "WHERE FALSE") || strings.Contains(query, "FROM ops_error_logs") {
		t.Fatalf("ambiguous rate limit schema was treated as upstream evidence: %s", query)
	}
}

func TestPriorityMetricsCompatUsesRequestIDSharedByBothTables(t *testing.T) {
	query := priorityMetricsQueryForColumns(
		priorityPoolColumns("id", "account_id", "created_at", "request_id", "client_request_id"),
		priorityPoolColumns("id", "account_id", "created_at", "client_request_id", "status_code", "error_owner"),
	)
	usagePrefix, _, ok := strings.Cut(query, "), usage_rows AS MATERIALIZED")
	if !ok {
		t.Fatal("compat query has no usage candidate boundary")
	}
	if strings.Contains(usagePrefix, "ul.request_id") || !strings.Contains(usagePrefix, "ul.client_request_id") {
		t.Fatalf("usage key did not use the shared client request ID: %s", usagePrefix)
	}
	if !strings.Contains(query, "oe.client_request_id") || strings.Contains(query, "oe.request_id") {
		t.Fatalf("error key did not use the shared client request ID: %s", query)
	}
}

func TestPriorityMetricsCompatCorrelatesUsageRequestToErrorClientRequest(t *testing.T) {
	query := priorityMetricsQueryForColumns(
		priorityPoolColumns("id", "account_id", "created_at", "request_id"),
		priorityPoolColumns("id", "account_id", "created_at", "request_id", "client_request_id", "status_code", "error_owner"),
	)
	usagePrefix, _, ok := strings.Cut(query, "), usage_rows AS MATERIALIZED")
	if !ok {
		t.Fatal("compat query has no usage candidate boundary")
	}
	if !strings.Contains(usagePrefix, "ul.request_id") || strings.Contains(usagePrefix, "ul.client_request_id") {
		t.Fatalf("usage side did not use request_id: %s", usagePrefix)
	}
	if !strings.Contains(query, "oe.client_request_id") || strings.Contains(query, "NULLIF(BTRIM(oe.request_id") {
		t.Fatalf("error side did not use client_request_id: %s", query)
	}
}

func TestAttachWindowSnapshotsKeeps24hAsPrimary(t *testing.T) {
	primary := []AccountMetrics{{
		ID:                 1,
		SuccessfulRequests: 8,
		TerminalFailures:   1,
		TotalTokens:        24,
		AccountCost:        2.4,
		Pools: []PoolMetrics{{
			Key:         "openai:gpt-4o:gpt-4o:api",
			TotalTokens: 24,
			AccountCost: 2.4,
		}},
	}}
	secondary := []AccountMetrics{{
		ID:          1,
		TotalTokens: 6,
		AccountCost: 0.6,
		Pools: []PoolMetrics{{
			Key:         "openai:gpt-4o:gpt-4o:api",
			TotalTokens: 6,
			AccountCost: 0.6,
		}},
	}}

	attachWindowSnapshots(primary, nil, snapshotWindow24h)
	attachWindowSnapshots(primary, secondary, snapshotWindow6h)
	if primary[0].Window24h == nil || primary[0].Window24h.TotalTokens != 24 {
		t.Fatalf("24h snapshot missing or not primary: %+v", primary[0].Window24h)
	}
	if primary[0].Window24h.SuccessfulRequests != 8 || primary[0].Window24h.TerminalFailures != 1 {
		t.Fatalf("24h snapshot dropped request outcomes: %+v", primary[0].Window24h)
	}
	if primary[0].Window6h == nil || primary[0].Window6h.TotalTokens != 6 {
		t.Fatalf("6h snapshot not attached: %+v", primary[0].Window6h)
	}
	if primary[0].Pools[0].Window6h == nil || primary[0].Pools[0].Window6h.TotalTokens != 6 {
		t.Fatalf("pool 6h snapshot not attached: %+v", primary[0].Pools[0].Window6h)
	}
}

func TestAttachWindowSnapshotsIncludesSevenDayOnlyPools(t *testing.T) {
	primary := []AccountMetrics{{ID: 1}}
	secondary := []AccountMetrics{{
		ID:    1,
		Pools: []PoolMetrics{{Key: "openai:gpt-4o:gpt-4o:legacy", TotalTokens: 7}},
	}}
	attachWindowSnapshots(primary, secondary, snapshotWindow7d)
	if len(primary[0].Pools) != 1 || primary[0].Pools[0].Window7d == nil || primary[0].Pools[0].Window7d.TotalTokens != 7 {
		t.Fatalf("7d-only pool was not attached: %+v", primary[0].Pools)
	}
	if primary[0].Pools[0].TotalTokens != 0 || poolHasRecentEvidence(primary[0].Pools[0]) {
		t.Fatalf("7d-only pool was treated as recent evidence: %+v", primary[0].Pools[0])
	}
}

func TestAccountMetricSnapshotPreservesCacheAndCostDetails(t *testing.T) {
	account := AccountMetrics{
		ID:                       1,
		TotalTokens:              100,
		InputTokens:              70,
		OutputTokens:             10,
		CacheCreationTokens:      5,
		CacheReadTokens:          15,
		AccountCost:              10,
		ActualCost:               9,
		CostP75PerMillion:        100000,
		InputCost:                7,
		OutputCost:               1,
		CacheCreationCost:        0.5,
		CacheReadCost:            1.5,
		HasCacheReadCost:         true,
		TrailingTerminalFailures: 2,
	}
	_, fallback, hitRate, tokens := accountWindowRiskCost(account)
	if tokens != account.TotalTokens {
		t.Fatalf("legacy risk cost used %d tokens, want %d", tokens, account.TotalTokens)
	}
	if math.Abs(hitRate-15.0/90.0) > 1e-9 {
		t.Fatalf("legacy cache hit rate = %v, want %v", hitRate, 15.0/90.0)
	}
	if math.Abs(fallback-100000) > 1e-9 {
		t.Fatalf("legacy fallback miss cost = %v, want 100000", fallback)
	}
	primary := []AccountMetrics{account}
	attachWindowSnapshots(primary, nil, snapshotWindow24h)
	snapshot := primary[0].Window24h
	if snapshot == nil {
		t.Fatal("account snapshot is nil")
	}
	if snapshot.InputTokens != account.InputTokens || snapshot.OutputTokens != account.OutputTokens ||
		snapshot.CacheCreationTokens != account.CacheCreationTokens || snapshot.CacheReadTokens != account.CacheReadTokens {
		t.Fatalf("account snapshot lost token details: %+v", snapshot)
	}
	if snapshot.InputCost != account.InputCost || snapshot.OutputCost != account.OutputCost ||
		snapshot.CacheCreationCost != account.CacheCreationCost || snapshot.CacheReadCost != account.CacheReadCost {
		t.Fatalf("account snapshot lost cost details: %+v", snapshot)
	}
	if !snapshot.HasCacheReadCost || snapshot.TrailingTerminalFailures != 2 {
		t.Fatalf("account snapshot lost cache-cost provenance or failure streak: %+v", snapshot)
	}
	_, fallback, hitRate, tokens = accountWindowRiskCost(primary[0])
	if tokens != account.TotalTokens {
		t.Fatalf("risk cost used %d tokens, want %d", tokens, account.TotalTokens)
	}
	if math.Abs(hitRate-15.0/90.0) > 1e-9 {
		t.Fatalf("cache hit rate = %v, want %v", hitRate, 15.0/90.0)
	}
	if math.Abs(fallback-100000) > 1e-9 {
		t.Fatalf("fallback miss cost = %v, want 100000", fallback)
	}
}
