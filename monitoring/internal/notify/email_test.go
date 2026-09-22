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
		APIKeyID:             7,
		Model:                "gpt-5.6-sol",
		ChannelName:          "渠道 A",
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
	for _, fragment := range []string{"实际倍率异常", "API Key ID: 7", "Tokens 分项: 输入 80000，输出 20000，缓存写入 10000，缓存读取 10000", "缓存写入 0.8000", "倍率相对历史基线翻倍"} {
		if !strings.Contains(text, fragment) {
			t.Fatalf("email is missing %q: %s", fragment, text)
		}
	}
	if strings.Contains(text, password) {
		t.Fatal("email contains SMTP password")
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
