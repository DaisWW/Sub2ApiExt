package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
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

func TestImageCreaterInitialRatesPublishWithoutUsage(t *testing.T) {
	snapshot := imageCreaterSnapshot{Balance: 100, TodayCost: 1, TodayRequests: 10}
	upstream := newImageCreaterTestUpstream(t, &snapshot)
	defer upstream.Close()
	upstreamURL, _ := url.Parse(upstream.URL)
	puts := make(map[int64][]float64)
	admin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var id int64
		if _, err := fmt.Sscanf(r.URL.Path, "/api/v1/admin/accounts/%d", &id); err != nil || r.Method != http.MethodPut {
			http.NotFound(w, r)
			return
		}
		var payload accountUpdate
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Error(err)
		}
		puts[id] = append(puts[id], payload.RateMultiplier)
		writeJSON(t, w, map[string]any{"code": 0})
	}))
	defer admin.Close()
	source := &imageCreaterTestSource{
		staticChannelSource: &staticChannelSource{channels: imageCreaterInitialTestChannels()},
		latestID:            100,
	}
	syncer := newAccountTestSyncer(t, source, admin.URL, false, source.channels[0].BaseURL, 0.9)
	syncer.client = &http.Client{Transport: imageCreaterInitialTestTransport{upstream: upstreamURL}}
	now := time.Date(2026, 9, 18, 12, 0, 0, 0, time.Local)
	if err := syncer.RunOnce(context.Background(), now); err != nil {
		t.Fatal(err)
	}
	want := map[int64][]float64{76: {0.225}, 77: {0.162}, 78: {0.225}}
	if !reflect.DeepEqual(puts, want) {
		t.Fatalf("initial rates = %+v, want %+v", puts, want)
	}
	if len(source.calls) != 0 {
		t.Fatalf("initialization consumed historical usage: %+v", source.calls)
	}
	loaded, err := syncer.store.Load()
	if err != nil {
		t.Fatal(err)
	}
	syncer.state = loaded
	if err := syncer.RunOnce(context.Background(), now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(puts, want) {
		t.Fatalf("restart reapplied initial rates: %+v", puts)
	}

	// The first valid real window must replace the temporary rate.
	snapshot = imageCreaterSnapshot{Balance: 99.88, TodayCost: 1.12, TodayRequests: 11}
	source.latestID = 101
	source.usage = []AccountUsageStats{{AccountID: 76, Requests: 1, BaseCost: 0.4}}
	if err := syncer.RunOnce(context.Background(), now.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(puts[76], []float64{0.225, 0.27}) {
		t.Fatalf("first real rate did not replace the initial rate: %+v", puts)
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

func TestImageCreaterInitialRatesRetryPartialFailureWithoutOverwritingManualRates(t *testing.T) {
	channels := imageCreaterInitialTestChannels()
	channels[0].AccountRateMultiplier = 0.3
	grok := channels[0]
	grok.AccountID = 79
	grok.AccountRateMultiplier = 0.12
	grok.Group.ID = 56
	channels = append(channels, grok)
	puts := make(map[int64][]float64)
	admin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var id int64
		if _, err := fmt.Sscanf(r.URL.Path, "/api/v1/admin/accounts/%d", &id); err != nil {
			http.NotFound(w, r)
			return
		}
		var payload accountUpdate
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Error(err)
		}
		puts[id] = append(puts[id], payload.RateMultiplier)
		if id == 78 && len(puts[id]) == 1 {
			http.Error(w, "temporary failure", http.StatusBadGateway)
			return
		}
		writeJSON(t, w, map[string]any{"code": 0})
	}))
	defer admin.Close()
	syncer := newAccountTestSyncer(t, &staticChannelSource{}, admin.URL, false, "", 1)
	report := newSyncReport("account", channels)
	if err := syncer.initializeImageCreaterRates(context.Background(), channels, report); err == nil {
		t.Fatal("partial failure was not reported")
	}
	if syncer.state.ImageCreaterInitialRatesApplied || channels[1].AccountRateMultiplier != 0.18 {
		t.Fatalf("partial initialization was incorrectly completed: state=%+v channels=%+v", syncer.state, channels)
	}
	if err := syncer.initializeImageCreaterRates(context.Background(), channels, report); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(puts, map[int64][]float64{77: {0.18}, 78: {0.25, 0.25}}) || channels[0].AccountRateMultiplier != 0.3 || channels[3].AccountRateMultiplier != 0.12 {
		t.Fatalf("retry overwrote manual rates or repeated a completed account: %+v", puts)
	}
}

func TestImageCreaterInitialRatesRespectDryRunAndScope(t *testing.T) {
	for _, scenario := range []string{"dry-run", "different-upstream", "different-group"} {
		t.Run(scenario, func(t *testing.T) {
			channels := imageCreaterInitialTestChannels()
			if scenario == "different-upstream" {
				channels[1].BaseURL = "https://other.example/api/v1"
			}
			if scenario == "different-group" {
				for i := range channels {
					channels[i].Group.ID = 99
				}
			}
			before := append([]Channel(nil), channels...)
			syncer := newAccountTestSyncer(t, &staticChannelSource{}, "http://admin.invalid", scenario == "dry-run", "", 1)
			if err := syncer.initializeImageCreaterRates(context.Background(), channels, newSyncReport("account", channels)); err != nil {
				t.Fatal(err)
			}
			if syncer.state.ImageCreaterInitialRatesApplied || !reflect.DeepEqual(channels, before) {
				t.Fatal("dry-run or unmatched scope initialized rates")
			}
		})
	}
}
