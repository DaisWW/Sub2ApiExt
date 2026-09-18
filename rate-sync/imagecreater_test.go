package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"testing"
	"time"
)

type imageCreaterUsageCall struct {
	AccountIDs []int64
	AfterID    int64
	ThroughID  int64
}

type imageCreaterTestSource struct {
	*staticChannelSource
	latestID int64
	usage    []AccountUsageStats
	calls    []imageCreaterUsageCall
}

func (s *imageCreaterTestSource) LatestAccountUsageID(context.Context) (int64, error) {
	return s.latestID, nil
}

func (s *imageCreaterTestSource) ListAccountUsageSince(_ context.Context, accountIDs []int64, afterID, throughID int64) ([]AccountUsageStats, error) {
	s.calls = append(s.calls, imageCreaterUsageCall{
		AccountIDs: append([]int64(nil), accountIDs...),
		AfterID:    afterID,
		ThroughID:  throughID,
	})
	return append([]AccountUsageStats(nil), s.usage...), nil
}

type imageCreaterSnapshot struct {
	Balance       float64
	TodayCost     float64
	TodayRequests int64
}

func TestImageCreaterBalancePublishesAfterTwoMatchingWindows(t *testing.T) {
	var snapshotMu sync.RWMutex
	snapshot := imageCreaterSnapshot{Balance: 100, TodayCost: 1, TodayRequests: 10}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/user/balance" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") != "Bearer key-a" && r.Header.Get("Authorization") != "Bearer key-b" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		snapshotMu.RLock()
		current := snapshot
		snapshotMu.RUnlock()
		writeJSON(t, w, map[string]any{
			"balance":       current.Balance,
			"todayCost":     current.TodayCost,
			"todayRequests": current.TodayRequests,
			"unit":          "USD",
		})
	}))
	defer upstream.Close()

	var putMu sync.Mutex
	putRates := make(map[int64][]float64)
	admin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var accountID int64
		if _, err := fmt.Sscanf(r.URL.Path, "/api/v1/admin/accounts/%d", &accountID); err != nil || r.Method != http.MethodPut {
			http.NotFound(w, r)
			return
		}
		var payload accountUpdate
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Errorf("decode account update: %v", err)
		}
		putMu.Lock()
		putRates[accountID] = append(putRates[accountID], payload.RateMultiplier)
		putMu.Unlock()
		writeJSON(t, w, map[string]any{"code": 0})
	}))
	defer admin.Close()

	channels := imageCreaterTestChannels(upstream.URL + "/api/v1")
	source := &imageCreaterTestSource{
		staticChannelSource: &staticChannelSource{channels: channels},
		latestID:            100,
	}
	syncer := newAccountTestSyncer(t, source, admin.URL, false, "", 1)
	now := time.Date(2026, 9, 17, 12, 0, 0, 0, time.Local)

	if err := syncer.RunOnce(context.Background(), now); err != nil {
		t.Fatal(err)
	}
	if len(source.calls) != 0 || len(putRates) != 0 {
		t.Fatalf("baseline consumed usage or published a rate: calls=%+v puts=%+v", source.calls, putRates)
	}
	hostKey, _ := imageCreaterBaseKey(channels[0].BaseURL)
	hostState := syncer.state.ImageCreaterHosts[hostKey]
	if hostState == nil || hostState.LastUsageID != 100 || !hostState.Initialized {
		t.Fatalf("missing initial host watermark: %+v", hostState)
	}

	snapshotMu.Lock()
	snapshot = imageCreaterSnapshot{Balance: 99.9, TodayCost: 1.1, TodayRequests: 11}
	snapshotMu.Unlock()
	source.latestID = 101
	source.usage = []AccountUsageStats{{AccountID: 18, Requests: 1, BaseCost: 0.4}}
	if err := syncer.RunOnce(context.Background(), now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	firstState := syncer.state.Rules["account:18"]
	if firstState == nil || firstState.Template != templateImageCreaterBalance || firstState.CandidateUpstreamRate != 0.25 || firstState.CandidateCount != 1 {
		t.Fatalf("first balance candidate = %+v", firstState)
	}
	if len(putRates) != 0 {
		t.Fatalf("first observation must not publish: %+v", putRates)
	}

	snapshotMu.Lock()
	snapshot = imageCreaterSnapshot{Balance: 99.85, TodayCost: 1.15, TodayRequests: 12}
	snapshotMu.Unlock()
	source.latestID = 102
	source.usage = []AccountUsageStats{{AccountID: 18, Requests: 1, BaseCost: 0.2}}
	if err := syncer.RunOnce(context.Background(), now.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	putMu.Lock()
	gotRates := append([]float64(nil), putRates[18]...)
	otherRates := append([]float64(nil), putRates[19]...)
	putMu.Unlock()
	if !reflect.DeepEqual(gotRates, []float64{0.25}) || len(otherRates) != 0 {
		t.Fatalf("published rates = account18:%v account19:%v", gotRates, otherRates)
	}
	if firstState.CandidateCount != 0 || firstState.CandidateUpstreamRate != 0 {
		t.Fatalf("published candidate was not cleared: %+v", firstState)
	}
	wantCalls := []imageCreaterUsageCall{
		{AccountIDs: []int64{18, 19}, AfterID: 100, ThroughID: 101},
		{AccountIDs: []int64{18, 19}, AfterID: 101, ThroughID: 102},
	}
	if !reflect.DeepEqual(source.calls, wantCalls) {
		t.Fatalf("usage watermark calls = %+v, want %+v", source.calls, wantCalls)
	}
}

func TestImageCreaterBalanceResetsCandidateForMixedAccounts(t *testing.T) {
	snapshot := imageCreaterSnapshot{Balance: 99.925, TodayCost: 1.075, TodayRequests: 12}
	upstream := newImageCreaterTestUpstream(t, &snapshot)
	defer upstream.Close()
	channels := imageCreaterTestChannels(upstream.URL + "/api/v1")
	source := &imageCreaterTestSource{
		staticChannelSource: &staticChannelSource{channels: channels},
		latestID:            102,
		usage: []AccountUsageStats{
			{AccountID: 18, Requests: 1, BaseCost: 0.2},
			{AccountID: 19, Requests: 1, BaseCost: 0.1},
		},
	}
	syncer := newAccountTestSyncer(t, source, "http://admin.invalid", false, "", 1)
	group := imageCreaterChannelGroups(channels)[0]
	syncer.state.ImageCreaterHosts[group.key] = &ImageCreaterHostState{
		Identity:      imageCreaterGroupIdentity(group.channels),
		Day:           "2026-09-17",
		Balance:       100,
		TodayCost:     1,
		TodayRequests: 10,
		LastUsageID:   100,
		Initialized:   true,
	}
	for _, channel := range group.channels {
		syncer.state.Rules[channelStateKeyForTarget(channel, "account")] = &RuleState{
			Identity: channelIdentityForTarget(channel, "account"),
			Template: templateImageCreaterBalance,
		}
	}
	syncer.state.Rules["account:18"].CandidateUpstreamRate = 0.25
	syncer.state.Rules["account:18"].CandidateCount = 1

	matched, results := syncer.syncImageCreaterGroup(
		context.Background(), source, group,
		time.Date(2026, 9, 17, 12, 0, 0, 0, time.Local),
		newSyncReport("account", channels),
	)
	var skipped skipError
	if !matched || !errors.As(results[18], &skipped) || !errors.As(results[19], &skipped) {
		t.Fatalf("mixed window results = matched:%t errors:%+v", matched, results)
	}
	state := syncer.state.Rules["account:18"]
	if state.CandidateCount != 0 || state.CandidateUpstreamRate != 0 {
		t.Fatalf("unsafe window retained candidate: %+v", state)
	}
	if syncer.state.ImageCreaterHosts[group.key].LastUsageID != 102 {
		t.Fatalf("unsafe window did not advance the watermark: %+v", syncer.state.ImageCreaterHosts[group.key])
	}
}

func TestImageCreaterBalanceResetsCandidateAfterProbeFailure(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unavailable", http.StatusBadGateway)
	}))
	defer upstream.Close()
	channels := imageCreaterTestChannels(upstream.URL + "/api/v1")
	source := &imageCreaterTestSource{
		staticChannelSource: &staticChannelSource{channels: channels},
		latestID:            101,
	}
	syncer := newAccountTestSyncer(t, source, "http://admin.invalid", false, "", 1)
	group := imageCreaterChannelGroups(channels)[0]
	syncer.state.ImageCreaterHosts[group.key] = &ImageCreaterHostState{
		Identity:      imageCreaterGroupIdentity(group.channels),
		Day:           "2026-09-17",
		Balance:       100,
		TodayCost:     1,
		TodayRequests: 10,
		LastUsageID:   100,
		Initialized:   true,
	}
	for _, channel := range group.channels {
		syncer.state.Rules[channelStateKeyForTarget(channel, "account")] = &RuleState{
			Identity: channelIdentityForTarget(channel, "account"),
			Template: templateImageCreaterBalance,
		}
	}
	syncer.state.Rules["account:18"].CandidateUpstreamRate = 0.25
	syncer.state.Rules["account:18"].CandidateCount = 1

	matched, results := syncer.syncImageCreaterGroup(
		context.Background(), source, group,
		time.Date(2026, 9, 17, 12, 0, 0, 0, time.Local),
		newSyncReport("account", channels),
	)
	if !matched || results[18] == nil || results[19] == nil {
		t.Fatalf("probe failure results = matched:%t errors:%+v", matched, results)
	}
	state := syncer.state.Rules["account:18"]
	if state.CandidateCount != 0 || state.CandidateUpstreamRate != 0 {
		t.Fatalf("probe failure retained candidate: %+v", state)
	}
	if syncer.state.ImageCreaterHosts[group.key] == nil {
		t.Fatal("probe failure discarded the last verified baseline")
	}
}

func TestImageCreaterBalanceClearsKnownStateWhenEndpointNoLongerMatches(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}))
	defer upstream.Close()
	channels := imageCreaterTestChannels(upstream.URL + "/api/v1")
	source := &imageCreaterTestSource{staticChannelSource: &staticChannelSource{channels: channels}}
	syncer := newAccountTestSyncer(t, source, "http://admin.invalid", false, "", 1)
	group := imageCreaterChannelGroups(channels)[0]
	syncer.state.ImageCreaterHosts[group.key] = &ImageCreaterHostState{Initialized: true}
	for _, channel := range group.channels {
		syncer.state.Rules[channelStateKeyForTarget(channel, "account")] = &RuleState{
			Identity:              channelIdentityForTarget(channel, "account"),
			Template:              templateImageCreaterBalance,
			CandidateUpstreamRate: 0.25,
			CandidateCount:        1,
			Day:                   "2026-09-17",
			Cost:                  1,
			ActualCost:            0.25,
			HasBaseline:           true,
		}
	}

	matched, results := syncer.syncImageCreaterGroup(
		context.Background(), source, group,
		time.Date(2026, 9, 17, 12, 0, 0, 0, time.Local),
		newSyncReport("account", channels),
	)
	if matched || results != nil {
		t.Fatalf("unmatched endpoint result = matched:%t errors:%+v", matched, results)
	}
	if syncer.state.ImageCreaterHosts[group.key] != nil {
		t.Fatal("unmatched endpoint retained the shared balance baseline")
	}
	for _, channel := range group.channels {
		state := syncer.state.Rules[channelStateKeyForTarget(channel, "account")]
		if state.Template != "" || state.CandidateCount != 0 || state.CandidateUpstreamRate != 0 || state.HasBaseline {
			t.Fatalf("unmatched endpoint retained imageCreater state: %+v", state)
		}
	}
}

func TestEvaluateImageCreaterWindowRejectsUnmatchedEvidence(t *testing.T) {
	tests := []struct {
		name          string
		deltaRequests int64
		deltaCost     float64
		deltaBalance  float64
		usage         []AccountUsageStats
	}{
		{
			name:          "external request",
			deltaRequests: 2,
			deltaCost:     0.1,
			deltaBalance:  0.1,
			usage:         []AccountUsageStats{{AccountID: 18, Requests: 1, BaseCost: 0.4}},
		},
		{
			name:          "recharge or adjustment",
			deltaRequests: 1,
			deltaCost:     0.1,
			deltaBalance:  -9.9,
			usage:         []AccountUsageStats{{AccountID: 18, Requests: 1, BaseCost: 0.4}},
		},
		{
			name:          "counter regression",
			deltaRequests: -1,
			deltaCost:     -0.1,
			deltaBalance:  0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			accountID, _, reason := evaluateImageCreaterWindow(tt.deltaRequests, tt.deltaCost, tt.deltaBalance, tt.usage)
			if accountID != 0 || reason == "" {
				t.Fatalf("unsafe window was accepted: account=%d reason=%q", accountID, reason)
			}
		})
	}
}

func TestImageCreaterBalanceResetsAcrossLocalDay(t *testing.T) {
	snapshot := imageCreaterSnapshot{Balance: 9, TodayCost: 2, TodayRequests: 20}
	upstream := newImageCreaterTestUpstream(t, &snapshot)
	defer upstream.Close()
	channels := imageCreaterTestChannels(upstream.URL + "/api/v1")
	source := &imageCreaterTestSource{
		staticChannelSource: &staticChannelSource{channels: channels},
		latestID:            30,
		usage:               []AccountUsageStats{{AccountID: 18, Requests: 10, BaseCost: 4}},
	}
	syncer := newAccountTestSyncer(t, source, "http://admin.invalid", false, "", 1)
	group := imageCreaterChannelGroups(channels)[0]
	syncer.state.ImageCreaterHosts[group.key] = &ImageCreaterHostState{
		Identity:      imageCreaterGroupIdentity(group.channels),
		Day:           "2026-09-16",
		Balance:       10,
		TodayCost:     1,
		TodayRequests: 10,
		LastUsageID:   20,
		Initialized:   true,
	}
	for _, channel := range group.channels {
		syncer.state.Rules[channelStateKeyForTarget(channel, "account")] = &RuleState{
			Identity:              channelIdentityForTarget(channel, "account"),
			Template:              templateImageCreaterBalance,
			CandidateUpstreamRate: 0.25,
			CandidateCount:        1,
		}
	}

	matched, results := syncer.syncImageCreaterGroup(
		context.Background(), source, group,
		time.Date(2026, 9, 17, 0, 1, 0, 0, time.Local),
		newSyncReport("account", channels),
	)
	if !matched || results[18] != nil || results[19] != nil {
		t.Fatalf("day reset results = matched:%t errors:%+v", matched, results)
	}
	state := syncer.state.ImageCreaterHosts[group.key]
	if state.Day != "2026-09-17" || state.LastUsageID != 30 || state.Balance != 9 || state.TodayCost != 2 || state.TodayRequests != 20 {
		t.Fatalf("day reset did not replace the full baseline: %+v", state)
	}
	if len(source.calls) != 0 || syncer.state.Rules["account:18"].CandidateCount != 0 {
		t.Fatalf("day reset consumed usage or retained a candidate: calls=%+v rule=%+v", source.calls, syncer.state.Rules["account:18"])
	}
}

func TestUpstreamBasePathEndpointPreservesAPIRoot(t *testing.T) {
	endpoint, err := upstreamBasePathEndpoint("https://image.example/api/v1/?old=value", "/user/balance", "")
	if err != nil {
		t.Fatal(err)
	}
	if endpoint != "https://image.example/api/v1/user/balance" {
		t.Fatalf("endpoint = %q", endpoint)
	}
}

func imageCreaterTestChannels(baseURL string) []Channel {
	first := testChannel(baseURL, 0.1)
	first.AccountName = "image-a"
	first.AccountRateMultiplier = 0.5
	first.APIKey = "key-a"
	second := first
	second.AccountID = 19
	second.AccountName = "image-b"
	second.AccountRateMultiplier = 0.5
	second.APIKey = "key-b"
	second.Group.ID = 25
	second.Group.Name = "image-b"
	return []Channel{first, second}
}

func newImageCreaterTestUpstream(t *testing.T, snapshot *imageCreaterSnapshot) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/user/balance" {
			http.NotFound(w, r)
			return
		}
		writeJSON(t, w, map[string]any{
			"balance":       snapshot.Balance,
			"todayCost":     snapshot.TodayCost,
			"todayRequests": snapshot.TodayRequests,
			"unit":          "USD",
		})
	}))
}
