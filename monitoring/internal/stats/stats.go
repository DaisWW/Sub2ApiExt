package stats

import (
	"math"
	"sort"
	"time"

	"github.com/DaisWW/Sub2ApiExt/monitoring/internal/model"
)

// Summarize 将原始样本转换为面板使用的紧凑指标，分位数语义与
// PostgreSQL percentile_cont 保持一致。
func Summarize(samples []int) model.MetricStats {
	if len(samples) == 0 {
		return model.MetricStats{}
	}
	values := append([]int(nil), samples...)
	sort.Ints(values)
	fastest := values[0]
	median := percentile(values, 0.5)
	p95 := percentile(values, 0.95)
	return model.MetricStats{FastestMs: &fastest, MedianMs: &median, P95Ms: &p95}
}

func percentile(values []int, fraction float64) float64 {
	position := float64(len(values)-1) * fraction
	lower := int(math.Floor(position))
	upper := int(math.Ceil(position))
	if lower == upper {
		return float64(values[lower])
	}
	weight := position - float64(lower)
	return float64(values[lower])*(1-weight) + float64(values[upper])*weight
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
		// 失败样本的实测延迟也参与分组统计，确保全失败分组仍能展示超时或响应耗时。
		if result.LatencyMs != nil {
			latency = append(latency, *result.LatencyMs)
		}
		if result.FirstByteMs != nil {
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
		return "全部账户正常"
	}
	return formatCount(healthy) + "/" + formatCount(total) + " 个账户正常"
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
