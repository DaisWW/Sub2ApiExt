package notify

import (
	"fmt"
	"mime"
	"strconv"
	"strings"
	"time"

	"github.com/DaisWW/Sub2ApiExt/monitoring/internal/config"
	"github.com/DaisWW/Sub2ApiExt/monitoring/internal/model"
)

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
	subject := costAlertSubject(events)
	body := costAlertBody(events)
	return formatEmail(from, recipients, subject, body), nil
}

func costAlertSubject(events []model.CostAlertEvent) string {
	severity := "告警"
	allRecovery := len(events) > 0
	hasEscalation := false
	hasReminder := false
	for _, event := range events {
		if event.NotificationType != model.CostAlertNotificationRecovery {
			allRecovery = false
		}
		if event.NotificationType == model.CostAlertNotificationEscalation {
			hasEscalation = true
		}
		if event.NotificationType == model.CostAlertNotificationReminder {
			hasReminder = true
		}
		if event.Severity == "critical" {
			severity = "严重告警"
		}
	}
	subjectLabel := fmt.Sprintf("费用%s", severity)
	if allRecovery {
		subjectLabel = "费用恢复"
	} else if hasEscalation {
		subjectLabel = fmt.Sprintf("费用升级-%s", severity)
	} else if hasReminder {
		subjectLabel = fmt.Sprintf("费用持续-%s", severity)
	}
	return mime.QEncoding.Encode("UTF-8", fmt.Sprintf("[Sub2API%s] %d 条异常", subjectLabel, len(events)))
}

func costAlertBody(events []model.CostAlertEvent) string {
	var body strings.Builder
	body.WriteString("Sub2API 费用异常通知\n")
	body.WriteString("以下异常由已完成的 usage_logs 请求数据分析得到。监控只读，不会修改网关路由或账户状态。\n\n")
	for index, event := range events {
		if index > 0 {
			body.WriteString("\n----------------------------------------\n\n")
		}
		writeCostAlertHeader(&body, event)
		writeCostAlertWindow(&body, event)
		writeCostAlertMetrics(&body, event)
		body.WriteString(fmt.Sprintf("说明: %s\n", event.Message))
	}
	body.WriteString("\n建议检查对应用户的客户端、模型/渠道切换、缓存字段和倍率配置。\n")
	return body.String()
}

func writeCostAlertHeader(body *strings.Builder, event model.CostAlertEvent) {
	body.WriteString(fmt.Sprintf("[%s][%s] %s\n", event.Severity, notificationLabel(event.NotificationType), event.Title))
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
}

func writeCostAlertWindow(body *strings.Builder, event model.CostAlertEvent) {
	if !event.WindowStart.IsZero() && !event.WindowEnd.IsZero() {
		body.WriteString(fmt.Sprintf("分析窗口: %s 至 %s\n",
			event.WindowStart.In(time.Local).Format(time.RFC3339), event.WindowEnd.In(time.Local).Format(time.RFC3339)))
	}
	if event.IncidentStartedAt.IsZero() {
		return
	}
	body.WriteString(fmt.Sprintf("首次发现: %s\n", event.IncidentStartedAt.In(time.Local).Format(time.RFC3339)))
	if !event.CreatedAt.IsZero() && event.CreatedAt.After(event.IncidentStartedAt) {
		body.WriteString(fmt.Sprintf("持续时间: %s\n", event.CreatedAt.Sub(event.IncidentStartedAt).Round(time.Second)))
	}
}

func writeCostAlertMetrics(body *strings.Builder, event model.CostAlertEvent) {
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
}

func formatEmail(from string, recipients []string, subject, body string) []byte {
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
	message.WriteString(strings.ReplaceAll(body, "\n", "\r\n"))
	return []byte(message.String())
}

func notificationLabel(value string) string {
	switch value {
	case model.CostAlertNotificationReminder:
		return "持续"
	case model.CostAlertNotificationEscalation:
		return "升级"
	case model.CostAlertNotificationRecovery:
		return "恢复"
	default:
		return "开始"
	}
}
