package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLoadConfigUsesSimpleDefaults(t *testing.T) {
	path := writeTestConfig(t, `{"factors":{"LUCEN.CC.":0.85}}`)

	config, err := loadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if config.ProxyURL != "" || len(config.ProxyFallbackURLs) != 0 || config.Interval != 5*time.Minute || config.HistoryWindow != 24*time.Hour || config.MinHistoryCostUSD != 0.01 || config.Confirmations != 2 || config.DryRun {
		t.Fatalf("unexpected defaults: %+v", config)
	}
	factor, host, err := config.factorForBaseURL("https://lucen.cc/v1")
	if err != nil || factor != 0.85 || host != "lucen.cc" {
		t.Fatalf("factorForBaseURL() = %v, %q, %v", factor, host, err)
	}
	factor, _, err = config.factorForBaseURL("https://new-upstream.test")
	if err != nil || factor != 1 {
		t.Fatalf("default factor = %v, %v", factor, err)
	}
}

func TestLoadConfigUsesUpstreamFactorsAsAllowlistAndFactors(t *testing.T) {
	path := writeTestConfig(t, `{
  "sync_target":"account",
  "upstream_factors":{
    "WWW.CODEXAPIS.COM.":1,
    "xixiapi.io":0.9,
    "PPSUBAPI.COM":0.9
  }
}`)

	config, err := loadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(config.SyncHosts) != 3 || len(config.Factors) != 3 {
		t.Fatalf("unexpected upstream config: %+v", config)
	}
	for host := range config.Factors {
		if _, ok := config.SyncHosts[host]; !ok {
			t.Fatalf("upstream host %q was not added to sync allowlist", host)
		}
	}
	if factor, host, err := config.factorForBaseURL("https://www.codexapis.com/v1"); err != nil || factor != 1 || host != "www.codexapis.com" {
		t.Fatalf("DaoGe factorForBaseURL() = %v, %q, %v", factor, host, err)
	}
	if factor, host, err := config.factorForBaseURL("https://ppsubapi.com"); err != nil || factor != 0.9 || host != "ppsubapi.com" {
		t.Fatalf("TokenHorse factorForBaseURL() = %v, %q, %v", factor, host, err)
	}
}

func TestLoadConfigCanonicalEmptyUpstreamFactorsDoNotAllowAllHosts(t *testing.T) {
	config, err := loadConfig(writeTestConfig(t, `{"sync_target":"account","upstream_factors":{}}`))
	if err != nil {
		t.Fatal(err)
	}
	if config.SyncHosts == nil {
		t.Fatal("canonical empty upstream_factors should remain an explicit empty allowlist")
	}
	if !config.syncHostsConfigured {
		t.Fatal("canonical upstream_factors should mark the allowlist as configured")
	}
}

func TestLoadConfigAllowsOptionalRuntimeSettings(t *testing.T) {
	path := writeTestConfig(t, `{
	  "sync_target":"account",
	  "sync_hosts":["WWW.CODEXAPIS.COM", "xixiapi.io", "ppsubapi.com"],
	  "factors":{"ppsubapi.com":0.9},
	  "usage_bootstrap":true,
	  "proxy_url":"http://host.docker.internal:7897",
	  "proxy_fallback_urls":["http://host.docker.internal:7890"],
  "interval":"90s",
  "confirmations":3,
  "dry_run":true
}`)
	config, err := loadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if config.ProxyURL != "http://host.docker.internal:7897" || len(config.ProxyFallbackURLs) != 1 || config.ProxyFallbackURLs[0] != "http://host.docker.internal:7890" || config.Interval != 90*time.Second || config.Confirmations != 3 || !config.DryRun || config.SyncTarget != "account" || !config.UsageBootstrap {
		t.Fatalf("unexpected config: %+v", config)
	}
	if _, ok := config.SyncHosts["www.codexapis.com"]; !ok {
		t.Fatalf("sync_hosts missing normalized DaoGe host: %+v", config.SyncHosts)
	}
	if _, ok := config.SyncHosts["xixiapi.io"]; !ok {
		t.Fatalf("sync_hosts missing Lucen host: %+v", config.SyncHosts)
	}
	if factor, host, err := config.factorForBaseURL("https://ppsubapi.com"); err != nil || factor != 0.9 || host != "ppsubapi.com" {
		t.Fatalf("factorForBaseURL() = %v, %q, %v", factor, host, err)
	}
}

func TestLoadConfigRejectsInvalidSyncTarget(t *testing.T) {
	if _, err := loadConfig(writeTestConfig(t, `{"sync_target":"both"}`)); err == nil {
		t.Fatal("loadConfig() error = nil")
	}
}

func TestLoadConfigRejectsInvalidProxyURL(t *testing.T) {
	for _, input := range []string{
		`{"proxy_url":"ftp://127.0.0.1:21"}`,
		`{"proxy_url":"http://127.0.0.1:7897/path"}`,
		`{"proxy_fallback_urls":["http://127.0.0.1:7890/path"]}`,
	} {
		if _, err := loadConfig(writeTestConfig(t, input)); err == nil {
			t.Fatalf("loadConfig(%s) error = nil", input)
		}
	}
}

func TestLoadConfigRejectsInvalidHistoryWindow(t *testing.T) {
	for _, input := range []string{
		`{"history_window":"30s"}`,
		`{"history_window":"31d"}`,
	} {
		if _, err := loadConfig(writeTestConfig(t, input)); err == nil {
			t.Fatalf("loadConfig(%s) error = nil", input)
		}
	}
}

func TestLoadConfigRejectsInvalidFactor(t *testing.T) {
	for _, input := range []string{
		`{"factors":{"https://lucen.cc":0.85}}`,
		`{"factors":{"lucen.cc":0}}`,
		`{"factors":{"lucen.cc/path":0.85}}`,
		`{"upstream_factors":{"https://lucen.cc":0.9}}`,
		`{"upstream_factors":{"lucen.cc":0}}`,
		`{"upstream_factors":{"lucen.cc/path":0.9}}`,
	} {
		if _, err := loadConfig(writeTestConfig(t, input)); err == nil {
			t.Fatalf("loadConfig(%s) error = nil", input)
		}
	}
}

func TestLoadConfigRejectsMixedUpstreamFactorFormats(t *testing.T) {
	for _, input := range []string{
		`{"upstream_factors":{"lucen.cc":0.9},"sync_hosts":["lucen.cc"]}`,
		`{"upstream_factors":{"lucen.cc":0.9},"factors":{"lucen.cc":0.9}}`,
		`{"upstream_factors":{},"sync_hosts":[]}`,
		`{"upstream_factors":null}`,
	} {
		if _, err := loadConfig(writeTestConfig(t, input)); err == nil || !strings.Contains(err.Error(), "upstream_factors") {
			t.Fatalf("loadConfig(%s) error = %v, want mixed-format error", input, err)
		}
	}
}

func TestLoadConfigRejectsInvalidSyncHost(t *testing.T) {
	for _, input := range []string{
		`{"sync_hosts":["https://xixiapi.io"]}`,
		`{"sync_hosts":["xixiapi.io/path"]}`,
		`{"sync_hosts":["xixiapi.io", "XIXIAPI.IO"]}`,
	} {
		if _, err := loadConfig(writeTestConfig(t, input)); err == nil {
			t.Fatalf("loadConfig(%s) error = nil", input)
		}
	}
}

func TestLoadConfigRejectsOldRules(t *testing.T) {
	_, err := loadConfig(writeTestConfig(t, `{"rules":[]}`))
	if err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("error = %v, want unknown field", err)
	}
}

func writeTestConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
