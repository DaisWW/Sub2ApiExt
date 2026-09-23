package notify

import (
	"context"
	"crypto/tls"
	"fmt"
	"mime"
	"net"
	"net/mail"
	"net/smtp"
	"strconv"
	"strings"
	"time"

	"github.com/DaisWW/Sub2ApiExt/monitoring/internal/config"
	"github.com/DaisWW/Sub2ApiExt/monitoring/internal/model"
)

type EmailSender struct {
	config config.EmailConfig
}

func NewEmailSender(cfg config.EmailConfig) *EmailSender {
	return &EmailSender{config: cfg}
}

func (s *EmailSender) Enabled() bool {
	if s == nil {
		return false
	}
	return strings.TrimSpace(s.config.Host) != "" &&
		s.config.Port > 0 &&
		(s.config.Security == "implicit_tls" || s.config.Security == "starttls") &&
		s.config.Timeout > 0 &&
		strings.TrimSpace(s.config.Username) != "" &&
		s.config.Password != "" &&
		strings.TrimSpace(s.config.From) != "" &&
		len(s.config.To) > 0
}

func (s *EmailSender) SendCostAlerts(ctx context.Context, events []model.CostAlertEvent) error {
	if len(events) == 0 || !s.Enabled() {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	message, err := buildCostAlertMessage(s.config, events)
	if err != nil {
		return err
	}
	return s.send(ctx, message)
}

func buildCostAlertMessage(cfg config.EmailConfig, events []model.CostAlertEvent) ([]byte, error) {
	from, err := parseMailbox(cfg.From)
	if err != nil {
		return nil, fmt.Errorf("invalid email sender: %w", err)
	}
	recipients := make([]string, 0, len(cfg.To))
	for _, value := range cfg.To {
		address, err := parseMailbox(value)
		if err != nil {
			return nil, fmt.Errorf("invalid email recipient: %w", err)
		}
		recipients = append(recipients, address)
	}
	severity := "告警"
	for _, event := range events {
		if event.Severity == "critical" {
			severity = "严重告警"
			break
		}
	}
	subject := mime.QEncoding.Encode("UTF-8", fmt.Sprintf("[Sub2API费用%s] %d 条异常", severity, len(events)))
	var body strings.Builder
	body.WriteString("Sub2API 费用异常通知\n")
	body.WriteString("以下异常由已完成的 usage_logs 请求数据分析得到。监控只读，不会修改网关路由或账户状态。\n\n")
	for index, event := range events {
		if index > 0 {
			body.WriteString("\n----------------------------------------\n\n")
		}
		body.WriteString(fmt.Sprintf("[%s] %s\n", event.Severity, event.Title))
		body.WriteString(fmt.Sprintf("用户: %s\n", formatIdentity(event.UserName, event.UserEmail, event.UserKey, "用户")))
		if event.APIKeyID > 0 || strings.TrimSpace(event.APIKeyName) != "" {
			body.WriteString(fmt.Sprintf("API Key: %s\n", formatIdentity(event.APIKeyName, "", strconv.FormatInt(event.APIKeyID, 10), "API Key")))
		}
		if event.Model != "" {
			body.WriteString(fmt.Sprintf("模型: %s\n", event.Model))
		}
		if event.ChannelName != "" {
			body.WriteString(fmt.Sprintf("渠道: %s\n", event.ChannelName))
		}
		if event.AccountID > 0 {
			body.WriteString(fmt.Sprintf("账户: %s\n", formatIdentity(event.AccountName, "", strconv.FormatInt(event.AccountID, 10), "账户")))
		} else if accountName := strings.TrimSpace(event.AccountName); accountName != "" {
			body.WriteString(fmt.Sprintf("账户: %s\n", cleanText(accountName)))
		}
		if !event.WindowStart.IsZero() && !event.WindowEnd.IsZero() {
			body.WriteString(fmt.Sprintf("分析窗口: %s 至 %s\n",
				event.WindowStart.UTC().Format(time.RFC3339), event.WindowEnd.UTC().Format(time.RFC3339)))
		}
		requestCountLabel := "请求数"
		if event.Kind == model.CostAlertAccountSwitch {
			requestCountLabel = "涉及请求数"
		}
		body.WriteString(fmt.Sprintf("%s: %d\nTokens: %d\n当前成本: %.4f\n", requestCountLabel, event.Requests, event.TotalTokens, event.CurrentCost))
		if event.Kind == model.CostAlertAccountSwitch {
			body.WriteString(fmt.Sprintf("账户切换次数: %d（涉及 session: %d）\n", event.AccountSwitches, event.AccountSwitchSessions))
		}
		if event.MaxRequestCost > 0 {
			body.WriteString(fmt.Sprintf("最高单条成本: %.4f\n", event.MaxRequestCost))
		}
		body.WriteString(fmt.Sprintf("Tokens 分项: 输入 %d，输出 %d，缓存写入 %d，缓存读取 %d\n",
			event.InputTokens, event.OutputTokens, event.CacheCreationTokens, event.CacheReadTokens))
		if event.InputCost > 0 || event.OutputCost > 0 || event.CacheCreationCost > 0 || event.CacheReadCost > 0 {
			body.WriteString(fmt.Sprintf("成本分项: 输入 %.4f，输出 %.4f，缓存写入 %.4f，缓存读取 %.4f\n",
				event.InputCost, event.OutputCost, event.CacheCreationCost, event.CacheReadCost))
		}
		requestLevel := event.Kind == model.CostAlertCacheMiss || event.Kind == model.CostAlertAccountSwitch
		if event.CurrentUnitCost > 0 || event.BaselineUnitCost > 0 {
			if requestLevel {
				body.WriteString(fmt.Sprintf("单位成本: %.4f（本规则未使用历史基线）\n", event.CurrentUnitCost))
			} else {
				body.WriteString(fmt.Sprintf("单位成本: %.4f（历史 %.4f）\n", event.CurrentUnitCost, event.BaselineUnitCost))
			}
		}
		if event.CurrentCacheHitRate > 0 || event.BaselineCacheHitRate > 0 {
			if requestLevel {
				body.WriteString(fmt.Sprintf("缓存命中率: %.1f%%（本规则未使用历史基线）\n", event.CurrentCacheHitRate))
			} else {
				body.WriteString(fmt.Sprintf("缓存命中率: %.1f%%（历史 %.1f%%）\n", event.CurrentCacheHitRate, event.BaselineCacheHitRate))
			}
		}
		if event.CurrentMultiplier > 0 || event.BaselineMultiplier > 0 {
			if requestLevel {
				body.WriteString(fmt.Sprintf("实际倍率: %.2fx（本规则未使用历史基线）\n", event.CurrentMultiplier))
			} else {
				body.WriteString(fmt.Sprintf("实际倍率: %.2fx（历史 %.2fx）\n", event.CurrentMultiplier, event.BaselineMultiplier))
			}
		}
		if event.DailyCost > 0 || event.ProjectedCost > 0 {
			body.WriteString(fmt.Sprintf("今日成本: %.4f，预计今日成本: %.4f\n", event.DailyCost, event.ProjectedCost))
		}
		writeRequestSamples(&body, event)
		body.WriteString(fmt.Sprintf("说明: %s\n", event.Message))
	}
	body.WriteString("\n建议检查对应用户的客户端、模型/渠道切换、缓存字段和倍率配置。\n")

	var message strings.Builder
	message.WriteString("From: ")
	message.WriteString(from)
	message.WriteString("\r\nTo: ")
	message.WriteString(strings.Join(recipients, ", "))
	message.WriteString("\r\nSubject: ")
	message.WriteString(subject)
	message.WriteString("\r\nDate: ")
	message.WriteString(time.Now().Format(time.RFC1123Z))
	message.WriteString("\r\nMIME-Version: 1.0\r\nContent-Type: text/plain; charset=UTF-8\r\nContent-Transfer-Encoding: 8bit\r\n\r\n")
	message.WriteString(strings.ReplaceAll(body.String(), "\n", "\r\n"))
	return []byte(message.String()), nil
}

func formatIdentity(name, email, id, fallback string) string {
	name = cleanText(name)
	email = cleanText(email)
	id = cleanText(id)
	if name == "未归属账户" || name == "未知账户" || name == "unknown" {
		name = ""
	}
	label := name
	if email != "" && !strings.EqualFold(email, name) {
		if label == "" {
			label = email
		} else {
			label += " <" + email + ">"
		}
	}
	if label == "" {
		label = fallback
	}
	if id != "" && id != "0" && id != "unknown" {
		label += " #" + id
	}
	return label
}

func cleanText(value string) string {
	return strings.Join(strings.Fields(strings.TrimSpace(value)), " ")
}

func writeRequestSamples(body *strings.Builder, event model.CostAlertEvent) {
	if len(event.RequestSamples) == 0 {
		if event.Requests > 0 {
			body.WriteString("异常请求: 当前窗口没有可列出的请求明细\n")
		}
		return
	}
	total := event.Requests
	if total < int64(len(event.RequestSamples)) {
		total = int64(len(event.RequestSamples))
	}
	body.WriteString(fmt.Sprintf("异常请求（当前窗口列出 %d/%d 条，按成本/倍率排序）:\n",
		len(event.RequestSamples), total))
	for _, request := range event.RequestSamples {
		body.WriteString(fmt.Sprintf("- 时间(UTC): %s，记录 #%d，成本 %.4f，Tokens %d",
			request.CreatedAt.UTC().Format(time.RFC3339), request.UsageLogID,
			request.ActualCost, request.TotalTokens))
		body.WriteString(fmt.Sprintf("，输入 %d，输出 %d，缓存写入 %d，缓存读取 %d",
			request.InputTokens, request.OutputTokens, request.CacheCreationTokens, request.CacheReadTokens))
		if request.BaseCost > 0 {
			body.WriteString(fmt.Sprintf("，原始成本 %.4f，倍率 %.2fx", request.BaseCost, request.Multiplier))
		}
		if request.CacheReadTokens > 0 || request.CacheCreationTokens > 0 || event.Kind == model.CostAlertCacheMiss {
			body.WriteString(fmt.Sprintf("，缓存命中率 %.1f%%", request.CacheHitRate))
		}
		if request.FirstTokenMS > 0 || request.DurationMS > 0 {
			body.WriteString(fmt.Sprintf("，首字 %dms，总耗时 %dms", request.FirstTokenMS, request.DurationMS))
		}
		if event.Kind == model.CostAlertAccountSwitch {
			previous := formatIdentity(request.PreviousAccountName, "", strconv.FormatInt(request.PreviousAccountID, 10), "账户")
			current := formatIdentity(request.AccountName, "", strconv.FormatInt(request.AccountID, 10), "账户")
			body.WriteString(fmt.Sprintf("，模型 %s，渠道 %s，账户切换 %s -> %s",
				cleanText(request.Model), cleanText(request.ChannelName), previous, current))
		} else if event.Kind == model.CostAlertBudgetBurn || event.Kind == model.CostAlertCacheMiss {
			account := strings.TrimSpace(request.AccountName)
			if request.AccountID > 0 {
				account = formatIdentity(request.AccountName, "", strconv.FormatInt(request.AccountID, 10), "账户")
			}
			body.WriteString(fmt.Sprintf("，模型 %s，渠道 %s，账户 %s",
				cleanText(request.Model), cleanText(request.ChannelName),
				account))
		}
		body.WriteString("\n")
	}
}

func parseMailbox(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" || strings.ContainsAny(value, "\r\n") {
		return "", fmt.Errorf("mailbox is empty or contains a newline")
	}
	address, err := mail.ParseAddress(value)
	if err != nil || address.Address == "" {
		return "", fmt.Errorf("invalid mailbox %q", value)
	}
	return address.Address, nil
}

func (s *EmailSender) send(ctx context.Context, message []byte) error {
	address := net.JoinHostPort(s.config.Host, strconv.Itoa(s.config.Port))
	dialer := &net.Dialer{Timeout: s.config.Timeout}
	conn, err := dialer.DialContext(ctx, "tcp", address)
	if err != nil {
		return fmt.Errorf("dial smtp server: %w", err)
	}
	deadline := time.Now().Add(s.config.Timeout)
	_ = conn.SetDeadline(deadline)
	defer conn.Close()

	var client *smtp.Client
	if s.config.Security == "implicit_tls" {
		tlsConn := tls.Client(conn, &tls.Config{ServerName: s.config.Host, MinVersion: tls.VersionTLS12})
		if err := tlsConn.HandshakeContext(ctx); err != nil {
			return fmt.Errorf("smtp TLS handshake: %w", err)
		}
		client, err = smtp.NewClient(tlsConn, s.config.Host)
	} else {
		client, err = smtp.NewClient(conn, s.config.Host)
		if err == nil {
			if ok, _ := client.Extension("STARTTLS"); !ok {
				_ = client.Close()
				return fmt.Errorf("smtp server does not support STARTTLS")
			}
			err = client.StartTLS(&tls.Config{ServerName: s.config.Host, MinVersion: tls.VersionTLS12})
		}
	}
	if err != nil {
		return fmt.Errorf("open smtp client: %w", err)
	}
	defer client.Close()
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := client.Auth(smtp.PlainAuth("", s.config.Username, s.config.Password, s.config.Host)); err != nil {
		// The server reply is untrusted and may echo the authorization code.
		return fmt.Errorf("smtp authentication failed; check the SMTP authorization code")
	}
	from, err := parseMailbox(s.config.From)
	if err != nil {
		return err
	}
	if err := client.Mail(from); err != nil {
		return fmt.Errorf("smtp MAIL FROM failed: %w", err)
	}
	for _, recipient := range s.config.To {
		address, err := parseMailbox(recipient)
		if err != nil {
			return err
		}
		if err := client.Rcpt(address); err != nil {
			return fmt.Errorf("smtp RCPT TO failed: %w", err)
		}
	}
	writer, err := client.Data()
	if err != nil {
		return fmt.Errorf("smtp DATA failed: %w", err)
	}
	if _, err := writer.Write(message); err != nil {
		_ = writer.Close()
		return fmt.Errorf("write email body: %w", err)
	}
	if err := writer.Close(); err != nil {
		return fmt.Errorf("close email body: %w", err)
	}
	if err := client.Quit(); err != nil {
		return fmt.Errorf("finish smtp session: %w", err)
	}
	return nil
}
