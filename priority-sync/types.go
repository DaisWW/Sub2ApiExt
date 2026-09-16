package main

import "time"

// AccountMetrics 是策略层从 Sub2API 原始表读取的最小账号快照。
// 不包含 credentials、Token 或其他敏感字段。
type AccountMetrics struct {
	ID                    int64
	Name                  string
	Platform              string
	Type                  string
	Status                string
	CurrentPriority       int
	RateMultiplier        float64
	RateLimitResetAt      *time.Time
	TempUnschedulableTill *time.Time
	OverloadUntil         *time.Time

	SuccessfulRequests       int64
	PricedRequests           int64
	PricedTokens             int64
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
	HasCacheReadCost         bool
	CostP75PerMillion        float64
	LatencyP90Ms             float64
	FirstTokenP90Ms          float64
	ErrorRequests            int64
	TerminalFailures         int64
	TrailingTerminalFailures int64
	RateLimitedRequests      int64
	RecoveredRateLimited     int64
	RecoveredRateLimitWeight float64
	GroupDataAvailable       bool
	GroupPriorities          map[int64]int
	Pools                    []PoolMetrics
	// The primary fields above represent the configured reporting window.
	// These optional snapshots let scoring use fixed decision and observation
	// windows when the store can provide them. They are intentionally kept
	// out of the report because they are internal evidence, not credentials or
	// mutable account state.
	DecisionWindowsAvailable bool
	Window30m                *MetricSnapshot
	Window2h                 *MetricSnapshot
	Window24h                *MetricSnapshot
	Window6h                 *MetricSnapshot
	Window7d                 *MetricSnapshot
	recoveryPlaceholder      bool
}

// MetricSnapshot is an aggregate for one time window.  Costs are kept by
// token class so a window can be normalized to a common cache mix later.
type MetricSnapshot struct {
	SuccessfulRequests       int64
	PricedRequests           int64
	PricedTokens             int64
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
	HasCacheReadCost         bool
	TrailingTerminalFailures int64
}

// PoolMetrics 是同一分组档位、平台、模型、端点和上下文类型内的账户观测。
type PoolMetrics struct {
	Key                 string
	Platform            string
	RequestedModel      string
	UpstreamModel       string
	UpstreamEndpoint    string
	GroupID             int64
	GroupPriority       int
	GroupDataAvailable  bool
	LongContext         bool
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
	HasCacheReadCost    bool
	LatencyP90Ms        float64
	FirstTokenP90Ms     float64
	RateMultiplier      float64
	Window24h           *MetricSnapshot
	Window6h            *MetricSnapshot
	Window7d            *MetricSnapshot
}

type Recommendation struct {
	ID                           int64                   `json:"id"`
	Name                         string                  `json:"name"`
	Platform                     string                  `json:"platform"`
	Status                       string                  `json:"status"`
	CurrentPriority              int                     `json:"current_priority"`
	RecommendedPriority          int                     `json:"recommended_priority"`
	CostPerMillionTokens         float64                 `json:"cost_per_million_tokens,omitempty"`
	ObservedCostPerMillion       float64                 `json:"observed_cost_per_million_tokens,omitempty"`
	FallbackMissCostPerMillion   float64                 `json:"fallback_miss_cost_per_million_tokens,omitempty"`
	CostAdvantage                float64                 `json:"cost_advantage,omitempty"`
	CacheHitRate                 float64                 `json:"cache_hit_rate,omitempty"`
	CacheHitRateKnown            bool                    `json:"cache_hit_rate_known"`
	PoolCount                    int                     `json:"pool_count,omitempty"`
	LatencyP90Ms                 float64                 `json:"latency_p90_ms,omitempty"`
	FirstTokenP90Ms              float64                 `json:"first_token_p90_ms,omitempty"`
	SuccessfulRequests           int64                   `json:"successful_requests"`
	TerminalFailures             int64                   `json:"terminal_failures"`
	RateLimitedRequests          int64                   `json:"rate_limited_requests"`
	RecoveredRateLimited         int64                   `json:"recovered_rate_limited"`
	RecoveredRateLimitWeight     float64                 `json:"recovered_rate_limit_weight"`
	Availability                 float64                 `json:"availability"`
	ScoringWindow                string                  `json:"scoring_window,omitempty"`
	ScoringPricedRequests        int64                   `json:"scoring_priced_requests"`
	ScoringTokens                int64                   `json:"scoring_tokens"`
	ScoringSuccessfulRequests    int64                   `json:"scoring_successful_requests"`
	ScoringTerminalFailures      int64                   `json:"scoring_terminal_failures"`
	TrailingTerminalFailures     int64                   `json:"trailing_terminal_failures"`
	ScoringAvailability          float64                 `json:"scoring_availability"`
	Confidence                   float64                 `json:"confidence"`
	CostScore                    float64                 `json:"cost_score"`
	MultiplierScore              float64                 `json:"multiplier_score"`
	SpeedScore                   float64                 `json:"speed_score"`
	AvailabilityScore            float64                 `json:"availability_score"`
	Score                        float64                 `json:"score"`
	AnchorPriority               int                     `json:"anchor_priority,omitempty"`
	NextPriority                 int                     `json:"next_priority,omitempty"`
	HardExcluded                 bool                    `json:"hard_excluded"`
	PromotionFrozen              bool                    `json:"promotion_frozen,omitempty"`
	Exploration                  bool                    `json:"exploration,omitempty"`
	Recovery                     bool                    `json:"recovery,omitempty"`
	RecoveryAnchorCostPerMillion float64                 `json:"recovery_anchor_cost_per_million,omitempty"`
	RecoveryPeerCount            int                     `json:"recovery_peer_count,omitempty"`
	CostWindows                  []CostWindowObservation `json:"cost_windows,omitempty"`
	ApplyStatus                  string                  `json:"apply_status,omitempty"`
	Reason                       string                  `json:"reason"`
	applyImmediately             bool
	costAdvantageKnown           bool
	explorationStart             bool
	explorationEnd               bool
	recoveryOutcome              string
	recoveryBaselineFailures     int64
	recoveryMissing              bool
}

type CostWindowObservation struct {
	Window         string  `json:"window"`
	PricedRequests int64   `json:"priced_requests"`
	PricedTokens   int64   `json:"priced_tokens"`
	TotalTokens    int64   `json:"total_tokens"`
	CostPerMillion float64 `json:"cost_per_million_tokens,omitempty"`
	FormalEligible bool    `json:"formal_eligible"`
}

type PriorityReport struct {
	GeneratedAt time.Time        `json:"generated_at"`
	WindowStart time.Time        `json:"window_start"`
	WindowEnd   time.Time        `json:"window_end"`
	DryRun      bool             `json:"dry_run"`
	Accounts    []Recommendation `json:"accounts"`
}
