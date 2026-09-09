package stats

import (
	"math"
	"strings"
	"testing"
	"time"

	"github.com/DaisWW/Sub2ApiExt/monitoring/internal/model"
)

func TestSummarize(t *testing.T) {
	got := Summarize([]int{9, 1, 5, 3})
	if *got.FastestMs != 1 || *got.MedianMs != 4 || math.Abs(*got.P95Ms-8.4) > 0.001 {
		t.Fatalf("unexpected stats: %+v", got)
	}
	odd := Summarize([]int{7, 2, 4})
	if *odd.MedianMs != 4 {
		t.Fatalf("unexpected odd median: %+v", odd)
	}
	if got := Summarize(nil); got.FastestMs != nil || got.MedianMs != nil || got.P95Ms != nil {
		t.Fatalf("empty stats should be null: %+v", got)
	}
}

func TestAggregateGroup(t *testing.T) {
	now := time.Unix(100, 0).UTC()
	group := model.Group{ID: 3, Name: "primary"}
	results := []model.ProbeResult{
		{Status: model.StatusOperational, LatencyMs: intPtr(10), FirstByteMs: intPtr(4)},
		{Status: model.StatusFailed, LatencyMs: intPtr(90)},
	}
	got := AggregateGroup("group:3", group, results, now)
	if got.Status != model.StatusOperational || got.LatencyMs == nil || *got.LatencyMs != 10 || got.CheckedAt != now {
		t.Fatalf("unexpected group result: %+v", got)
	}
}

func TestAggregateGroupUsesHealthyLatencyForMixedRoute(t *testing.T) {
	now := time.Unix(100, 0).UTC()
	group := model.Group{ID: 3, Name: "primary"}
	results := []model.ProbeResult{
		{Status: model.StatusOperational, LatencyMs: intPtr(1200), FirstByteMs: intPtr(400)},
		{Status: model.StatusFailed, LatencyMs: intPtr(30000)},
	}
	got := AggregateGroup("group:3", group, results, now)
	if got.Status != model.StatusOperational {
		t.Fatalf("mixed group status = %q, want operational while one account is usable", got.Status)
	}
	if got.LatencyMs == nil || *got.LatencyMs != 1200 {
		t.Fatalf("mixed group latency = %+v, want healthy path latency", got.LatencyMs)
	}
	if got.FirstByteMs == nil || *got.FirstByteMs != 400 {
		t.Fatalf("mixed group first-byte latency = %+v, want healthy path latency", got.FirstByteMs)
	}
}

func TestAggregateFailedGroupKeepsMeasuredLatency(t *testing.T) {
	now := time.Unix(100, 0).UTC()
	group := model.Group{ID: 3, Name: "primary"}
	results := []model.ProbeResult{
		{Status: model.StatusFailed, LatencyMs: intPtr(120)},
		{Status: model.StatusError, LatencyMs: intPtr(300)},
	}
	got := AggregateGroup("group:3", group, results, now)
	if got.Status != model.StatusFailed || got.LatencyMs == nil || *got.LatencyMs != 210 {
		t.Fatalf("unexpected failed group result: %+v", got)
	}
}

func TestAggregateGroupUsesAnyOperationalAccount(t *testing.T) {
	now := time.Unix(100, 0).UTC()
	group := model.Group{
		ID: 4,
		Members: []model.GroupMember{
			{AccountID: 1, GroupPriority: 1, AccountPriority: 1},
			{AccountID: 2, GroupPriority: 2, AccountPriority: 1},
		},
	}
	got := AggregateGroup("group:4", group, []model.ProbeResult{
		{EntityID: 1, Status: model.StatusOperational},
		{EntityID: 2, Status: model.StatusFailed},
	}, now)
	if got.Status != model.StatusOperational {
		t.Fatalf("lower-priority failure should not make the primary route unavailable: %+v", got)
	}
	if !strings.Contains(got.Message, "异常 1") {
		t.Fatalf("group message should retain the member risk: %q", got.Message)
	}
}

func TestAggregateGroupKeepsHealthyFallbackOperationalWithRateLimitedPeer(t *testing.T) {
	now := time.Unix(100, 0).UTC()
	group := model.Group{ID: 40, Members: []model.GroupMember{
		{AccountID: 1}, {AccountID: 2},
	}}
	got := AggregateGroup("group:40", group, []model.ProbeResult{
		{EntityID: 1, Status: model.StatusDegraded, HealthReason: model.HealthReasonRateLimited},
		{EntityID: 2, Status: model.StatusOperational},
	}, now)
	if got.Status != model.StatusOperational || got.HealthReason != model.HealthReasonRateLimited {
		t.Fatalf("healthy fallback with throttled peer = %+v", got)
	}
	if !strings.Contains(got.Message, "阶段性限速") {
		t.Fatalf("group message omitted rate-limit warning: %q", got.Message)
	}
}

func TestAggregateGroupMarksOnlyRateLimitedRouteDegraded(t *testing.T) {
	now := time.Unix(100, 0).UTC()
	group := model.Group{ID: 41, AccountIDs: []int64{1}}
	got := AggregateGroup("group:41", group, []model.ProbeResult{
		{EntityID: 1, Status: model.StatusOperational, HealthReason: model.HealthReasonRateLimited},
	}, now)
	if got.Status != model.StatusDegraded || got.HealthReason != model.HealthReasonRateLimited {
		t.Fatalf("rate-limited route = %+v, want degraded rate_limited", got)
	}
}

func TestAggregateGroupMarksHealthyFallbackOperational(t *testing.T) {
	now := time.Unix(100, 0).UTC()
	group := model.Group{
		ID: 5,
		Members: []model.GroupMember{
			{AccountID: 1, GroupPriority: 1, AccountPriority: 1},
			{AccountID: 2, GroupPriority: 2, AccountPriority: 1},
		},
	}
	got := AggregateGroup("group:5", group, []model.ProbeResult{
		{EntityID: 1, Status: model.StatusFailed},
		{EntityID: 2, Status: model.StatusOperational},
	}, now)
	if got.Status != model.StatusOperational {
		t.Fatalf("failed primary with healthy fallback should be operational: %+v", got)
	}
}

func TestAggregateGroupIgnoresPriorityForHealthStatus(t *testing.T) {
	now := time.Unix(100, 0).UTC()
	group := model.Group{
		ID: 6,
		Members: []model.GroupMember{
			{AccountID: 1, GroupPriority: 1, AccountPriority: 1, RequestCount: 1000},
			{AccountID: 2, GroupPriority: 1, AccountPriority: 1, RequestCount: 1},
		},
	}
	got := AggregateGroup("group:6", group, []model.ProbeResult{
		{EntityID: 1, Status: model.StatusOperational},
		{EntityID: 2, Status: model.StatusFailed},
	}, now)
	if got.Status != model.StatusOperational {
		t.Fatalf("a nearly unused failed peer should not outweigh the observed route: %+v", got)
	}
}

func TestAggregateGroupUsesHealthyFallbackWithUnknownPeer(t *testing.T) {
	now := time.Unix(100, 0).UTC()
	group := model.Group{
		ID: 7,
		Members: []model.GroupMember{
			{AccountID: 1, GroupPriority: 1, AccountPriority: 1},
			{AccountID: 2, GroupPriority: 2, AccountPriority: 1},
		},
	}
	got := AggregateGroup("group:7", group, []model.ProbeResult{
		{EntityID: 1, Status: model.StatusUnknown},
		{EntityID: 2, Status: model.StatusOperational},
	}, now)
	if got.Status != model.StatusOperational {
		t.Fatalf("unknown primary with a healthy fallback should be operational: %+v", got)
	}
}

func TestAggregateGroupDefaultsToOperationalWhenCandidatesAreUnobserved(t *testing.T) {
	now := time.Unix(100, 0).UTC()
	group := model.Group{ID: 70, Members: []model.GroupMember{
		{AccountID: 1}, {AccountID: 2}, {AccountID: 3},
	}}
	got := AggregateGroup("group:70", group, []model.ProbeResult{
		{EntityID: 1, Status: model.StatusFailed},
		{EntityID: 2, Status: model.StatusUnknown},
	}, now)
	if got.Status != model.StatusOperational {
		t.Fatalf("unobserved candidate should keep group usable: %+v", got)
	}
	if !strings.Contains(got.Message, "待验证") {
		t.Fatalf("group message should disclose insufficient evidence: %q", got.Message)
	}
}

func TestAggregateGroupDisclosesUnobservedAccountIDsWithoutMembers(t *testing.T) {
	now := time.Unix(100, 0).UTC()
	group := model.Group{ID: 71, AccountIDs: []int64{1, 2}}
	got := AggregateGroup("group:71", group, []model.ProbeResult{
		{EntityID: 1, Status: model.StatusFailed},
	}, now)
	if got.Status != model.StatusOperational {
		t.Fatalf("unobserved account ID should keep group usable: %+v", got)
	}
	if !strings.Contains(got.Message, "数据不足") || !strings.Contains(got.Message, "待验证") {
		t.Fatalf("group message should disclose insufficient evidence: %q", got.Message)
	}
}

func TestAggregateGroupAllKnownFailuresIsFailed(t *testing.T) {
	now := time.Unix(100, 0).UTC()
	group := model.Group{ID: 8, Members: []model.GroupMember{
		{AccountID: 1, GroupPriority: 1, AccountPriority: 1},
		{AccountID: 2, GroupPriority: 2, AccountPriority: 1},
	}}
	got := AggregateGroup("group:8", group, []model.ProbeResult{
		{EntityID: 1, Status: model.StatusFailed},
		{EntityID: 2, Status: model.StatusError},
	}, now)
	if got.Status != model.StatusFailed {
		t.Fatalf("all known candidates failed should be failed: %+v", got)
	}
}

func TestAggregateGroupUsesDegradedWhenOnlySlowAccountWorks(t *testing.T) {
	now := time.Unix(100, 0).UTC()
	group := model.Group{ID: 9, AccountIDs: []int64{1, 2}}
	got := AggregateGroup("group:9", group, []model.ProbeResult{
		{EntityID: 1, Status: model.StatusDegraded, LatencyMs: intPtr(25_000)},
		{EntityID: 2, Status: model.StatusFailed},
	}, now)
	if got.Status != model.StatusDegraded || got.LatencyMs == nil || *got.LatencyMs != 25_000 {
		t.Fatalf("only slow usable account should make group degraded: %+v", got)
	}
}

func TestWindowFromOutcomesMarksIntermittentRateLimitAsDegraded(t *testing.T) {
	got := WindowFromOutcomes(8, 2, 0, []int{900, 1100, 1200, 1300, 1000, 1050, 1150, 1250}, DefaultHealthPolicy)
	if got.Status != model.StatusDegraded || !got.Available || got.Reason != model.HealthReasonRateLimited {
		t.Fatalf("rate-limited window = %+v, want available degraded rate_limited", got)
	}
	if got.SuccessRate != 100 || got.RateLimitRate != 20 || got.HardFailureRate != 0 || got.Attempts != 10 {
		t.Fatalf("rate-limited ratios = %+v", got)
	}
	if got.Latency.P95Ms == nil || *got.Latency.P95Ms <= 0 {
		t.Fatalf("successful latency metrics missing: %+v", got.Latency)
	}
}

func TestWindowFromOutcomesKeepsSparseRateLimitOperationalWithLowConfidence(t *testing.T) {
	got := WindowFromOutcomes(4, 1, 0, []int{900, 1100, 1200, 1300}, HealthPolicy{
		MinimumSamples:        10,
		RateLimitDegradePct:   10,
		HardFailureDegradePct: 10,
		SlowLatencyMs:         20_000,
	})
	if got.Status != model.StatusOperational || !got.Available {
		t.Fatalf("sparse window = %+v, want available operational", got)
	}
	if got.Confidence != model.HealthConfidenceLow || got.RateLimitRate != 20 {
		t.Fatalf("sparse window confidence/ratio = %+v", got)
	}
}

func TestWindowFromOutcomesTreatsEmptyWindowAsAvailable(t *testing.T) {
	got := WindowFromOutcomes(0, 0, 0, nil, DefaultHealthPolicy)
	if got.Status != model.StatusOperational || !got.Available {
		t.Fatalf("empty window = %+v, want available operational", got)
	}
	if got.Confidence != model.HealthConfidenceLow || got.Reason != model.HealthReasonNoEvidence {
		t.Fatalf("empty window confidence/reason = %+v", got)
	}
}

func TestWindowFromOutcomesTreatsSparseAllFailuresAsAvailable(t *testing.T) {
	got := WindowFromOutcomes(0, 0, 6, nil, DefaultHealthPolicy)
	if got.Status != model.StatusOperational || !got.Available {
		t.Fatalf("sparse failures = %+v, want available operational", got)
	}
	if got.Confidence != model.HealthConfidenceLow || got.Reason != model.HealthReasonUpstreamError {
		t.Fatalf("sparse failures confidence/reason = %+v", got)
	}
}

func TestWindowFromOutcomesMarksAllRateLimitedAsFailed(t *testing.T) {
	got := WindowFromOutcomes(0, 12, 0, nil, DefaultHealthPolicy)
	if got.Status != model.StatusFailed || got.Available || got.Reason != model.HealthReasonRateLimited {
		t.Fatalf("all rate-limited window = %+v, want unavailable failed rate_limited", got)
	}
}

func TestWindowFromOutcomesPrefersHardFailureReason(t *testing.T) {
	got := WindowFromOutcomes(8, 2, 2, []int{100, 200}, DefaultHealthPolicy)
	if got.Status != model.StatusDegraded || got.Reason != model.HealthReasonUpstreamError {
		t.Fatalf("mixed failure window = %+v, want upstream_error degradation", got)
	}
}

func intPtr(value int) *int { return &value }
