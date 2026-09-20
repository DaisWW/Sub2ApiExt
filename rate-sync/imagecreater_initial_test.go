package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type imageCreaterInitialTestTransport struct {
	upstream *url.URL
}

func (tr imageCreaterInitialTestTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Hostname() == "image.qzcy3.top" {
		req = req.Clone(req.Context())
		req.URL.Scheme = tr.upstream.Scheme
		req.URL.Host = tr.upstream.Host
	}
	return http.DefaultTransport.RoundTrip(req)
}

func imageCreaterInitialTestChannels() []Channel {
	channels := make([]Channel, 0, 3)
	for _, id := range []int64{76, 77, 78} {
		channel := testChannel("https://image.qzcy3.top/api/v1", 1)
		channel.AccountID = id
		channel.AccountName = fmt.Sprintf("renamed-%d", id)
		channel.AccountRateMultiplier = 1
		channel.Group.ID = 55
		channel.Group.Name = "renamed-group"
		channels = append(channels, channel)
	}
	return channels
}

func TestImageCreaterManualAccountsKeepCurrentRates(t *testing.T) {
	snapshot := imageCreaterSnapshot{Balance: 100, TodayCost: 1, TodayRequests: 10}
	var upstreamCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
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
	defer upstream.Close()
	upstreamURL, _ := url.Parse(upstream.URL)
	var puts atomic.Int32
	admin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			http.NotFound(w, r)
			return
		}
		puts.Add(1)
		writeJSON(t, w, map[string]any{"code": 0})
	}))
	defer admin.Close()

	channels := imageCreaterInitialTestChannels()
	for i, rate := range []float64{0.25, 0.18, 0.25} {
		channels[i].AccountRateMultiplier = rate
	}
	grok := channels[0]
	grok.AccountID = 79
	grok.AccountRateMultiplier = 0.12
	grok.Group.ID = 56
	channels = append(channels, grok)
	source := &imageCreaterTestSource{staticChannelSource: &staticChannelSource{channels: channels}, latestID: 100}
	syncer := newAccountTestSyncer(t, source, admin.URL, false, "", 1)
	syncer.config.ManualAccountBaseURLs = map[string]bool{historicalImageCreaterBaseKey: true}
	syncer.client = &http.Client{Transport: imageCreaterInitialTestTransport{upstream: upstreamURL}}
	var output bytes.Buffer
	syncer.logger = log.New(&output, "", 0)
	now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.Local)
	if err := syncer.RunOnce(context.Background(), now); err != nil {
		t.Fatal(err)
	}
	snapshot = imageCreaterSnapshot{Balance: 99.88, TodayCost: 1.12, TodayRequests: 11}
	source.latestID = 101
	source.usage = []AccountUsageStats{{AccountID: 76, Requests: 1, BaseCost: 0.4}}
	if err := syncer.RunOnce(context.Background(), now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if upstreamCalls.Load() != 0 || puts.Load() != 0 {
		t.Fatalf("manual accounts were probed or updated: upstream=%d puts=%d", upstreamCalls.Load(), puts.Load())
	}
	if !strings.Contains(output.String(), "暂不自动｜手动维护") {
		t.Fatalf("manual status missing from account report:\n%s", output.String())
	}
}

func TestImageCreaterManualAccountsDoNotApplyTemporaryInitialRates(t *testing.T) {
	channels := imageCreaterInitialTestChannels()
	var puts atomic.Int32
	admin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		puts.Add(1)
		writeJSON(t, w, map[string]any{"code": 0})
	}))
	defer admin.Close()
	syncer := newAccountTestSyncer(t, &staticChannelSource{channels: channels}, admin.URL, false, "", 1)
	syncer.config.ManualAccountBaseURLs = map[string]bool{historicalImageCreaterBaseKey: true}
	if err := syncer.RunOnce(context.Background(), time.Now()); err != nil {
		t.Fatal(err)
	}
	if puts.Load() != 0 || syncer.state.ImageCreaterInitialRatesApplied {
		t.Fatalf("manual accounts received temporary initial rates: puts=%d state=%+v", puts.Load(), syncer.state)
	}
}

func TestImageCreaterInitialGroupRebasesOldMemoryWithoutReplayingUsage(t *testing.T) {
	var puts []float64
	var syncer *Syncer
	admin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/admin/groups/55" || r.Method != http.MethodPut {
			http.NotFound(w, r)
			return
		}
		var payload groupUpdate
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Error(err)
		}
		if payload.DailyLimitUSD != -1 || payload.WeeklyLimitUSD != -1 || payload.MonthlyLimitUSD != -1 {
			t.Errorf("group quotas changed: %+v", payload)
		}
		puts = append(puts, payload.RateMultiplier)
		persisted, err := syncer.store.Load()
		if err != nil || !persisted.ImageCreaterInitialRatesApplied || !persisted.DynamicGroups[55].HasPendingTarget {
			t.Errorf("initial target was not checkpointed before publication: state=%+v err=%v", persisted, err)
		}
		if len(puts) == 1 {
			http.Error(w, "temporary failure", http.StatusBadGateway)
			return
		}
		writeJSON(t, w, map[string]any{"code": 0})
	}))
	defer admin.Close()
	channels := imageCreaterInitialTestChannels()
	channels[0].AccountRateMultiplier = 0.25
	channels[1].AccountRateMultiplier = 0.18
	channels[2].AccountRateMultiplier = 0.25
	source := &incrementalTestSource{staticChannelSource: &staticChannelSource{channels: channels}, latestID: 100}
	syncer = newTestSyncer(t, source, admin.URL, false, "", 1)
	state := seedDynamicGroupState([]GroupUsageAccountStats{{GroupID: 55, AccountID: 78, Requests: 30, StandardCost: 10, BaseCost: 10, AccountCost: 10, CurrentAccountRate: 1}}, 100)
	state.LastAccountRates[76] = 1
	state.LastAccountRates[77] = 1
	syncer.state.DynamicGroups[55] = state
	other := &DynamicGroupState{LastUsageID: 42}
	syncer.state.DynamicGroups[999] = other
	now := time.Date(2026, 9, 18, 12, 0, 0, 0, time.Local)
	if err := syncer.RunOnce(context.Background(), now); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(puts, []float64{0.25}) {
		t.Fatalf("initial group rate = %v, want [0.25]", puts)
	}
	state = syncer.state.DynamicGroups[55]
	fast, slow, predicted, ok := dynamicRates(state)
	if !ok || fast != 0.25 || slow != 0.25 || predicted != 0.25 || state.LastUsageID != 100 || source.bootstrapRun != 0 {
		t.Fatalf("memory was not safely rebased: state=%+v rates=%v/%v/%v bootstrap=%d", state, fast, slow, predicted, source.bootstrapRun)
	}
	if syncer.state.DynamicGroups[999] != other {
		t.Fatal("initialization changed an unrelated group")
	}
	if !state.HasPendingTarget || state.PendingTarget != 0.25 {
		t.Fatalf("failed publication lost its initial target: %+v", state)
	}
	loaded, err := syncer.store.Load()
	if err != nil {
		t.Fatal(err)
	}
	syncer.state = loaded
	if err := syncer.RunOnce(context.Background(), now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(puts, []float64{0.25, 0.25}) || syncer.state.DynamicGroups[55].HasPendingTarget {
		t.Fatalf("restart did not retry the initial target: %v", puts)
	}
	for i := range source.channels {
		source.channels[i].Group.RateMultiplier = 0.25
	}
	if err := syncer.RunOnce(context.Background(), now.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if len(puts) != 2 {
		t.Fatalf("old memory pulled the initialized group price upward: %v", puts)
	}
}
