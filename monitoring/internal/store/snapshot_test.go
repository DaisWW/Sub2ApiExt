package store

import (
	"database/sql"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/DaisWW/Sub2ApiExt/monitoring/internal/model"
)

func TestBuildSnapshotFiltersInactiveTargetsAndSorts(t *testing.T) {
	accounts := map[int64]*model.Account{
		1: {ID: 1, Name: "beta", Priority: 9, Status: "active", Schedulable: true, GroupIDs: []int64{10, 20}},
		2: {ID: 2, Name: "alpha", Priority: 3, Status: "active", Schedulable: true, GroupIDs: []int64{10}},
		3: {ID: 3, Name: "alpha", Status: "active", Schedulable: true},
		4: {ID: 4, Name: "disabled", Status: "active", Schedulable: false, GroupIDs: []int64{10}},
		5: {ID: 5, Name: "error", Status: "error", Schedulable: true, GroupIDs: []int64{10}},
	}
	groups := map[int64]*model.Group{
		10: {ID: 10, Name: "beta", Status: "active", AccountIDs: []int64{1, 2, 4, 5}, HasActiveChannel: true},
		20: {ID: 20, Name: "alpha", Status: "active", AccountIDs: []int64{1}, HasActiveChannel: true},
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
	if got, want := snapshot.Groups[2].AccountIDs, []int64{1, 2}; !reflect.DeepEqual(got, want) {
		t.Fatalf("active group accounts = %v, want %v", got, want)
	}
	if !snapshot.Groups[2].ProbeEnabled {
		t.Fatal("group with active accounts must remain probe enabled")
	}
	if snapshot.Groups[1].ProbeEnabled {
		t.Fatal("empty active group must not be probe enabled")
	}
	if got, want := snapshot.Groups[2].Members, []model.GroupMember{
		{AccountID: 2, AccountPriority: 3},
		{AccountID: 1, AccountPriority: 9},
	}; !reflect.DeepEqual(got, want) {
		t.Fatalf("group routing metadata = %+v, want %+v", got, want)
	}
}

func TestBuildSnapshotRequiresAnEnabledChannelForGroupRouting(t *testing.T) {
	accounts := map[int64]*model.Account{
		1: {ID: 1, Status: "active", Schedulable: true, GroupIDs: []int64{10, 20}},
	}
	groups := map[int64]*model.Group{
		10: {ID: 10, Name: "with-channel", Status: "active", AccountIDs: []int64{1}, HasActiveChannel: true},
		20: {ID: 20, Name: "without-channel", Status: "active", AccountIDs: []int64{1}},
	}
	snapshot := buildSnapshot(accounts, groups)
	byID := make(map[int64]model.Group, len(snapshot.Groups))
	for _, group := range snapshot.Groups {
		byID[group.ID] = group
	}
	if !byID[10].ProbeEnabled {
		t.Fatal("group with an enabled channel and candidate must be routable")
	}
	if byID[20].ProbeEnabled {
		t.Fatal("group without an enabled channel must not be routable")
	}
}

func TestFilterGroupMembersPreservesConfiguredPriority(t *testing.T) {
	accounts := map[int64]*model.Account{
		1: {ID: 1, Priority: 30, Status: "active", Schedulable: true},
		2: {ID: 2, Priority: 10, Status: "active", Schedulable: true},
	}
	group := model.Group{
		ID:         9,
		AccountIDs: []int64{1, 2},
		Members: []model.GroupMember{
			{AccountID: 1, GroupPriority: 5},
			{AccountID: 2, GroupPriority: 1},
		},
	}
	got := filterGroupMembers(group, accounts)
	if got[0].AccountID != 2 || got[0].GroupPriority != 1 || got[0].AccountPriority != 10 {
		t.Fatalf("group members were not ordered by routing priority: %+v", got)
	}
	if got[1].AccountID != 1 || got[1].AccountPriority != 30 {
		t.Fatalf("account priority was not refreshed from account snapshot: %+v", got)
	}
}

func TestSnapshotQueryBatchesRoutingSignals(t *testing.T) {
	for _, fragment := range []string{
		"WITH recent_account_usage AS MATERIALIZED",
		"MAX(created_at) AS created_at",
		"recent_group_usage AS MATERIALIZED",
		"a.priority",
		"ag.priority",
		"INTERVAL '24 hours'",
		"channel_groups",
		"JOIN channels",
		"c.status = 'active'",
		"last_probe.error_class",
		"last_probe.status_code",
		"failure_streak",
	} {
		if !strings.Contains(snapshotQuery, fragment) {
			t.Fatalf("snapshot query missing %q", fragment)
		}
	}
	if strings.Contains(snapshotQuery, "COUNT(*)::bigint AS request_count\n    FROM usage_logs ul\n    WHERE ul.account_id = a.id") {
		t.Fatal("snapshot query must not count usage once per account-group row")
	}
}

func TestAlertStateFailureStreakIgnoresStateBeforeAccountUpdate(t *testing.T) {
	accountUpdatedAt := time.Date(2026, 8, 27, 10, 0, 0, 0, time.UTC)
	staleState := sql.NullTime{Time: accountUpdatedAt.Add(-time.Minute), Valid: true}
	currentState := sql.NullTime{Time: accountUpdatedAt.Add(time.Minute), Valid: true}
	updated := sql.NullTime{Time: accountUpdatedAt, Valid: true}

	if alertStateCurrent(staleState, updated) {
		t.Fatal("alert state from before account update must not carry failure streak")
	}
	if !alertStateCurrent(currentState, updated) {
		t.Fatal("alert state after account update should carry failure streak")
	}
	if !alertStateCurrent(currentState, sql.NullTime{}) {
		t.Fatal("account without update timestamp should accept a valid alert state")
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
