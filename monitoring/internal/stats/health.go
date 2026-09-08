package stats

import "github.com/DaisWW/Sub2ApiExt/monitoring/internal/model"

// HealthPolicy controls the small amount of hysteresis-like policy that is
// applied to a request window. The window itself is built by the store; this
// type keeps classification deterministic and easy to test.
type HealthPolicy struct {
	// MinimumSamples controls when the window is considered statistically
	// mature. Smaller windows remain usable but carry low confidence.
	MinimumSamples int
	// MinimumSignalSamples lets a short burst expose a repeatable 429 pattern
	// before the normal confidence threshold is reached.
	MinimumSignalSamples  int
	RateLimitDegradePct   float64
	HardFailureDegradePct float64
	SlowLatencyMs         int
}

// DefaultHealthPolicy is deliberately conservative: a few sparse requests
// can expose a warning, but do not change the public state until the window
// has enough evidence.
var DefaultHealthPolicy = HealthPolicy{
	MinimumSamples:        10,
	MinimumSignalSamples:  5,
	RateLimitDegradePct:   10,
	HardFailureDegradePct: 10,
	SlowLatencyMs:         20_000,
}

// EvaluateHealth classifies a completed request window. A degraded result is
// still available when at least one request succeeded; its reason explains
// whether the quality problem is throttling, latency, or a hard error.
func EvaluateHealth(window model.HealthWindow, policy HealthPolicy) model.HealthWindow {
	window = normalizeWindow(window)
	window.Confidence = confidenceFor(window.Samples, policy.MinimumSamples)
	signalSamples := policy.MinimumSignalSamples
	if signalSamples <= 0 {
		signalSamples = policy.MinimumSamples
	}
	if signalSamples <= 0 {
		signalSamples = 1
	}

	switch {
	case window.Samples == 0:
		window.Status = model.StatusUnknown
		window.Reason = model.HealthReasonNoEvidence
		window.Available = false
	case window.Successful == 0:
		window.Status = model.StatusFailed
		window.Reason = failureReason(window)
		window.Available = false
	case window.Samples < signalSamples:
		window.Status = model.StatusOperational
		window.Reason = warningReason(window, policy)
		window.Available = true
	default:
		window.Status, window.Reason = qualityStatus(window, policy)
		window.Available = true
	}
	return window
}

func normalizeWindow(window model.HealthWindow) model.HealthWindow {
	window.Samples = nonNegative(window.Samples)
	window.Attempts = nonNegative(window.Attempts)
	window.Successful = nonNegative(window.Successful)
	window.RateLimited = nonNegative(window.RateLimited)
	window.HardFailures = nonNegative(window.HardFailures)
	if window.Samples == 0 {
		if window.RateLimited > 0 {
			// A rate-limit-only request is a final failed outcome when no later
			// success is available to close it.
			window.Samples = window.RateLimited
		}
	}
	if window.Attempts == 0 {
		window.Attempts = window.Samples + window.RateLimited
	}
	if window.Samples == 0 {
		window.SuccessRate = 0
		window.RateLimitRate = 0
		window.HardFailureRate = 0
		return window
	}
	window.SuccessRate = percentage(window.Successful, window.Samples)
	if window.Attempts > 0 {
		window.RateLimitRate = percentage(window.RateLimited, window.Attempts)
	}
	window.HardFailureRate = percentage(window.HardFailures, window.Samples)
	return window
}

func qualityStatus(window model.HealthWindow, policy HealthPolicy) (string, string) {
	if window.HardFailureRate >= policy.HardFailureDegradePct {
		return model.StatusDegraded, model.HealthReasonUpstreamError
	}
	if window.RateLimitRate >= policy.RateLimitDegradePct {
		return model.StatusDegraded, model.HealthReasonRateLimited
	}
	if slowLatency(window.Latency, policy.SlowLatencyMs) {
		return model.StatusDegraded, model.HealthReasonSlow
	}
	return model.StatusOperational, ""
}

func warningReason(window model.HealthWindow, policy HealthPolicy) string {
	if window.RateLimited > 0 {
		return model.HealthReasonRateLimited
	}
	if window.HardFailures > 0 {
		return model.HealthReasonUpstreamError
	}
	if slowLatency(window.Latency, policy.SlowLatencyMs) {
		return model.HealthReasonSlow
	}
	return ""
}

func failureReason(window model.HealthWindow) string {
	if window.RateLimited > 0 && window.HardFailures == 0 {
		return model.HealthReasonRateLimited
	}
	return model.HealthReasonUpstreamError
}

func confidenceFor(samples, minimum int) string {
	if samples <= 0 || samples < minimum {
		return model.HealthConfidenceLow
	}
	return model.HealthConfidenceNormal
}

func slowLatency(metrics model.MetricStats, threshold int) bool {
	return threshold > 0 && metrics.P95Ms != nil && *metrics.P95Ms >= float64(threshold)
}

func percentage(value, total int) float64 {
	return float64(value) * 100 / float64(total)
}

func nonNegative(value int) int {
	if value < 0 {
		return 0
	}
	return value
}

// WindowFromOutcomes is useful for tests and non-SQL callers that already
// have normalized request outcomes. Latency values belong to successful
// requests only.
func WindowFromOutcomes(
	successful, rateLimited, hardFailures int,
	latencies []int,
	policy HealthPolicy,
) model.HealthWindow {
	window := model.HealthWindow{
		Successful:   successful,
		RateLimited:  rateLimited,
		HardFailures: hardFailures,
	}
	window.Samples = successful + hardFailures
	if window.Samples == 0 && rateLimited > 0 {
		window.Samples = rateLimited
	}
	window.Attempts = window.Samples + rateLimited
	if len(latencies) > 0 {
		window.Latency = Summarize(latencies)
	}
	return EvaluateHealth(window, policy)
}
