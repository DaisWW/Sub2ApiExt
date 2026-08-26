package main

import (
	"testing"
	"time"
)

func TestChooseAdaptiveUsagePrefersShortestSufficientWindow(t *testing.T) {
	byWindow := map[time.Duration]map[int64]GroupUsageStats{
		time.Hour: {
			24: {GroupID: 24, Requests: 29, StandardCost: 20, AccountCost: 2},
		},
		6 * time.Hour: {
			24: {GroupID: 24, Requests: 40, StandardCost: 8, AccountCost: 1.2},
		},
		12 * time.Hour: {
			24: {GroupID: 24, Requests: 100, StandardCost: 30, AccountCost: 4},
		},
		24 * time.Hour: {
			24: {GroupID: 24, Requests: 200, StandardCost: 60, AccountCost: 8},
		},
	}
	choice, reason, ok := chooseAdaptiveUsage(24, byWindow, adaptiveHistoryWindowOrder)
	if !ok || reason != "" {
		t.Fatalf("choice failed: %+v reason=%q ok=%v", choice, reason, ok)
	}
	if choice.Window != 6*time.Hour || choice.Stats.Requests != 40 {
		t.Fatalf("choice=%+v, want 6h sample", choice)
	}
}

func TestChooseAdaptiveUsageFallsBackThroughAllWindows(t *testing.T) {
	byWindow := map[time.Duration]map[int64]GroupUsageStats{
		time.Hour: {
			24: {GroupID: 24, Requests: 2, StandardCost: 1, AccountCost: .1},
		},
		6 * time.Hour: {
			24: {GroupID: 24, Requests: 10, StandardCost: 2, AccountCost: .2},
		},
		12 * time.Hour: {
			24: {GroupID: 24, Requests: 20, StandardCost: 4, AccountCost: .4},
		},
		24 * time.Hour: {
			24: {GroupID: 24, Requests: 29, StandardCost: 4.9, AccountCost: .49},
		},
	}
	_, reason, ok := chooseAdaptiveUsage(24, byWindow, adaptiveHistoryWindowOrder)
	if ok || reason == "" || len(reason) < 4 {
		t.Fatalf("want insufficient-sample reason, got reason=%q ok=%v", reason, ok)
	}
	if got := formatWindowChain(adaptiveHistoryWindowOrder); got != "1h/6h/12h/24h" {
		t.Fatalf("window chain=%q", got)
	}
}

func TestAdaptiveHistoryWindowsHonorsConfiguredMaximum(t *testing.T) {
	windows := adaptiveHistoryWindows(12 * time.Hour)
	if len(windows) != 3 || windows[0] != time.Hour || windows[2] != 12*time.Hour {
		t.Fatalf("windows=%v", windows)
	}
}
