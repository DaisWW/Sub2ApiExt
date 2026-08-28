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

func TestNewCycleBatchDoesNotProbeErrorAccountWithoutChannelError(t *testing.T) {
	now := time.Now().UTC()
	updated := now.Add(-30 * time.Minute)
	oldProbe := now.Add(-2 * time.Hour)
	snapshot := model.Snapshot{Accounts: []model.Account{{
		ID: 3, Platform: "openai", Type: "api_key", Status: "error", Schedulable: true,
		UpdatedAt: &updated, LastProbeAt: &oldProbe,
	}}}
	_, accounts := newCycleBatch(snapshot, now, time.Minute)
	if len(accounts) != 0 {
		t.Fatalf("仅有账户错误状态不应进入恢复探测：accounts=%d", len(accounts))
	}
}

func TestNewCycleBatchStopsErrorRecoveryAfterSuccessfulProbe(t *testing.T) {
	now := time.Now().UTC()
	updated := now.Add(-2 * time.Hour)
	lastProbe := now.Add(-16 * time.Minute)
	errorAt := now.Add(-2 * time.Hour)
	snapshot := model.Snapshot{Accounts: []model.Account{{
		ID: 4, Platform: "openai", Type: "api_key", Status: "error", Schedulable: true,
		UpdatedAt: &updated, LastProbeAt: &lastProbe, LastProbeStatus: model.StatusOperational,
		LastChannelErrorAt: &errorAt,
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
	batch, accounts := newCycleBatch(snapshot, now, time.Minute)
	if len(accounts) != 0 {
		t.Fatalf("账户更新后的旧证据不应触发主动探测：accounts=%d", len(accounts))
	}
	if _, exists := batch.accountResults[5]; exists {
		t.Fatalf("账户更新后的旧证据不应继续作为当前证据：%+v", batch.accountResults[5])
	}
}

func TestEvidenceTimeValidAllowsUsageAccountingLag(t *testing.T) {
	now := time.Now().UTC()
	activity := now.Add(-time.Minute)
	updated := activity.Add(10 * time.Second)
	if !evidenceTimeValid(&activity, &updated) {
		t.Fatal("recent request should remain valid during accounting lag")
	}

	oldActivity := now.Add(-time.Hour)
	largeUpdate := oldActivity.Add(model.SourceUpdateActivityGrace + time.Second)
	if evidenceTimeValid(&oldActivity, &largeUpdate) {
		t.Fatal("evidence before a larger source update must be invalid")
	}
}

func TestNewCycleBatchProbesFailedAccountOnFifteenMinuteSchedule(t *testing.T) {
	now := time.Now().UTC()
	updated := now.Add(-2 * time.Hour)
	activity := now.Add(-time.Hour)
	lastProbe := now.Add(-16 * time.Minute)
	errorAt := now.Add(-2 * time.Hour)
	snapshot := model.Snapshot{Accounts: []model.Account{{
		ID: 6, Platform: "openai", Type: "api_key", Status: "error", Schedulable: true,
		UpdatedAt: &updated, LastActivityAt: &activity, LastProbeAt: &lastProbe, LastProbeStatus: model.StatusFailed,
		LastChannelErrorAt: &errorAt,
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
	errorAt := now.Add(-2 * time.Hour)
	snapshot := model.Snapshot{Accounts: []model.Account{{
		ID: 7, Platform: "openai", Type: "api_key", Status: "error", Schedulable: true,
		UpdatedAt: &updated, LastProbeAt: &lastProbe, LastProbeStatus: model.StatusFailed,
		LastChannelErrorAt: &errorAt,
	}}}
	batch, accounts := newCycleBatch(snapshot, now, time.Minute)
	if len(accounts) != 0 || batch.deferredAccounts != 1 {
		t.Fatalf("失败账户在 15 分钟内未保持低频重试：accounts=%d deferred=%d", len(accounts), batch.deferredAccounts)
	}
}

func TestProbeRetryIntervalBacksOffByFailureKindAndStreak(t *testing.T) {
	tests := []struct {
		name    string
		account model.Account
		want    time.Duration
	}{
		{name: "first recoverable failure", account: model.Account{ID: 11, LastProbeErrorClass: "network", ProbeFailureStreak: 1}, want: 15 * time.Minute},
		{name: "second recoverable failure", account: model.Account{ID: 11, LastProbeErrorClass: "timeout", ProbeFailureStreak: 2}, want: time.Hour},
		{name: "third recoverable failure", account: model.Account{ID: 11, LastProbeErrorClass: "read", ProbeFailureStreak: 3}, want: 6 * time.Hour},
		{name: "persistent recoverable failure", account: model.Account{ID: 11, LastProbeErrorClass: "upstream", ProbeFailureStreak: 4}, want: 24 * time.Hour},
		{name: "configuration failure", account: model.Account{ID: 11, LastProbeErrorClass: "configuration", ProbeFailureStreak: 1}, want: 24 * time.Hour},
		{name: "authentication failure", account: model.Account{ID: 11, LastProbeErrorClass: "upstream", LastProbeStatusCode: intPtr(401), ProbeFailureStreak: 1}, want: 24 * time.Hour},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := probeRetryInterval(test.account, time.Minute); got != test.want {
				t.Fatalf("probeRetryInterval() = %s, want %s", got, test.want)
			}
		})
	}
}

func TestProbeRetryIntervalAddsBoundedDeterministicJitter(t *testing.T) {
	account := model.Account{ID: 1, LastProbeErrorClass: "network", ProbeFailureStreak: 1}
	base := probeRetryInterval(account, time.Minute)
	first := addProbeJitter(account, base)
	second := addProbeJitter(account, base)
	if first != second || first < base || first > base+base/10 {
		t.Fatalf("retry jitter = %s/%s, want deterministic value in [%s, %s]", first, second, base, base+base/10)
	}
}

func TestNewCycleBatchDoesNotRecheckStaleEvidenceWithoutChannelError(t *testing.T) {
	now := time.Now().UTC()
	updated := now.Add(-48 * time.Hour)
	staleProbe := now.Add(-26 * time.Hour)
	snapshot := model.Snapshot{
		Accounts: []model.Account{
			{ID: 1, Platform: "openai", Type: "api_key", Priority: 1, Status: "active", Schedulable: true, UpdatedAt: &updated, LastProbeAt: &staleProbe, LastProbeStatus: model.StatusOperational},
			{ID: 2, Platform: "openai", Type: "api_key", Priority: 10, Status: "active", Schedulable: true, UpdatedAt: &updated, LastProbeAt: &staleProbe, LastProbeStatus: model.StatusOperational},
		},
		Groups: []model.Group{{
			ID: 20, Status: "active", ProbeEnabled: true, AccountIDs: []int64{1, 2},
			Members: []model.GroupMember{
				{AccountID: 1, AccountPriority: 1},
				{AccountID: 2, AccountPriority: 10},
			},
		}},
	}
	batch, accounts := newCycleBatch(snapshot, now, time.Minute)
	if got := accountIDsForTest(accounts); len(got) != 0 {
		t.Fatalf("无渠道错误时不应按路由关键账户探测：queued=%v", got)
	}
	for _, accountID := range []int64{1, 2} {
		if cached := batch.accountResults[accountID]; cached.Status != model.StatusOperational || cached.Source != "cache" {
			t.Fatalf("stale evidence for account %d = %+v, want cached operational", accountID, cached)
		}
	}
}

func TestNewCycleBatchKeepsRecentSuccessfulEvidence(t *testing.T) {
	now := time.Now().UTC()
	updated := now.Add(-48 * time.Hour)
	recentProbe := now.Add(-23 * time.Hour)
	snapshot := model.Snapshot{
		Accounts: []model.Account{{
			ID: 1, Platform: "openai", Type: "api_key", Status: "active", Schedulable: true,
			UpdatedAt: &updated, LastProbeAt: &recentProbe, LastProbeStatus: model.StatusOperational,
		}},
		Groups: []model.Group{{ID: 20, Status: "active", ProbeEnabled: true, AccountIDs: []int64{1}}},
	}
	batch, accounts := newCycleBatch(snapshot, now, time.Minute)
	if len(accounts) != 0 || batch.accountResults[1].Status != model.StatusOperational {
		t.Fatalf("fresh critical evidence unexpectedly rechecked: accounts=%v evidence=%+v", accounts, batch.accountResults[1])
	}
}

func TestNewCycleBatchDoesNotProbeAfterConfigurationUpdateAlone(t *testing.T) {
	now := time.Now().UTC()
	updated := now.Add(-time.Minute)
	lastProbe := now.Add(-5 * time.Minute)
	snapshot := model.Snapshot{Accounts: []model.Account{{
		ID: 12, Platform: "openai", Type: "api_key", Status: "active", Schedulable: true,
		UpdatedAt: &updated, LastProbeAt: &lastProbe, LastProbeStatus: model.StatusError,
		LastProbeErrorClass: "configuration", ProbeFailureStreak: 4,
	}}}
	_, accounts := newCycleBatch(snapshot, now, time.Minute)
	if len(accounts) != 0 {
		t.Fatalf("配置更新不应单独触发主动探测：accounts=%d", len(accounts))
	}
}

func TestProbeEligibilityNormalizesAccountStatus(t *testing.T) {
	if probeEligible(model.Account{Status: " ACTIVE ", Schedulable: true}) {
		t.Fatal("active account without a channel error must not be probed")
	}
	if probeEligible(model.Account{Status: " ERROR "}) {
		t.Fatal("error status without a channel error must not be probed")
	}
	errorAt := time.Now().UTC().Add(-time.Minute)
	if !probeEligible(model.Account{Status: " ERROR ", LastChannelErrorAt: &errorAt}) {
		t.Fatal("normalized channel error should be probe eligible")
	}
	if probeEligible(model.Account{Status: "disabled", LastChannelErrorAt: &errorAt}) {
		t.Fatal("disabled account must not be probed even with stale channel evidence")
	}
}

func TestProbeEligibilityRequiresNewChannelErrorForActiveAccount(t *testing.T) {
	now := time.Now().UTC()
	updated := now.Add(-time.Hour)
	lastProbe := now.Add(-2 * time.Hour)
	withoutError := model.Account{Status: "active", Schedulable: true, UpdatedAt: &updated, LastProbeAt: &lastProbe}
	if probeEligible(withoutError) {
		t.Fatal("active account without a channel error must not be eligible")
	}
	errorAt := now.Add(-time.Minute)
	withError := withoutError
	withError.LastChannelErrorAt = &errorAt
	if !probeEligible(withError) {
		t.Fatal("new channel error should trigger one recovery probe")
	}
	probed := withError
	probed.LastProbeAt = &now
	if probeEligible(probed) {
		t.Fatal("a channel error already followed by a probe must not retrigger")
	}
}

func TestNewCycleBatchSlowsRecoveryAfterLongIdle(t *testing.T) {
	now := time.Now().UTC()
	updated := now.Add(-24 * time.Hour)
	lastProbe := now.Add(-30 * time.Minute)
	errorAt := now.Add(-2 * time.Hour)
	account := model.Account{
		ID: 13, Platform: "openai", Type: "api_key", Status: "error", Schedulable: true,
		UpdatedAt: &updated, LastProbeAt: &lastProbe, LastProbeStatus: model.StatusFailed,
		LastChannelErrorAt: &errorAt,
	}
	_, queued := newCycleBatch(model.Snapshot{Accounts: []model.Account{account}}, now, time.Minute)
	if len(queued) != 0 {
		t.Fatalf("长期无流量的恢复探测不应在 2 小时前重试：queued=%v", accountIDsForTest(queued))
	}
	if got := probeRetryDelayForAccount(account, time.Minute, now); got < idleRecoveryProbeInterval {
		t.Fatalf("idle recovery delay = %s, want at least %s", got, idleRecoveryProbeInterval)
	}
}

func TestNewCycleBatchProbesNewChannelErrorEvenWithOldSuccess(t *testing.T) {
	now := time.Now().UTC()
	updated := now.Add(-24 * time.Hour)
	activity := now.Add(-time.Hour)
	errorAt := now.Add(-2 * time.Minute)
	lastProbe := now.Add(-2 * time.Hour)
	account := model.Account{
		ID: 14, Platform: "openai", Type: "api_key", Status: "active", Schedulable: true,
		UpdatedAt: &updated, LastActivityAt: &activity, LastProbeAt: &lastProbe,
		LastProbeStatus: model.StatusOperational, LastChannelErrorAt: &errorAt,
	}
	_, queued := newCycleBatch(model.Snapshot{Accounts: []model.Account{account}}, now, time.Minute)
	if got := accountIDsForTest(queued); len(got) != 1 || got[0] != account.ID {
		t.Fatalf("new channel error did not supersede old success: queued=%v", got)
	}
}

func TestNewCycleBatchProbesNewChannelErrorAfterInvalidatedProbe(t *testing.T) {
	now := time.Now().UTC()
	updated := now.Add(-5 * time.Minute)
	lastProbe := now.Add(-10 * time.Minute)
	errorAt := now.Add(-time.Minute)
	account := model.Account{
		ID: 20, Platform: "openai", Type: "api_key", Status: "active", Schedulable: true,
		UpdatedAt: &updated, LastProbeAt: &lastProbe, LastProbeStatus: model.StatusOperational,
		LastChannelErrorAt: &errorAt,
	}
	_, queued := newCycleBatch(model.Snapshot{Accounts: []model.Account{account}}, now, time.Minute)
	if len(queued) != 1 || queued[0].ID != account.ID {
		t.Fatalf("作废旧探测后的新渠道错误未立即恢复探测：queued=%v", accountIDsForTest(queued))
	}
}

func TestNewCycleBatchDebouncesNewErrorsAfterRecentSuccessfulProbe(t *testing.T) {
	now := time.Now().UTC()
	updated := now.Add(-24 * time.Hour)
	lastProbe := now.Add(-5 * time.Minute)
	errorAt := now.Add(-time.Minute)
	account := model.Account{
		ID: 17, Platform: "openai", Type: "api_key", Status: "active", Schedulable: true,
		UpdatedAt: &updated, LastProbeAt: &lastProbe, LastProbeStatus: model.StatusOperational,
		LastChannelErrorAt: &errorAt,
	}
	batch, queued := newCycleBatch(model.Snapshot{Accounts: []model.Account{account}}, now, time.Minute)
	if len(queued) != 0 || batch.deferredAccounts != 1 {
		t.Fatalf("近期成功探测后的连续错误不应立即重复请求：queued=%v deferred=%d", accountIDsForTest(queued), batch.deferredAccounts)
	}
}

func TestNewCycleBatchDoesNotDelayFirstRecoveryProbeWhenIdle(t *testing.T) {
	now := time.Now().UTC()
	updated := now.Add(-24 * time.Hour)
	lastProbe := now.Add(-30 * time.Minute)
	errorAt := now.Add(-time.Minute)
	account := model.Account{
		ID: 15, Platform: "openai", Type: "api_key", Status: "active", Schedulable: true,
		UpdatedAt: &updated, LastProbeAt: &lastProbe, LastProbeStatus: model.StatusOperational,
		LastChannelErrorAt: &errorAt,
	}
	_, queued := newCycleBatch(model.Snapshot{Accounts: []model.Account{account}}, now, time.Minute)
	if len(queued) != 1 || queued[0].ID != account.ID {
		t.Fatalf("空闲账户的新渠道错误未立即进入恢复探测：queued=%v", accountIDsForTest(queued))
	}
}

func TestNewCycleBatchBacksOffRepeatedChannelErrorsAfterFailedProbe(t *testing.T) {
	now := time.Now().UTC()
	updated := now.Add(-24 * time.Hour)
	lastProbe := now.Add(-30 * time.Minute)
	errorAt := now.Add(-time.Minute)
	account := model.Account{
		ID: 16, Platform: "openai", Type: "api_key", Status: "active", Schedulable: true,
		UpdatedAt: &updated, LastProbeAt: &lastProbe, LastProbeStatus: model.StatusFailed,
		LastChannelErrorAt: &errorAt,
	}
	batch, queued := newCycleBatch(model.Snapshot{Accounts: []model.Account{account}}, now, time.Minute)
	if len(queued) != 0 || batch.deferredAccounts != 1 {
		t.Fatalf("持续错误日志不应打穿闲置账户的退避：queued=%v deferred=%d", accountIDsForTest(queued), batch.deferredAccounts)
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
	if len(accounts) != 0 {
		t.Fatalf("无渠道错误时无效探测结果也不应触发例行探测：accounts=%d", len(accounts))
	}
}

func TestLatestAccountEvidenceKeepsRealRequestOverInvalidProbe(t *testing.T) {
	now := time.Now().UTC()
	updated := now.Add(-2 * time.Hour)
	activity := now.Add(-10 * time.Minute)
	probeAt := now.Add(-5 * time.Minute)
	account := model.Account{
		ID: 18, Platform: "openai", Type: "api_key", Status: "active", Schedulable: true,
		UpdatedAt: &updated, LastActivityAt: &activity,
		LastProbeAt: &probeAt, LastProbeStatus: model.StatusDisabled,
	}
	evidence := latestAccountEvidence(account)
	if !evidence.valid || evidence.source != "history" || evidence.checkedAt == nil || !evidence.checkedAt.Equal(activity) {
		t.Fatalf("invalid probe replaced real request evidence: %+v", evidence)
	}
	batch, queued := newCycleBatch(model.Snapshot{Accounts: []model.Account{account}}, now, time.Minute)
	if len(queued) != 0 || batch.accountResults[account.ID].Source != "cache" {
		t.Fatalf("real request was not reused after invalid probe: queued=%v result=%+v", accountIDsForTest(queued), batch.accountResults[account.ID])
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
		LastProbeAt: &lastProbe, LastProbeStatus: model.StatusFailed, ProbeFailureStreak: 1,
	}}}
	batch, accounts := newCycleBatch(snapshot, now, time.Minute)
	if len(accounts) != 0 || len(batch.observations) != 1 {
		t.Fatalf("新真实请求未恢复失败账户：accounts=%d observations=%d", len(accounts), len(batch.observations))
	}
	if batch.observations[0].Source != "history" || batch.observations[0].Status != model.StatusOperational {
		t.Fatalf("恢复观测 = %+v，期望 history/operational", batch.observations[0])
	}
}

func TestNewCycleBatchConsumesHistoryRecoveryOnce(t *testing.T) {
	now := time.Now().UTC()
	updated := now.Add(-2 * time.Hour)
	activity := now.Add(-20 * time.Minute)
	lastProbe := now.Add(-30 * time.Minute)
	account := model.Account{
		ID: 19, Platform: "openai", Type: "api_key", Status: "active", Schedulable: true,
		UpdatedAt: &updated, LastActivityAt: &activity,
		LastProbeAt: &lastProbe, LastProbeStatus: model.StatusFailed, ProbeFailureStreak: 1,
	}
	first, queued := newCycleBatch(model.Snapshot{Accounts: []model.Account{account}}, now, time.Minute)
	if len(queued) != 0 || len(first.observations) != 1 {
		t.Fatalf("首次真实请求恢复未生成观测：queued=%v observations=%d", accountIDsForTest(queued), len(first.observations))
	}
	account.ProbeFailureStreak = 0
	second, queued := newCycleBatch(model.Snapshot{Accounts: []model.Account{account}}, now, time.Minute)
	if len(queued) != 0 || len(second.observations) != 0 {
		t.Fatalf("同一条历史恢复被重复消费：queued=%v observations=%d", accountIDsForTest(queued), len(second.observations))
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

func TestAggregateGroupsUsesMembersWhenAccountIDsAreMissing(t *testing.T) {
	now := time.Now().UTC()
	batch := &cycleBatch{accountResults: map[int64]model.ProbeResult{
		1: {TargetKey: "account:1", Kind: model.KindAccount, EntityID: 1, Status: model.StatusOperational, Source: "probe", CheckedAt: now},
	}}
	snapshot := model.Snapshot{
		Accounts: []model.Account{{ID: 1, Status: "active", Schedulable: true}},
		Groups:   []model.Group{{ID: 16, Status: "active", ProbeEnabled: true, Members: []model.GroupMember{{AccountID: 1}}}},
	}

	batch.aggregateGroups(snapshot, indexAccounts(snapshot.Accounts), now)
	if len(batch.observations) != 1 || batch.observations[0].Status != model.StatusOperational {
		t.Fatalf("Members-only group did not aggregate its result: %+v", batch.observations)
	}
	if len(batch.persisted) != 1 || batch.persisted[0].TargetKey != "group:16" {
		t.Fatalf("Members-only aggregate was not persisted: %+v", batch.persisted)
	}
}

func TestAggregateGroupsPersistsAllKnownProbeFailures(t *testing.T) {
	now := time.Now().UTC()
	batch := &cycleBatch{accountResults: map[int64]model.ProbeResult{
		1: {TargetKey: "account:1", Kind: model.KindAccount, EntityID: 1, Status: model.StatusFailed, Source: "probe", CheckedAt: now},
		2: {TargetKey: "account:2", Kind: model.KindAccount, EntityID: 2, Status: model.StatusError, Source: "probe", CheckedAt: now},
	}}
	snapshot := model.Snapshot{
		Accounts: []model.Account{
			{ID: 1, Status: "active", Schedulable: true},
			{ID: 2, Status: "active", Schedulable: true},
		},
		Groups: []model.Group{{ID: 17, Status: "active", ProbeEnabled: true, AccountIDs: []int64{1, 2}, Members: []model.GroupMember{
			{AccountID: 1}, {AccountID: 2},
		}}},
	}

	batch.aggregateGroups(snapshot, indexAccounts(snapshot.Accounts), now)
	if len(batch.persisted) != 1 || batch.persisted[0].Status != model.StatusFailed || batch.persisted[0].Message != "当前无可用候选：0/2" {
		t.Fatalf("all failed candidates did not persist a failed aggregate: %+v", batch.persisted)
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

func TestAggregateGroupsPersistsHistoryRecovery(t *testing.T) {
	now := time.Now().UTC()
	batch := &cycleBatch{accountResults: map[int64]model.ProbeResult{
		1: {TargetKey: "account:1", Kind: model.KindAccount, EntityID: 1, Status: model.StatusOperational, Source: "history", CheckedAt: now},
	}}
	snapshot := model.Snapshot{
		Accounts: []model.Account{{ID: 1, Status: "active", Schedulable: true}},
		Groups:   []model.Group{{ID: 15, Status: "active", ProbeEnabled: true, AccountIDs: []int64{1}}},
	}

	batch.aggregateGroups(snapshot, indexAccounts(snapshot.Accounts), now)
	if len(batch.observations) != 1 || batch.observations[0].Status != model.StatusOperational {
		t.Fatalf("history recovery did not produce a healthy group observation: %+v", batch.observations)
	}
	if len(batch.persisted) != 1 || batch.persisted[0].Source != "aggregate" {
		t.Fatalf("history recovery aggregate was not persisted: %+v", batch.persisted)
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

func accountIDsForTest(accounts []model.Account) []int64 {
	ids := make([]int64, 0, len(accounts))
	for _, account := range accounts {
		ids = append(ids, account.ID)
	}
	return ids
}
