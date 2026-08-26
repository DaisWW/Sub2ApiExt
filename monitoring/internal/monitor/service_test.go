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

func TestNewCycleBatchPrefersRecentHistory(t *testing.T) {
	now := time.Now().UTC()
	recent := now.Add(-30 * time.Second)
	snapshot := model.Snapshot{Accounts: []model.Account{{
		ID: 1, Status: "active", Schedulable: true, LastActivityAt: &recent,
	}}}
	batch, accounts := newCycleBatch(snapshot, now, time.Minute)
	if len(accounts) != 0 {
		t.Fatalf("近期有真实请求时仍排入了 %d 个主动探测", len(accounts))
	}
	if batch.passiveAccounts != 1 || len(batch.observations) != 1 {
		t.Fatalf("被动观测统计异常：%+v", batch)
	}
	if batch.observations[0].Source != "history" {
		t.Fatalf("观测来源为 %q，期望 history", batch.observations[0].Source)
	}
}

func TestNewCycleBatchAlwaysProbesErrorAccount(t *testing.T) {
	now := time.Now().UTC()
	recent := now.Add(-10 * time.Second)
	snapshot := model.Snapshot{Accounts: []model.Account{{
		ID: 2, Platform: "openai", Type: "api_key", Status: "error",
		Schedulable: true, LastActivityAt: &recent,
	}}}
	batch, accounts := newCycleBatch(snapshot, now, time.Minute)
	if len(accounts) != 1 || batch.passiveAccounts != 0 {
		t.Fatalf("错误账户未进入主动探测：accounts=%d passive=%d", len(accounts), batch.passiveAccounts)
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
