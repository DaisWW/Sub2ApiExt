package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/DaisWW/Sub2ApiExt/monitoring/internal/model"
	"github.com/DaisWW/Sub2ApiExt/monitoring/internal/stats"
)

func scanDashboardTarget(rows *sql.Rows, now time.Time, staleAfter time.Duration) (model.DashboardTarget, bool, error) {
	var target model.DashboardTarget
	var latestStatus, latestHealthReason, latestSource, latestMessage sql.NullString
	var routeConfigured bool
	var routeMessage sql.NullString
	var latestLatency, latestFirst sql.NullInt64
	var latestAt sql.NullTime
	var samples, successful, rateLimited, hardFailures, attempts int
	var firstFastest, latencyFastest sql.NullInt64
	var firstMedian, firstP95, latencyMedian, latencyP95 sql.NullFloat64
	var recentJSON []byte
	var currentRate sql.NullFloat64
	var priority sql.NullInt64
	var recoveryTrigger sql.NullTime
	var currentWindowSeconds, currentSamples, currentSuccessful int
	var currentRateLimited, currentHardFailures, currentAttempts int
	var currentLatestAt sql.NullTime
	var currentFastest sql.NullInt64
	var currentMedian, currentP95 sql.NullFloat64
	var currentFirstSamples int
	var currentFirstFastest sql.NullInt64
	var currentFirstMedian, currentFirstP95 sql.NullFloat64
	var lastRequestJSON []byte
	var affectedAccounts, observedAccounts, memberAccounts int
	if err := rows.Scan(
		&target.Key, &target.Kind, &target.EntityID, &target.Name, &target.Platform,
		&target.SourceStatus, &target.ProbeEnabled, &recoveryTrigger,
		&currentRate, &priority, &routeConfigured, &routeMessage,
		&latestStatus, &latestHealthReason, &latestLatency,
		&latestFirst, &latestAt, &latestSource, &latestMessage, &samples, &successful,
		&rateLimited, &hardFailures, &attempts,
		&firstFastest, &firstMedian, &firstP95, &latencyFastest,
		&latencyMedian, &latencyP95,
		&currentWindowSeconds, &currentSamples, &currentSuccessful,
		&currentRateLimited, &currentHardFailures, &currentAttempts, &currentLatestAt,
		&currentFastest, &currentMedian, &currentP95,
		&currentFirstSamples, &currentFirstFastest, &currentFirstMedian, &currentFirstP95,
		&affectedAccounts, &observedAccounts, &memberAccounts, &lastRequestJSON,
		&recentJSON,
	); err != nil {
		return model.DashboardTarget{}, false, fmt.Errorf("scan dashboard: %w", err)
	}
	if currentRate.Valid {
		value := currentRate.Float64
		target.RateMultiplier = &value
	}
	if priority.Valid {
		value := int(priority.Int64)
		target.Priority = &value
	}
	if recoveryTrigger.Valid {
		value := recoveryTrigger.Time.UTC()
		target.RecoveryTriggerAt = &value
	}
	target.RouteConfigured = routeConfigured
	target.RouteMessage = strings.TrimSpace(routeMessage.String)
	applyLatestTargetStateWithMessage(&target, latestStatus, latestSource, latestMessage, latestLatency, latestFirst, latestAt, now, staleAfter)
	target.HealthReason = strings.TrimSpace(latestHealthReason.String)
	target.Stats = targetStats(samples, successful, rateLimited, hardFailures, attempts, firstFastest, firstMedian, firstP95, latencyFastest, latencyMedian, latencyP95)
	target.CurrentHealth = evaluateCurrentHealth(currentWindowSeconds, currentSamples, currentSuccessful,
		currentRateLimited, currentHardFailures, currentAttempts, currentLatestAt,
		currentFastest, currentMedian, currentP95,
		currentFirstSamples, currentFirstFastest, currentFirstMedian, currentFirstP95,
		affectedAccounts, observedAccounts, memberAccounts)
	if err := json.Unmarshal(lastRequestJSON, &target.LastRequestHealth); err != nil {
		return model.DashboardTarget{}, false, fmt.Errorf("decode last request health: %w", err)
	}
	target.LastRequestHealth = stats.EvaluateHealth(target.LastRequestHealth, stats.DefaultHealthPolicy)
	applyCurrentHealth(&target)
	if err := json.Unmarshal(recentJSON, &target.RecentSamples); err != nil {
		return model.DashboardTarget{}, false, fmt.Errorf("decode recent samples: %w", err)
	}
	carryForwardStatusSamples(target.RecentSamples)
	if target.LastCheckedAt != nil && isObservedStatus(target.Status) {
		// A target can have valid evidence older than the fixed 24-hour
		// display window. Keep the trajectory continuous by using that latest
		// status as a carried baseline for any buckets still empty after the
		// in-window carry-forward pass. The carried marker makes the age of the
		// evidence visible without pretending those buckets were freshly probed.
		carrySource := target.LatestSource
		if carrySource == "" {
			carrySource = "cache"
		}
		carryForwardTargetStatus(target.RecentSamples, target.Status, carrySource, *target.LastCheckedAt)
		if target.LatestSource == "request_error" &&
			!target.LastCheckedAt.Before(now.Add(-dashboardWindow)) {
			// Keep the winning error visible in the current and later buckets even
			// when an older successful sample occupies the same display window.
			overlayLatestTargetStatus(target.RecentSamples, target.Status, target.LatestSource, *target.LastCheckedAt, now.Add(-dashboardWindow))
		}
	}
	return target, targetContributesAvailability(target), nil
}
