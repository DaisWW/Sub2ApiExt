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
		"WHERE $1 = 'group:-1'",
		"monitoring_checks.checked_at >= auth.source_updated_at",
		"usage_logs.created_at >= auth.source_updated_at",
	} {
		if !strings.Contains(historyQuery, fragment) {
			t.Fatalf("history query missing %q", fragment)
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
		"VALUES ($1,$2,$3,$4,$5,$6,$7,TRUE,$8,$9,$10,$11,$12,$13,NOW())",
		"EXCLUDED.last_channel_error_at > EXCLUDED.last_channel_error_resolved_at",
		"monitoring_targets.last_channel_error_at > monitoring_targets.last_channel_error_resolved_at",
		"EXCLUDED.source_updated_at IS NULL",
		"last_channel_error_resolved_at = CASE",
		"WHEN EXCLUDED.last_channel_error_resolved_at IS NULL",
		"monitoring_targets.last_channel_error_resolved_at IS NULL",
		"monitoring_targets.last_channel_error_at",
		"source_updated_at = CASE",
		"WHEN EXCLUDED.source_updated_at IS NULL THEN monitoring_targets.source_updated_at",
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
