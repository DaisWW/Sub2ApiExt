package main

import (
	"fmt"
	"math"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	defaultSub2APIURL    = "http://sub2api:8080"
	defaultInterval      = 10 * time.Minute
	defaultWindow        = time.Hour
	defaultCooldown      = 15 * time.Minute
	defaultMinSamples    = 5
	defaultConfirmations = 2
	defaultStateFile     = "/data/state.json"
	defaultReportFile    = "/data/priority-report.json"
)

const (
	costWeight         = 0.70
	speedWeight        = 0.20
	availabilityWeight = 0.10
)

// 优先级数值越小越优先。自动档位之间保留空档，探索档位用于给低样本账号
// 一个有限的观测机会，不与正式评分档位混用。
const (
	priorityBest         = 10
	priorityExplore      = 20
	priorityGood         = 30
	priorityNeutral      = 50
	priorityDegraded     = 70
	priorityPoor         = 90
	priorityUnavailable  = 1000
	priorityRampStep     = 20
	priorityRampMaxSteps = 4
	coldAnchorFloor      = priorityGood

	explorationDuration = 30 * time.Minute
)

type Config struct {
	DatabaseURL    string
	Sub2APIURL     string
	AdminAPIKey    string
	Interval       time.Duration
	Window         time.Duration
	ChangeCooldown time.Duration
	MinSamples     int
	Confirmations  int
	DryRun         bool
	StateFile      string
	ReportFile     string
}

func LoadConfig() (Config, error) {
	c := Config{
		DatabaseURL:    strings.TrimSpace(os.Getenv("PRIORITY_SYNC_DATABASE_URL")),
		Sub2APIURL:     envString("PRIORITY_SYNC_SUB2API_URL", defaultSub2APIURL),
		AdminAPIKey:    strings.TrimSpace(os.Getenv("PRIORITY_SYNC_ADMIN_API_KEY")),
		Interval:       envDuration("PRIORITY_SYNC_INTERVAL", defaultInterval),
		Window:         envDuration("PRIORITY_SYNC_WINDOW", defaultWindow),
		ChangeCooldown: envDuration("PRIORITY_SYNC_CHANGE_COOLDOWN", defaultCooldown),
		MinSamples:     envInt("PRIORITY_SYNC_MIN_SAMPLES", defaultMinSamples),
		Confirmations:  envInt("PRIORITY_SYNC_CONFIRMATIONS", defaultConfirmations),
		DryRun:         envBool("PRIORITY_SYNC_DRY_RUN", false),
		StateFile:      envString("PRIORITY_SYNC_STATE_FILE", defaultStateFile),
		ReportFile:     envString("PRIORITY_SYNC_REPORT_FILE", defaultReportFile),
	}
	if c.DatabaseURL == "" {
		c.DatabaseURL = buildDatabaseURL()
	}
	if c.DatabaseURL == "" {
		return Config{}, fmt.Errorf("PRIORITY_SYNC_DATABASE_URL 或 DATABASE_* 变量是必需的")
	}
	readOnlyURL, err := forceDatabaseReadOnly(c.DatabaseURL)
	if err != nil {
		return Config{}, err
	}
	c.DatabaseURL = readOnlyURL
	if err := validateHTTPURL(c.Sub2APIURL); err != nil {
		return Config{}, err
	}
	if c.Interval < 30*time.Second {
		return Config{}, fmt.Errorf("PRIORITY_SYNC_INTERVAL 不能小于 30s")
	}
	if c.Window < 15*time.Minute || c.Window > 7*24*time.Hour {
		return Config{}, fmt.Errorf("PRIORITY_SYNC_WINDOW 必须在 15m 到 168h 之间")
	}
	if c.ChangeCooldown <= 0 {
		return Config{}, fmt.Errorf("PRIORITY_SYNC_CHANGE_COOLDOWN 必须为正数")
	}
	if c.MinSamples < 1 || c.MinSamples > 10000 {
		return Config{}, fmt.Errorf("PRIORITY_SYNC_MIN_SAMPLES 必须在 1 到 10000 之间")
	}
	if c.Confirmations < 1 || c.Confirmations > 5 {
		return Config{}, fmt.Errorf("PRIORITY_SYNC_CONFIRMATIONS 必须在 1 到 5 之间")
	}
	if strings.TrimSpace(c.StateFile) == "" || strings.TrimSpace(c.ReportFile) == "" {
		return Config{}, fmt.Errorf("状态和报告文件路径不能为空")
	}
	return c, nil
}

func envString(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

func envDuration(name string, fallback time.Duration) time.Duration {
	value := strings.TrimSpace(os.Getenv(name))
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

func envInt(name string, fallback int) int {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return fallback
	}
	return parsed
}

func envBool(name string, fallback bool) bool {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		return fallback
	}
	return parsed
}

func buildDatabaseURL() string {
	host := firstNonEmptyEnv("DATABASE_HOST", "PGHOST")
	if host == "" {
		return ""
	}
	port := firstNonEmptyEnv("DATABASE_PORT", "PGPORT")
	if port == "" {
		port = "5432"
	}
	user := firstNonEmptyEnv("DATABASE_USER", "PGUSER")
	password := firstNonEmptyEnv("DATABASE_PASSWORD", "PGPASSWORD")
	database := firstNonEmptyEnv("DATABASE_DBNAME", "PGDATABASE")
	if database == "" {
		database = "sub2api"
	}
	sslmode := firstNonEmptyEnv("DATABASE_SSLMODE", "PGSSLMODE")
	if sslmode == "" {
		sslmode = "disable"
	}
	connectionURL := &url.URL{
		Scheme: "postgres",
		Host:   net.JoinHostPort(strings.Trim(host, "[]"), port),
		Path:   "/" + database,
		User:   url.UserPassword(user, password),
		RawQuery: url.Values{
			"sslmode": []string{sslmode},
			"options": []string{"-c default_transaction_read_only=on"},
		}.Encode(),
	}
	return connectionURL.String()
}

func forceDatabaseReadOnly(value string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(value))
	if err != nil || parsed.Host == "" || (parsed.Scheme != "postgres" && parsed.Scheme != "postgresql") {
		return "", fmt.Errorf("PRIORITY_SYNC_DATABASE_URL 必须是有效的 postgres:// 或 postgresql:// URL")
	}
	query := parsed.Query()
	options := strings.TrimSpace(query.Get("options"))
	if options != "" {
		options += " "
	}
	options += "-c default_transaction_read_only=on"
	query.Set("options", options)
	parsed.RawQuery = query.Encode()
	return parsed.String(), nil
}

func firstNonEmptyEnv(names ...string) string {
	for _, name := range names {
		if value := strings.TrimSpace(os.Getenv(name)); value != "" {
			return value
		}
	}
	return ""
}

func validateHTTPURL(value string) error {
	parsed, err := url.Parse(strings.TrimSpace(value))
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return fmt.Errorf("PRIORITY_SYNC_SUB2API_URL 必须是有效的 http/https URL")
	}
	return nil
}

func validScore(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0)
}
