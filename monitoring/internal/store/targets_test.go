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

func TestGroupSourceUpdatedAtUsesRecentMemberActivityAfterGroupUpdate(t *testing.T) {
	activity := time.Date(2026, 8, 28, 10, 0, 0, 0, time.UTC)
	updated := activity.Add(20 * time.Second)
	group := model.Group{ID: 9, UpdatedAt: &updated, AccountIDs: []int64{1}}
	accounts := map[int64]*model.Account{
		1: {ID: 1, LastActivityAt: &activity},
	}

	got := groupSourceUpdatedAt(group, accounts)
	if got == nil || !got.Equal(activity) {
		t.Fatalf("group source update = %v, want request time %s", got, activity)
	}
}

func TestEffectiveSourceUpdatedAtUsesRecentRequestAfterAccountUpdate(t *testing.T) {
	activity := time.Date(2026, 8, 28, 10, 0, 0, 0, time.UTC)
	updated := activity.Add(15 * time.Second)

	got := effectiveSourceUpdatedAt(&updated, &activity)
	if got == nil || !got.Equal(activity) {
		t.Fatalf("effective source update = %v, want request time %s", got, activity)
	}
}

func TestEffectiveSourceUpdatedAtUsesRecentRequestBeforeAccountUpdate(t *testing.T) {
	activity := time.Date(2026, 8, 28, 10, 0, 0, 0, time.UTC)
	updated := activity.Add(-15 * time.Second)

	got := effectiveSourceUpdatedAt(&updated, &activity)
	if got == nil || !got.Equal(activity) {
		t.Fatalf("effective source update = %v, want request time %s", got, activity)
	}
}

func TestEffectiveSourceUpdatedAtKeepsLargerSourceChange(t *testing.T) {
	activity := time.Date(2026, 8, 28, 10, 0, 0, 0, time.UTC)
	updated := activity.Add(model.SourceUpdateActivityGrace + time.Second)

	got := effectiveSourceUpdatedAt(&updated, &activity)
	if got == nil || !got.Equal(updated) {
		t.Fatalf("effective source update = %v, want source update %s", got, updated)
	}
}

func TestAccountChannelErrorResolvedAtUsesSuccessfulEvidence(t *testing.T) {
	activity := time.Date(2026, 8, 28, 10, 0, 0, 0, time.UTC)
	probeAt := activity.Add(10 * time.Minute)
	updated := probeAt.Add(time.Hour)
	account := model.Account{
		LastActivityAt:  &activity,
		LastProbeAt:     &probeAt,
		LastProbeStatus: model.StatusOperational,
		UpdatedAt:       &updated,
	}

	got := accountChannelErrorResolvedAt(account)
	if got == nil || !got.Equal(updated) {
		t.Fatalf("resolved watermark = %v, want latest source update %s", got, updated)
	}
}

func TestAccountChannelErrorResolvedAtIgnoresStatusUpdateLag(t *testing.T) {
	errorAt := time.Date(2026, 8, 28, 10, 0, 0, 0, time.UTC)
	updated := errorAt.Add(time.Minute)
	account := model.Account{
		Status:                model.StatusError,
		LastChannelErrorAt:    &errorAt,
		UpdatedAt:             &updated,
		LastChannelErrorClass: "upstream_error",
	}

	if got := accountChannelErrorResolvedAt(account); got != nil {
		t.Fatalf("status bookkeeping update should not resolve channel error: %s", got)
	}
}

func TestAccountChannelErrorResolvedAtAcceptsLaterSourceChange(t *testing.T) {
	errorAt := time.Date(2026, 8, 28, 10, 0, 0, 0, time.UTC)
	updated := errorAt.Add(model.SourceUpdateActivityGrace + time.Second)
	account := model.Account{
		Status:             model.StatusError,
		LastChannelErrorAt: &errorAt,
		UpdatedAt:          &updated,
	}

	got := accountChannelErrorResolvedAt(account)
	if got == nil || !got.Equal(updated) {
		t.Fatalf("later source change watermark = %v, want %s", got, updated)
	}
}
