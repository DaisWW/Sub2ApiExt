package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type staticChannelSource struct {
	channels []Channel
	err      error
}

func (s *staticChannelSource) List(context.Context) ([]Channel, error) {
	if s.err != nil {
		return nil, s.err
	}
	return append([]Channel(nil), s.channels...), nil
}

type usageChannelSource struct {
	*staticChannelSource
	usage []GroupUsageStats
}

func (s *usageChannelSource) ListGroupUsageStats(context.Context, time.Time, time.Time) ([]GroupUsageStats, error) {
	return append([]GroupUsageStats(nil), s.usage...), nil
}

type dynamicAdminSource struct {
	adminAPIKey string
	listCalls   int
}

func (s *dynamicAdminSource) AdminAPIKey(context.Context) (string, error) {
	return s.adminAPIKey, nil
}

func (s *dynamicAdminSource) List(context.Context) ([]Channel, error) {
	s.listCalls++
	return nil, nil
}

func TestSyncerWaitsForAdminAPIKeyAndRetries(t *testing.T) {
	source := &dynamicAdminSource{}
	syncer := newTestSyncer(t, source, "http://admin.invalid", false, 1, "", 1)
	syncer.config.AdminAPIKey = ""
	var output bytes.Buffer
	syncer.logger = log.New(&output, "", 0)

	if err := syncer.RunOnce(context.Background(), time.Now()); err != nil {
		t.Fatal(err)
	}
	if source.listCalls != 0 || !strings.Contains(output.String(), "Admin API Key 尚未配置") {
		t.Fatalf("unexpected waiting state, listCalls=%d:\n%s", source.listCalls, output.String())
	}

	source.adminAPIKey = "admin-later"
	if err := syncer.RunOnce(context.Background(), time.Now()); err != nil {
		t.Fatal(err)
	}
	if source.listCalls != 1 || syncer.config.AdminAPIKey != "admin-later" {
		t.Fatalf("retry failed, listCalls=%d key=%q", source.listCalls, syncer.config.AdminAPIKey)
	}
}

func TestSyncerDiscoversUsageTemplateAndAppliesFactor(t *testing.T) {
	values := []upstreamToday{
		{Cost: 10, ActualCost: 1},
		{Cost: 20, ActualCost: 2},
		{Cost: 30, ActualCost: 3},
	}
	upstreamCall := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/usage" || r.URL.Query().Get("days") != "1" {
			t.Errorf("unexpected upstream URL: %s", r.URL)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer sk-upstream" {
			t.Errorf("Authorization = %q", got)
		}
		index := upstreamCall
		if index >= len(values) {
			index = len(values) - 1
		}
		upstreamCall++
		writeJSON(t, w, map[string]any{"usage": map[string]any{"today": values[index]}})
	}))
	defer upstream.Close()

	daily := 5.0
	monthly := 0.0
	var updated groupUpdate
	putCount := 0
	admin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("x-api-key"); got != "admin-test" {
			t.Errorf("x-api-key = %q", got)
		}
		if r.Method != http.MethodPut || r.URL.Path != "/api/v1/admin/groups/24" {
			t.Errorf("unexpected admin request: %s %s", r.Method, r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&updated); err != nil {
			t.Errorf("decode update: %v", err)
		}
		putCount++
		writeJSON(t, w, map[string]any{"code": 0})
	}))
	defer admin.Close()

	channel := testChannel(upstream.URL, 0.1)
	channel.Group.DailyLimitUSD = &daily
	channel.Group.MonthlyLimitUSD = &monthly
	source := &staticChannelSource{channels: []Channel{channel}}
	syncer := newTestSyncer(t, source, admin.URL, false, 2, upstream.URL, 0.85)
	var output bytes.Buffer
	syncer.logger = log.New(&output, "", 0)
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.FixedZone("CST", 8*60*60))
	for i := 0; i < 3; i++ {
		if err := syncer.RunOnce(context.Background(), now.Add(time.Duration(i)*time.Minute)); err != nil {
			t.Fatal(err)
		}
	}

	if putCount != 1 || updated.RateMultiplier != 0.085 {
		t.Fatalf("updates = %d, payload = %+v", putCount, updated)
	}
	if updated.DailyLimitUSD != 5 || updated.WeeklyLimitUSD != -1 || updated.MonthlyLimitUSD != 0 {
		t.Fatalf("limits changed: %+v", updated)
	}
	state := syncer.state.Rules["account:18/group:24"]
	if state == nil || state.Template != templateUsageRatio {
		t.Fatalf("unexpected state: %+v", state)
	}
	if !strings.Contains(output.String(), "已自动识别价格模板: sub2api_usage") {
		t.Fatalf("missing template log:\n%s", output.String())
	}
}

func TestAccountTargetUpdatesAccountRateWithoutUpdatingGroup(t *testing.T) {
	values := []upstreamToday{
		{Cost: 10, ActualCost: 1},
		{Cost: 20, ActualCost: 2},
		{Cost: 30, ActualCost: 3},
	}
	upstreamCall := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/pricing" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if r.URL.Path != "/v1/usage" {
			t.Errorf("unexpected upstream URL: %s", r.URL)
		}
		index := upstreamCall
		if index >= len(values) {
			index = len(values) - 1
		}
		upstreamCall++
		writeJSON(t, w, map[string]any{"usage": map[string]any{"today": values[index]}})
	}))
	defer upstream.Close()

	channel := testChannel(upstream.URL, 0.2)
	channel.AccountRateMultiplier = 0.1
	source := &staticChannelSource{channels: []Channel{channel}}
	var updated accountUpdate
	accountPuts := 0
	groupPuts := 0
	admin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/admin/accounts/18":
			if r.Method != http.MethodPut {
				t.Errorf("account method = %s", r.Method)
			}
			if err := json.NewDecoder(r.Body).Decode(&updated); err != nil {
				t.Errorf("decode account update: %v", err)
			}
			accountPuts++
		case "/api/v1/admin/groups/24":
			groupPuts++
		default:
			t.Errorf("unexpected admin path: %s", r.URL.Path)
		}
		writeJSON(t, w, map[string]any{"code": 0})
	}))
	defer admin.Close()

	syncer := newTestSyncer(t, source, admin.URL, false, 2, upstream.URL, 0.85)
	syncer.config.SyncTarget = "account"
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.FixedZone("CST", 8*60*60))
	for i := 0; i < 3; i++ {
		if err := syncer.RunOnce(context.Background(), now.Add(time.Duration(i)*time.Minute)); err != nil {
			t.Fatal(err)
		}
	}

	if accountPuts != 1 || updated.RateMultiplier != 0.085 || groupPuts != 0 {
		t.Fatalf("account puts=%d updated=%+v group puts=%d", accountPuts, updated, groupPuts)
	}
}

func TestAccountTargetBootstrapsIdleUsageAccount(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/pricing":
			w.WriteHeader(http.StatusNotFound)
		case "/v1/usage":
			writeJSON(t, w, map[string]any{
				"usage": map[string]any{"today": upstreamToday{Cost: 10, ActualCost: 1}},
			})
		default:
			t.Errorf("unexpected upstream URL: %s", r.URL)
		}
	}))
	defer upstream.Close()

	channel := testChannel(upstream.URL, 0.08)
	channel.AccountRateMultiplier = 1.0
	source := &staticChannelSource{channels: []Channel{channel}}
	var updated accountUpdate
	accountPuts := 0
	admin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut || r.URL.Path != "/api/v1/admin/accounts/18" {
			t.Errorf("unexpected admin request: %s %s", r.Method, r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&updated); err != nil {
			t.Errorf("decode account update: %v", err)
		}
		accountPuts++
		writeJSON(t, w, map[string]any{"code": 0})
	}))
	defer admin.Close()

	syncer := newTestSyncer(t, source, admin.URL, false, 2, upstream.URL, 0.85)
	syncer.config.SyncTarget = "account"
	syncer.config.UsageBootstrap = true
	var output bytes.Buffer
	syncer.logger = log.New(&output, "", 0)
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.FixedZone("CST", 8*60*60))
	for i := 0; i < 2; i++ {
		if err := syncer.RunOnce(context.Background(), now.Add(time.Duration(i)*15*time.Minute)); err != nil {
			t.Fatal(err)
		}
	}

	if accountPuts != 1 || updated.RateMultiplier != 0.085 {
		t.Fatalf("account puts=%d updated=%+v", accountPuts, updated)
	}
	if !strings.Contains(output.String(), "累计用量估算") {
		t.Fatalf("missing cumulative bootstrap log:\n%s", output.String())
	}
}

func TestAccountTargetHonorsSyncHostAllowlist(t *testing.T) {
	var allowedCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/pricing" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if r.URL.Path != "/v1/usage" {
			t.Errorf("unexpected upstream URL: %s", r.URL)
		}
		allowedCalls.Add(1)
		writeJSON(t, w, map[string]any{"usage": map[string]any{"today": upstreamToday{Cost: 10, ActualCost: 1}}})
	}))
	defer upstream.Close()
	parsed, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}

	allowed := testChannel(upstream.URL, 0.1)
	disallowed := testChannel("https://other-upstream.invalid", 0.2)
	disallowed.AccountID = 19
	disallowed.AccountName = "other-test"
	disallowed.Group.ID = 25
	disallowed.Group.Name = "other-test"
	source := &staticChannelSource{channels: []Channel{allowed, disallowed}}
	syncer := newTestSyncer(t, source, "http://admin.invalid", false, 1, "", 1)
	syncer.config.SyncTarget = "account"
	syncer.config.SyncHosts = map[string]struct{}{parsed.Hostname(): {}}
	var output bytes.Buffer
	syncer.logger = log.New(&output, "", 0)

	if err := syncer.RunOnce(context.Background(), time.Now()); err != nil {
		t.Fatal(err)
	}
	if allowedCalls.Load() != 1 {
		t.Fatalf("allowed upstream calls = %d", allowedCalls.Load())
	}
	if !strings.Contains(output.String(), "白名单未包含上游主机") || !strings.Contains(output.String(), "已检查=1") {
		t.Fatalf("allowlist was not enforced:\n%s", output.String())
	}
}

func TestUsageTemplateLogsCurrentLocalRateWithoutNewUsage(t *testing.T) {
	now := time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, map[string]any{
			"usage": map[string]any{"today": upstreamToday{Cost: 10, ActualCost: 0.6}},
		})
	}))
	defer upstream.Close()

	channel := testChannel(upstream.URL, 0.051)
	syncer := newTestSyncer(t, &staticChannelSource{channels: []Channel{channel}}, "http://admin.invalid", false, 2, "", 1)
	syncer.state.Rules[channelStateKey(&channel)] = &RuleState{
		Identity:    channelIdentity(&channel),
		Template:    templateUsageRatio,
		Day:         now.Format("2006-01-02"),
		Cost:        10,
		ActualCost:  0.6,
		HasBaseline: true,
	}
	var output bytes.Buffer
	syncer.logger = log.New(&output, "", 0)

	if err := syncer.RunOnce(context.Background(), now); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "无新增用量，当前本地使用倍率 0.0510") {
		t.Fatalf("current local rate was not logged:\n%s", output.String())
	}
}

func TestChannelRenameKeepsStableState(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, map[string]any{"usage": map[string]any{"today": upstreamToday{Cost: 10, ActualCost: 1}}})
	}))
	defer upstream.Close()
	admin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, map[string]any{"code": 0})
	}))
	defer admin.Close()

	source := &staticChannelSource{channels: []Channel{testChannel(upstream.URL, 0.1)}}
	syncer := newTestSyncer(t, source, admin.URL, false, 2, "", 1)
	var output bytes.Buffer
	syncer.logger = log.New(&output, "", 0)
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	if err := syncer.RunOnce(context.Background(), now); err != nil {
		t.Fatal(err)
	}
	source.channels[0].AccountName = "新账号名"
	source.channels[0].Group.Name = "新分组名"
	if err := syncer.RunOnce(context.Background(), now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if len(syncer.state.Rules) != 1 {
		t.Fatalf("state count = %d", len(syncer.state.Rules))
	}
	if count := strings.Count(output.String(), "新渠道或上游凭据已变化"); count != 1 {
		t.Fatalf("baseline reset count = %d:\n%s", count, output.String())
	}
}

func TestSyncerAutomaticallyResolvesNewAPIPriceGroup(t *testing.T) {
	modelCalls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/usage":
			w.WriteHeader(http.StatusNotFound)
		case "/api/pricing":
			writeJSON(t, w, groupRatioResponse{
				Success:    true,
				GroupRatio: map[string]float64{"cc-max": 1, "kiro-特价": 0.2},
			})
		case "/api/log/token":
			writeJSON(t, w, newAPITokenLogResponse{
				Success: true,
				Data: []newAPITokenLog{
					{ID: 30, CreatedAt: 300, Type: 5, Group: "cc-max"},
					{ID: 10, CreatedAt: 100, Type: newAPIConsumeLogType, Group: "cc-max"},
					{ID: 20, CreatedAt: 200, Type: newAPIConsumeLogType, Group: "kiro-特价"},
				},
			})
		case "/v1/models":
			modelCalls++
			writeJSON(t, w, map[string]any{"data": []any{}})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer upstream.Close()

	updatedRate := 0.0
	admin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload groupUpdate
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		updatedRate = payload.RateMultiplier
		writeJSON(t, w, map[string]any{"code": 0})
	}))
	defer admin.Close()

	source := &staticChannelSource{channels: []Channel{testChannel(upstream.URL, 0.01)}}
	syncer := newTestSyncer(t, source, admin.URL, false, 1, "", 1)
	if err := syncer.RunOnce(context.Background(), time.Now()); err != nil {
		t.Fatal(err)
	}
	state := syncer.state.Rules["account:18/group:24"]
	if state.Template != templateNewAPIRatio || state.PriceKey != "kiro-特价" {
		t.Fatalf("unexpected NewAPI state: %+v", state)
	}
	if updatedRate != 0.2 {
		t.Fatalf("updated rate = %.4f", updatedRate)
	}
	if modelCalls != 0 {
		t.Fatalf("model list calls = %d", modelCalls)
	}
}

func TestNewAPIRefreshesBillingGroupEveryCycle(t *testing.T) {
	logCalls := 0
	pricingCalls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/usage":
			w.WriteHeader(http.StatusNotFound)
		case "/api/pricing":
			pricingCalls++
			writeJSON(t, w, groupRatioResponse{Success: true, GroupRatio: map[string]float64{"gptplus": 0.08}})
		case "/api/log/token":
			logCalls++
			writeJSON(t, w, newAPITokenLogResponse{
				Success: true,
				Data:    []newAPITokenLog{{ID: 1, CreatedAt: 100, Type: newAPIConsumeLogType, Group: "gptplus"}},
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer upstream.Close()

	source := &staticChannelSource{channels: []Channel{testChannel(upstream.URL, 0.05)}}
	syncer := newTestSyncer(t, source, "http://admin.invalid", true, 2, "", 1)
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 2; i++ {
		if err := syncer.RunOnce(context.Background(), now.Add(time.Duration(i)*5*time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	if logCalls != 2 || pricingCalls != 2 {
		t.Fatalf("log calls = %d, pricing calls = %d", logCalls, pricingCalls)
	}
	state := syncer.state.Rules["account:18/group:24"]
	if state.PriceKey != "gptplus" || state.CandidateCount != 2 {
		t.Fatalf("unexpected live state: %+v", state)
	}
}

func TestNewAPIFallsBackToBillingLogRatioWhenPricingRequiresLogin(t *testing.T) {
	for _, pricingStatus := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		t.Run(http.StatusText(pricingStatus), func(t *testing.T) {
			logCalls := 0
			pricingCalls := 0
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/v1/usage":
					w.WriteHeader(http.StatusNotFound)
				case "/api/pricing":
					pricingCalls++
					w.WriteHeader(pricingStatus)
				case "/api/log/token":
					logCalls++
					writeJSON(t, w, newAPITokenLogResponse{
						Success: true,
						Data: []newAPITokenLog{
							{ID: 2, CreatedAt: 200, Type: newAPIConsumeLogType, Group: "gptplus", Other: `{"group_ratio":0.1}`},
							{ID: 3, CreatedAt: 300, Type: 5, Group: "other", Other: `{"group_ratio":1}`},
						},
					})
				default:
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer upstream.Close()

			channel := testChannel(upstream.URL, 0.5)
			source := &staticChannelSource{channels: []Channel{channel}}
			updatedRate := 0.0
			putCount := 0
			admin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var payload groupUpdate
				if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
					t.Fatal(err)
				}
				updatedRate = payload.RateMultiplier
				source.channels[0].Group.RateMultiplier = payload.RateMultiplier
				putCount++
				writeJSON(t, w, map[string]any{"code": 0})
			}))
			defer admin.Close()

			syncer := newTestSyncer(t, source, admin.URL, false, 2, "", 1)
			now := time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC)
			for i := 0; i < 3; i++ {
				if err := syncer.RunOnce(context.Background(), now.Add(time.Duration(i)*5*time.Minute)); err != nil {
					t.Fatal(err)
				}
			}

			state := syncer.state.Rules[channelStateKey(&channel)]
			if logCalls != 3 || pricingCalls != 3 || putCount != 1 || updatedRate != 0.1 {
				t.Fatalf("log=%d pricing=%d puts=%d rate=%.4f", logCalls, pricingCalls, putCount, updatedRate)
			}
			if state.Template != templateNewAPIRatio || state.PriceKey != "gptplus" ||
				state.CandidateUpstreamRate != 0.1 || state.CandidateCount != 2 {
				t.Fatalf("unexpected fallback state: %+v", state)
			}
		})
	}
}

func TestNewAPIFallbackRefreshesBillingLogEveryCycle(t *testing.T) {
	logCalls := 0
	ratios := []float64{0.1, 0.035, 0.035}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/pricing":
			w.WriteHeader(http.StatusUnauthorized)
		case "/api/log/token":
			ratio := ratios[logCalls]
			logCalls++
			writeJSON(t, w, newAPITokenLogResponse{
				Success: true,
				Data: []newAPITokenLog{{
					ID: int64(logCalls), CreatedAt: int64(logCalls), Type: newAPIConsumeLogType,
					Group: "gptplus", Other: fmt.Sprintf(`{"group_ratio":%g}`, ratio),
				}},
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer upstream.Close()

	channel := testChannel(upstream.URL, 0.1)
	source := &staticChannelSource{channels: []Channel{channel}}
	updatedRate := 0.0
	putCount := 0
	admin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload groupUpdate
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		updatedRate = payload.RateMultiplier
		source.channels[0].Group.RateMultiplier = payload.RateMultiplier
		putCount++
		writeJSON(t, w, map[string]any{"code": 0})
	}))
	defer admin.Close()

	syncer := newTestSyncer(t, source, admin.URL, false, 2, "", 1)
	now := time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)
	syncer.state.Rules[channelStateKey(&channel)] = &RuleState{
		Identity:              channelIdentity(&channel),
		Template:              templateNewAPIRatio,
		PriceKey:              "gptplus",
		CandidateUpstreamRate: 0.1,
		CandidateCount:        2,
	}

	for i := 0; i < len(ratios); i++ {
		if err := syncer.RunOnce(context.Background(), now.Add(time.Duration(i)*5*time.Minute)); err != nil {
			t.Fatal(err)
		}
	}

	state := syncer.state.Rules[channelStateKey(&channel)]
	if logCalls != len(ratios) || putCount != 1 || updatedRate != 0.035 {
		t.Fatalf("log=%d puts=%d rate=%.4f", logCalls, putCount, updatedRate)
	}
	if state.CandidateUpstreamRate != 0.035 || state.CandidateCount != 2 {
		t.Fatalf("unexpected fallback state: %+v", state)
	}
}

func TestNewAPIFallbackReplacesStoredCandidateWithLiveRate(t *testing.T) {
	logCalls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/pricing":
			w.WriteHeader(http.StatusUnauthorized)
		case "/api/log/token":
			logCalls++
			writeJSON(t, w, newAPITokenLogResponse{
				Success: true,
				Data: []newAPITokenLog{{
					ID: 1, CreatedAt: 100, Type: newAPIConsumeLogType, Group: "gptplus", Other: `{"group_ratio":0.1}`,
				}},
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer upstream.Close()

	putCount := 0
	admin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		putCount++
		writeJSON(t, w, map[string]any{"code": 0})
	}))
	defer admin.Close()

	channel := testChannel(upstream.URL, 0.5)
	syncer := newTestSyncer(t, &staticChannelSource{channels: []Channel{channel}}, admin.URL, false, 2, "", 1)
	now := time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC)
	syncer.state.Rules[channelStateKey(&channel)] = &RuleState{
		Identity:              channelIdentity(&channel),
		Template:              templateNewAPIRatio,
		PriceKey:              "gptplus",
		CandidateUpstreamRate: 0.2,
		CandidateCount:        1,
	}

	if err := syncer.RunOnce(context.Background(), now); err != nil {
		t.Fatal(err)
	}
	state := syncer.state.Rules[channelStateKey(&channel)]
	if logCalls != 1 || putCount != 0 || state.CandidateUpstreamRate != 0.1 || state.CandidateCount != 1 {
		t.Fatalf("log=%d puts=%d state=%+v", logCalls, putCount, state)
	}
}

func TestNewAPILogRatioDoesNotUpdateWithoutValidGroupRatio(t *testing.T) {
	tests := []struct {
		name  string
		other string
	}{
		{name: "missing", other: `{}`},
		{name: "malformed", other: `{`},
		{name: "zero", other: `{"group_ratio":0}`},
		{name: "negative", other: `{"group_ratio":-0.1}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/v1/usage":
					w.WriteHeader(http.StatusNotFound)
				case "/api/pricing":
					w.WriteHeader(http.StatusUnauthorized)
				case "/api/log/token":
					writeJSON(t, w, newAPITokenLogResponse{
						Success: true,
						Data: []newAPITokenLog{{
							ID: 1, CreatedAt: 100, Type: newAPIConsumeLogType, Group: "gptplus", Other: test.other,
						}},
					})
				default:
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer upstream.Close()

			putCount := 0
			admin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				putCount++
				writeJSON(t, w, map[string]any{"code": 0})
			}))
			defer admin.Close()

			channel := testChannel(upstream.URL, 0.5)
			syncer := newTestSyncer(t, &staticChannelSource{channels: []Channel{channel}}, admin.URL, false, 1, "", 1)
			var output bytes.Buffer
			syncer.logger = log.New(&output, "", 0)
			if err := syncer.RunOnce(context.Background(), time.Now()); err != nil {
				t.Fatal(err)
			}

			state := syncer.state.Rules[channelStateKey(&channel)]
			if putCount != 0 || state.CandidateUpstreamRate != 0 || state.CandidateCount != 0 {
				t.Fatalf("unsafe rate was used, puts=%d state=%+v", putCount, state)
			}
			if !strings.Contains(output.String(), "检查正常=0 暂不自动=0 失败=1") {
				t.Fatalf("unexpected log:\n%s", output.String())
			}
		})
	}
}

func TestNewAPIPricingFailsWithoutLiveBillingLog(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/pricing":
			writeJSON(t, w, groupRatioResponse{Success: true, GroupRatio: map[string]float64{"gptplus": 0.08}})
		case "/api/log/token":
			w.WriteHeader(http.StatusTooManyRequests)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer upstream.Close()

	channel := testChannel(upstream.URL, 0.08)
	source := &staticChannelSource{channels: []Channel{channel}}
	syncer := newTestSyncer(t, source, "http://admin.invalid", false, 1, "", 1)
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	syncer.state.Rules[channelStateKey(&channel)] = &RuleState{
		Identity:              channelIdentity(&channel),
		Template:              templateNewAPIRatio,
		PriceKey:              "gptplus",
		CandidateUpstreamRate: 0.1,
		CandidateCount:        2,
	}
	var output bytes.Buffer
	syncer.logger = log.New(&output, "", 0)

	if err := syncer.RunOnce(context.Background(), now); err != nil {
		t.Fatal(err)
	}
	state := syncer.state.Rules[channelStateKey(&channel)]
	if state.PriceKey != "gptplus" || state.CandidateUpstreamRate != 0 || state.CandidateCount != 0 {
		t.Fatalf("stale evidence was preserved: %+v", state)
	}
	if !strings.Contains(output.String(), "实时计费日志接口限流") ||
		!strings.Contains(output.String(), "检查正常=0 暂不自动=0 失败=1") {
		t.Fatalf("missing live-check failure log:\n%s", output.String())
	}
}

func TestTemplateChangeDropsOldEvidence(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/usage":
			w.WriteHeader(http.StatusNotFound)
		case "/api/pricing":
			writeJSON(t, w, groupRatioResponse{Success: true, GroupRatio: map[string]float64{"gptplus": 0.08}})
		case "/api/log/token":
			writeJSON(t, w, newAPITokenLogResponse{
				Success: true,
				Data:    []newAPITokenLog{{ID: 1, CreatedAt: 100, Type: newAPIConsumeLogType, Group: "gptplus"}},
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer upstream.Close()

	putCount := 0
	admin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		putCount++
		writeJSON(t, w, map[string]any{"code": 0})
	}))
	defer admin.Close()

	channel := testChannel(upstream.URL, 0.5)
	syncer := newTestSyncer(t, &staticChannelSource{channels: []Channel{channel}}, admin.URL, false, 2, "", 1)
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	syncer.state.Rules[channelStateKey(&channel)] = &RuleState{
		Identity:              channelIdentity(&channel),
		Template:              templateUsageRatio,
		PriceKey:              "old-group",
		Day:                   "2026-07-29",
		Cost:                  10,
		ActualCost:            0.8,
		HasBaseline:           true,
		CandidateUpstreamRate: 0.08,
		CandidateCount:        2,
	}

	if err := syncer.RunOnce(context.Background(), now); err != nil {
		t.Fatal(err)
	}
	state := syncer.state.Rules[channelStateKey(&channel)]
	if putCount != 0 || state.Template != templateNewAPIRatio || state.PriceKey != "gptplus" ||
		state.HasBaseline || state.Day != "" || state.CandidateUpstreamRate != 0.08 || state.CandidateCount != 1 {
		t.Fatalf("old template evidence was reused, puts=%d state=%+v", putCount, state)
	}
}

func TestNewAPIGroupChangeDropsOldConfirmation(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/pricing":
			writeJSON(t, w, groupRatioResponse{
				Success:    true,
				GroupRatio: map[string]float64{"old-group": 0.08, "new-group": 0.08},
			})
		case "/api/log/token":
			writeJSON(t, w, newAPITokenLogResponse{
				Success: true,
				Data:    []newAPITokenLog{{ID: 2, CreatedAt: 200, Type: newAPIConsumeLogType, Group: "new-group"}},
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer upstream.Close()

	putCount := 0
	admin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		putCount++
		writeJSON(t, w, map[string]any{"code": 0})
	}))
	defer admin.Close()

	channel := testChannel(upstream.URL, 0.5)
	syncer := newTestSyncer(t, &staticChannelSource{channels: []Channel{channel}}, admin.URL, false, 2, "", 1)
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	syncer.state.Rules[channelStateKey(&channel)] = &RuleState{
		Identity:              channelIdentity(&channel),
		Template:              templateNewAPIRatio,
		PriceKey:              "old-group",
		CandidateUpstreamRate: 0.08,
		CandidateCount:        2,
	}

	if err := syncer.RunOnce(context.Background(), now); err != nil {
		t.Fatal(err)
	}
	state := syncer.state.Rules[channelStateKey(&channel)]
	if putCount != 0 || state.PriceKey != "new-group" || state.CandidateUpstreamRate != 0.08 || state.CandidateCount != 1 {
		t.Fatalf("old group confirmation was reused, puts=%d state=%+v", putCount, state)
	}
}

func TestNewAPIMissingStoredGroupDropsOldEvidence(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/pricing":
			writeJSON(t, w, groupRatioResponse{Success: true, GroupRatio: map[string]float64{"new-group": 0.12}})
		case "/api/log/token":
			w.WriteHeader(http.StatusTooManyRequests)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer upstream.Close()

	channel := testChannel(upstream.URL, 0.08)
	syncer := newTestSyncer(t, &staticChannelSource{channels: []Channel{channel}}, "http://admin.invalid", false, 2, "", 1)
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	syncer.state.Rules[channelStateKey(&channel)] = &RuleState{
		Identity:              channelIdentity(&channel),
		Template:              templateNewAPIRatio,
		PriceKey:              "old-group",
		CandidateUpstreamRate: 0.08,
		CandidateCount:        2,
	}

	if err := syncer.RunOnce(context.Background(), now); err != nil {
		t.Fatal(err)
	}
	state := syncer.state.Rules[channelStateKey(&channel)]
	if state.PriceKey != "" || state.CandidateUpstreamRate != 0 || state.CandidateCount != 0 {
		t.Fatalf("missing stored group evidence was preserved: %+v", state)
	}
}

func TestLatestNewAPIBillingGroupUsesNewestConsumeLog(t *testing.T) {
	billingLog, err := latestNewAPIBillingLog([]newAPITokenLog{
		{ID: 4, CreatedAt: 400, Type: 5, Group: "cc-max"},
		{ID: 3, CreatedAt: 300, Type: newAPIConsumeLogType, Group: "gptplus"},
		{ID: 1, CreatedAt: 100, Type: newAPIConsumeLogType, Group: "cc-max"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if billingLog.Group != "gptplus" {
		t.Fatalf("group = %q", billingLog.Group)
	}
}

func TestLatestNewAPIBillingGroupRejectsMissingConsumeLogs(t *testing.T) {
	if _, err := latestNewAPIBillingLog([]newAPITokenLog{
		{ID: 1, CreatedAt: 100, Type: 5, Group: "cc-max"},
	}); err == nil {
		t.Fatal("latestNewAPIBillingLog() error = nil")
	}
}

func TestLatestNewAPIBillingLogRejectsNewestConsumeLogWithoutGroup(t *testing.T) {
	if _, err := latestNewAPIBillingLog([]newAPITokenLog{
		{ID: 2, CreatedAt: 200, Type: newAPIConsumeLogType},
		{ID: 1, CreatedAt: 100, Type: newAPIConsumeLogType, Group: "old-group"},
	}); err == nil {
		t.Fatal("latestNewAPIBillingLog() error = nil")
	}
}

func TestNewAPIPricingDoesNotGuessWithoutBillingLog(t *testing.T) {
	modelCalls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/usage":
			w.WriteHeader(http.StatusNotFound)
		case "/api/pricing":
			writeJSON(t, w, groupRatioResponse{
				Success:    true,
				GroupRatio: map[string]float64{"cc-max": 1, "kiro-特价": 0.2},
			})
		case "/api/log/token":
			writeJSON(t, w, newAPITokenLogResponse{Success: true})
		case "/v1/models":
			modelCalls++
			writeJSON(t, w, map[string]any{"data": []map[string]string{{"id": "shared-model"}}})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer upstream.Close()

	putCount := 0
	admin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		putCount++
		writeJSON(t, w, map[string]any{"code": 0})
	}))
	defer admin.Close()

	source := &staticChannelSource{channels: []Channel{testChannel(upstream.URL, 0.2)}}
	syncer := newTestSyncer(t, source, admin.URL, false, 1, "", 1)
	var output bytes.Buffer
	syncer.logger = log.New(&output, "", 0)
	if err := syncer.RunOnce(context.Background(), time.Now()); err != nil {
		t.Fatal(err)
	}
	state := syncer.state.Rules["account:18/group:24"]
	if putCount != 0 || state.Template != templateNewAPIRatio || state.PriceKey != "" {
		t.Fatalf("unexpected result, puts=%d state=%+v", putCount, state)
	}
	if modelCalls != 0 || !strings.Contains(output.String(), "尚无已计费的真实请求日志") ||
		!strings.Contains(output.String(), "检查正常=0 暂不自动=0 失败=1") {
		t.Fatalf("unsafe fallback used, modelCalls=%d:\n%s", modelCalls, output.String())
	}
}

func TestUnavailableNewAPILogIsReportedWithoutChangingRate(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/usage" {
			writeJSON(t, w, map[string]any{"code": 500})
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer upstream.Close()
	putCount := 0
	admin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		putCount++
		writeJSON(t, w, map[string]any{"code": 0})
	}))
	defer admin.Close()

	source := &staticChannelSource{channels: []Channel{testChannel(upstream.URL, 0.5)}}
	syncer := newTestSyncer(t, source, admin.URL, false, 1, "", 1)
	var output bytes.Buffer
	syncer.logger = log.New(&output, "", 0)
	if err := syncer.RunOnce(context.Background(), time.Now()); err != nil {
		t.Fatal(err)
	}
	if putCount != 0 || !strings.Contains(output.String(), "检查正常=0 暂不自动=0 失败=1") {
		t.Fatalf("unexpected result, puts=%d:\n%s", putCount, output.String())
	}
}

func TestKnownTemplateNetworkFailureIsReported(t *testing.T) {
	status := http.StatusOK
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if status != http.StatusOK {
			w.WriteHeader(status)
			return
		}
		writeJSON(t, w, map[string]any{"usage": map[string]any{"today": upstreamToday{Cost: 10, ActualCost: 1}}})
	}))
	defer upstream.Close()
	source := &staticChannelSource{channels: []Channel{testChannel(upstream.URL, 0.1)}}
	syncer := newTestSyncer(t, source, "http://admin.invalid", false, 2, "", 1)
	var output bytes.Buffer
	syncer.logger = log.New(&output, "", 0)
	if err := syncer.RunOnce(context.Background(), time.Now()); err != nil {
		t.Fatal(err)
	}
	status = http.StatusInternalServerError
	if err := syncer.RunOnce(context.Background(), time.Now().Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "同步失败: 已识别模板 sub2api_usage 请求失败: 上游返回 HTTP 500") ||
		!strings.Contains(output.String(), "检查正常=0 暂不自动=0 失败=1") {
		t.Fatalf("unexpected log:\n%s", output.String())
	}
}

func TestInvalidUsageDropsOldEvidence(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, map[string]any{"usage": map[string]any{"today": upstreamToday{Cost: -1, ActualCost: 1}}})
	}))
	defer upstream.Close()

	syncer := &Syncer{client: http.DefaultClient, logger: log.New(io.Discard, "", 0)}
	channel := testChannel(upstream.URL, 0.1)
	state := &RuleState{
		Day:                   "2026-07-29",
		Cost:                  10,
		ActualCost:            1,
		HasBaseline:           true,
		CandidateUpstreamRate: 0.1,
		CandidateCount:        2,
	}

	matched, err := syncer.applyUsageTemplate(context.Background(), &channel, state, time.Now())
	if !matched || err == nil {
		t.Fatalf("matched=%t err=%v", matched, err)
	}
	if state.Day != "" || state.Cost != 0 || state.ActualCost != 0 || state.HasBaseline ||
		state.CandidateUpstreamRate != 0 || state.CandidateCount != 0 {
		t.Fatalf("invalid usage evidence was preserved: %+v", state)
	}
}

func TestDuplicateGroupBindingsAreSafelySkipped(t *testing.T) {
	first := testChannel("https://one.test", 0.1)
	second := testChannel("https://two.test", 0.1)
	second.AccountID = 19
	second.AccountName = "second"
	source := &staticChannelSource{channels: []Channel{first, second}}
	syncer := newTestSyncer(t, source, "http://admin.invalid", false, 1, "", 1)
	var output bytes.Buffer
	syncer.logger = log.New(&output, "", 0)
	if err := syncer.RunOnce(context.Background(), time.Now()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "分组同时绑定了 2 个可用账号") || !strings.Contains(output.String(), "已检查=0") {
		t.Fatalf("unexpected log:\n%s", output.String())
	}
}

func TestSingleAccountGroupInheritsAccountRate(t *testing.T) {
	channel := testChannel("https://lucen.cc", 0.5)
	channel.AccountRateMultiplier = 0.085
	source := &staticChannelSource{channels: []Channel{channel}}
	var updated groupUpdate
	putCount := 0
	admin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut || r.URL.Path != "/api/v1/admin/groups/24" {
			t.Errorf("unexpected admin request: %s %s", r.Method, r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&updated); err != nil {
			t.Errorf("decode group update: %v", err)
		}
		putCount++
		writeJSON(t, w, map[string]any{"code": 0})
	}))
	defer admin.Close()

	syncer := newTestSyncer(t, source, admin.URL, false, 2, "https://lucen.cc", 0.85)
	var output bytes.Buffer
	syncer.logger = log.New(&output, "", 0)
	if err := syncer.RunOnce(context.Background(), time.Now()); err != nil {
		t.Fatal(err)
	}
	if putCount != 1 || !almostEqual(updated.RateMultiplier, 0.085) {
		t.Fatalf("puts=%d updated=%+v", putCount, updated)
	}
	if !strings.Contains(output.String(), "已按单账号倍率更新分组") {
		t.Fatal("missing single-account inheritance log")
	}
}

func TestMultipleAccountGroupUsesHistoryCost(t *testing.T) {
	first := testChannel("https://one.test", 0.5)
	first.AccountRateMultiplier = 0.1
	second := first
	second.AccountID = 19
	second.AccountName = "second"
	second.AccountRateMultiplier = 0.2
	source := &usageChannelSource{
		staticChannelSource: &staticChannelSource{channels: []Channel{first, second}},
		usage:               []GroupUsageStats{{GroupID: 24, Requests: 30, StandardCost: 10, AccountCost: 1.5}},
	}
	var updated groupUpdate
	putCount := 0
	admin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&updated); err != nil {
			t.Errorf("decode group update: %v", err)
		}
		putCount++
		writeJSON(t, w, map[string]any{"code": 0})
	}))
	defer admin.Close()

	syncer := newTestSyncer(t, source, admin.URL, false, 2, "", 1)
	syncer.config.HistoryWindow = time.Hour
	syncer.config.MinHistoryCostUSD = 0.01
	if err := syncer.RunOnce(context.Background(), time.Now()); err != nil {
		t.Fatal(err)
	}
	if putCount != 1 || !almostEqual(updated.RateMultiplier, 0.15) {
		t.Fatalf("puts=%d updated=%+v", putCount, updated)
	}
}

func TestMultipleAccountGroupDoesNotInheritOneAccountRate(t *testing.T) {
	first := testChannel("https://one.test", 0.5)
	first.AccountRateMultiplier = 0.1
	second := first
	second.AccountID = 19
	second.AccountName = "second"
	second.AccountRateMultiplier = 0.2
	source := &staticChannelSource{channels: []Channel{first, second}}
	putCount := 0
	admin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		putCount++
		writeJSON(t, w, map[string]any{"code": 0})
	}))
	defer admin.Close()

	syncer := newTestSyncer(t, source, admin.URL, false, 2, "", 1)
	if err := syncer.RunOnce(context.Background(), time.Now()); err != nil {
		t.Fatal(err)
	}
	if putCount != 0 {
		t.Fatalf("multiple-account group was updated directly: puts=%d", putCount)
	}
}

func TestAccountBindingsAreCheckedOnce(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/pricing" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if r.URL.Path != "/v1/usage" {
			t.Errorf("unexpected upstream URL: %s", r.URL)
		}
		calls.Add(1)
		writeJSON(t, w, map[string]any{"usage": map[string]any{"today": upstreamToday{Cost: 1, ActualCost: 1}}})
	}))
	defer upstream.Close()

	first := testChannel(upstream.URL, 0.1)
	second := first
	second.Group.ID = 25
	second.Group.Name = "second"
	source := &staticChannelSource{channels: []Channel{first, second}}
	syncer := newTestSyncer(t, source, "http://admin.invalid", false, 1, "", 1)
	syncer.config.SyncTarget = "account"
	var output bytes.Buffer
	syncer.logger = log.New(&output, "", 0)

	if err := syncer.RunOnce(context.Background(), time.Now()); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 || !strings.Contains(output.String(), "账号模式仅检查一次") ||
		!strings.Contains(output.String(), "已检查=1") {
		t.Fatalf("calls=%d output=%s", calls.Load(), output.String())
	}
	if _, ok := syncer.state.Rules["account:18"]; !ok {
		t.Fatalf("account state was not stored: %+v", syncer.state.Rules)
	}
}

func TestRunOnceChecksDifferentGroupsConcurrently(t *testing.T) {
	var active atomic.Int32
	var maximum atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		current := active.Add(1)
		defer active.Add(-1)
		for {
			old := maximum.Load()
			if current <= old || maximum.CompareAndSwap(old, current) {
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
		writeJSON(t, w, map[string]any{"usage": map[string]any{"today": upstreamToday{Cost: 1, ActualCost: 1}}})
	}))
	defer upstream.Close()

	first := testChannel(upstream.URL, 1)
	second := testChannel(upstream.URL, 1)
	second.AccountID = 19
	second.Group.ID = 25
	second.AccountName = "second"
	second.Group.Name = "second"
	source := &staticChannelSource{channels: []Channel{first, second}}
	syncer := newTestSyncer(t, source, "http://admin.invalid", false, 1, "", 1)
	if err := syncer.RunOnce(context.Background(), time.Now()); err != nil {
		t.Fatal(err)
	}
	if maximum.Load() < 2 {
		t.Fatalf("maximum concurrent checks = %d", maximum.Load())
	}
}

func TestDiscoverFailureReturnsError(t *testing.T) {
	syncer := newTestSyncer(t, &staticChannelSource{err: fmt.Errorf("db down")}, "http://admin.invalid", false, 1, "", 1)
	if err := syncer.RunOnce(context.Background(), time.Now()); err == nil || !strings.Contains(err.Error(), "db down") {
		t.Fatalf("RunOnce() error = %v", err)
	}
}

func TestObserveDoesNotCountPastConfirmations(t *testing.T) {
	syncer := &Syncer{config: &Config{Confirmations: 2}, logger: log.New(io.Discard, "", 0)}
	state := &RuleState{
		Day:                   "2026-07-28",
		Cost:                  10,
		ActualCost:            1,
		HasBaseline:           true,
		CandidateUpstreamRate: 0.1,
		CandidateCount:        2,
	}
	syncer.observeUsage("test", state, upstreamToday{Cost: 20, ActualCost: 2}, 0.1, time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC))
	if state.CandidateCount != 2 {
		t.Fatalf("CandidateCount = %d", state.CandidateCount)
	}
}

func TestRateChangeSignificantUsesAbsoluteAndRelativeThresholds(t *testing.T) {
	if rateChangeSignificant(0.2, 0.201) {
		t.Fatal("small absolute and relative change should be ignored")
	}
	if !rateChangeSignificant(0.2, 0.204) {
		t.Fatal("relative change above 1% should be applied")
	}
	if !rateChangeSignificant(1.0, 1.006) {
		t.Fatal("absolute change above 0.005 should be applied")
	}
}

func TestObserveUsageDropsCandidateOnDayChange(t *testing.T) {
	syncer := &Syncer{config: &Config{Confirmations: 2}, logger: log.New(io.Discard, "", 0)}
	state := &RuleState{
		Day:                   "2026-07-28",
		Cost:                  10,
		ActualCost:            1,
		HasBaseline:           true,
		CandidateUpstreamRate: 0.1,
		CandidateCount:        2,
	}

	syncer.observeUsage("test", state, upstreamToday{Cost: 2, ActualCost: 0.2}, 0.1, time.Date(2026, 7, 29, 0, 1, 0, 0, time.UTC))
	if state.Day != "2026-07-29" || state.Cost != 2 || state.ActualCost != 0.2 || !state.HasBaseline ||
		state.CandidateUpstreamRate != 0 || state.CandidateCount != 0 {
		t.Fatalf("previous-day candidate was preserved: %+v", state)
	}
}

func newTestSyncer(t *testing.T, source ChannelSource, sub2APIURL string, dryRun bool, confirmations int, factorURL string, factor float64) *Syncer {
	t.Helper()
	factors := map[string]float64{}
	if factorURL != "" {
		parsed, err := url.Parse(factorURL)
		if err != nil {
			t.Fatal(err)
		}
		factors[parsed.Hostname()] = factor
	}
	config := &Config{
		Sub2APIURL:    sub2APIURL,
		AdminAPIKey:   "admin-test",
		Interval:      time.Minute,
		DryRun:        dryRun,
		Confirmations: confirmations,
		StateFile:     filepath.Join(t.TempDir(), "state.json"),
		Factors:       factors,
	}
	store := StateStore{Path: config.StateFile}
	return NewSyncer(config, source, http.DefaultClient, store, newState(), log.New(io.Discard, "", 0))
}

func testChannel(baseURL string, rate float64) Channel {
	return Channel{
		AccountID:   18,
		AccountName: "lucen-test",
		BaseURL:     baseURL,
		APIKey:      "sk-upstream",
		Group: sub2APIGroup{
			ID:             24,
			Name:           "lucen-test",
			RateMultiplier: rate,
		},
	}
}

func TestUpstreamClientUsesChannelProxyBeforeGlobalProxy(t *testing.T) {
	global := &Config{ProxyURL: "http://global.proxy:7897"}
	syncer := &Syncer{config: global, client: &http.Client{Timeout: time.Second}}
	channel := &Channel{ProxyURL: "http://channel.proxy:7897"}
	client, err := syncer.upstreamClient(channel)
	if err != nil {
		t.Fatal(err)
	}
	transport, ok := client.Transport.(*http.Transport)
	if !ok || transport.Proxy == nil {
		t.Fatal("expected proxy transport")
	}
	request, _ := http.NewRequest(http.MethodGet, "https://example.com", nil)
	proxyURL, err := transport.Proxy(request)
	if err != nil || proxyURL == nil || proxyURL.Host != "channel.proxy:7897" {
		t.Fatalf("proxy = %v, err = %v", proxyURL, err)
	}
}

func TestUpstreamProxyURLsUsesDatabaseProxyWhenNoGlobalProxyConfigured(t *testing.T) {
	syncer := &Syncer{config: &Config{}}
	channel := &Channel{ProxyURL: "http://database.proxy:7897"}
	proxies, err := syncer.upstreamProxyURLs(channel)
	if err != nil {
		t.Fatal(err)
	}
	if len(proxies) != 1 || proxies[0] != "http://database.proxy:7897" {
		t.Fatalf("proxies = %#v, want only the account-bound proxy", proxies)
	}

	channel.ProxyURL = ""
	proxies, err = syncer.upstreamProxyURLs(channel)
	if err != nil {
		t.Fatal(err)
	}
	if len(proxies) != 1 || proxies[0] != "" {
		t.Fatalf("unbound account proxies = %#v, want direct connection", proxies)
	}

	syncer.config.ProxyFallbackURLs = []string{"http://backup.proxy:7890"}
	proxies, err = syncer.upstreamProxyURLs(channel)
	if err != nil {
		t.Fatal(err)
	}
	if len(proxies) != 1 || proxies[0] != "http://backup.proxy:7890" {
		t.Fatalf("fallback-only proxies = %#v, want configured proxy without direct first", proxies)
	}
}

func TestFetchUpstreamRetriesNetworkFailureWithFallbackProxy(t *testing.T) {
	var firstCalls atomic.Int32
	firstProxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		firstCalls.Add(1)
		time.Sleep(200 * time.Millisecond)
	}))
	defer firstProxy.Close()

	var secondCalls atomic.Int32
	secondProxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		secondCalls.Add(1)
		writeJSON(t, w, map[string]any{"usage": map[string]any{"today": upstreamToday{Cost: 10, ActualCost: 1}}})
	}))
	defer secondProxy.Close()

	var output bytes.Buffer
	syncer := NewSyncer(
		&Config{ProxyURL: firstProxy.URL, ProxyFallbackURLs: []string{secondProxy.URL}},
		nil,
		&http.Client{Timeout: 25 * time.Millisecond},
		StateStore{},
		newState(),
		log.New(&output, "", 0),
	)
	channel := testChannel("http://upstream.invalid", 0.1)
	var payload upstreamResponse
	status, err := syncer.fetchUpstreamJSON(context.Background(), &channel, "/v1/usage", "days=1", &payload)
	if err != nil {
		t.Fatal(err)
	}
	if status != http.StatusOK || payload.Usage == nil || payload.Usage.Today == nil || payload.Usage.Today.ActualCost != 1 {
		t.Fatalf("status=%d payload=%+v", status, payload)
	}
	if firstCalls.Load() == 0 || secondCalls.Load() != 1 {
		t.Fatalf("proxy calls: first=%d second=%d", firstCalls.Load(), secondCalls.Load())
	}
	if !strings.Contains(output.String(), "切换代理") || !strings.Contains(output.String(), "备用代理") {
		t.Fatalf("missing fallback logs:\n%s", output.String())
	}
}

func TestFetchUpstreamDoesNotRetryHTTPError(t *testing.T) {
	var firstCalls atomic.Int32
	firstProxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		firstCalls.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer firstProxy.Close()

	var secondCalls atomic.Int32
	secondProxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		secondCalls.Add(1)
		writeJSON(t, w, map[string]any{"usage": map[string]any{"today": upstreamToday{Cost: 10, ActualCost: 1}}})
	}))
	defer secondProxy.Close()

	syncer := NewSyncer(
		&Config{ProxyURL: firstProxy.URL, ProxyFallbackURLs: []string{secondProxy.URL}},
		nil,
		&http.Client{Timeout: time.Second},
		StateStore{},
		newState(),
		log.New(io.Discard, "", 0),
	)
	channel := testChannel("http://upstream.invalid", 0.1)
	var payload upstreamResponse
	status, err := syncer.fetchUpstreamJSON(context.Background(), &channel, "/v1/usage", "days=1", &payload)
	if err != nil {
		t.Fatal(err)
	}
	if status != http.StatusUnauthorized || firstCalls.Load() != 1 || secondCalls.Load() != 0 {
		t.Fatalf("status=%d proxy calls: first=%d second=%d", status, firstCalls.Load(), secondCalls.Load())
	}
}

func writeJSON(t *testing.T, w http.ResponseWriter, value any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(value); err != nil {
		t.Errorf("encode response: %v", err)
	}
}
