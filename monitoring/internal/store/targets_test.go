package store

import (
	"testing"
	"time"

	"github.com/DaisWW/Sub2ApiExt/monitoring/internal/model"
)

func TestGroupSourceUpdatedAtIncludesLatestMemberUpdate(t *testing.T) {
	groupUpdated := time.Date(2026, 8, 27, 10, 0, 0, 0, time.UTC)
	memberUpdated := groupUpdated.Add(time.Hour)
	olderUpdated := groupUpdated.Add(-time.Hour)
	group := model.Group{ID: 9, UpdatedAt: &groupUpdated, AccountIDs: []int64{1, 2}}
	accounts := map[int64]*model.Account{
		1: {ID: 1, UpdatedAt: &memberUpdated},
		2: {ID: 2, UpdatedAt: &olderUpdated},
	}

	got := groupSourceUpdatedAt(group, accounts)
	if got == nil || !got.Equal(memberUpdated) {
		t.Fatalf("group source update = %v, want %s", got, memberUpdated)
	}
}

func TestGroupSourceUpdatedAtHandlesMissingTimestamps(t *testing.T) {
	group := model.Group{ID: 9, AccountIDs: []int64{1}}
	if got := groupSourceUpdatedAt(group, map[int64]*model.Account{1: {ID: 1}}); got != nil {
		t.Fatalf("source update = %v, want nil", got)
	}
}
