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
	InputTokens              int64
	OutputTokens             int64
	CacheCreationTokens      int64
	CacheReadTokens          int64
	AccountCost              float64
	ActualCost               float64
	InputCost                float64
	OutputCost               float64
	CacheCreationCost        float64
	CacheReadCost            float64
	CostP75PerMillion        float64
	LatencyP90Ms             float64
	FirstTokenP90Ms          float64
	ErrorRequests            int64
	TerminalFailures         int64
	RateLimitedRequests      int64
	RecoveredRateLimited     int64
	RecoveredRateLimitWeight float64
	Pools                    []PoolMetrics
	// The primary fields above represent the configured reporting window.
	// These optional snapshots let scoring use the fixed 24h/6h/7d policy
	// windows when the store can provide them.  They are intentionally kept
	// out of the report because they are internal evidence, not credentials or
	// mutable account state.
	Window24h *MetricSnapshot
	Window6h  *MetricSnapshot
	Window7d  *MetricSnapshot
}

// MetricSnapshot is an aggregate for one time window.  Costs are kept by
// token class so a window can be normalized to a common cache mix later.
type MetricSnapshot struct {
	SuccessfulRequests       int64
	TerminalFailures         int64
	RecoveredRateLimitWeight float64
	TotalTokens              int64
	InputTokens              int64
	OutputTokens             int64
	CacheCreationTokens      int64
	CacheReadTokens          int64
	AccountCost              float64
	ActualCost               float64
	CostP75PerMillion        float64
	InputCost                float64
	OutputCost               float64
	CacheCreationCost        float64
	CacheReadCost            float64
}

// PoolMetrics 是同一平台、请求模型和实际上游模型比较池内的账户观测。
type PoolMetrics struct {
	Key                 string
	Platform            string
	RequestedModel      string
	UpstreamModel       string
	UpstreamEndpoint    string
	Model               string
	SuccessfulRequests  int64
	TotalTokens         int64
	InputTokens         int64
	OutputTokens        int64
	CacheCreationTokens int64
	CacheReadTokens     int64
	AccountCost         float64
	ActualCost          float64
	CostP75PerMillion   float64
	InputCost           float64
	OutputCost          float64
	CacheCreationCost   float64
	CacheReadCost       float64
	LatencyP90Ms        float64
	FirstTokenP90Ms     float64
	RateMultiplier      float64
	Window24h           *MetricSnapshot
	Window6h            *MetricSnapshot
	Window7d            *MetricSnapshot
}

type Recommendation struct {
	ID                         int64   `json:"id"`
	Name                       string  `json:"name"`
	Platform                   string  `json:"platform"`
	Status                     string  `json:"status"`
	CurrentPriority            int     `json:"current_priority"`
	RecommendedPriority        int     `json:"recommended_priority"`
	CostPerMillionTokens       float64 `json:"cost_per_million_tokens,omitempty"`
	ObservedCostPerMillion     float64 `json:"observed_cost_per_million_tokens,omitempty"`
	FallbackMissCostPerMillion float64 `json:"fallback_miss_cost_per_million_tokens,omitempty"`
	CostAdvantage              float64 `json:"cost_advantage,omitempty"`
	CacheHitRate               float64 `json:"cache_hit_rate,omitempty"`
	PoolCount                  int     `json:"pool_count,omitempty"`
	LatencyP90Ms               float64 `json:"latency_p90_ms,omitempty"`
	FirstTokenP90Ms            float64 `json:"first_token_p90_ms,omitempty"`
	SuccessfulRequests         int64   `json:"successful_requests"`
	TerminalFailures           int64   `json:"terminal_failures"`
	RateLimitedRequests        int64   `json:"rate_limited_requests"`
	RecoveredRateLimited       int64   `json:"recovered_rate_limited"`
	RecoveredRateLimitWeight   float64 `json:"recovered_rate_limit_weight"`
	Availability               float64 `json:"availability"`
	Confidence                 float64 `json:"confidence"`
	CostScore                  float64 `json:"cost_score"`
	MultiplierScore            float64 `json:"multiplier_score"`
	SpeedScore                 float64 `json:"speed_score"`
	AvailabilityScore          float64 `json:"availability_score"`
	Score                      float64 `json:"score"`
	AnchorPriority             int     `json:"anchor_priority,omitempty"`
	NextPriority               int     `json:"next_priority,omitempty"`
	HardExcluded               bool    `json:"hard_excluded"`
	PromotionFrozen            bool    `json:"promotion_frozen,omitempty"`
	Exploration                bool    `json:"exploration,omitempty"`
	ApplyStatus                string  `json:"apply_status,omitempty"`
	Reason                     string  `json:"reason"`
	applyImmediately           bool
	costAdvantageKnown         bool
	explorationStart           bool
	explorationEnd             bool
}

type PriorityReport struct {
	GeneratedAt time.Time        `json:"generated_at"`
	WindowStart time.Time        `json:"window_start"`
	WindowEnd   time.Time        `json:"window_end"`
	DryRun      bool             `json:"dry_run"`
	Accounts    []Recommendation `json:"accounts"`
}
