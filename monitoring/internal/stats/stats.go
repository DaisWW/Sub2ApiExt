package stats

import (
	"sort"
	"time"

	"github.com/DaisWW/Sub2ApiExt/monitoring/internal/model"
)

// Summarize converts raw successful samples into the compact metrics shown by
// the dashboard. The median is interpolated for even-sized samples, which is
// stable and matches PostgreSQL percentile_cont semantics.
func Summarize(samples []int) model.MetricStats {
	if len(samples) == 0 {
		return model.MetricStats{}
	}
	values := append([]int(nil), samples...)
	sort.Ints(values)
	median := float64(values[len(values)/2])
	if len(values)%2 == 0 {
		median = float64(values[len(values)/2-1]+values[len(values)/2]) / 2
	}
	fastest, slowest := values[0], values[len(values)-1]
	return model.MetricStats{FastestMs: &fastest, MedianMs: &median, SlowestMs: &slowest}
}

func StatusFromResults(results []model.ProbeResult) string {
	if len(results) == 0 {
		return model.StatusUnknown
	}
	ok := 0
	for _, result := range results {
		if result.Status == model.StatusOperational || result.Status == model.StatusDegraded {
			ok++
		}
	}
	if ok == len(results) {
		return model.StatusOperational
	}
	if ok > 0 {
		return model.StatusDegraded
	}
	return model.StatusFailed
}

func AggregateGroup(key string, group model.Group, results []model.ProbeResult, now time.Time) model.ProbeResult {
	if len(results) == 0 {
		return model.ProbeResult{TargetKey: key, Kind: model.KindGroup, EntityID: group.ID, Status: model.StatusUnknown}
	}
	var latency, firstByte []int
	healthy := 0
	for _, result := range results {
		isHealthy := result.Status == model.StatusOperational || result.Status == model.StatusDegraded
		if isHealthy {
			healthy++
		}
		if result.LatencyMs != nil && isHealthy {
			latency = append(latency, *result.LatencyMs)
		}
		if result.FirstByteMs != nil && isHealthy {
			firstByte = append(firstByte, *result.FirstByteMs)
		}
	}
	status := StatusFromResults(results)
	result := model.ProbeResult{
		TargetKey: key,
		Kind:      model.KindGroup,
		EntityID:  group.ID,
		Status:    status,
		CheckedAt: now,
		Message:   groupMessage(len(results), healthy),
		Source:    "aggregate",
	}
	if len(latency) > 0 {
		value := int(medianValue(Summarize(latency)))
		result.LatencyMs = &value
	}
	if len(firstByte) > 0 {
		value := int(medianValue(Summarize(firstByte)))
		result.FirstByteMs = &value
	}
	return result
}

func groupMessage(total, healthy int) string {
	if total == healthy {
		return "all accounts healthy"
	}
	return formatCount(healthy) + "/" + formatCount(total) + " accounts healthy"
}

func formatCount(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [12]byte
	pos := len(buf)
	for n > 0 {
		pos--
		buf[pos] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[pos:])
}

func medianValue(m model.MetricStats) float64 {
	if m.MedianMs == nil {
		return 0
	}
	return *m.MedianMs
}
