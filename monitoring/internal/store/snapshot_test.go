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

func TestAccountStatusesAreNormalizedForMonitoringTargets(t *testing.T) {
	for _, status := range []string{" ACTIVE ", "Error"} {
		account := model.Account{Status: status, Schedulable: true}
		if !accountIsMonitored(account) || !accountHistoryEligible(account) {
			t.Fatalf("status %q was not recognized as a monitored account", status)
		}
	}
	if accountIsMonitored(model.Account{Status: "error", Schedulable: false}) {
		t.Fatal("disabled error account must not be monitored")
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

func TestLinkGroupPreservesSourceUpdatedAt(t *testing.T) {
	updated := time.Date(2026, 8, 27, 10, 0, 0, 0, time.UTC)
	row := snapshotRow{
		groupID:               sql.NullInt64{Int64: 7, Valid: true},
		groupName:             sql.NullString{String: "group", Valid: true},
		groupStatus:           sql.NullString{String: "active", Valid: true},
		groupUpdatedAt:        sql.NullTime{Time: updated, Valid: true},
		groupHasActiveChannel: true,
	}
	account := &model.Account{ID: 3}
	groups := make(map[int64]*model.Group)

	linkGroup(row, account, groups)

	group := groups[7]
	if group == nil {
		t.Fatal("group was not linked")
	}
	if group.UpdatedAt == nil || !group.UpdatedAt.Equal(updated) {
		t.Fatalf("group source update time = %#v, want %s", group.UpdatedAt, updated)
	}
}

func TestBuildSnapshotKeepsFilteredMemberUpdateForGroupEvidence(t *testing.T) {
	groupUpdated := time.Date(2026, 8, 27, 10, 0, 0, 0, time.UTC)
	filteredMemberUpdated := groupUpdated.Add(time.Hour)
	accounts := map[int64]*model.Account{
		1: {ID: 1, Status: "active", Schedulable: true, GroupIDs: []int64{7}, UpdatedAt: &groupUpdated},
		2: {ID: 2, Status: "error", Schedulable: true, GroupIDs: []int64{7}, UpdatedAt: &filteredMemberUpdated},
	}
	groups := map[int64]*model.Group{
		7: {ID: 7, Status: "active", AccountIDs: []int64{1, 2}, UpdatedAt: &groupUpdated},
	}

	snapshot := buildSnapshot(accounts, groups)
	if len(snapshot.Groups) != 1 {
		t.Fatalf("groups = %d, want 1", len(snapshot.Groups))
	}
	group := snapshot.Groups[0]
	if !reflect.DeepEqual(group.AccountIDs, []int64{1}) {
		t.Fatalf("routable account IDs = %v, want [1]", group.AccountIDs)
	}
	if group.UpdatedAt == nil || !group.UpdatedAt.Equal(filteredMemberUpdated) {
		t.Fatalf("group source update = %v, want %s", group.UpdatedAt, filteredMemberUpdated)
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
		"LOWER(TRIM(c.status)) = 'active'",
		"LOWER(TRIM(p.status)) = 'active'",
		"p.expires_at > NOW()",
		"last_probe.error_class",
		"last_probe.status_code",
		"LEFT JOIN LATERAL (",
		"monitoring_targets persisted_target",
		"persisted_target.active = TRUE",
		"persisted_target.last_channel_error_at",
		"persisted_target.last_channel_error_resolved_at",
		"UNION ALL",
		"persisted_target.source_updated_at",
		"ops_error_logs",
		"oe.created_at >= NOW() - INTERVAL '24 hours'",
		"oe.created_at >= persisted_target.source_updated_at - INTERVAL '2 minutes'",
		"oe.created_at > persisted_target.last_channel_error_resolved_at",
		"oe.is_business_limited",
		"LOWER(BTRIM(COALESCE(oe.error_owner, ''))) = 'provider'",
		"IN ('account_auth', 'network', 'upstream')",
		"IN ('upstream_http', 'upstream_network')",
		"COALESCE(NULLIF(oe.upstream_status_code, 0), oe.status_code, 0) <> 429",
		"channel_error.created_at",
		"failure_streak",
		"g.updated_at",
	} {
		if !strings.Contains(snapshotQuery, fragment) {
			t.Fatalf("snapshot query missing %q", fragment)
		}
	}
	if strings.Contains(snapshotQuery, "COUNT(*)::bigint AS request_count\n    FROM usage_logs ul\n    WHERE ul.account_id = a.id") {
		t.Fatal("snapshot query must not count usage once per account-group row")
	}
}

func TestSnapshotRowCarriesChannelErrorTrigger(t *testing.T) {
	errorAt := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	row := snapshotRow{
		id:                    sql.NullInt64{Int64: 9, Valid: true},
		status:                sql.NullString{String: "active", Valid: true},
		schedulable:           true,
		lastChannelError:      sql.NullTime{Time: errorAt, Valid: true},
		lastChannelErrorClass: sql.NullString{String: "upstream_5xx", Valid: true},
		lastChannelErrorCode:  sql.NullInt64{Int64: 502, Valid: true},
		credentials:           []byte(`{}`),
	}
	account, err := row.account()
	if err != nil {
		t.Fatal(err)
	}
	if account.LastChannelErrorAt == nil || !account.LastChannelErrorAt.Equal(errorAt) ||
		account.LastChannelErrorClass != "upstream_5xx" || account.LastChannelErrorStatusCode == nil || *account.LastChannelErrorStatusCode != 502 {
		t.Fatalf("channel error evidence = %+v", account)
	}
}

func TestSnapshotRowDropsChannelErrorAfterSuccessfulEvidence(t *testing.T) {
	errorAt := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	probeAt := errorAt.Add(time.Minute)
	row := snapshotRow{
		id:                    sql.NullInt64{Int64: 11, Valid: true},
		status:                sql.NullString{String: "active", Valid: true},
		schedulable:           true,
		lastProbe:             sql.NullTime{Time: probeAt, Valid: true},
		lastProbeStatus:       sql.NullString{String: model.StatusOperational, Valid: true},
		lastChannelError:      sql.NullTime{Time: errorAt, Valid: true},
		lastChannelErrorClass: sql.NullString{String: "upstream_error", Valid: true},
		lastChannelErrorCode:  sql.NullInt64{Int64: 502, Valid: true},
		credentials:           []byte(`{}`),
	}
	account, err := row.account()
	if err != nil {
		t.Fatal(err)
	}
	if account.LastChannelErrorAt != nil || account.LastChannelErrorClass != "" || account.LastChannelErrorStatusCode != nil {
		t.Fatalf("resolved channel error was retained: %+v", account)
	}
}

func TestSnapshotRowDropsChannelErrorAfterSuccessfulRequest(t *testing.T) {
	errorAt := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	activity := errorAt.Add(time.Minute)
	row := snapshotRow{
		id:                    sql.NullInt64{Int64: 12, Valid: true},
		status:                sql.NullString{String: "active", Valid: true},
		schedulable:           true,
		lastActivity:          sql.NullTime{Time: activity, Valid: true},
		lastChannelError:      sql.NullTime{Time: errorAt, Valid: true},
		lastChannelErrorClass: sql.NullString{String: "upstream_error", Valid: true},
		credentials:           []byte(`{}`),
	}
	account, err := row.account()
	if err != nil {
		t.Fatal(err)
	}
	if account.LastChannelErrorAt != nil {
		t.Fatalf("resolved request error was retained: %+v", account)
	}
}

func TestChannelErrorIsNotResolvedByEvidenceAtSameTimestamp(t *testing.T) {
	at := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name       string
		activityAt *time.Time
		probeAt    *time.Time
	}{
		{name: "request", activityAt: &at},
		{name: "probe", probeAt: &at},
	} {
		t.Run(test.name, func(t *testing.T) {
			if channelErrorResolved(at, test.activityAt, test.probeAt, model.StatusOperational) {
				t.Fatal("evidence sharing an error timestamp must not hide the error")
			}
		})
	}
}

func TestSnapshotRowUsesRecentRequestAsEffectiveUpdate(t *testing.T) {
	activity := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	updated := activity.Add(10 * time.Second)
	row := snapshotRow{
		id:           sql.NullInt64{Int64: 10, Valid: true},
		status:       sql.NullString{String: "active", Valid: true},
		schedulable:  true,
		updatedAt:    sql.NullTime{Time: updated, Valid: true},
		lastActivity: sql.NullTime{Time: activity, Valid: true},
		credentials:  []byte(`{}`),
	}
	account, err := row.account()
	if err != nil {
		t.Fatal(err)
	}
	if account.UpdatedAt == nil || !account.UpdatedAt.Equal(activity) {
		t.Fatalf("effective account update = %v, want request time %s", account.UpdatedAt, activity)
	}
}

func TestSnapshotRowUsesPersistedWatermarkForUnchangedSource(t *testing.T) {
	activity := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	updated := activity.Add(10 * time.Second)
	row := snapshotRow{
		id:           sql.NullInt64{Int64: 10, Valid: true},
		status:       sql.NullString{String: "active", Valid: true},
		schedulable:  true,
		updatedAt:    sql.NullTime{Time: updated, Valid: true},
		lastActivity: sql.NullTime{Time: activity, Valid: true},
		credentials:  []byte(`{}`),
	}
	first, err := row.account()
	if err != nil {
		t.Fatal(err)
	}
	watermark := activity.Add(-time.Hour)
	row.persistedSourceFingerprint = sql.NullString{String: first.SourceFingerprint, Valid: true}
	row.persistedSourceUpdatedAt = sql.NullTime{Time: watermark, Valid: true}
	got, err := row.account()
	if err != nil {
		t.Fatal(err)
	}
	if got.SourceUpdatedAt == nil || !got.SourceUpdatedAt.Equal(watermark) {
		t.Fatalf("unchanged source watermark = %v, want %s", got.SourceUpdatedAt, watermark)
	}
}

func TestSnapshotRowUsesCandidateWatermarkForChangedSource(t *testing.T) {
	activity := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	row := snapshotRow{
		id:           sql.NullInt64{Int64: 10, Valid: true},
		status:       sql.NullString{String: "active", Valid: true},
		schedulable:  true,
		updatedAt:    sql.NullTime{Time: activity, Valid: true},
		lastActivity: sql.NullTime{Time: activity, Valid: true},
		credentials:  []byte(`{}`),
		persistedSourceFingerprint: sql.NullString{
			String: sourceFingerprintVersion + ":different", Valid: true,
		},
	}
	got, err := row.account()
	if err != nil {
		t.Fatal(err)
	}
	if got.SourceUpdatedAt == nil || !got.SourceUpdatedAt.Equal(activity) {
		t.Fatalf("changed source watermark = %v, want %s", got.SourceUpdatedAt, activity)
	}
}

func TestSnapshotRowRebaselinesOlderSourceFingerprintVersion(t *testing.T) {
	activity := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	row := snapshotRow{
		id:           sql.NullInt64{Int64: 10, Valid: true},
		status:       sql.NullString{String: "active", Valid: true},
		schedulable:  true,
		updatedAt:    sql.NullTime{Time: activity, Valid: true},
		lastActivity: sql.NullTime{Time: activity, Valid: true},
		credentials:  []byte(`{}`),
		persistedSourceFingerprint: sql.NullString{
			String: "old-version:different", Valid: true,
		},
	}
	got, err := row.account()
	if err != nil {
		t.Fatal(err)
	}
	if got.SourceUpdatedAt != nil {
		t.Fatalf("older fingerprint watermark = %v, want nil", got.SourceUpdatedAt)
	}
}

func TestGroupSnapshotQueryIncludesUnboundGroups(t *testing.T) {
	for _, fragment := range []string{
		"FROM groups g",
		"g.deleted_at IS NULL",
		"EXISTS (",
		"channel_groups cg",
		"LOWER(TRIM(c.status)) = 'active'",
	} {
		if !strings.Contains(groupSnapshotQuery, fragment) {
			t.Fatalf("group snapshot query missing %q", fragment)
		}
	}
}

func TestMergeGroupSnapshotRowAddsEmptyGroup(t *testing.T) {
	updated := time.Date(2026, 8, 27, 10, 0, 0, 0, time.UTC)
	groups := make(map[int64]*model.Group)
	mergeGroupSnapshotRow(groupSnapshotRow{
		groupID:          sql.NullInt64{Int64: 12, Valid: true},
		name:             sql.NullString{String: "empty", Valid: true},
		platform:         sql.NullString{String: "openai", Valid: true},
		status:           sql.NullString{String: "active", Valid: true},
		updatedAt:        sql.NullTime{Time: updated, Valid: true},
		hasActiveChannel: false,
	}, groups)

	group := groups[12]
	if group == nil || group.Name != "empty" || group.Status != "active" || len(group.AccountIDs) != 0 || group.HasActiveChannel {
		t.Fatalf("empty group was not merged correctly: %+v", group)
	}
	if group.UpdatedAt == nil || !group.UpdatedAt.Equal(updated) {
		t.Fatalf("group update = %v, want %s", group.UpdatedAt, updated)
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

func TestAlertStateSurvivesActivityCoupledAccountUpdate(t *testing.T) {
	activity := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	stateBeforeActivity := sql.NullTime{Time: activity.Add(-time.Hour), Valid: true}
	activitySourceUpdate := sql.NullTime{Time: activity, Valid: true}
	activityAt := sql.NullTime{Time: activity, Valid: true}

	if !alertStateCurrentForSource(stateBeforeActivity, activitySourceUpdate, activityAt) {
		t.Fatal("probe failure state must survive an activity-coupled account update")
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
