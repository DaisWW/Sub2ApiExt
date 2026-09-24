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
		body.WriteString(fmt.Sprintf("用户: %s\n",
			model.FormatIdentity(event.UserName, event.UserEmail, event.UserKey, "用户")))
		if event.APIKeyID > 0 || strings.TrimSpace(event.APIKeyName) != "" {
			body.WriteString(fmt.Sprintf("API Key: %s\n", model.FormatIdentity(
				event.APIKeyName, "", strconv.FormatInt(event.APIKeyID, 10), "API Key")))
		}
		if event.Model != "" {
			body.WriteString(fmt.Sprintf("模型: %s\n", event.Model))
		}
		if event.ChannelName != "" {
			body.WriteString(fmt.Sprintf("渠道: %s\n", event.ChannelName))
		}
		if event.AccountName != "" {
			body.WriteString(fmt.Sprintf("账户: %s\n", event.AccountName))
		}
		if !event.WindowStart.IsZero() && !event.WindowEnd.IsZero() {
			body.WriteString(fmt.Sprintf("分析窗口: %s 至 %s\n",
				event.WindowStart.UTC().Format(time.RFC3339), event.WindowEnd.UTC().Format(time.RFC3339)))
		}
		body.WriteString(fmt.Sprintf("请求数: %d\nTokens: %d\n当前成本: %.4f\n", event.Requests, event.TotalTokens, event.CurrentCost))
		if event.MaxRequestCost > 0 {
			body.WriteString(fmt.Sprintf("最高单条成本: %.4f\n", event.MaxRequestCost))
		}
		body.WriteString(fmt.Sprintf("Tokens 分项: 输入 %d，输出 %d，缓存写入 %d，缓存读取 %d\n",
			event.InputTokens, event.OutputTokens, event.CacheCreationTokens, event.CacheReadTokens))
		if event.InputCost > 0 || event.OutputCost > 0 || event.CacheCreationCost > 0 || event.CacheReadCost > 0 {
			body.WriteString(fmt.Sprintf("成本分项: 输入 %.4f，输出 %.4f，缓存写入 %.4f，缓存读取 %.4f\n",
				event.InputCost, event.OutputCost, event.CacheCreationCost, event.CacheReadCost))
		}
		if event.CurrentUnitCost > 0 || event.BaselineUnitCost > 0 {
			body.WriteString(fmt.Sprintf("单位成本: %.4f（历史 %.4f）\n", event.CurrentUnitCost, event.BaselineUnitCost))
		}
		if event.CurrentCacheHitRate > 0 || event.BaselineCacheHitRate > 0 {
			body.WriteString(fmt.Sprintf("缓存命中率: %.1f%%（历史 %.1f%%）\n", event.CurrentCacheHitRate, event.BaselineCacheHitRate))
		}
		if event.CurrentMultiplier > 0 || event.BaselineMultiplier > 0 {
			body.WriteString(fmt.Sprintf("实际倍率: %.2fx（历史 %.2fx）\n", event.CurrentMultiplier, event.BaselineMultiplier))
		}
		if event.DailyCost > 0 || event.ProjectedCost > 0 {
			body.WriteString(fmt.Sprintf("今日成本: %.4f，预计今日成本: %.4f\n", event.DailyCost, event.ProjectedCost))
		}
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
