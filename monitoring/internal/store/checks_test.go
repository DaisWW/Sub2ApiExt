package store

import (
	"strings"
	"testing"

	"github.com/DaisWW/Sub2ApiExt/monitoring/internal/model"
)

func TestHistoryQueryUsesAccountEvidenceAndGroupAggregates(t *testing.T) {
	for _, fragment := range []string{
		"WITH authorized_target AS",
		"FROM monitoring_targets",
		"target_key = $1 AND active = TRUE",
		"account_error_events AS",
		"CASE WHEN usage_logs.duration_ms >= 20000 THEN 'degraded' ELSE 'operational' END",
		"auth.kind = 'account'",
		"auth.last_channel_error_at > auth.last_channel_error_resolved_at",
		"errors.target_key, 'account', errors.account_id, NULL::bigint,",
		"FROM account_error_events",
		"auth.kind = 'account' AND monitoring_checks.source IN ('probe', 'request_error')",
		"auth.kind = 'group' AND monitoring_checks.source = 'aggregate'",
		"auth.kind = 'group'",
		"usage_logs.account_id",
		"CASE\n           WHEN usage_logs.account_id IS NULL THEN '未知账户'",
		"COALESCE(NULLIF(BTRIM(a.name), ''), CASE",
		"ELSE '账户 #' || usage_logs.account_id::text",
		"ORDER BY checked_at DESC",
	} {
		if !strings.Contains(historyQuery, fragment) {
			t.Fatalf("history query missing %q", fragment)
		}
	}
	if strings.Contains(historyQuery, "FROM ops_error_logs") {
		t.Fatal("group history must not read raw error logs directly")
	}
	if strings.Contains(historyQuery, "CASE WHEN kind = 'group' AND source = 'history'") {
		t.Fatal("history rows must not be prioritized by type before the global limit")
	}
	if strings.Contains(historyQuery, "monitoring_checks.checked_at >= auth.source_updated_at") ||
		strings.Contains(historyQuery, "usage_logs.created_at >= auth.source_updated_at") {
		t.Fatal("a current source change must not erase historical health rows")
	}
}

func TestHistoryQueryDoesNotDoubleCountPersistedAccountEvidence(t *testing.T) {
	if strings.Contains(historyQuery, "monitoring_checks.source IN ('probe', 'history'") {
		t.Fatal("persisted history watermarks must not duplicate raw usage rows")
	}
	for _, fragment := range []string{
		"consumed.source = 'request_error'",
		"consumed.checked_at = errors.created_at",
	} {
		if !strings.Contains(historyQuery, fragment) {
			t.Fatalf("unresolved request-error fallback is not deduplicated: missing %q", fragment)
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
		"last_channel_error_at IS NOT NULL AND last_channel_error_at >= $2",
		"last_channel_error_at = CASE",
		"last_channel_error_class = CASE",
		"last_channel_error_status_code = CASE",
		"last_channel_error_at IS NOT NULL AND last_channel_error_at < $2",
		"kind = 'account'",
	} {
		if !strings.Contains(clearResolvedChannelErrorQuery, fragment) {
			t.Fatalf("clear query missing %q", fragment)
		}
	}
}

func TestObservedEvidenceColumns(t *testing.T) {
	for source, want := range map[string]string{
		"history":       "last_observed_activity_at",
		"request_error": "last_observed_channel_error_at",
		"probe":         "",
		"aggregate":     "",
	} {
		if got := observedEvidenceColumn(source); got != want {
			t.Fatalf("observed column for %q = %q, want %q", source, got, want)
		}
	}
}

func TestPruneKeepsLatestEvidencePerTargetAndSource(t *testing.T) {
	for _, fragment := range []string{
		"newer_check.target_key = old_check.target_key",
		"newer_check.source = old_check.source",
		"newer_check.checked_at > old_check.checked_at",
		"newer_check.id > old_check.id",
	} {
		if !strings.Contains(pruneChecksQuery, fragment) {
			t.Fatalf("prune query must retain the latest durable state: missing %q", fragment)
		}
	}
}
