package main

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"
)

func TestDynamicRawTargetRespondsQuicklyUpAndConservativelyDown(t *testing.T) {
	if got := dynamicRawTarget(0.112, 0.100); !almostEqual(got, 0.112) {
		t.Fatalf("upward target = %.4f, want 0.1120", got)
	}
	if got := dynamicRawTarget(0.080, 0.100); !almostEqual(got, 0.0902) {
		t.Fatalf("downward target = %.4f, want 0.0902", got)
	}
	if got := dynamicRawTarget(0.096, 0.100); !almostEqual(got, 0.100) {
		t.Fatalf("small downward noise should be ignored, got %.4f", got)
	}
}

func TestDynamicPublishedTargetUsesAsymmetricStepLimits(t *testing.T) {
	if got := dynamicPublishedTarget(0.100, 0.200); !almostEqual(got, 0.120) {
		t.Fatalf("upward limited target = %.4f", got)
	}
	if got := dynamicPublishedTarget(0.100, 0.020); !almostEqual(got, 0.090) {
		t.Fatalf("downward limited target = %.4f", got)
	}
}

func TestDynamicGroupDeadbandUsesOnePercentOrPoint001(t *testing.T) {
	if dynamicGroupRateChangeSignificant(0.100, 0.1009) {
		t.Fatal("change inside 0.001 deadband should be ignored")
	}
	if !dynamicGroupRateChangeSignificant(0.100, 0.101) {
		t.Fatal("0.001 change should be applied")
	}
	if dynamicGroupRateChangeSignificant(0.500, 0.5049) {
		t.Fatal("change inside 1% deadband should be ignored")
	}
	if !dynamicGroupRateChangeSignificant(0.500, 0.505) {
		t.Fatal("1% change should be applied")
	}
}

func TestDynamicMemoryDecaysByConsumedCostAndRepricesSavedWeights(t *testing.T) {
	memory := DynamicCostMemory{
		Denominator: 5,
		AccountBase: map[int64]float64{1: 5},
	}
	rows := []GroupUsageAccountStats{{AccountID: 2, StandardCost: 5, BaseCost: 5}}
	updateDynamicMemory(&memory, 5, rows)
	wantOldBase := 5 * mathExpMinusOne
	if diff := memory.AccountBase[1] - wantOldBase; diff < -1e-9 || diff > 1e-9 {
		t.Fatalf("decayed old base = %.8f, want %.8f", memory.AccountBase[1], wantOldBase)
	}
	rate, ok := dynamicMemoryRate(memory, map[int64]float64{1: 0.1, 2: 0.2})
	if !ok || rate <= 0.15 || rate >= 0.2 {
		t.Fatalf("unexpected mixed rate %.6f ok=%t", rate, ok)
	}
	repriced, ok := dynamicMemoryRate(memory, map[int64]float64{1: 0.4, 2: 0.2})
	if !ok || repriced <= rate {
		t.Fatalf("saved weights were not repriced: old=%.6f new=%.6f", rate, repriced)
	}
}

const mathExpMinusOne = 0.36787944117144233

func TestDynamicBootstrapNormalizesMemoryBudgets(t *testing.T) {
	rows := []GroupUsageAccountStats{
		{GroupID: 24, AccountID: 1, Requests: 20, StandardCost: 70, BaseCost: 70, CurrentAccountRate: 0.1},
		{GroupID: 24, AccountID: 2, Requests: 20, StandardCost: 30, BaseCost: 30, CurrentAccountRate: 0.2},
	}
	state := seedDynamicGroupState(rows, 99)
	if state == nil {
		t.Fatal("bootstrap returned nil")
	}
	if state.Fast.Denominator != dynamicFastBudgetUSD || state.Slow.Denominator != dynamicSlowBudgetUSD {
		t.Fatalf("bootstrap memory was not normalized: %+v", state)
	}
	fast, fastOK := dynamicMemoryRate(state.Fast, state.LastAccountRates)
	slow, slowOK := dynamicMemoryRate(state.Slow, state.LastAccountRates)
	if !fastOK || !slowOK || !almostEqual(fast, 0.13) || !almostEqual(slow, 0.13) {
		t.Fatalf("bootstrap rates fast=%.4f/%t slow=%.4f/%t", fast, fastOK, slow, slowOK)
	}
}

type incrementalTestSource struct {
	*staticChannelSource
	latestID         int64
	bootstrap        []GroupUsageAccountStats
	incremental      []GroupUsageAccountStats
	watermarks       []int64
	bootstrapRun     int
	bootstrapWindows []time.Duration
}

func (s *incrementalTestSource) LatestGroupUsageID(context.Context) (int64, error) {
	return s.latestID, nil
}

func (s *incrementalTestSource) ListGroupUsageSince(_ context.Context, watermarks map[int64]int64, throughID int64) ([]GroupUsageAccountStats, error) {
	s.watermarks = append(s.watermarks, watermarks[24])
	if watermarks[24] < s.latestID && throughID >= s.latestID {
		return append([]GroupUsageAccountStats(nil), s.incremental...), nil
	}
	return nil, nil
}

func (s *incrementalTestSource) ListGroupUsageAccounts(_ context.Context, start, end time.Time, _ int64, _ []int64) ([]GroupUsageAccountStats, error) {
	s.bootstrapRun++
	s.bootstrapWindows = append(s.bootstrapWindows, end.Sub(start))
	return append([]GroupUsageAccountStats(nil), s.bootstrap...), nil
}

type bootstrapWindowSource struct {
	rowsByWindow map[time.Duration][]GroupUsageAccountStats
	windows      []time.Duration
	groups       [][]int64
}

func (s *bootstrapWindowSource) LatestGroupUsageID(context.Context) (int64, error) {
	return 1, nil
}

func (s *bootstrapWindowSource) ListGroupUsageSince(context.Context, map[int64]int64, int64) ([]GroupUsageAccountStats, error) {
	return nil, nil
}

func (s *bootstrapWindowSource) ListGroupUsageAccounts(_ context.Context, start, end time.Time, _ int64, groupIDs []int64) ([]GroupUsageAccountStats, error) {
	window := end.Sub(start)
	s.windows = append(s.windows, window)
	s.groups = append(s.groups, append([]int64(nil), groupIDs...))
	return append([]GroupUsageAccountStats(nil), s.rowsByWindow[window]...), nil
}

func TestDynamicBootstrapChoosesShortestSufficientWindow(t *testing.T) {
	source := &bootstrapWindowSource{
		rowsByWindow: map[time.Duration][]GroupUsageAccountStats{
			time.Hour: {
				{GroupID: 24, AccountID: 1, Requests: 30, StandardCost: 5, BaseCost: 5, CurrentAccountRate: 0.1},
				{GroupID: 25, AccountID: 1, Requests: 1, StandardCost: 1, BaseCost: 1, CurrentAccountRate: 0.1},
			},
			6 * time.Hour: {
				{GroupID: 25, AccountID: 1, Requests: 30, StandardCost: 5, BaseCost: 5, CurrentAccountRate: 0.1},
			},
		},
	}
	choices, insufficient, err := loadDynamicBootstrap(
		context.Background(),
		source,
		time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC),
		1,
		[]int64{24, 25, 26},
	)
	if err != nil {
		t.Fatal(err)
	}
	if choices[24].window != time.Hour || choices[25].window != 6*time.Hour {
		t.Fatalf("unexpected bootstrap choices: %+v", choices)
	}
	if len(insufficient) != 1 || insufficient[0] != 26 {
		t.Fatalf("unexpected insufficient groups: %v", insufficient)
	}
	if len(source.windows) != 3 || source.windows[0] != time.Hour || source.windows[1] != 6*time.Hour || source.windows[2] != 24*time.Hour {
		t.Fatalf("unexpected bootstrap windows: %v", source.windows)
	}
	if len(source.groups[0]) != 3 || len(source.groups[1]) != 2 || len(source.groups[2]) != 1 || source.groups[1][0] != 25 || source.groups[2][0] != 26 {
		t.Fatalf("bootstrap did not narrow unresolved groups: %v", source.groups)
	}
}

func TestDynamicSyncFreezesWithoutUsageAndRepricesAccountChanges(t *testing.T) {
	first := testChannel("https://one.test", 0.15)
	first.AccountID = 1
	first.AccountName = "plus"
	first.AccountRateMultiplier = 0.1
	second := first
	second.AccountID = 2
	second.AccountName = "pro"
	second.AccountRateMultiplier = 0.2
	source := &incrementalTestSource{
		staticChannelSource: &staticChannelSource{channels: []Channel{first, second}},
		latestID:            100,
		bootstrap: []GroupUsageAccountStats{
			{GroupID: 24, AccountID: 1, Requests: 20, StandardCost: 50, BaseCost: 50, CurrentAccountRate: 0.1},
			{GroupID: 24, AccountID: 2, Requests: 20, StandardCost: 50, BaseCost: 50, CurrentAccountRate: 0.2},
		},
	}
	putCount := 0
	updatedRate := 0.0
	admin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload groupUpdate
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Error(err)
		}
		putCount++
		updatedRate = payload.RateMultiplier
		for index := range source.channels {
			source.channels[index].Group.RateMultiplier = updatedRate
		}
		writeJSON(t, w, map[string]any{"code": 0})
	}))
	defer admin.Close()

	syncer := newTestSyncer(t, source, admin.URL, false, 1, "", 1)
	syncer.store = StateStore{Path: filepath.Join(t.TempDir(), "state.json")}
	syncer.logger = log.New(io.Discard, "", 0)
	now := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	if err := syncer.RunOnce(context.Background(), now); err != nil {
		t.Fatal(err)
	}
	if putCount != 0 || syncer.state.DynamicGroups[24].LastUsageID != 100 || len(source.bootstrapWindows) != 1 || source.bootstrapWindows[0] != time.Hour {
		t.Fatalf("bootstrap unexpectedly changed rate: puts=%d state=%+v", putCount, syncer.state.DynamicGroups[24])
	}
	if err := syncer.RunOnce(context.Background(), now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if putCount != 0 || len(source.watermarks) != 1 || source.watermarks[0] != 100 || source.bootstrapRun != 1 {
		t.Fatalf("idle cycle did not freeze: puts=%d watermarks=%v", putCount, source.watermarks)
	}

	source.channels[1].AccountRateMultiplier = 0.4
	if err := syncer.RunOnce(context.Background(), now.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if putCount != 1 || !almostEqual(updatedRate, 0.17) {
		t.Fatalf("account rate change was not immediately repriced: puts=%d rate=%.4f", putCount, updatedRate)
	}

	source.latestID = 101
	source.incremental = []GroupUsageAccountStats{
		{GroupID: 24, AccountID: 2, Requests: 1, StandardCost: 5, BaseCost: 5, CurrentAccountRate: 0.4},
	}
	before := syncer.state.DynamicGroups[24].Fast.Denominator
	if err := syncer.RunOnce(context.Background(), now.Add(3*time.Minute)); err != nil {
		t.Fatal(err)
	}
	after := syncer.state.DynamicGroups[24].Fast.Denominator
	if after <= before || syncer.state.DynamicGroups[24].LastUsageID != 101 {
		t.Fatalf("new usage was not consumed: before=%.4f after=%.4f state=%+v", before, after, syncer.state.DynamicGroups[24])
	}
	if err := syncer.RunOnce(context.Background(), now.Add(4*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if !almostEqual(syncer.state.DynamicGroups[24].Fast.Denominator, after) {
		t.Fatalf("usage was consumed twice: first=%.8f second=%.8f", after, syncer.state.DynamicGroups[24].Fast.Denominator)
	}
}

func TestDynamicPendingTargetSurvivesRestartAndRetriesWithoutReconsumingUsage(t *testing.T) {
	first := testChannel("https://one.test", 0.15)
	first.AccountID = 1
	first.AccountName = "plus"
	first.AccountRateMultiplier = 0.1
	second := first
	second.AccountID = 2
	second.AccountName = "pro"
	second.AccountRateMultiplier = 0.2
	source := &incrementalTestSource{
		staticChannelSource: &staticChannelSource{channels: []Channel{first, second}},
		latestID:            100,
		bootstrap: []GroupUsageAccountStats{
			{GroupID: 24, AccountID: 1, Requests: 20, StandardCost: 50, BaseCost: 50, CurrentAccountRate: 0.1},
			{GroupID: 24, AccountID: 2, Requests: 20, StandardCost: 50, BaseCost: 50, CurrentAccountRate: 0.2},
		},
	}
	var putRates []float64
	admin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload groupUpdate
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Error(err)
		}
		putRates = append(putRates, payload.RateMultiplier)
		if len(putRates) == 1 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		writeJSON(t, w, map[string]any{"code": 0})
	}))
	defer admin.Close()

	statePath := filepath.Join(t.TempDir(), "state.json")
	store := StateStore{Path: statePath}
	syncer := newTestSyncer(t, source, admin.URL, false, 1, "", 1)
	syncer.store = store
	syncer.logger = log.New(io.Discard, "", 0)
	now := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	if err := syncer.RunOnce(context.Background(), now); err != nil {
		t.Fatal(err)
	}
	source.latestID = 101
	source.incremental = []GroupUsageAccountStats{
		{GroupID: 24, AccountID: 2, Requests: 1, StandardCost: 5, BaseCost: 5, CurrentAccountRate: 0.4},
	}
	beforeRetry := syncer.state.DynamicGroups[24].Fast.Denominator
	if err := syncer.RunOnce(context.Background(), now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	failedState := syncer.state.DynamicGroups[24]
	if len(putRates) != 1 || !failedState.HasPendingTarget || failedState.PendingTarget <= 0 || failedState.LastUsageID != 101 {
		t.Fatalf("failed publish did not retain replay state: puts=%v state=%+v", putRates, failedState)
	}
	if failedState.Fast.Denominator <= beforeRetry {
		t.Fatalf("incremental usage was not consumed before publish failure: before=%.8f after=%.8f", beforeRetry, failedState.Fast.Denominator)
	}

	loaded, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	persisted := loaded.DynamicGroups[24]
	if persisted == nil || !persisted.HasPendingTarget || persisted.PendingTarget != failedState.PendingTarget || persisted.LastUsageID != 101 {
		t.Fatalf("pending replay state was not persisted: %+v", persisted)
	}
	persistedDenominator := persisted.Fast.Denominator

	source.latestID = 102
	source.incremental = []GroupUsageAccountStats{
		{GroupID: 24, AccountID: 1, Requests: 1, StandardCost: 5, BaseCost: 5, CurrentAccountRate: 0.5},
	}
	restarted := newTestSyncer(t, source, admin.URL, false, 1, "", 1)
	restarted.store = store
	restarted.state = loaded
	restarted.logger = log.New(io.Discard, "", 0)
	if err := restarted.RunOnce(context.Background(), now.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if len(putRates) != 2 || !almostEqual(putRates[0], putRates[1]) {
		t.Fatalf("restart did not retry the same absolute target: puts=%v", putRates)
	}
	if !almostEqual(restarted.state.DynamicGroups[24].Fast.Denominator, persistedDenominator) {
		t.Fatalf("restart re-consumed usage: before=%.8f after=%.8f", persistedDenominator, restarted.state.DynamicGroups[24].Fast.Denominator)
	}
	if restarted.state.DynamicGroups[24].HasPendingTarget {
		t.Fatal("pending target was not cleared after successful retry")
	}
	if len(source.watermarks) != 1 || source.watermarks[0] != 100 {
		t.Fatalf("pending retry should not query usage: %v", source.watermarks)
	}
	if restarted.state.DynamicGroups[24].LastUsageID != 101 {
		t.Fatalf("pending retry advanced usage watermark: got %d, want 101", restarted.state.DynamicGroups[24].LastUsageID)
	}
	beforeCatchUp := restarted.state.DynamicGroups[24].Fast.Denominator
	if err := restarted.RunOnce(context.Background(), now.Add(3*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if restarted.state.DynamicGroups[24].Fast.Denominator <= beforeCatchUp || restarted.state.DynamicGroups[24].LastUsageID != 102 {
		t.Fatalf("usage accumulated while pending was not consumed after retry: before=%.8f state=%+v", beforeCatchUp, restarted.state.DynamicGroups[24])
	}
	if len(source.watermarks) != 2 || source.watermarks[1] != 101 {
		t.Fatalf("usage watermark was not advanced after pending retry: %v", source.watermarks)
	}
}

func TestDynamicPendingTargetReplaysAfterSuccessfulPublishBeforeSave(t *testing.T) {
	first := testChannel("https://one.test", 0.15)
	first.AccountID = 1
	first.AccountName = "plus"
	first.AccountRateMultiplier = 0.1
	second := first
	second.AccountID = 2
	second.AccountName = "pro"
	second.AccountRateMultiplier = 0.2
	source := &incrementalTestSource{
		staticChannelSource: &staticChannelSource{channels: []Channel{first, second}},
		latestID:            100,
	}
	state := seedDynamicGroupState([]GroupUsageAccountStats{
		{GroupID: 24, AccountID: 1, Requests: 20, StandardCost: 50, BaseCost: 50, CurrentAccountRate: 0.1},
		{GroupID: 24, AccountID: 2, Requests: 20, StandardCost: 50, BaseCost: 50, CurrentAccountRate: 0.2},
	}, 100)
	if state == nil {
		t.Fatal("failed to seed dynamic state")
	}
	state.PendingTarget = 0.17
	state.HasPendingTarget = true
	statePath := filepath.Join(t.TempDir(), "state.json")
	store := StateStore{Path: statePath}
	persistedState := newState()
	persistedState.DynamicGroups[24] = state
	if err := store.Save(persistedState); err != nil {
		t.Fatal(err)
	}

	var putRates []float64
	admin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload groupUpdate
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Error(err)
		}
		putRates = append(putRates, payload.RateMultiplier)
		writeJSON(t, w, map[string]any{"code": 0})
	}))
	defer admin.Close()

	loaded, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	syncer := newTestSyncer(t, source, admin.URL, false, 1, "", 1)
	syncer.store = store
	syncer.state = loaded
	syncer.logger = log.New(io.Discard, "", 0)
	group := source.channels[0].Group
	report := newSyncReport("group", source.channels)
	syncer.publishDynamicChange(context.Background(), &group, loaded.DynamicGroups[24], 0.17, 0.15, 0.15, 0.15, "重放", dynamicUsageSummary{}, report)
	if loaded.DynamicGroups[24].HasPendingTarget {
		t.Fatal("successful publish did not clear in-memory pending target")
	}
	diskState, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if !diskState.DynamicGroups[24].HasPendingTarget {
		t.Fatal("test setup did not preserve pending target on disk")
	}

	restarted := newTestSyncer(t, source, admin.URL, false, 1, "", 1)
	restarted.store = store
	restarted.state = diskState
	restarted.logger = log.New(io.Discard, "", 0)
	if err := restarted.RunOnce(context.Background(), time.Date(2026, 8, 26, 12, 1, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	if len(putRates) != 2 || !almostEqual(putRates[0], 0.17) || !almostEqual(putRates[1], 0.17) {
		t.Fatalf("stale pending target was not replayed idempotently: puts=%v", putRates)
	}
	if restarted.state.DynamicGroups[24].HasPendingTarget {
		t.Fatal("replayed pending target was not cleared")
	}
}

func TestDynamicPendingTargetSurvivesInvalidStateReset(t *testing.T) {
	state := newState()
	state.DynamicGroups[24] = &DynamicGroupState{
		Initialized:      true,
		LastUsageID:      100,
		Fast:             DynamicCostMemory{Denominator: 5},
		Slow:             DynamicCostMemory{Denominator: 100},
		LastAccountRates: map[int64]float64{},
		PendingTarget:    0.17,
		HasPendingTarget: true,
	}
	syncer := &Syncer{state: state}

	watermarks, uninitialized := syncer.prepareDynamicStates([]int64{24}, 99)
	recovered := state.DynamicGroups[24]
	if len(watermarks) != 0 || len(uninitialized) != 1 || recovered == nil {
		t.Fatalf("invalid state was not reinitialized: watermarks=%v uninitialized=%v state=%+v", watermarks, uninitialized, recovered)
	}
	if !recovered.HasPendingTarget || !almostEqual(recovered.PendingTarget, 0.17) {
		t.Fatalf("pending target was lost during state reset: %+v", recovered)
	}
}

func TestDynamicPendingTargetRetriesWithoutValidMemory(t *testing.T) {
	channel := testChannel("https://one.test", 0.10)
	state := newState()
	state.DynamicGroups[24] = &DynamicGroupState{
		Initialized:      true,
		Fast:             DynamicCostMemory{Denominator: 0},
		Slow:             DynamicCostMemory{Denominator: 0},
		LastAccountRates: map[int64]float64{},
		PendingTarget:    0.17,
		HasPendingTarget: true,
	}
	var updated groupUpdate
	admin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&updated); err != nil {
			t.Error(err)
		}
		writeJSON(t, w, map[string]any{"code": 0})
	}))
	defer admin.Close()

	syncer := newTestSyncer(t, &staticChannelSource{channels: []Channel{channel}}, admin.URL, false, 1, "", 1)
	syncer.state = state
	report := newSyncReport("group", []Channel{channel})
	syncer.publishDynamicGroup(context.Background(), &channel.Group, state.DynamicGroups[24], false, false, "待发布重试", dynamicUsageSummary{}, report)

	if !almostEqual(updated.RateMultiplier, 0.17) || state.DynamicGroups[24].HasPendingTarget {
		t.Fatalf("pending target was not retried independently of memory: update=%+v state=%+v", updated, state.DynamicGroups[24])
	}
}

func TestDynamicBootstrapPreservesPendingRetryFailureEvidence(t *testing.T) {
	channel := testChannel("https://one.test", 0.10)
	admin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer admin.Close()

	syncer := newTestSyncer(t, &staticChannelSource{channels: []Channel{channel}}, admin.URL, false, 1, "", 1)
	syncer.state.DynamicGroups[24] = &DynamicGroupState{
		PendingTarget:    0.17,
		HasPendingTarget: true,
	}
	report := newSyncReport("group", []Channel{channel})
	plan := dynamicGroupPlan{
		bindings:   buildGroupBindings([]Channel{channel}),
		suspicious: map[int64]bool{},
	}

	syncer.reportDynamicBootstrap(context.Background(), plan, nil, []int64{24}, 100, report)

	row := report.rows["account:18/group:24"]
	if row == nil || row.status != reportStatusFailed {
		t.Fatalf("pending retry status = %+v, want failed", row)
	}
	if row.window != "待发布重试" || row.detail != "待发布目标=0.1700" {
		t.Fatalf("pending retry evidence was overwritten: %+v", row)
	}
}
