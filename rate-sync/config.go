package main

import (
	"encoding/json"
	"fmt"
	"math"
	"net/url"
	"os"
	"strings"
	"time"
)

const (
	defaultSub2APIURL        = "http://sub2api:8080"
	defaultInterval          = "300s"
	defaultStateFile         = "/data/state.json"
	defaultSyncTarget        = "group"
	defaultHistoryWindow     = "24h"
	defaultMinHistoryCostUSD = 0.01
)

type fileConfig struct {
	Sub2APIURL         string             `json:"sub2api_url"`
	ProxyURL           string             `json:"proxy_url"`
	ProxyFallbackURLs  []string           `json:"proxy_fallback_urls"`
	Interval           string             `json:"interval"`
	SyncTarget         string             `json:"sync_target"`
	SyncHosts          []string           `json:"sync_hosts"`
	UsageBootstrap     bool               `json:"usage_bootstrap"`
	HistoryWindow      string             `json:"history_window"`
	MinHistoryCostUSD  float64            `json:"min_history_cost_usd"`
	DryRun             bool               `json:"dry_run"`
	Confirmations      int                `json:"confirmations"`
	StateFile          string             `json:"state_file"`
	UpstreamFactors    map[string]float64 `json:"upstream_factors"`
	Factors            map[string]float64 `json:"factors"`
	upstreamFactorsSet bool
	syncHostsSet       bool
	factorsSet         bool
}

type Config struct {
	Sub2APIURL        string
	ProxyURL          string
	ProxyFallbackURLs []string
	AdminAPIKey       string
	Interval          time.Duration
	SyncTarget        string
	// SyncHosts is derived from the canonical upstream_factors mapping.
	SyncHosts         map[string]struct{}
	UsageBootstrap    bool
	HistoryWindow     time.Duration
	MinHistoryCostUSD float64
	DryRun            bool
	Confirmations     int
	StateFile         string
	// Factors contains the normalized discount coefficients derived from upstream_factors.
	Factors map[string]float64
	// syncHostsConfigured distinguishes an explicit canonical empty allowlist from legacy defaults.
	syncHostsConfigured bool
}

func loadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取配置文件: %w", err)
	}

	var raw fileConfig
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&raw); err != nil {
		return nil, fmt.Errorf("解析配置文件: %w", err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return nil, fmt.Errorf("解析配置文件: %w", err)
	}
	_, raw.upstreamFactorsSet = fields["upstream_factors"]
	_, raw.syncHostsSet = fields["sync_hosts"]
	_, raw.factorsSet = fields["factors"]
	return normalizeFileConfig(raw)
}

func normalizeFileConfig(raw fileConfig) (*Config, error) {
	applyConfigDefaults(&raw)
	if err := validateSyncTarget(raw.SyncTarget); err != nil {
		return nil, err
	}
	historyWindow, err := parseHistoryWindow(raw.HistoryWindow)
	if err != nil {
		return nil, err
	}
	if err := validateMinHistoryCost(raw.MinHistoryCostUSD); err != nil {
		return nil, err
	}
	interval, err := parseSyncInterval(raw.Interval)
	if err != nil {
		return nil, err
	}
	if err := validateConfirmations(raw.Confirmations); err != nil {
		return nil, err
	}
	if err := validateHTTPURL(raw.Sub2APIURL, "sub2api_url"); err != nil {
		return nil, err
	}
	proxyFallbackURLs, err := normalizeProxyFallbacks(raw.ProxyURL, raw.ProxyFallbackURLs)
	if err != nil {
		return nil, err
	}
	factors, syncHosts, err := normalizeUpstreamConfig(raw)
	if err != nil {
		return nil, err
	}
	canonicalUpstream := raw.upstreamFactorsSet || raw.UpstreamFactors != nil
	return &Config{
		Sub2APIURL:          strings.TrimRight(raw.Sub2APIURL, "/"),
		ProxyURL:            strings.TrimRight(strings.TrimSpace(raw.ProxyURL), "/"),
		ProxyFallbackURLs:   proxyFallbackURLs,
		Interval:            interval,
		SyncTarget:          raw.SyncTarget,
		SyncHosts:           syncHosts,
		UsageBootstrap:      raw.UsageBootstrap,
		HistoryWindow:       historyWindow,
		MinHistoryCostUSD:   raw.MinHistoryCostUSD,
		DryRun:              raw.DryRun,
		Confirmations:       raw.Confirmations,
		StateFile:           raw.StateFile,
		Factors:             factors,
		syncHostsConfigured: canonicalUpstream,
	}, nil
}

func normalizeUpstreamConfig(raw fileConfig) (map[string]float64, map[string]struct{}, error) {
	hasUpstreamFactors := raw.upstreamFactorsSet || raw.UpstreamFactors != nil
	hasSyncHosts := raw.syncHostsSet || raw.SyncHosts != nil
	hasFactors := raw.factorsSet || raw.Factors != nil
	if hasUpstreamFactors {
		if hasSyncHosts || hasFactors {
			return nil, nil, fmt.Errorf("upstream_factors 不能与 sync_hosts 或 factors 同时配置，请只保留 upstream_factors")
		}
		if raw.UpstreamFactors == nil {
			return nil, nil, fmt.Errorf("upstream_factors 必须是域名到折扣系数的对象")
		}
		factors, err := normalizeFactorsForField(raw.UpstreamFactors, "upstream_factors")
		if err != nil {
			return nil, nil, err
		}
		syncHosts := make(map[string]struct{}, len(factors))
		for host := range factors {
			syncHosts[host] = struct{}{}
		}
		return factors, syncHosts, nil
	}

	// 兼容旧版拆分配置；新配置应使用 upstream_factors，避免白名单和系数漂移。
	if raw.factorsSet && raw.Factors == nil {
		return nil, nil, fmt.Errorf("factors 必须是域名到系数的对象")
	}
	factors, err := normalizeFactors(raw.Factors)
	if err != nil {
		return nil, nil, err
	}
	if raw.syncHostsSet && raw.SyncHosts == nil {
		return nil, nil, fmt.Errorf("sync_hosts 必须是域名数组")
	}
	syncHosts, err := normalizeSyncHosts(raw.SyncHosts)
	if err != nil {
		return nil, nil, err
	}
	return factors, syncHosts, nil
}

func applyConfigDefaults(raw *fileConfig) {
	if raw.Sub2APIURL == "" {
		raw.Sub2APIURL = defaultSub2APIURL
	}
	if raw.Interval == "" {
		raw.Interval = defaultInterval
	}
	if raw.SyncTarget == "" {
		raw.SyncTarget = defaultSyncTarget
	}
	raw.SyncTarget = strings.ToLower(strings.TrimSpace(raw.SyncTarget))
	if raw.HistoryWindow == "" {
		raw.HistoryWindow = defaultHistoryWindow
	}
	if raw.MinHistoryCostUSD <= 0 {
		raw.MinHistoryCostUSD = defaultMinHistoryCostUSD
	}
	if raw.Confirmations == 0 {
		raw.Confirmations = 2
	}
	if raw.StateFile == "" {
		raw.StateFile = defaultStateFile
	}
}

func validateSyncTarget(target string) error {
	if target != "group" && target != "account" {
		return fmt.Errorf("sync_target 必须是 group 或 account")
	}
	return nil
}

func parseHistoryWindow(value string) (time.Duration, error) {
	historyWindow, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("history_window 无效: %w", err)
	}
	if historyWindow < time.Minute || historyWindow > 30*24*time.Hour {
		return 0, fmt.Errorf("history_window 必须在 1m 到 720h 之间")
	}
	return historyWindow, nil
}

func parseSyncInterval(value string) (time.Duration, error) {
	interval, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("interval 无效: %w", err)
	}
	if interval < 10*time.Second {
		return 0, fmt.Errorf("interval 不能小于 10s")
	}
	return interval, nil
}

func validateMinHistoryCost(value float64) error {
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return fmt.Errorf("min_history_cost_usd 必须是大于 0 的有限数字")
	}
	return nil
}

func validateConfirmations(value int) error {
	if value < 1 || value > 5 {
		return fmt.Errorf("confirmations 必须在 1 到 5 之间")
	}
	return nil
}

func normalizeProxyFallbacks(proxyURL string, fallbackURLs []string) ([]string, error) {
	if strings.TrimSpace(proxyURL) != "" {
		if err := validateProxyURL(proxyURL, "proxy_url"); err != nil {
			return nil, err
		}
	}
	result := make([]string, 0, len(fallbackURLs))
	for index, fallbackURL := range fallbackURLs {
		fallbackURL = strings.TrimSpace(fallbackURL)
		if err := validateProxyURL(fallbackURL, fmt.Sprintf("proxy_fallback_urls[%d]", index)); err != nil {
			return nil, err
		}
		result = append(result, strings.TrimRight(fallbackURL, "/"))
	}
	return result, nil
}

func normalizeFactors(values map[string]float64) (map[string]float64, error) {
	return normalizeFactorsForField(values, "factors")
}

func normalizeFactorsForField(values map[string]float64, name string) (map[string]float64, error) {
	factors := make(map[string]float64, len(values))
	for value, factor := range values {
		host, err := normalizeHost(value, name)
		if err != nil {
			return nil, err
		}
		if factor <= 0 || math.IsNaN(factor) || math.IsInf(factor, 0) {
			return nil, fmt.Errorf("%s[%q] 必须是大于 0 的有限数字", name, value)
		}
		if _, exists := factors[host]; exists {
			return nil, fmt.Errorf("%s 中的域名 %q 重复", name, host)
		}
		factors[host] = factor
	}
	return factors, nil
}

func normalizeSyncHosts(values []string) (map[string]struct{}, error) {
	if values == nil {
		return nil, nil
	}
	syncHosts := make(map[string]struct{}, len(values))
	for _, value := range values {
		host, err := normalizeHost(value, "sync_hosts")
		if err != nil {
			return nil, err
		}
		if _, exists := syncHosts[host]; exists {
			return nil, fmt.Errorf("sync_hosts 中的域名 %q 重复", host)
		}
		syncHosts[host] = struct{}{}
	}
	return syncHosts, nil
}

func (c *Config) factorForBaseURL(baseURL string) (float64, string, error) {
	parsed, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil || parsed.Hostname() == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return 0, "", fmt.Errorf("账号 base_url 必须是有效的 http/https URL")
	}
	host := strings.ToLower(strings.TrimSuffix(parsed.Hostname(), "."))
	if factor, exists := c.Factors[host]; exists {
		return factor, host, nil
	}
	return 1, host, nil
}

func normalizeHost(value, field string) (string, error) {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" || strings.Contains(value, "://") {
		return "", fmt.Errorf("%s 中的值 %q 必须是域名，不要填写 URL", field, value)
	}
	parsed, err := url.Parse("https://" + value)
	if err != nil || parsed.Hostname() == "" || parsed.User != nil || parsed.Port() != "" || parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", fmt.Errorf("%s 中的值 %q 必须是有效域名", field, value)
	}
	return strings.TrimSuffix(parsed.Hostname(), "."), nil
}

func validateHTTPURL(value, field string) error {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return fmt.Errorf("%s 必须是有效的 http/https URL", field)
	}
	return nil
}

func validateProxyURL(value, field string) error {
	parsed, err := url.Parse(strings.TrimSpace(value))
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") ||
		parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return fmt.Errorf("%s 必须是有效的 http/https 代理 URL", field)
	}
	return nil
}
