package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLoadConfigUsesSimpleDefaults(t *testing.T) {
	path := writeTestConfig(t, `{"recharge_discounts":{"LUCEN.CC.":0.85}}`)

	config, err := loadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if config.ProxyURL != "" || len(config.ProxyFallbackURLs) != 0 || config.Interval != 5*time.Minute || config.HistoryWindow != 24*time.Hour || config.MinHistoryCostUSD != 0.01 || config.DryRun {
		t.Fatalf("unexpected defaults: %+v", config)
	}
	discount, host, err := config.rechargeDiscountForBaseURL("https://lucen.cc/v1")
	if err != nil || discount != 0.85 || host != "lucen.cc" {
		t.Fatalf("rechargeDiscountForBaseURL() = %v, %q, %v", discount, host, err)
	}
	discount, _, err = config.rechargeDiscountForBaseURL("https://new-upstream.test")
	if err != nil || discount != 1 {
		t.Fatalf("default recharge discount = %v, %v", discount, err)
	}
}

func TestLoadConfigUsesRechargeDiscountMap(t *testing.T) {
	path := writeTestConfig(t, `{
  "sync_target":"account",
  "recharge_discounts":{
    "WWW.CODEXAPIS.COM.":1,
    "xixiapi.io":0.9,
    "PPSUBAPI.COM":0.9
  }
}`)

	config, err := loadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(config.RechargeDiscounts) != 3 {
		t.Fatalf("unexpected recharge discount config: %+v", config)
	}
	if discount, host, err := config.rechargeDiscountForBaseURL("https://www.codexapis.com/v1"); err != nil || discount != 1 || host != "www.codexapis.com" {
		t.Fatalf("DaoGe rechargeDiscountForBaseURL() = %v, %q, %v", discount, host, err)
	}
	if discount, host, err := config.rechargeDiscountForBaseURL("https://ppsubapi.com"); err != nil || discount != 0.9 || host != "ppsubapi.com" {
		t.Fatalf("TokenHorse rechargeDiscountForBaseURL() = %v, %q, %v", discount, host, err)
	}
}

func TestLoadConfigEmptyRechargeDiscountsDefaultToOne(t *testing.T) {
	config, err := loadConfig(writeTestConfig(t, `{"sync_target":"account","recharge_discounts":{}}`))
	if err != nil {
		t.Fatal(err)
	}
	if discount, host, err := config.rechargeDiscountForBaseURL("https://xinghubai.top/v1"); err != nil || discount != 1 || host != "xinghubai.top" {
		t.Fatalf("unlisted host rechargeDiscountForBaseURL() = %v, %q, %v", discount, host, err)
	}
}

func TestLoadConfigAllowsOptionalRuntimeSettings(t *testing.T) {
	path := writeTestConfig(t, `{
	  "sync_target":"account",
	  "recharge_discounts":{"ppsubapi.com":0.9},
	  "proxy_url":"http://host.docker.internal:7897",
	  "proxy_fallback_urls":["http://host.docker.internal:7890"],
	  "interval":"90s",
  "dry_run":true
}`)
	config, err := loadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if config.ProxyURL != "http://host.docker.internal:7897" || len(config.ProxyFallbackURLs) != 1 || config.ProxyFallbackURLs[0] != "http://host.docker.internal:7890" || config.Interval != 90*time.Second || !config.DryRun || config.SyncTarget != "account" {
		t.Fatalf("unexpected config: %+v", config)
	}
	if discount, host, err := config.rechargeDiscountForBaseURL("https://ppsubapi.com"); err != nil || discount != 0.9 || host != "ppsubapi.com" {
		t.Fatalf("rechargeDiscountForBaseURL() = %v, %q, %v", discount, host, err)
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

func TestLoadConfigRejectsInvalidRechargeDiscount(t *testing.T) {
	for _, input := range []string{
		`{"recharge_discounts":{"https://lucen.cc":0.85}}`,
		`{"recharge_discounts":{"lucen.cc":0}}`,
		`{"recharge_discounts":{"lucen.cc/path":0.85}}`,
	} {
		if _, err := loadConfig(writeTestConfig(t, input)); err == nil {
			t.Fatalf("loadConfig(%s) error = nil", input)
		}
	}
}

func TestLoadConfigRejectsRemovedConfigurationFields(t *testing.T) {
	for _, input := range []string{
		`{"upstream_factors":{"lucen.cc":0.9}}`,
		`{"factors":{"lucen.cc":0.9}}`,
		`{"sync_hosts":["lucen.cc"]}`,
		`{"usage_bootstrap":true}`,
		`{"confirmations":2}`,
	} {
		if _, err := loadConfig(writeTestConfig(t, input)); err == nil || !strings.Contains(err.Error(), "unknown field") {
			t.Fatalf("loadConfig(%s) error = %v, want removed-field error", input, err)
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
