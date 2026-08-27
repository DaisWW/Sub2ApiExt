package config

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode"
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
	FrameAncestors    string
}

const maxWindowDays = 90

func Load() (Config, error) {
	frameAncestors, err := parseFrameAncestors(envString("MONITORING_FRAME_ANCESTORS", "'self'"))
	if err != nil {
		return Config{}, fmt.Errorf("MONITORING_FRAME_ANCESTORS: %w", err)
	}
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
		FrameAncestors:    frameAncestors,
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
	if c.RequestTimeout <= 0 || c.Retention <= 0 || c.ProbeConcurrency <= 0 {
		return Config{}, fmt.Errorf("monitoring timeout, retention, and concurrency must be positive")
	}
	if c.WindowDays <= 0 || c.WindowDays > maxWindowDays {
		return Config{}, fmt.Errorf("MONITORING_WINDOW_DAYS must be between 1 and %d", maxWindowDays)
	}
	if c.FailureThreshold <= 0 || c.RecoveryThreshold <= 0 {
		return Config{}, fmt.Errorf("alert thresholds must be positive")
	}
	return c, nil
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
