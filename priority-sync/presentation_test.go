package main

import (
	"strings"
	"testing"
	"time"
)

func TestAccountLabelUsesNameAndStableID(t *testing.T) {
	if got := accountLabel("  demo\nchannel  ", 7); got != "demo channel #7" {
		t.Fatalf("accountLabel = %q", got)
	}
	if got := accountLabel("\t\r\n", 7); got != "#7" {
		t.Fatalf("empty accountLabel = %q", got)
	}
}

func TestTableAccountLabelTruncatesNameAndPreservesID(t *testing.T) {
	got := tableAccountLabel("这是一个很长的账户名称", 42, 16)
	if len([]rune(got)) > 16 {
		t.Fatalf("label has %d runes, want <= 16: %q", len([]rune(got)), got)
	}
	if !strings.HasSuffix(got, " #42") {
		t.Fatalf("label lost stable ID: %q", got)
	}
}

func TestRecommendationTableIncludesAccountLabel(t *testing.T) {
	var output strings.Builder
	err := writeRecommendationTable(&output, "2026-09-09T00:00:00Z", []Recommendation{{
		ID:                  7,
		Name:                "demo",
		RecommendedPriority: 10,
		CurrentPriority:     50,
		SuccessfulRequests:  5,
		Availability:        1,
		Score:               92.5,
		ApplyStatus:         "updated",
	}})
	if err != nil {
		t.Fatal(err)
	}
	for _, marker := range []string{"账号", "优先级", "demo #7", "已更新", "共 1 个"} {
		if !strings.Contains(output.String(), marker) {
			t.Fatalf("table missing %q: %s", marker, output.String())
		}
	}
}

func TestTableActionLabelAndLatency(t *testing.T) {
	if got := tableActionLabel("deferred-exploration"); got != "等待探索" {
		t.Fatalf("action label = %q", got)
	}
	if got := formatTableLatency(26298, false); got != "26.3s" {
		t.Fatalf("latency seconds = %q", got)
	}
	if got := formatTableLatency(860, true); got != "860ms*" {
		t.Fatalf("fallback latency = %q", got)
	}
}

func TestSelectExplorationCandidateRotatesAndSkipsExcluded(t *testing.T) {
	now := nowForTest()
	cooling := now.Add(time.Minute)
	accounts := []AccountMetrics{
		{ID: 1, Status: "active", CurrentPriority: 90},
		{ID: 2, Status: "active", CurrentPriority: 90},
		{ID: 3, Status: "active", CurrentPriority: 90, RateLimitResetAt: &cooling},
	}
	got, ok := selectExplorationCandidate(accounts, nil, 1, now, 5)
	if !ok || got.ID != 2 {
		t.Fatalf("candidate after cursor = %+v, ok=%v", got, ok)
	}
	got, ok = selectExplorationCandidate(accounts, nil, 2, now, 5)
	if !ok || got.ID != 1 {
		t.Fatalf("wrapped candidate = %+v, ok=%v", got, ok)
	}
}

func nowForTest() time.Time { return time.Date(2026, 9, 9, 0, 0, 0, 0, time.UTC) }
