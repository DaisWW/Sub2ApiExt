package main

import (
	"context"
	"fmt"
	"math"
	"strings"
	"time"
)

const (
	adaptiveMinRequests       int64   = 30
	adaptiveMinStandardCostUSD        = 5.0
	adaptiveMaxHistoryWindow           = 24 * time.Hour
)

var adaptiveHistoryWindowOrder = []time.Duration{
	time.Hour,
	6 * time.Hour,
	12 * time.Hour,
	24 * time.Hour,
}

type adaptiveUsageChoice struct {
	Stats  GroupUsageStats
	Window time.Duration
}

func adaptiveHistoryWindows(maxWindow time.Duration) []time.Duration {
	if maxWindow <= 0 || maxWindow > adaptiveMaxHistoryWindow {
		maxWindow = adaptiveMaxHistoryWindow
	}
	windows := make([]time.Duration, 0, len(adaptiveHistoryWindowOrder))
	for _, window := range adaptiveHistoryWindowOrder {
		if window <= maxWindow {
			windows = append(windows, window)
		}
	}
	if len(windows) == 0 {
		// Keep the configured lower bound useful in tests or custom deployments.
		windows = append(windows, maxWindow)
	}
	return windows
}

func adaptiveSampleSufficient(stats GroupUsageStats) bool {
	return stats.Requests >= adaptiveMinRequests &&
		stats.StandardCost >= adaptiveMinStandardCostUSD &&
		stats.AccountCost > 0 &&
		stats.StandardCost > 0 &&
		finiteNonNegative(stats.StandardCost) &&
		finiteNonNegative(stats.AccountCost)
}

func finiteNonNegative(value float64) bool {
	return value >= 0 && !math.IsNaN(value) && !math.IsInf(value, 0)
}

func chooseAdaptiveUsage(groupID int64, byWindow map[time.Duration]map[int64]GroupUsageStats, windows []time.Duration) (adaptiveUsageChoice, string, bool) {
	var latest *GroupUsageStats
	var latestWindow time.Duration
	for _, window := range windows {
		stats, exists := byWindow[window][groupID]
		if exists {
			copy := stats
			latest = &copy
			latestWindow = window
			if adaptiveSampleSufficient(stats) {
				return adaptiveUsageChoice{Stats: stats, Window: window}, "", true
			}
		}
	}

	if latest == nil {
		return adaptiveUsageChoice{}, fmt.Sprintf("样本不足：%s均无用量", formatWindowChain(windows)), false
	}
	reasons := make([]string, 0, 2)
	if latest.Requests < adaptiveMinRequests {
		reasons = append(reasons, fmt.Sprintf("请求=%d（需≥%d）", latest.Requests, adaptiveMinRequests))
	}
	if !finiteNonNegative(latest.StandardCost) || latest.StandardCost < adaptiveMinStandardCostUSD {
		reasons = append(reasons, fmt.Sprintf("标准=%.4f（需≥%.2f）", latest.StandardCost, adaptiveMinStandardCostUSD))
	}
	if !finiteNonNegative(latest.AccountCost) || latest.AccountCost <= 0 {
		reasons = append(reasons, "账号成本无效")
	}
	if len(reasons) == 0 {
		reasons = append(reasons, "数据未达到稳定样本条件")
	}
	return adaptiveUsageChoice{}, fmt.Sprintf("样本不足：%s窗口 %s；%s", formatHistoryWindow(latestWindow), formatWindowChain(windows), strings.Join(reasons, "，")), false
}

func formatHistoryWindow(window time.Duration) string {
	switch window {
	case time.Hour:
		return "1h"
	case 6 * time.Hour:
		return "6h"
	case 12 * time.Hour:
		return "12h"
	case 24 * time.Hour:
		return "24h"
	default:
		return window.String()
	}
}

func formatWindowChain(windows []time.Duration) string {
	parts := make([]string, 0, len(windows))
	for _, window := range windows {
		parts = append(parts, formatHistoryWindow(window))
	}
	return strings.Join(parts, "/")
}

func (s *Syncer) loadGroupUsageByWindows(ctx context.Context, end time.Time, windows []time.Duration) (map[time.Duration]map[int64]GroupUsageStats, error) {
	if s == nil || s.source == nil {
		return nil, nil
	}
	result := make(map[time.Duration]map[int64]GroupUsageStats, len(windows))
	for _, window := range windows {
		result[window] = make(map[int64]GroupUsageStats)
	}
	if source, ok := s.source.(groupUsageWindowSource); ok {
		rows, err := source.ListGroupUsageStatsByWindows(ctx, end, windows)
		if err != nil {
			return nil, err
		}
		for _, row := range rows {
			if _, exists := result[row.Window]; !exists {
				result[row.Window] = make(map[int64]GroupUsageStats)
			}
			result[row.Window][row.GroupID] = row.GroupUsageStats
		}
		return result, nil
	}
	source, ok := s.source.(groupUsageSource)
	if !ok {
		return result, nil
	}
	for _, window := range windows {
		rows, err := source.ListGroupUsageStats(ctx, end.Add(-window), end)
		if err != nil {
			return nil, err
		}
		statsByGroup := make(map[int64]GroupUsageStats, len(rows))
		for _, row := range rows {
			statsByGroup[row.GroupID] = row
		}
		result[window] = statsByGroup
	}
	return result, nil
}
