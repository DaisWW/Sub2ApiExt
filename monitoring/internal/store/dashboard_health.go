package store

import (
	"database/sql"
	"fmt"
	"strings"

	"github.com/DaisWW/Sub2ApiExt/monitoring/internal/model"
	"github.com/DaisWW/Sub2ApiExt/monitoring/internal/stats"
)

func targetStats(
	samples, successful, rateLimited, hardFailures int,
	firstFastest sql.NullInt64,
	firstMedian sql.NullFloat64,
	firstP95 sql.NullFloat64,
	latencyFastest sql.NullInt64,
	latencyMedian, latencyP95 sql.NullFloat64,
) model.TargetStats {
	stats := model.TargetStats{
		Samples: samples, Successful: successful, Errors: samples - successful,
		RateLimited: rateLimited, HardFailures: hardFailures,
	}
	if samples > 0 {
		stats.Availability = float64(successful) * 100 / float64(samples)
		stats.RateLimitRate = float64(rateLimited) * 100 / float64(samples)
		stats.HardFailureRate = float64(hardFailures) * 100 / float64(samples)
	}
	stats.FirstByte = metricStats(firstFastest, firstMedian, firstP95)
	stats.Latency = metricStats(latencyFastest, latencyMedian, latencyP95)
	return stats
}

func evaluateCurrentHealth(
	windowSeconds, samples, successful, rateLimited, hardFailures, attempts int,
	latestAt sql.NullTime, fastest sql.NullInt64, median, p95 sql.NullFloat64,
	affectedAccounts, observedAccounts, memberAccounts int,
) model.HealthWindow {
	window := model.HealthWindow{
		WindowSeconds: windowSeconds,
		Samples:       samples, Attempts: attempts, Successful: successful,
		RateLimited: rateLimited, HardFailures: hardFailures,
		AffectedAccounts: affectedAccounts, ObservedAccounts: observedAccounts, MemberAccounts: memberAccounts,
		Latency: metricStats(fastest, median, p95),
	}
	if latestAt.Valid {
		value := latestAt.Time.UTC()
		window.LatestAt = &value
	}
	if window.WindowSeconds <= 0 {
		window.WindowSeconds = 300
	}
	return stats.EvaluateHealth(window, stats.DefaultHealthPolicy)
}

func applyCurrentHealth(target *model.DashboardTarget) {
	if target == nil {
		return
	}
	health := target.CurrentHealth
	if !currentHealthOverridesTarget(*target, health) {
		return
	}
	target.Status = health.Status
	target.Available = health.Available
	target.HealthReason = health.Reason
	if health.Reason != model.HealthReasonRateLimited && isCurrentRateLimitMessage(target.LatestMessage) {
		target.LatestMessage = ""
	}
	if health.LatestAt != nil {
		target.LastCheckedAt = health.LatestAt
		target.Stale = false
	}
	if health.Reason == model.HealthReasonRateLimited {
		if target.Kind == model.KindGroup {
			target.LatestMessage = fmt.Sprintf("当前仍可用；近 %d 分钟阶段性限速 %.1f%%，受影响账户 %d/%d",
				health.WindowSeconds/60, health.RateLimitRate, health.AffectedAccounts, health.MemberAccounts)
		} else {
			target.LatestMessage = fmt.Sprintf("当前可用，但近 %d 分钟有 %.1f%% 上游尝试被限速",
				health.WindowSeconds/60, health.RateLimitRate)
		}
	}
}

func currentHealthOverridesTarget(target model.DashboardTarget, health model.HealthWindow) bool {
	if health.Samples == 0 {
		return false
	}
	if target.Kind != model.KindGroup || health.Available || !target.Available {
		return true
	}
	return health.MemberAccounts > 0 && health.ObservedAccounts >= health.MemberAccounts
}

func isCurrentRateLimitMessage(message string) bool {
	return strings.HasPrefix(message, "当前仍可用；近 ") && strings.Contains(message, "分钟阶段性限速 ") ||
		strings.HasPrefix(message, "当前可用，但近 ") && strings.Contains(message, "上游尝试被限速")
}

func addDashboardSummary(summary *model.Summary, target model.DashboardTarget) {
	summary.Targets++
	switch target.Status {
	case model.StatusOperational:
		summary.Operational++
	case model.StatusDegraded:
		summary.Degraded++
	case model.StatusFailed, model.StatusError:
		summary.Failed++
	default:
		summary.Unknown++
	}
	if targetContributesAvailability(target) {
		summary.Availability += targetAvailabilityValue(target)
	}
}

func targetContributesAvailability(target model.DashboardTarget) bool {
	return target.Stats.Samples > 0 || target.CurrentHealth.Samples > 0
}

func targetAvailabilityValue(target model.DashboardTarget) float64 {
	if target.CurrentHealth.Samples > 0 {
		return target.CurrentHealth.SuccessRate
	}
	return target.Stats.Availability
}

func metricStats(fastest sql.NullInt64, median, p95 sql.NullFloat64) model.MetricStats {
	var stats model.MetricStats
	if fastest.Valid {
		value := int(fastest.Int64)
		stats.FastestMs = &value
	}
	if median.Valid {
		value := median.Float64
		stats.MedianMs = &value
	}
	if p95.Valid {
		value := p95.Float64
		stats.P95Ms = &value
	}
	return stats
}
