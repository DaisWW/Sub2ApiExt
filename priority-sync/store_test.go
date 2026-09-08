package main

import (
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
		"actual_cost > 0",
		"client_request_id",
		"REGEXP_REPLACE",
		"LOWER(REGEXP_REPLACE",
		"created_at AS success_at",
		"ORDER BY account_id, request_key, created_at DESC, id DESC",
		"MAX(oe.created_at) AS error_at",
		"s.success_at >= e.error_at",
	} {
		if !strings.Contains(priorityMetricsQuery, marker) {
			t.Errorf("query missing %q", marker)
		}
	}
}
