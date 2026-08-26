package model

import "time"

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

// Account is the minimum account snapshot needed by the monitor. Credentials
// are never returned by the HTTP layer and are not persisted by this service.
type Account struct {
	ID             int64
	Name           string
	Platform       string
	Type           string
	Status         string
	Schedulable    bool
	Credentials    map[string]any
	GroupIDs       []int64
	LastActivityAt *time.Time
	RecentModel    string
	ProxyURL       string
	ProxyError     string
}

type Group struct {
	ID           int64
	Name         string
	Platform     string
	Status       string
	AccountIDs   []int64
	ProbeEnabled bool
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
	SlowestMs *int     `json:"slowest_ms,omitempty"`
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
	Stats TargetStats `json:"stats"`
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
	WindowDays   int               `json:"window_days"`
	IntervalSec  int               `json:"interval_seconds"`
	Summary      Summary           `json:"summary"`
	Targets      []DashboardTarget `json:"targets"`
	ProbeRunning bool              `json:"probe_running"`
}

type Alert struct {
	ID             int64      `json:"id"`
	TargetKey      string     `json:"target_key"`
	TargetName     string     `json:"target_name"`
	Kind           string     `json:"kind"`
	Status         string     `json:"status"`
	Title          string     `json:"title"`
	Message        string     `json:"message"`
	CreatedAt      time.Time  `json:"created_at"`
	AcknowledgedAt *time.Time `json:"acknowledged_at,omitempty"`
}

type AlertPolicy struct {
	FailureThreshold  int
	RecoveryThreshold int
}

func TargetKey(kind string, id int64) string {
	return kind + ":" + formatID(id)
}

func formatID(id int64) string {
	// Avoid strconv in the many call sites while keeping this model package
	// independent from the persistence and HTTP layers.
	if id == 0 {
		return "0"
	}
	negative := id < 0
	if negative {
		id = -id
	}
	var buf [20]byte
	pos := len(buf)
	for id > 0 {
		pos--
		buf[pos] = byte('0' + id%10)
		id /= 10
	}
	if negative {
		pos--
		buf[pos] = '-'
	}
	return string(buf[pos:])
}
