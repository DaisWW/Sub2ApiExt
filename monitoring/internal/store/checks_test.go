package store

import (
	"strings"
	"testing"
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
		"VALUES ($1,$2,$3,$4,$5,$6,$7,TRUE,$8,$9,NOW())",
		"source_updated_at = EXCLUDED.source_updated_at",
	} {
		if !strings.Contains(syncTargetsUpsert, fragment) {
			t.Fatalf("sync targets upsert missing %q", fragment)
		}
	}
}
