package store

import (
	"time"

	"github.com/DaisWW/Sub2ApiExt/monitoring/internal/model"
)

const dashboardWindow = 24 * time.Hour
const dashboardBucket = dashboardWindow / 24

func carryForwardStatusSamples(samples []model.StatusSample) {
	// Carry the latest known state through empty buckets. A source change marks
	// evidence as old, but it does not erase the last user-visible state: a
	// silent interval is not proof that the account or route became unknown.
	var previous *model.StatusSample
	for i := range samples {
		sample := &samples[i]
		if isObservedStatus(sample.Status) {
			observed := *sample
			previous = &observed
			continue
		}
		if previous != nil {
			carryStatusSample(sample, *previous)
		}
	}
}

func carryForwardTargetStatus(samples []model.StatusSample, status, source string, checkedAt time.Time) {
	if !isObservedStatus(status) || checkedAt.IsZero() {
		return
	}
	baseline := model.StatusSample{Status: status, Source: source, CheckedAt: checkedAt}
	for index := range samples {
		// Preserve both real observations and gaps that were already carried
		// from an earlier observation. Only genuinely empty buckets at or after
		// the target-level evidence can use this baseline. In particular, do not
		// paint buckets before a first failure red.
		if isObservedStatus(samples[index].Status) || samples[index].CarriedFrom != nil {
			continue
		}
		if samples[index].CheckedAt.Before(checkedAt) {
			continue
		}
		carryStatusSample(&samples[index], baseline)
	}
}

func carryStatusSample(sample *model.StatusSample, previous model.StatusSample) {
	if sample == nil {
		return
	}
	sample.Status = previous.Status
	sample.HealthReason = previous.HealthReason
	sample.Source = previous.Source
	if previous.LatencyMs == nil {
		sample.LatencyMs = nil
	} else {
		latency := *previous.LatencyMs
		sample.LatencyMs = &latency
	}
	carriedFrom := previous.CheckedAt
	if previous.CarriedFrom != nil {
		carriedFrom = *previous.CarriedFrom
	}
	sample.CarriedFrom = &carriedFrom
}

func overlayLatestTargetStatus(samples []model.StatusSample, status, source string, checkedAt, windowStart time.Time) {
	if checkedAt.Before(windowStart) {
		return
	}
	for index := range samples {
		sample := &samples[index]
		if sample.CheckedAt.Before(checkedAt) {
			// Empty buckets use their end time as CheckedAt. Include the bucket
			// containing the trigger, while preserving real observations before it.
			if sample.CarriedFrom == nil || !sample.CheckedAt.Add(dashboardBucket).After(checkedAt) {
				continue
			}
		}
		sample.Status = status
		if status == model.StatusFailed || status == model.StatusError {
			sample.HealthReason = model.HealthReasonUpstreamError
		}
		sample.Source = source
		sample.CarriedFrom = nil
	}
}

func isObservedStatus(status string) bool {
	switch status {
	case model.StatusOperational, model.StatusDegraded, model.StatusFailed, model.StatusError:
		return true
	default:
		return false
	}
}
