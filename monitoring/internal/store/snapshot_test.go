package store

import (
	"reflect"
	"testing"

	"github.com/DaisWW/Sub2ApiExt/monitoring/internal/model"
)

func TestBuildSnapshotFiltersInactiveTargetsAndSorts(t *testing.T) {
	accounts := map[int64]*model.Account{
		1: {ID: 1, Name: "beta", Status: "active", Schedulable: true, GroupIDs: []int64{10, 20}},
		2: {ID: 2, Name: "alpha", Status: "active", Schedulable: true, GroupIDs: []int64{10}},
		3: {ID: 3, Name: "alpha", Status: "active", Schedulable: true},
		4: {ID: 4, Name: "disabled", Status: "active", Schedulable: false, GroupIDs: []int64{10}},
		5: {ID: 5, Name: "error", Status: "error", Schedulable: true, GroupIDs: []int64{10}},
	}
	groups := map[int64]*model.Group{
		10: {ID: 10, Name: "beta", Status: "active", AccountIDs: []int64{1, 2, 4, 5}},
		20: {ID: 20, Name: "alpha", Status: "active", AccountIDs: []int64{1}},
		30: {ID: 30, Name: "alpha", Status: "active"},
		40: {ID: 40, Name: "disabled", Status: "disabled", AccountIDs: []int64{1}},
		-1: {ID: -1, Name: "Ungrouped", Status: "disabled", AccountIDs: []int64{3}},
	}

	snapshot := buildSnapshot(accounts, groups)
	if got, want := accountIDs(snapshot.Accounts), []int64{2, 3, 1, 5}; !reflect.DeepEqual(got, want) {
		t.Fatalf("account order = %v, want %v", got, want)
	}
	if got, want := groupIDs(snapshot.Groups), []int64{20, 30, 10}; !reflect.DeepEqual(got, want) {
		t.Fatalf("group order = %v, want %v", got, want)
	}
	if got, want := snapshot.Accounts[0].GroupIDs, []int64{10}; !reflect.DeepEqual(got, want) {
		t.Fatalf("active account groups = %v, want %v", got, want)
	}
	if got, want := snapshot.Groups[2].AccountIDs, []int64{1, 2, 5}; !reflect.DeepEqual(got, want) {
		t.Fatalf("active group accounts = %v, want %v", got, want)
	}
	if !snapshot.Groups[2].ProbeEnabled {
		t.Fatal("group with active accounts must remain probe enabled")
	}
	if snapshot.Groups[1].ProbeEnabled {
		t.Fatal("empty active group must not be probe enabled")
	}
}

func accountIDs(accounts []model.Account) []int64 {
	ids := make([]int64, 0, len(accounts))
	for _, account := range accounts {
		ids = append(ids, account.ID)
	}
	return ids
}

func groupIDs(groups []model.Group) []int64 {
	ids := make([]int64, 0, len(groups))
	for _, group := range groups {
		ids = append(ids, group.ID)
	}
	return ids
}
