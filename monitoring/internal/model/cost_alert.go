package model

import "time"

const (
	CostAlertCacheDegraded = "cache_degraded"
	CostAlertMultiplier    = "multiplier_spike"
	CostAlertUnitCost      = "unit_cost_spike"
	CostAlertSingleRequest = "single_request_cost"
	CostAlertBudgetBurn    = "budget_burn"

	CostAlertNotificationStart      = "start"
	CostAlertNotificationReminder   = "reminder"
	CostAlertNotificationEscalation = "escalation"
	CostAlertNotificationRecovery   = "recovery"
)

// CostAlertEvent is an internal, notification-ready summary of a request-cost
// anomaly. It contains bounded identity metadata and token/cost aggregates;
// request prompts, responses, credentials, and raw request identifiers are excluded.
type CostAlertEvent struct {
	ID                   int64
	AlertKey             string
	Kind                 string
	NotificationType     string
	Severity             string
	TargetKey            string
	Title                string
	Message              string
	UserKey              string
	UserName             string
	UserEmail            string
	APIKeyID             int64
	APIKeyName           string
	Model                string
	ChannelName          string
	AccountName          string
	Requests             int64
	TotalTokens          int64
	MaxRequestCost       float64
	InputTokens          int64
	OutputTokens         int64
	CacheCreationTokens  int64
	CacheReadTokens      int64
	CurrentCost          float64
	BaselineCost         float64
	InputCost            float64
	OutputCost           float64
	CacheCreationCost    float64
	CacheReadCost        float64
	CurrentUnitCost      float64
	BaselineUnitCost     float64
	CurrentCacheHitRate  float64
	BaselineCacheHitRate float64
	CurrentMultiplier    float64
	BaselineMultiplier   float64
	DailyCost            float64
	ProjectedCost        float64
	WindowStart          time.Time
	WindowEnd            time.Time
	IncidentStartedAt    time.Time
	CreatedAt            time.Time
}
