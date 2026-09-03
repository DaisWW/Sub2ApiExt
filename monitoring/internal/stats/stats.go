package stats

import (
	"fmt"
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

func AggregateGroup(key string, group model.Group, results []model.ProbeResult, now time.Time) model.ProbeResult {
	if len(results) == 0 && len(group.AccountIDs) == 0 && len(group.Members) == 0 {
		return model.ProbeResult{TargetKey: key, Kind: model.KindGroup, EntityID: group.ID, Status: model.StatusUnknown}
	}
	members := normalizeMembers(group, results)
	resultByAccount := make(map[int64]model.ProbeResult, len(results))
	for _, result := range results {
		resultByAccount[result.EntityID] = result
	}
	// Keep the public helper useful for callers that provide only a result slice
	// (the historical API did not require EntityID or AccountIDs).
	if len(members) == 0 && len(results) > 0 {
		members = make([]model.GroupMember, 0, len(results))
		resultByAccount = make(map[int64]model.ProbeResult, len(results))
		for index, result := range results {
			accountID := int64(index + 1)
			members = append(members, model.GroupMember{AccountID: accountID})
			resultByAccount[accountID] = result
		}
	}
	var allLatency, allFirstByte []int
	var healthyLatency, healthyFirstByte []int
	operational, degraded, healthy, failed, unknown := 0, 0, 0, 0, 0
	for _, member := range members {
		result, exists := resultByAccount[member.AccountID]
		status := model.StatusUnknown
		if exists {
			status = result.Status
			// 保留失败样本耗时供全失败分组诊断；混合分组最终只使用健康路径。
			if result.LatencyMs != nil {
				allLatency = append(allLatency, *result.LatencyMs)
			}
			if result.FirstByteMs != nil {
				allFirstByte = append(allFirstByte, *result.FirstByteMs)
			}
		}
		switch {
		case status == model.StatusOperational:
			operational++
			healthy++
			if exists {
				if result.LatencyMs != nil {
					healthyLatency = append(healthyLatency, *result.LatencyMs)
				}
				if result.FirstByteMs != nil {
					healthyFirstByte = append(healthyFirstByte, *result.FirstByteMs)
				}
			}
		case status == model.StatusDegraded:
			degraded++
			healthy++
			if exists {
				if result.LatencyMs != nil {
					healthyLatency = append(healthyLatency, *result.LatencyMs)
				}
				if result.FirstByteMs != nil {
					healthyFirstByte = append(healthyFirstByte, *result.FirstByteMs)
				}
			}
		case status == model.StatusFailed || status == model.StatusError:
			failed++
		default:
			unknown++
		}
	}
	status := groupStatus(operational, degraded, failed, unknown)
	result := model.ProbeResult{
		TargetKey: key,
		Kind:      model.KindGroup,
		EntityID:  group.ID,
		Status:    status,
		CheckedAt: now,
		Message:   routingGroupMessage(group, status, len(members), healthy, failed, unknown),
		Source:    "aggregate",
	}
	latencySamples := allLatency
	firstByteSamples := allFirstByte
	if healthy > 0 {
		// A usable account is the route the channel can actually serve. Do not
		// let a failed fallback's timeout make a mixed, usable group look slow.
		// If every candidate failed, retain all measured timings for diagnosis.
		latencySamples = healthyLatency
		firstByteSamples = healthyFirstByte
	}
	if len(latencySamples) > 0 {
		value := int(medianValue(Summarize(latencySamples)))
		result.LatencyMs = &value
	}
	if len(firstByteSamples) > 0 {
		value := int(medianValue(Summarize(firstByteSamples)))
		result.FirstByteMs = &value
	}
	return result
}

func normalizeMembers(group model.Group, results []model.ProbeResult) []model.GroupMember {
	members := make([]model.GroupMember, 0, len(group.Members)+len(group.AccountIDs)+len(results))
	seen := make(map[int64]struct{})
	appendMember := func(member model.GroupMember) {
		if member.AccountID <= 0 {
			return
		}
		if _, exists := seen[member.AccountID]; exists {
			return
		}
		seen[member.AccountID] = struct{}{}
		members = append(members, member)
	}
	for _, member := range group.Members {
		appendMember(member)
	}
	for _, accountID := range group.AccountIDs {
		appendMember(model.GroupMember{AccountID: accountID})
	}
	for _, result := range results {
		appendMember(model.GroupMember{AccountID: result.EntityID})
	}
	return members
}

func groupStatus(operational, degraded, failed, unknown int) string {
	// The group is a user-facing route: any usable account keeps it usable.
	// A slow account is still usable, but is only the public status when no
	// low-latency account is available.
	switch {
	case operational > 0:
		return model.StatusOperational
	case degraded > 0:
		return model.StatusDegraded
	case unknown > 0:
		return model.StatusUnknown
	case failed > 0:
		return model.StatusFailed
	default:
		return model.StatusUnknown
	}
}

func routingGroupMessage(group model.Group, status string, total, healthy, failed, unknown int) string {
	if len(group.Members) == 0 {
		return groupMessage(total, healthy)
	}
	if status == model.StatusFailed {
		return fmt.Sprintf("当前无可用候选：%d/%d", healthy, total)
	}
	if status == model.StatusUnknown {
		return fmt.Sprintf("无法确认可用路由：%d/%d 个候选可用；%d 个账户待验证", healthy, total, unknown)
	}
	if failed == 0 && unknown == 0 {
		return "全部账户正常"
	}
	return fmt.Sprintf("当前仍可用：%d/%d 个账户；异常 %d，待验证 %d",
		healthy, total, failed, unknown)
}

// groupMessage retains the historical helper used by callers without routing
// metadata. New snapshots use routingGroupMessage above.
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
