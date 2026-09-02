package store

import (
	"strings"
	"testing"

	"github.com/DaisWW/Sub2ApiExt/monitoring/internal/model"
)

func TestHistoryQueryRequiresActiveTarget(t *testing.T) {
	for _, fragment := range []string{
		"WITH authorized_target AS",
		"FROM monitoring_targets",
		"target_key = $1 AND active = TRUE",
		"group_request_rows AS MATERIALIZED",
		"group_request_ranked AS",
		"group_error_candidates AS",
		"account_error_events AS",
		"FROM ops_error_logs oe",
		"PARTITION BY group_id, request_key",
		"WHERE position = 1",
		"COALESCE(is_business_limited, FALSE) = FALSE",
		"COALESCE(NULLIF(upstream_status_code, 0), NULLIF(status_code, 0)) AS status_code",
		"COALESCE(NULLIF(upstream_status_code, 0), NULLIF(status_code, 0), 0) <> 429",
		"COALESCE(NULLIF(upstream_status_code, 0), NULLIF(status_code, 0), 0) >= 500",
		"CASE WHEN duration_ms >= 20000 THEN 'degraded' ELSE 'operational' END",
		"status_code, '', message, created_at, 'request_error'",
		"targets.kind = 'account'",
		"targets.last_channel_error_at > targets.last_channel_error_resolved_at",
		"target_key, 'account', account_id, NULL::bigint, 'failed'",
		"FROM account_error_events",
		"WHERE $1 = 'group:-1'",
		"monitoring_checks.checked_at >= auth.source_updated_at - INTERVAL '2 minutes'",
		"usage_logs.created_at >= auth.source_updated_at - INTERVAL '2 minutes'",
	} {
		if !strings.Contains(historyQuery, fragment) {
			t.Fatalf("history query missing %q", fragment)
		}
	}
	rowsStart := strings.Index(historyQuery, "group_request_rows AS MATERIALIZED")
	rankedStart := strings.Index(historyQuery, "group_request_ranked AS")
	errorsStart := strings.Index(historyQuery, "group_error_candidates AS")
	if rowsStart < 0 || rankedStart <= rowsStart || errorsStart <= rankedStart {
		t.Fatalf("history query must rank all request rows before filtering failures: rows=%d ranked=%d errors=%d", rowsStart, rankedStart, errorsStart)
	}
}

func TestHistoryQuerySuppressesIntermediateGroupErrors(t *testing.T) {
	for _, fragment := range []string{
		"successful_group_request_keys AS MATERIALIZED",
		"regexp_replace(usage.request_id, '^client:', '')",
		"AND NOT EXISTS (",
		"success.request_key IN (group_request_ranked.request_id, group_request_ranked.client_request_id)",
	} {
		if !strings.Contains(historyQuery, fragment) {
			t.Fatalf("history query must suppress errors followed by a final success: missing %q", fragment)
		}
	}
}

func TestSyncTargetsPersistsSourceUpdatedAt(t *testing.T) {
	for _, fragment := range []string{
		"source_updated_at",
		"last_channel_error_at",
		"last_channel_error_class",
		"last_channel_error_resolved_at",
		"last_activity_at = CASE",
		"WHEN EXCLUDED.last_activity_at IS NULL THEN monitoring_targets.last_activity_at",
		"EXCLUDED.last_activity_at >= monitoring_targets.last_activity_at",
		"VALUES ($1,$2,$3,$4,$5,$6,$7,TRUE,$8,$9,$10,$11,$12,$13,$14,NOW())",
		"source_fingerprint = EXCLUDED.source_fingerprint",
		"EXCLUDED.source_fingerprint IS DISTINCT FROM monitoring_targets.source_fingerprint",
		"THEN EXCLUDED.source_updated_at",
		"ELSE monitoring_targets.source_updated_at",
		"EXCLUDED.last_channel_error_at > EXCLUDED.last_channel_error_resolved_at",
		"monitoring_targets.last_channel_error_at > monitoring_targets.last_channel_error_resolved_at",
		"EXCLUDED.source_updated_at IS NULL",
		"last_channel_error_resolved_at = CASE",
		"WHEN EXCLUDED.last_channel_error_resolved_at IS NULL",
		"monitoring_targets.last_channel_error_resolved_at IS NULL",
		"monitoring_targets.last_channel_error_at",
		"source_updated_at = CASE",
	} {
		if !strings.Contains(syncTargetsUpsert, fragment) {
			t.Fatalf("sync targets upsert missing %q", fragment)
		}
	}
}

func TestProbeResolutionClearsOnlySuccessfulAccountProbe(t *testing.T) {
	if !probeResolvesChannelError(model.ProbeResult{Source: "probe", Status: model.StatusOperational}) {
		t.Fatal("successful probe should clear its channel error")
	}
	for _, result := range []model.ProbeResult{
		{Source: "probe", Status: model.StatusFailed},
		{Source: "probe", Status: model.StatusError},
		{Source: "history", Status: model.StatusOperational},
	} {
		if probeResolvesChannelError(result) {
			t.Fatalf("result should not clear channel error: %+v", result)
		}
	}
	for _, fragment := range []string{
		"UPDATE monitoring_targets",
		"last_channel_error_resolved_at = CASE",
		"last_channel_error_at = CASE",
		"last_channel_error_class = CASE",
		"last_channel_error_status_code = CASE",
		"last_channel_error_at IS NOT NULL AND last_channel_error_at <= $2",
		"kind = 'account'",
	} {
		if !strings.Contains(clearResolvedChannelErrorQuery, fragment) {
			t.Fatalf("clear query missing %q", fragment)
		}
	}
}
