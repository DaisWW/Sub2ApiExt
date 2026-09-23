package notify

import (
	"strings"
	"testing"
	"time"

	"github.com/DaisWW/Sub2ApiExt/monitoring/internal/config"
	"github.com/DaisWW/Sub2ApiExt/monitoring/internal/model"
)

func TestBuildCostAlertMessageContainsBreakdownWithoutPassword(t *testing.T) {
	password := "smtp-secret"
	message, err := buildCostAlertMessage(config.EmailConfig{
		Username: "sender@qq.com",
		Password: password,
		From:     "sender@qq.com",
		To:       []string{"admin@example.com"},
	}, []model.CostAlertEvent{{
		Severity:             "critical",
		Title:                "实际倍率异常",
		UserKey:              "42",
		UserName:             "Owner",
		UserEmail:            "owner@example.com",
		APIKeyID:             7,
		APIKeyName:           "Codex",
		Model:                "gpt-5.6-sol",
		ChannelName:          "渠道 A",
		AccountName:          "owner@example.com",
		AccountID:            75,
		Requests:             3,
		TotalTokens:          120_000,
		InputTokens:          80_000,
		OutputTokens:         20_000,
		CacheCreationTokens:  10_000,
		CacheReadTokens:      10_000,
		CurrentCost:          2.4,
		InputCost:            1.1,
		OutputCost:           0.3,
		CacheCreationCost:    0.8,
		CacheReadCost:        0.2,
		CurrentUnitCost:      20,
		BaselineUnitCost:     10,
		CurrentMultiplier:    2,
		BaselineMultiplier:   1,
		CurrentCacheHitRate:  5,
		BaselineCacheHitRate: 70,
		Message:              "倍率相对历史基线翻倍",
		CreatedAt:            time.Now(),
	}})
	if err != nil {
		t.Fatal(err)
	}
	text := string(message)
	for _, fragment := range []string{"实际倍率异常", "用户: Owner <owner@example.com> #42", "API Key: Codex #7", "账户: owner@example.com #75", "Tokens 分项: 输入 80000，输出 20000，缓存写入 10000，缓存读取 10000", "缓存写入 0.8000", "倍率相对历史基线翻倍"} {
		if !strings.Contains(text, fragment) {
			t.Fatalf("email is missing %q: %s", fragment, text)
		}
	}
	if strings.Contains(text, password) {
		t.Fatal("email contains SMTP password")
	}
}

func TestBuildCostAlertMessageFallsBackToAccountID(t *testing.T) {
	message, err := buildCostAlertMessage(config.EmailConfig{
		From: "sender@qq.com",
		To:   []string{"admin@example.com"},
	}, []model.CostAlertEvent{{
		Severity:  "warning",
		Title:     "单位成本异常",
		UserKey:   "42",
		AccountID: 75,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(message), "账户: 账户 #75") {
		t.Fatalf("email does not contain account ID fallback: %s", message)
	}
}

func TestBuildCostAlertMessageListsRequestSamples(t *testing.T) {
	message, err := buildCostAlertMessage(config.EmailConfig{
		From: "sender@qq.com",
		To:   []string{"admin@example.com"},
	}, []model.CostAlertEvent{{
		Severity: "critical",
		Title:    "每百万 Tokens 成本异常",
		UserKey:  "7",
		Requests: 2,
		RequestSamples: []model.CostAlertRequest{{
			UsageLogID: 123, CreatedAt: time.Date(2026, 9, 23, 2, 35, 0, 0, time.UTC),
			InputTokens: 100, OutputTokens: 20, CacheReadTokens: 30, TotalTokens: 150,
			BaseCost: 0.2, ActualCost: 0.8, Multiplier: 4, CacheHitRate: 23.1,
			DurationMS: 800, FirstTokenMS: 120,
		}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	text := string(message)
	for _, fragment := range []string{
		"异常请求（当前窗口列出 1/2 条，按成本/倍率排序）:",
		"时间(UTC): 2026-09-23T02:35:00Z",
		"记录 #123",
		"成本 0.8000",
		"倍率 4.00x",
		"首字 120ms，总耗时 800ms",
	} {
		if !strings.Contains(text, fragment) {
			t.Fatalf("email is missing %q: %s", fragment, text)
		}
	}
}

func TestBuildCostAlertMessageListsAccountsForCacheMissRequests(t *testing.T) {
	message, err := buildCostAlertMessage(config.EmailConfig{
		From: "sender@qq.com",
		To:   []string{"admin@example.com"},
	}, []model.CostAlertEvent{{
		Severity: "warning",
		Title:    "请求级缓存命中异常",
		Kind:     model.CostAlertCacheMiss,
		UserKey:  "7",
		Requests: 2,
		RequestSamples: []model.CostAlertRequest{{
			UsageLogID: 123, CreatedAt: time.Date(2026, 9, 23, 2, 35, 0, 0, time.UTC),
			Model: "gpt-5.6-sol", ChannelName: "渠道 A", AccountName: "账户 A", AccountID: 11,
			InputTokens: 100_000, CacheReadTokens: 1, TotalTokens: 100_001,
			ActualCost: 0.8, CacheHitRate: 0.001,
		}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(message), "账户 账户 A #11") {
		t.Fatalf("cache miss email does not contain account identity: %s", message)
	}
}

func TestFormatIdentityKeepsIDAndUsesAvailableNames(t *testing.T) {
	if got := formatIdentity("刘笑冬", "lxd@example.com", "7", "用户"); got != "刘笑冬 <lxd@example.com> #7" {
		t.Fatalf("identity = %q", got)
	}
	if got := formatIdentity("", "", "82", "API Key"); got != "API Key #82" {
		t.Fatalf("fallback identity = %q", got)
	}
}

func TestParseMailboxRejectsHeaderInjection(t *testing.T) {
	if _, err := parseMailbox("admin@example.com\r\nBcc: attacker@example.com"); err == nil {
		t.Fatal("header injection was accepted")
	}
}

func TestEmailSenderRequiresCompleteTransportConfiguration(t *testing.T) {
	base := config.EmailConfig{
		Host: "smtp.qq.com", Port: 465, Security: "implicit_tls",
		Username: "sender@qq.com", Password: "authorization-code",
		From: "sender@qq.com", To: []string{"admin@example.com"}, Timeout: 15 * time.Second,
	}
	if !NewEmailSender(base).Enabled() {
		t.Fatal("complete email configuration should be enabled")
	}
	base.Security = ""
	if NewEmailSender(base).Enabled() {
		t.Fatal("missing transport security should disable email")
	}
}
