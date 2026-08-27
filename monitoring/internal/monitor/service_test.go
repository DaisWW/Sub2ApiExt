package monitor

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/DaisWW/Sub2ApiExt/monitoring/internal/model"
)

func TestRunProbesUsesBoundedWorkersWithoutDeadlock(t *testing.T) {
	accounts := make([]model.Account, 50)
	for index := range accounts {
		accounts[index].ID = int64(index + 1)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var active, maximum atomic.Int32
	results, err := runProbes(ctx, accounts, 3, func(_ context.Context, account model.Account) model.ProbeResult {
		current := active.Add(1)
		defer active.Add(-1)
		for {
			observed := maximum.Load()
			if current <= observed || maximum.CompareAndSwap(observed, current) {
				break
			}
		}
		time.Sleep(time.Millisecond)
		return model.ProbeResult{EntityID: account.ID}
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != len(accounts) {
		t.Fatalf("got %d results, want %d", len(results), len(accounts))
	}
	if got := maximum.Load(); got > 3 {
		t.Fatalf("observed %d concurrent probes, want at most 3", got)
	}
}

func TestNewCycleBatchUsesSuccessfulHistoryWithoutProbing(t *testing.T) {
	now := time.Now().UTC()
	activity := now.Add(-24 * time.Hour)
	updated := now.Add(-48 * time.Hour)
	snapshot := model.Snapshot{Accounts: []model.Account{{
		ID: 1, Status: "active", Schedulable: true,
		LastActivityAt: &activity, UpdatedAt: &updated,
	}}}
	batch, accounts := newCycleBatch(snapshot, now, time.Minute)
	if len(accounts) != 0 {
		t.Fatalf("已有成功历史时仍排入了 %d 个主动探测", len(accounts))
	}
	if batch.verifiedAccounts != 1 || batch.passiveAccounts != 0 {
		t.Fatalf("成功历史验证统计异常：%+v", batch)
	}
	if cached := batch.accountResults[1]; cached.Source != "cache" || cached.Status != model.StatusOperational {
		t.Fatalf("未保留成功历史证据：%+v", cached)
	}
}

func TestNewCycleBatchStopsAfterSuccessfulProbe(t *testing.T) {
	now := time.Now().UTC()
	updated := now.Add(-2 * time.Hour)
	probeAt := now.Add(-90 * time.Minute)
	snapshot := model.Snapshot{Accounts: []model.Account{{
		ID: 2, Platform: "openai", Type: "api_key", Status: "active", Schedulable: true,
		UpdatedAt: &updated, LastProbeAt: &probeAt, LastProbeStatus: model.StatusOperational,
	}}}
	batch, accounts := newCycleBatch(snapshot, now, time.Minute)
	if len(accounts) != 0 || batch.verifiedAccounts != 1 {
		t.Fatalf("成功探测证据未停止主动探测：accounts=%d verified=%d", len(accounts), batch.verifiedAccounts)
	}
}

func TestNewCycleBatchProbesErrorAccountWithoutValidEvidence(t *testing.T) {
	now := time.Now().UTC()
	updated := now.Add(-30 * time.Minute)
	oldProbe := now.Add(-2 * time.Hour)
	snapshot := model.Snapshot{Accounts: []model.Account{{
		ID: 3, Platform: "openai", Type: "api_key", Status: "error", Schedulable: true,
		UpdatedAt: &updated, LastProbeAt: &oldProbe, LastProbeStatus: model.StatusOperational,
	}}}
	_, accounts := newCycleBatch(snapshot, now, time.Minute)
	if len(accounts) != 1 {
		t.Fatalf("没有有效证据的错误账户未进入恢复探测：accounts=%d", len(accounts))
	}
}

func TestNewCycleBatchStopsErrorRecoveryAfterSuccessfulProbe(t *testing.T) {
	now := time.Now().UTC()
	updated := now.Add(-2 * time.Hour)
	lastProbe := now.Add(-16 * time.Minute)
	snapshot := model.Snapshot{Accounts: []model.Account{{
		ID: 4, Platform: "openai", Type: "api_key", Status: "error", Schedulable: true,
		UpdatedAt: &updated, LastProbeAt: &lastProbe, LastProbeStatus: model.StatusOperational,
	}}}
	batch, accounts := newCycleBatch(snapshot, now, time.Minute)
	if len(accounts) != 0 || batch.verifiedAccounts != 1 {
		t.Fatalf("错误账户恢复后仍在探测：accounts=%d verified=%d", len(accounts), batch.verifiedAccounts)
	}
}

func TestNewCycleBatchInvalidatesEvidenceAfterAccountUpdate(t *testing.T) {
	now := time.Now().UTC()
	updated := now.Add(-10 * time.Minute)
	activity := now.Add(-2 * time.Hour)
	lastProbe := now.Add(-90 * time.Minute)
	snapshot := model.Snapshot{Accounts: []model.Account{{
		ID: 5, Platform: "openai", Type: "api_key", Status: "active", Schedulable: true,
		UpdatedAt: &updated, LastActivityAt: &activity,
		LastProbeAt: &lastProbe, LastProbeStatus: model.StatusOperational,
	}}}
	_, accounts := newCycleBatch(snapshot, now, time.Minute)
	if len(accounts) != 1 {
		t.Fatalf("账户更新后仍沿用旧成功证据：accounts=%d", len(accounts))
	}
}

func TestNewCycleBatchProbesFailedAccountOnFifteenMinuteSchedule(t *testing.T) {
	now := time.Now().UTC()
	updated := now.Add(-2 * time.Hour)
	lastProbe := now.Add(-16 * time.Minute)
	snapshot := model.Snapshot{Accounts: []model.Account{{
		ID: 6, Platform: "openai", Type: "api_key", Status: "active", Schedulable: true,
		UpdatedAt: &updated, LastProbeAt: &lastProbe, LastProbeStatus: model.StatusFailed,
	}}}
	_, accounts := newCycleBatch(snapshot, now, time.Minute)
	if len(accounts) != 1 {
		t.Fatalf("失败账户未按 15 分钟间隔恢复探测：accounts=%d", len(accounts))
	}
}

func TestNewCycleBatchDefersFailedAccountBeforeFifteenMinutes(t *testing.T) {
	now := time.Now().UTC()
	updated := now.Add(-2 * time.Hour)
	lastProbe := now.Add(-5 * time.Minute)
	snapshot := model.Snapshot{Accounts: []model.Account{{
		ID: 7, Platform: "openai", Type: "api_key", Status: "active", Schedulable: true,
		UpdatedAt: &updated, LastProbeAt: &lastProbe, LastProbeStatus: model.StatusFailed,
	}}}
	batch, accounts := newCycleBatch(snapshot, now, time.Minute)
	if len(accounts) != 0 || batch.deferredAccounts != 1 {
		t.Fatalf("失败账户在 15 分钟内未保持低频重试：accounts=%d deferred=%d", len(accounts), batch.deferredAccounts)
	}
}

func TestNewCycleBatchDoesNotTreatDisabledProbeAsEvidence(t *testing.T) {
	now := time.Now().UTC()
	updated := now.Add(-2 * time.Hour)
	lastProbe := now.Add(-5 * time.Minute)
	snapshot := model.Snapshot{Accounts: []model.Account{{
		ID: 9, Platform: "openai", Type: "api_key", Status: "active", Schedulable: true,
		UpdatedAt: &updated, LastProbeAt: &lastProbe, LastProbeStatus: model.StatusDisabled,
	}}}
	_, accounts := newCycleBatch(snapshot, now, time.Minute)
	if len(accounts) != 1 {
		t.Fatalf("无效探测结果不应延迟重新验证：accounts=%d", len(accounts))
	}
}

func TestNewCycleBatchUsesNewSuccessfulHistoryToRecoverFailedProbe(t *testing.T) {
	now := time.Now().UTC()
	updated := now.Add(-2 * time.Hour)
	activity := now.Add(-20 * time.Minute)
	lastProbe := now.Add(-30 * time.Minute)
	snapshot := model.Snapshot{Accounts: []model.Account{{
		ID: 8, Platform: "openai", Type: "api_key", Status: "active", Schedulable: true,
		UpdatedAt: &updated, LastActivityAt: &activity,
		LastProbeAt: &lastProbe, LastProbeStatus: model.StatusFailed,
	}}}
	batch, accounts := newCycleBatch(snapshot, now, time.Minute)
	if len(accounts) != 0 || len(batch.observations) != 1 {
		t.Fatalf("新真实请求未恢复失败账户：accounts=%d observations=%d", len(accounts), len(batch.observations))
	}
	if batch.observations[0].Source != "history" || batch.observations[0].Status != model.StatusOperational {
		t.Fatalf("恢复观测 = %+v，期望 history/operational", batch.observations[0])
	}
}

func TestAggregateGroupsSkipsCachedOnlyCycle(t *testing.T) {
	now := time.Now().UTC()
	batch := &cycleBatch{accountResults: map[int64]model.ProbeResult{
		1: {TargetKey: "account:1", Kind: model.KindAccount, EntityID: 1, Status: model.StatusOperational, Source: "cache", CheckedAt: now},
	}}
	snapshot := model.Snapshot{
		Accounts: []model.Account{{ID: 1, Status: "active", Schedulable: true}},
		Groups:   []model.Group{{ID: 12, Status: "active", ProbeEnabled: true, AccountIDs: []int64{1}}},
	}
	batch.aggregateGroups(snapshot, indexAccounts(snapshot.Accounts), now)
	if len(batch.observations) != 0 || len(batch.persisted) != 0 {
		t.Fatalf("纯缓存轮次不应生成分组观测：%+v", batch)
	}
}

func TestAggregateGroupsIncludesCachedFailureWithFreshProbe(t *testing.T) {
	now := time.Now().UTC()
	batch := &cycleBatch{accountResults: map[int64]model.ProbeResult{
		1: {TargetKey: "account:1", Kind: model.KindAccount, EntityID: 1, Status: model.StatusOperational, Source: "probe", CheckedAt: now},
		2: {TargetKey: "account:2", Kind: model.KindAccount, EntityID: 2, Status: model.StatusFailed, Source: "cache", CheckedAt: now.Add(-5 * time.Minute)},
	}}
	snapshot := model.Snapshot{
		Accounts: []model.Account{
			{ID: 1, Status: "active", Schedulable: true},
			{ID: 2, Status: "active", Schedulable: true},
		},
		Groups: []model.Group{{ID: 13, Status: "active", ProbeEnabled: true, AccountIDs: []int64{1, 2}}},
	}
	batch.aggregateGroups(snapshot, indexAccounts(snapshot.Accounts), now)
	if len(batch.persisted) != 1 || batch.persisted[0].Status != model.StatusDegraded {
		t.Fatalf("分组未保留退避成员的失败状态：%+v", batch.persisted)
	}
}

func TestAggregateGroupsExcludesErrorAccounts(t *testing.T) {
	now := time.Now().UTC()
	batch := &cycleBatch{accountResults: map[int64]model.ProbeResult{
		1: {TargetKey: "account:1", Kind: model.KindAccount, EntityID: 1, Status: model.StatusOperational, Source: "probe", CheckedAt: now},
		2: {TargetKey: "account:2", Kind: model.KindAccount, EntityID: 2, Status: model.StatusFailed, Source: "probe", CheckedAt: now},
	}}
	snapshot := model.Snapshot{
		Accounts: []model.Account{
			{ID: 1, Status: "active", Schedulable: true},
			{ID: 2, Status: "error", Schedulable: true},
		},
		Groups: []model.Group{{ID: 14, Status: "active", ProbeEnabled: true, AccountIDs: []int64{1, 2}}},
	}
	batch.aggregateGroups(snapshot, indexAccounts(snapshot.Accounts), now)
	if len(batch.observations) != 1 || batch.observations[0].Status != model.StatusOperational {
		t.Fatalf("错误账户不应影响分组健康：%+v", batch.observations)
	}
}

func TestAggregateGroupsOnlyIncludesEnabledGroups(t *testing.T) {
	now := time.Now().UTC()
	batch := &cycleBatch{
		accountResults: map[int64]model.ProbeResult{
			1: {TargetKey: "account:1", Kind: model.KindAccount, EntityID: 1, Status: model.StatusOperational, LatencyMs: intPtr(10), Source: "probe", CheckedAt: now},
		},
	}
	snapshot := model.Snapshot{
		Accounts: []model.Account{{ID: 1, Status: "active", Schedulable: true}},
		Groups: []model.Group{
			{ID: 10, Name: "停用分组", Status: "disabled", ProbeEnabled: false, AccountIDs: []int64{1}},
			{ID: 11, Name: "启用分组", Status: "active", ProbeEnabled: true, AccountIDs: []int64{1}},
		},
	}
	batch.aggregateGroups(snapshot, indexAccounts(snapshot.Accounts), now)
	if len(batch.observations) != 1 || batch.observations[0].TargetKey != "group:11" {
		t.Fatalf("分组观测为 %+v，期望只包含启用分组", batch.observations)
	}
}

func TestNextProbeTimeRoundTrip(t *testing.T) {
	service := &Service{}
	want := time.Date(2026, time.August, 26, 12, 30, 45, 123000000, time.UTC)
	service.setNextProbe(want)
	got := service.nextProbeAt()
	if got == nil || !got.Equal(want) {
		t.Fatalf("next probe = %v, want %v", got, want)
	}
}

func intPtr(value int) *int {
	return &value
}
