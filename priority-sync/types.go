package main

import "time"

// AccountMetrics 是策略层从 Sub2API 原始表读取的最小账号快照。
// 不包含 credentials、Token 或其他敏感字段。
type AccountMetrics struct {
	ID                    int64
	Name                  string
	Platform              string
	Status                string
	CurrentPriority       int
	RateMultiplier        float64
	RateLimitResetAt      *time.Time
	TempUnschedulableTill *time.Time
	OverloadUntil         *time.Time

	SuccessfulRequests       int64
	TotalTokens              int64
	AccountCost              float64
	ActualCost               float64
	LatencyP90Ms             float64
	FirstTokenP90Ms          float64
	ErrorRequests            int64
	TerminalFailures         int64
	RateLimitedRequests      int64
	RecoveredRateLimited     int64
	RecoveredRateLimitWeight float64
}

type Recommendation struct {
	ID                       int64   `json:"id"`
	Name                     string  `json:"name"`
	Platform                 string  `json:"platform"`
	Status                   string  `json:"status"`
	CurrentPriority          int     `json:"current_priority"`
	RecommendedPriority      int     `json:"recommended_priority"`
	CostPerMillionTokens     float64 `json:"cost_per_million_tokens,omitempty"`
	LatencyP90Ms             float64 `json:"latency_p90_ms,omitempty"`
	FirstTokenP90Ms          float64 `json:"first_token_p90_ms,omitempty"`
	SuccessfulRequests       int64   `json:"successful_requests"`
	TerminalFailures         int64   `json:"terminal_failures"`
	RateLimitedRequests      int64   `json:"rate_limited_requests"`
	RecoveredRateLimited     int64   `json:"recovered_rate_limited"`
	RecoveredRateLimitWeight float64 `json:"recovered_rate_limit_weight"`
	Availability             float64 `json:"availability"`
	Confidence               float64 `json:"confidence"`
	CostScore                float64 `json:"cost_score"`
	MultiplierScore          float64 `json:"multiplier_score"`
	SpeedScore               float64 `json:"speed_score"`
	AvailabilityScore        float64 `json:"availability_score"`
	Score                    float64 `json:"score"`
	AnchorPriority           int     `json:"anchor_priority,omitempty"`
	NextPriority             int     `json:"next_priority,omitempty"`
	HardExcluded             bool    `json:"hard_excluded"`
	Exploration              bool    `json:"exploration,omitempty"`
	ApplyStatus              string  `json:"apply_status,omitempty"`
	Reason                   string  `json:"reason"`
	applyImmediately         bool
	explorationStart         bool
	explorationEnd           bool
}

type PriorityReport struct {
	GeneratedAt time.Time        `json:"generated_at"`
	WindowStart time.Time        `json:"window_start"`
	WindowEnd   time.Time        `json:"window_end"`
	DryRun      bool             `json:"dry_run"`
	Accounts    []Recommendation `json:"accounts"`
}
