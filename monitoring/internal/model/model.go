package model

import (
	"strconv"
	"time"
)

const (
	KindAccount = "account"
	KindGroup   = "group"

	StatusOperational = "operational"
	StatusDegraded    = "degraded"
	StatusFailed      = "failed"
	StatusError       = "error"
	StatusUnknown     = "unknown"
	StatusDisabled    = "disabled"
)

// Account 是监控所需的最小账户快照。凭据不会由 HTTP 层返回，
// 也不会被监控服务持久化。
type Account struct {
	ID                  int64
	Name                string
	Platform            string
	Type                string
	Priority            int
	Status              string
	Schedulable         bool
	Credentials         map[string]any
	GroupIDs            []int64
	LastActivityAt      *time.Time
	UpdatedAt           *time.Time
	LastProbeAt         *time.Time
	LastProbeStatus     string
	LastProbeErrorClass string
	LastProbeStatusCode *int
	ProbeFailureStreak  int
	RecentModel         string
	ChatGPTAccount      string
	ProxyURL            string
	ProxyError          string
}

// GroupMember 是分组内一个可调度账户的路由元数据和近期真实流量。
// 账户优先级是主要排序信号，GroupPriority（数字越小越优先）用于同级候选；
// RequestCount 用于修正仅按成员等权聚合的偏差。
type GroupMember struct {
	AccountID       int64
	GroupPriority   int
	AccountPriority int
	RequestCount    int64
}

type Group struct {
	ID               int64
	Name             string
	Platform         string
	Status           string
	AccountIDs       []int64
	Members          []GroupMember
	HasActiveChannel bool
	ProbeEnabled     bool
}

type Snapshot struct {
	Accounts []Account
	Groups   []Group
}

type Target struct {
	Key               string     `json:"key"`
	Kind              string     `json:"kind"`
	EntityID          int64      `json:"entity_id"`
	Name              string     `json:"name"`
	Platform          string     `json:"platform"`
	SourceStatus      string     `json:"source_status"`
	ProbeEnabled      bool       `json:"probe_enabled"`
	LastCheckedAt     *time.Time `json:"last_checked_at,omitempty"`
	Status            string     `json:"status"`
	Stale             bool       `json:"stale"`
	LatestSource      string     `json:"latest_source,omitempty"`
	LatestMessage     string     `json:"latest_message,omitempty"`
	LatestLatencyMs   *int       `json:"latest_latency_ms,omitempty"`
	LatestFirstByteMs *int       `json:"latest_first_byte_ms,omitempty"`
}

type ProbeResult struct {
	TargetKey   string    `json:"target_key"`
	Kind        string    `json:"kind"`
	EntityID    int64     `json:"entity_id"`
	GroupID     *int64    `json:"group_id,omitempty"`
	Status      string    `json:"status"`
	LatencyMs   *int      `json:"latency_ms,omitempty"`
	FirstByteMs *int      `json:"first_byte_ms,omitempty"`
	StatusCode  *int      `json:"status_code,omitempty"`
	ErrorClass  string    `json:"error_class,omitempty"`
	Message     string    `json:"message,omitempty"`
	CheckedAt   time.Time `json:"checked_at"`
	Source      string    `json:"source"`
}

type MetricStats struct {
	FastestMs *int     `json:"fastest_ms,omitempty"`
	MedianMs  *float64 `json:"median_ms,omitempty"`
	P95Ms     *float64 `json:"p95_ms,omitempty"`
}

type TargetStats struct {
	Samples      int         `json:"samples"`
	Successful   int         `json:"successful"`
	Errors       int         `json:"errors"`
	Availability float64     `json:"availability"`
	FirstByte    MetricStats `json:"first_byte"`
	Latency      MetricStats `json:"latency"`
}

type DashboardTarget struct {
	Target
	RateMultiplier *float64       `json:"rate_multiplier,omitempty"`
	Stats          TargetStats    `json:"stats"`
	RecentSamples  []StatusSample `json:"recent_samples"`
}

// StatusSample 是目标最近一次观测的紧凑状态，用于绘制状态轨迹。
type StatusSample struct {
	Status      string     `json:"status"`
	CheckedAt   time.Time  `json:"checked_at"`
	Source      string     `json:"source"`
	CarriedFrom *time.Time `json:"carried_from,omitempty"`
}

type Summary struct {
	Targets      int     `json:"targets"`
	Operational  int     `json:"operational"`
	Degraded     int     `json:"degraded"`
	Failed       int     `json:"failed"`
	Unknown      int     `json:"unknown"`
	Availability float64 `json:"availability"`
}

type Dashboard struct {
	GeneratedAt  time.Time         `json:"generated_at"`
	NextProbeAt  *time.Time        `json:"next_probe_at,omitempty"`
	IntervalSec  int               `json:"interval_seconds"`
	Summary      Summary           `json:"summary"`
	Targets      []DashboardTarget `json:"targets"`
	ProbeRunning bool              `json:"probe_running"`
}

// UsageRanking 是由网关 usage_logs 生成的只读用量视图。
// Token 数量保持整数，成本保持浮点数，浏览器无需推断单位。
type UsageRanking struct {
	GeneratedAt time.Time       `json:"generated_at"`
	Period      string          `json:"period"`
	PeriodLabel string          `json:"period_label"`
	Bucket      string          `json:"bucket"`
	StartAt     time.Time       `json:"start_at"`
	EndAt       time.Time       `json:"end_at"`
	Summary     UsageSummary    `json:"summary"`
	Timeline    []UsageBucket   `json:"timeline"`
	Accounts    []UsageRankItem `json:"accounts"`
	Groups      []UsageRankItem `json:"groups"`
	Models      []UsageRankItem `json:"models"`
}

type UsageSummary struct {
	Requests             int64   `json:"requests"`
	TotalTokens          int64   `json:"total_tokens"`
	InputTokens          int64   `json:"input_tokens"`
	OutputTokens         int64   `json:"output_tokens"`
	CacheTokens          int64   `json:"cache_tokens"`
	CacheRead            int64   `json:"cache_read_tokens"`
	TotalCost            float64 `json:"total_cost"`
	CostPerMillionTokens float64 `json:"cost_per_million_tokens"`
	Accounts             int64   `json:"accounts"`
	Groups               int64   `json:"groups"`
}

type UsageBucket struct {
	StartAt              time.Time            `json:"start_at"`
	Requests             int64                `json:"requests"`
	TotalTokens          int64                `json:"total_tokens"`
	TotalCost            float64              `json:"total_cost"`
	CostPerMillionTokens float64              `json:"cost_per_million_tokens"`
	Channels             []UsageChannelBucket `json:"channels"`
}

type UsageChannelBucket struct {
	Name                 string  `json:"name"`
	Requests             int64   `json:"requests"`
	TotalTokens          int64   `json:"total_tokens"`
	TotalCost            float64 `json:"total_cost"`
	CostPerMillionTokens float64 `json:"cost_per_million_tokens"`
}

type UsageRankItem struct {
	Kind                 string  `json:"kind"`
	ID                   *int64  `json:"id,omitempty"`
	Key                  string  `json:"key"`
	Name                 string  `json:"name"`
	Platform             string  `json:"platform,omitempty"`
	Requests             int64   `json:"requests"`
	TotalTokens          int64   `json:"total_tokens"`
	InputTokens          int64   `json:"input_tokens"`
	CacheRead            int64   `json:"cache_read_tokens"`
	CacheHitRate         float64 `json:"cache_hit_rate"`
	TotalCost            float64 `json:"total_cost"`
	CostPerMillionTokens float64 `json:"cost_per_million_tokens"`
	SharePercent         float64 `json:"share_percent"`
}

type Alert struct {
	ID         int64     `json:"id"`
	TargetKey  string    `json:"target_key"`
	TargetName string    `json:"target_name"`
	Kind       string    `json:"kind"`
	Status     string    `json:"status"`
	Title      string    `json:"title"`
	Message    string    `json:"message"`
	CreatedAt  time.Time `json:"created_at"`
}

type AlertPolicy struct {
	FailureThreshold  int
	RecoveryThreshold int
}

func TargetKey(kind string, id int64) string {
	return kind + ":" + strconv.FormatInt(id, 10)
}
