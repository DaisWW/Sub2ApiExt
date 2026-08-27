package stats

import (
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/DaisWW/Sub2ApiExt/monitoring/internal/model"
)

const (
	// A lower-priority tier is a fallback in the gateway. Its failures still
	// matter, but should not make a healthy primary tier look unavailable unless
	// real traffic shows that the fallback is being used.
	groupFallbackRiskFactor  = 0.05
	groupFailureShareWarning = 0.10
	groupUnknownShareWarning = 0.25
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
	tiers := routeTiers(members)
	tierIndex := make(map[routeTier]int, len(tiers))
	for index, tier := range tiers {
		tierIndex[tier] = index
	}

	var latency, firstByte []int
	healthy, failed, unknown := 0, 0, 0
	primaryHealthy := false
	primaryUnknown := false
	primaryFailed := false
	totalRiskWeight, failedRiskWeight, unknownRiskWeight := 0.0, 0.0, 0.0
	activeTier := -1
	for _, member := range members {
		result, exists := resultByAccount[member.AccountID]
		status := model.StatusUnknown
		if exists {
			status = result.Status
			// 失败样本的实测延迟也参与分组统计，确保全失败分组仍能展示超时或响应耗时。
			if result.LatencyMs != nil {
				latency = append(latency, *result.LatencyMs)
			}
			if result.FirstByteMs != nil {
				firstByte = append(firstByte, *result.FirstByteMs)
			}
		}
		tier := tierIndex[routeTier{GroupPriority: member.GroupPriority, AccountPriority: member.AccountPriority}]
		if tier == 0 {
			switch {
			case isHealthyStatus(status):
				primaryHealthy = true
			case status == model.StatusFailed || status == model.StatusError:
				primaryFailed = true
			default:
				primaryUnknown = true
			}
		}
		weight := memberWeight(member, tier)
		if member.RequestCount == 0 && activeTier >= 0 && tier > activeTier {
			weight *= groupFallbackRiskFactor
		}
		// A tier with at least one known-good account is the first tier the
		// gateway can normally serve from. Lower tiers are fallback risk only.
		if isHealthyStatus(status) && activeTier < 0 {
			activeTier = tier
		}
		if activeTier >= 0 && tier > activeTier && member.RequestCount > 0 {
			// Real traffic overrides the fallback prior: it proves that this
			// tier is being selected in practice.
			weight = float64(member.RequestCount) + memberTierPrior(member, tier)
		}
		totalRiskWeight += weight
		switch {
		case isHealthyStatus(status):
			healthy++
		case status == model.StatusFailed || status == model.StatusError:
			failed++
			failedRiskWeight += weight
		default:
			unknown++
			unknownRiskWeight += weight
		}
	}
	status := groupStatus(healthy, failed, unknown, primaryHealthy, primaryFailed, primaryUnknown,
		activeTier, failedRiskWeight, unknownRiskWeight, totalRiskWeight)
	result := model.ProbeResult{
		TargetKey: key,
		Kind:      model.KindGroup,
		EntityID:  group.ID,
		Status:    status,
		CheckedAt: now,
		Message:   routingGroupMessage(group, status, len(members), healthy, failed, unknown, failedRiskWeight, totalRiskWeight),
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

func isHealthyStatus(status string) bool {
	return status == model.StatusOperational || status == model.StatusDegraded
}

type routeTier struct {
	AccountPriority int
	GroupPriority   int
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
		if member.RequestCount < 0 {
			member.RequestCount = 0
		}
		seen[member.AccountID] = struct{}{}
		members = append(members, member)
	}
	for _, member := range group.Members {
		appendMember(member)
	}
	seenAccountIDs := make(map[int64]struct{}, len(members))
	for _, member := range members {
		seenAccountIDs[member.AccountID] = struct{}{}
	}
	for _, accountID := range group.AccountIDs {
		if _, exists := seenAccountIDs[accountID]; exists {
			continue
		}
		appendMember(model.GroupMember{AccountID: accountID})
		seenAccountIDs[accountID] = struct{}{}
	}
	for _, result := range results {
		appendMember(model.GroupMember{AccountID: result.EntityID})
	}
	sort.SliceStable(members, func(i, j int) bool {
		left, right := members[i], members[j]
		if left.AccountPriority != right.AccountPriority {
			return left.AccountPriority < right.AccountPriority
		}
		if left.GroupPriority != right.GroupPriority {
			return left.GroupPriority < right.GroupPriority
		}
		return left.AccountID < right.AccountID
	})
	return members
}

func routeTiers(members []model.GroupMember) []routeTier {
	seen := make(map[routeTier]struct{}, len(members))
	tiers := make([]routeTier, 0, len(members))
	for _, member := range members {
		tier := routeTier{GroupPriority: member.GroupPriority, AccountPriority: member.AccountPriority}
		if _, exists := seen[tier]; exists {
			continue
		}
		seen[tier] = struct{}{}
		tiers = append(tiers, tier)
	}
	sort.Slice(tiers, func(i, j int) bool {
		if tiers[i].AccountPriority != tiers[j].AccountPriority {
			return tiers[i].AccountPriority < tiers[j].AccountPriority
		}
		return tiers[i].GroupPriority < tiers[j].GroupPriority
	})
	return tiers
}

func memberTierPrior(member model.GroupMember, tier int) float64 {
	return 1 / (1 + float64(tier))
}

func memberWeight(member model.GroupMember, tier int) float64 {
	prior := memberTierPrior(member, tier)
	if member.RequestCount > 0 {
		return float64(member.RequestCount) + prior
	}
	return prior
}

func groupStatus(
	healthy, failed, unknown int,
	primaryHealthy, primaryFailed, primaryUnknown bool,
	activeTier int,
	failedRiskWeight, unknownRiskWeight, totalRiskWeight float64,
) string {
	if healthy == 0 {
		if unknown > 0 {
			return model.StatusUnknown
		}
		return model.StatusFailed
	}
	if activeTier > 0 || (!primaryHealthy && (primaryFailed || primaryUnknown)) {
		return model.StatusDegraded
	}
	if totalRiskWeight > 0 {
		if failedRiskWeight/totalRiskWeight >= groupFailureShareWarning ||
			unknownRiskWeight/totalRiskWeight >= groupUnknownShareWarning {
			return model.StatusDegraded
		}
	}
	if failed > 0 || unknown > 0 {
		// Low-risk fallback anomalies are intentionally visible in the message,
		// but do not turn a normally healthy route into an outage.
		return model.StatusOperational
	}
	return model.StatusOperational
}

func routingGroupMessage(group model.Group, status string, total, healthy, failed, unknown int, failedRiskWeight, totalRiskWeight float64) string {
	if len(group.Members) == 0 {
		return groupMessage(total, healthy)
	}
	if status == model.StatusFailed {
		return fmt.Sprintf("%d/%d accounts healthy", healthy, total)
	}
	if status == model.StatusUnknown {
		return fmt.Sprintf("无法确认可用路由：%d/%d accounts healthy，%d 个账户待验证", healthy, total, unknown)
	}
	if failed == 0 && unknown == 0 {
		return "全部账户正常"
	}
	risk := 0.0
	if totalRiskWeight > 0 {
		risk = failedRiskWeight / totalRiskWeight * 100
	}
	return fmt.Sprintf("当前路由可用：%d/%d accounts healthy；异常 %d，待验证 %d；预计失败暴露 %.1f%%",
		healthy, total, failed, unknown, risk)
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
