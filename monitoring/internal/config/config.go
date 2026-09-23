package config

import (
	"fmt"
	"math"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode"
)

type Config struct {
	ListenAddr         string
	DatabaseURL        string
	RedisAddr          string
	RedisPassword      string
	RedisDB            int
	RedisTLS           bool
	ConcurrencySlotTTL time.Duration
	Interval           time.Duration
	RequestTimeout     time.Duration
	Retention          time.Duration
	ProbeConcurrency   int
	FailureThreshold   int
	RecoveryThreshold  int
	DefaultModel       string
	AllowPrivateHost   bool
	FrameAncestors     string
	CostAlerts         CostAlertConfig
}

// CostAlertConfig controls read-only request-cost anomaly analysis. A zero
// daily budget disables the budget-burn rule; the other rules remain active.
type CostAlertConfig struct {
	Enabled                     bool
	Window                      time.Duration
	Baseline                    time.Duration
	Cooldown                    time.Duration
	MinRequests                 int
	MinTokens                   int64
	CacheMissMinRequests        int
	CacheMissInputTokens        int64
	CacheMissMinCost            float64
	CacheMissMaxSpan            time.Duration
	AccountSwitchMinTransitions int
	MinCost                     float64
	SingleRequestCost           float64
	MinBaseCost                 float64
	CacheBaselineMin            float64
	CacheCurrentMax             float64
	CacheCostRatio              float64
	UnitCostRatio               float64
	MultiplierRatio             float64
	DailyBudget                 float64
	BurnRatio                   float64
	Email                       EmailConfig
}

type EmailConfig struct {
	Host     string
	Port     int
	Security string
	Username string
	Password string
	From     string
	To       []string
	Timeout  time.Duration
}

func Load() (Config, error) {
	frameAncestors, err := parseFrameAncestors(envString("MONITORING_FRAME_ANCESTORS", "'self'"))
	if err != nil {
		return Config{}, fmt.Errorf("MONITORING_FRAME_ANCESTORS: %w", err)
	}
	c := Config{
		ListenAddr:         envString("MONITORING_LISTEN_ADDR", ":8090"),
		DatabaseURL:        envString("MONITORING_DATABASE_URL", ""),
		RedisAddr:          buildRedisAddr(),
		RedisPassword:      os.Getenv("REDIS_PASSWORD"),
		RedisDB:            envIntAllowZero("REDIS_DB", 0),
		RedisTLS:           strings.EqualFold(envString("REDIS_ENABLE_TLS", "false"), "true"),
		ConcurrencySlotTTL: envDuration("MONITORING_CONCURRENCY_SLOT_TTL", 30*time.Minute),
		Interval:           envDuration("MONITORING_INTERVAL", 60*time.Second),
		RequestTimeout:     envDuration("MONITORING_REQUEST_TIMEOUT", 30*time.Second),
		Retention:          envDuration("MONITORING_RETENTION", 30*24*time.Hour),
		ProbeConcurrency:   envInt("MONITORING_PROBE_CONCURRENCY", 8),
		FailureThreshold:   envInt("MONITORING_FAILURE_THRESHOLD", 2),
		RecoveryThreshold:  envInt("MONITORING_RECOVERY_THRESHOLD", 1),
		DefaultModel:       envString("MONITORING_DEFAULT_MODEL", "gpt-4o-mini"),
		AllowPrivateHost:   strings.EqualFold(envString("MONITORING_ALLOW_PRIVATE_HOSTS", "false"), "true"),
		FrameAncestors:     frameAncestors,
		CostAlerts:         loadCostAlertConfig(),
	}
	if c.DatabaseURL == "" {
		c.DatabaseURL = buildDatabaseURL()
	}
	if c.DatabaseURL == "" {
		return Config{}, fmt.Errorf("MONITORING_DATABASE_URL or DATABASE_* variables are required")
	}
	if c.Interval < 15*time.Second {
		return Config{}, fmt.Errorf("MONITORING_INTERVAL must be at least 15s")
	}
	if c.RedisDB < 0 || c.ConcurrencySlotTTL <= 0 {
		return Config{}, fmt.Errorf("Redis DB and concurrency slot TTL must be non-negative/positive")
	}
	if c.RequestTimeout <= 0 || c.Retention <= 0 || c.ProbeConcurrency <= 0 {
		return Config{}, fmt.Errorf("monitoring timeout, retention, and concurrency must be positive")
	}
	if c.FailureThreshold <= 0 || c.RecoveryThreshold <= 0 {
		return Config{}, fmt.Errorf("alert thresholds must be positive")
	}
	if err := validateCostAlertConfig(c.CostAlerts); err != nil {
		return Config{}, err
	}
	return c, nil
}

func loadCostAlertConfig() CostAlertConfig {
	security := strings.ToLower(envString("MONITORING_COST_EMAIL_SECURITY", "implicit_tls"))
	to := envCSV("MONITORING_COST_EMAIL_TO")
	username := strings.TrimSpace(os.Getenv("MONITORING_COST_EMAIL_USERNAME"))
	from := strings.TrimSpace(os.Getenv("MONITORING_COST_EMAIL_FROM"))
	if from == "" {
		from = username
	}
	return CostAlertConfig{
		Enabled:                     envBool("MONITORING_COST_ALERTS_ENABLED", true),
		Window:                      envDuration("MONITORING_COST_WINDOW", 15*time.Minute),
		Baseline:                    envDuration("MONITORING_COST_BASELINE", 7*24*time.Hour),
		Cooldown:                    envDuration("MONITORING_COST_COOLDOWN", 30*time.Minute),
		MinRequests:                 envInt("MONITORING_COST_MIN_REQUESTS", 3),
		MinTokens:                   envInt64("MONITORING_COST_MIN_TOKENS", 100_000),
		CacheMissMinRequests:        envInt("MONITORING_COST_CACHE_MISS_MIN_REQUESTS", 3),
		CacheMissInputTokens:        envInt64("MONITORING_COST_CACHE_MISS_INPUT_TOKENS", 100_000),
		CacheMissMinCost:            envFloat("MONITORING_COST_CACHE_MISS_MIN_COST", 0.5),
		CacheMissMaxSpan:            envDuration("MONITORING_COST_CACHE_MISS_MAX_SPAN", 5*time.Minute),
		AccountSwitchMinTransitions: envInt("MONITORING_COST_ACCOUNT_SWITCH_MIN_TRANSITIONS", 2),
		MinCost:                     envFloat("MONITORING_COST_MIN_COST", 0.5),
		SingleRequestCost:           envFloat("MONITORING_COST_SINGLE_REQUEST_COST", 5),
		MinBaseCost:                 envFloat("MONITORING_COST_MIN_BASE_COST", 0.1),
		CacheBaselineMin:            envFloat("MONITORING_COST_CACHE_BASELINE_MIN", 0.60),
		CacheCurrentMax:             envFloat("MONITORING_COST_CACHE_CURRENT_MAX", 0.20),
		CacheCostRatio:              envFloat("MONITORING_COST_CACHE_COST_RATIO", 1.5),
		UnitCostRatio:               envFloat("MONITORING_COST_UNIT_COST_RATIO", 2.0),
		MultiplierRatio:             envFloat("MONITORING_COST_MULTIPLIER_RATIO", 2.0),
		DailyBudget:                 envFloat("MONITORING_COST_DAILY_BUDGET", 0),
		BurnRatio:                   envFloat("MONITORING_COST_BURN_RATIO", 1.5),
		Email: EmailConfig{
			Host:     envString("MONITORING_COST_EMAIL_HOST", "smtp.qq.com"),
			Port:     envInt("MONITORING_COST_EMAIL_PORT", 465),
			Security: security,
			Username: username,
			Password: os.Getenv("MONITORING_COST_EMAIL_PASSWORD"),
			From:     from,
			To:       to,
			Timeout:  envDuration("MONITORING_COST_EMAIL_TIMEOUT", 15*time.Second),
		},
	}
}

func validateCostAlertConfig(c CostAlertConfig) error {
	if c.Window <= 0 || c.Baseline <= c.Window || c.Cooldown <= 0 {
		return fmt.Errorf("cost alert window, baseline, and cooldown are invalid")
	}
	if c.MinRequests <= 0 || c.MinTokens <= 0 || c.CacheMissMinRequests <= 0 || c.CacheMissInputTokens <= 0 ||
		c.AccountSwitchMinTransitions <= 0 || c.CacheMissMaxSpan <= 0 || c.CacheMissMaxSpan > c.Window ||
		!finiteNonNegative(c.CacheMissMinCost) || !finiteNonNegative(c.MinCost) ||
		!finiteNonNegative(c.SingleRequestCost) || !finiteNonNegative(c.MinBaseCost) {
		return fmt.Errorf("cost alert sample thresholds are invalid")
	}
	if !finiteBetween(c.CacheBaselineMin, 0, 1) || !finiteBetween(c.CacheCurrentMax, 0, 1) ||
		!finiteAtLeast(c.CacheCostRatio, 1) || !finiteAtLeast(c.UnitCostRatio, 1) ||
		!finiteAtLeast(c.MultiplierRatio, 1) || !finiteNonNegative(c.DailyBudget) || !finiteAtLeast(c.BurnRatio, 1) {
		return fmt.Errorf("cost alert ratios or budget are invalid")
	}
	if c.Email.Timeout <= 0 {
		return fmt.Errorf("cost alert email timeout must be positive")
	}
	if c.Email.Security != "implicit_tls" && c.Email.Security != "starttls" {
		return fmt.Errorf("MONITORING_COST_EMAIL_SECURITY must be implicit_tls or starttls")
	}
	emailConfigured := strings.TrimSpace(os.Getenv("MONITORING_COST_EMAIL_TO")) != "" ||
		strings.TrimSpace(os.Getenv("MONITORING_COST_EMAIL_USERNAME")) != "" ||
		strings.TrimSpace(os.Getenv("MONITORING_COST_EMAIL_PASSWORD")) != "" ||
		strings.TrimSpace(os.Getenv("MONITORING_COST_EMAIL_FROM")) != ""
	if emailConfigured {
		if c.Email.Host == "" || c.Email.Port <= 0 || c.Email.Port > 65535 || c.Email.Username == "" || c.Email.Password == "" ||
			c.Email.From == "" || len(c.Email.To) == 0 {
			return fmt.Errorf("cost alert email requires host, port, username, password, from, and recipient")
		}
	}
	return nil
}

func finiteNonNegative(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0) && value >= 0
}

func finiteBetween(value, minimum, maximum float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0) && value >= minimum && value <= maximum
}

func finiteAtLeast(value, minimum float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0) && value >= minimum
}

func buildRedisAddr() string {
	host := strings.TrimSpace(os.Getenv("REDIS_HOST"))
	if host == "" {
		return ""
	}
	return net.JoinHostPort(strings.Trim(host, "[]"), envString("REDIS_PORT", "6379"))
}

func parseFrameAncestors(value string) (string, error) {
	tokens := strings.FieldsFunc(strings.TrimSpace(value), func(r rune) bool {
		return unicode.IsSpace(r) || r == ','
	})
	if len(tokens) == 0 {
		return "'self'", nil
	}
	seen := make(map[string]struct{}, len(tokens))
	normalized := make([]string, 0, len(tokens))
	for _, token := range tokens {
		if token == "'self'" {
			if _, exists := seen[token]; !exists {
				seen[token] = struct{}{}
				normalized = append(normalized, token)
			}
			continue
		}
		if token == "*" || strings.Contains(token, "*") {
			return "", fmt.Errorf("wildcard frame ancestors are not allowed")
		}
		parsed, err := url.Parse(token)
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Hostname() == "" ||
			parsed.User != nil || parsed.Path != "" && parsed.Path != "/" || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.Opaque != "" {
			return "", fmt.Errorf("must be 'self' or an http(s) origin: %q", token)
		}
		origin := strings.ToLower(parsed.Scheme) + "://" + strings.ToLower(parsed.Host)
		if _, exists := seen[origin]; exists {
			continue
		}
		seen[origin] = struct{}{}
		normalized = append(normalized, origin)
	}
	return strings.Join(normalized, " "), nil
}

func buildDatabaseURL() string {
	host := strings.TrimSpace(os.Getenv("DATABASE_HOST"))
	if host == "" {
		return ""
	}
	port := envString("DATABASE_PORT", "5432")
	user := os.Getenv("DATABASE_USER")
	password := os.Getenv("DATABASE_PASSWORD")
	dbname := envString("DATABASE_DBNAME", "sub2api")
	sslmode := envString("DATABASE_SSLMODE", "disable")
	connectionURL := &url.URL{
		Scheme:   "postgres",
		Host:     net.JoinHostPort(strings.Trim(host, "[]"), port),
		Path:     "/" + dbname,
		User:     url.UserPassword(user, password),
		RawQuery: url.Values{"sslmode": []string{sslmode}}.Encode(),
	}
	return connectionURL.String()
}

func envString(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}

func envInt(key string, fallback int) int {
	value, err := strconv.Atoi(strings.TrimSpace(os.Getenv(key)))
	if err != nil || value == 0 {
		return fallback
	}
	return value
}

func envInt64(key string, fallback int64) int64 {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return fallback
	}
	return parsed
}

func envFloat(key string, fallback float64) float64 {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.ParseFloat(value, 64)
	if err != nil {
		return fallback
	}
	return parsed
}

func envBool(key string, fallback bool) bool {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		return fallback
	}
	return parsed
}

func envCSV(key string) []string {
	values := strings.FieldsFunc(os.Getenv(key), func(r rune) bool {
		return unicode.IsSpace(r) || r == ',' || r == ';'
	})
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			result = append(result, value)
		}
	}
	return result
}

func envIntAllowZero(key string, fallback int) int {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return fallback
	}
	return parsed
}

func envDuration(key string, fallback time.Duration) time.Duration {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	parsed, err := time.ParseDuration(value)
	if err != nil {
		if seconds, numberErr := strconv.Atoi(value); numberErr == nil {
			return time.Duration(seconds) * time.Second
		}
		return fallback
	}
	return parsed
}
