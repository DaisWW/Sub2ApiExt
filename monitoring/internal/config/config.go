package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	ListenAddr        string
	DatabaseURL       string
	Interval          time.Duration
	RequestTimeout    time.Duration
	Retention         time.Duration
	ProbeConcurrency  int
	WindowDays        int
	FailureThreshold  int
	RecoveryThreshold int
	DefaultModel      string
	AllowPrivateHost  bool
	APIToken          string
}

func Load() (Config, error) {
	c := Config{
		ListenAddr:        envString("MONITORING_LISTEN_ADDR", ":8090"),
		DatabaseURL:       envString("MONITORING_DATABASE_URL", ""),
		Interval:          envDuration("MONITORING_INTERVAL", 60*time.Second),
		RequestTimeout:    envDuration("MONITORING_REQUEST_TIMEOUT", 30*time.Second),
		Retention:         envDuration("MONITORING_RETENTION", 30*24*time.Hour),
		ProbeConcurrency:  envInt("MONITORING_PROBE_CONCURRENCY", 8),
		WindowDays:        envInt("MONITORING_WINDOW_DAYS", 7),
		FailureThreshold:  envInt("MONITORING_FAILURE_THRESHOLD", 2),
		RecoveryThreshold: envInt("MONITORING_RECOVERY_THRESHOLD", 1),
		DefaultModel:      envString("MONITORING_DEFAULT_MODEL", "gpt-4o-mini"),
		AllowPrivateHost:  strings.EqualFold(envString("MONITORING_ALLOW_PRIVATE_HOSTS", "false"), "true"),
		APIToken:          strings.TrimSpace(os.Getenv("MONITORING_API_TOKEN")),
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
	if c.RequestTimeout <= 0 || c.ProbeConcurrency <= 0 || c.WindowDays <= 0 {
		return Config{}, fmt.Errorf("monitoring timeout, concurrency, and window must be positive")
	}
	if c.FailureThreshold <= 0 || c.RecoveryThreshold <= 0 {
		return Config{}, fmt.Errorf("alert thresholds must be positive")
	}
	return c, nil
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
	return fmt.Sprintf("postgres://%s:%s@%s:%s/%s?sslmode=%s", urlEscape(user), urlEscape(password), host, port, urlEscape(dbname), urlEscape(sslmode))
}

func urlEscape(value string) string {
	return strings.ReplaceAll(strings.ReplaceAll(value, "%", "%25"), "@", "%40")
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
