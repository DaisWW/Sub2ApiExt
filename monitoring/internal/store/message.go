package store

import (
	"regexp"
	"strings"
)

var httpStatusPattern = regexp.MustCompile(`(?i)\bHTTP\s+([1-5][0-9]{2})\b`)
var groupHealthPattern = regexp.MustCompile(`\b([0-9]+)\s*/\s*([0-9]+)\s+accounts?\s+healthy\b`)
var groupRoutePattern = regexp.MustCompile(`^当前路由可用：([0-9]+)\s*/\s*([0-9]+)\s+accounts?\s+healthy；异常\s*([0-9]+)，待验证\s*([0-9]+)；预计失败暴露\s*([0-9]+(?:\.[0-9]+)?)%$`)

// sanitizeUpstreamMessage 将新旧告警统一成简短、可行动且不含上游细节的文案。
func sanitizeUpstreamMessage(message string) string {
	text := strings.TrimSpace(message)
	if text == "" {
		return text
	}
	prefix, body := splitUnavailablePrefix(text)
	return prefix + sanitizeMessageBody(body)
}

func splitUnavailablePrefix(message string) (string, string) {
	for _, marker := range []string{"不可用：", "不可用: ", "不可用:"} {
		if index := strings.Index(message, marker); index >= 0 {
			end := index + len(marker)
			return message[:end], message[end:]
		}
	}
	return "", message
}

func sanitizeMessageBody(message string) string {
	body := strings.TrimSpace(message)
	if match := httpStatusPattern.FindStringSubmatch(body); len(match) == 2 {
		return "HTTP " + match[1]
	}
	if match := groupRoutePattern.FindStringSubmatch(body); len(match) == 6 {
		return "当前仍可用：" + match[1] + "/" + match[2] + " 个候选；异常 " + match[3] + "，待验证 " + match[4] + "；预计失败暴露 " + match[5] + "%"
	}
	lower := strings.ToLower(body)
	switch {
	case strings.Contains(lower, "context deadline exceeded"),
		strings.Contains(lower, "client.timeout"), strings.Contains(lower, "timed out"),
		strings.Contains(lower, "timeout"):
		return "上游请求超时"
	case strings.Contains(lower, "connection refused"):
		return "上游连接被拒绝"
	case strings.Contains(lower, "no such host"), strings.Contains(lower, "name resolution"),
		strings.Contains(lower, "dns"):
		return "上游域名无法解析"
	case groupHealthPattern.MatchString(lower):
		match := groupHealthPattern.FindStringSubmatch(lower)
		return "分组内健康账户 " + match[1] + "/" + match[2]
	case strings.Contains(lower, "service unavailable"),
		strings.Contains(lower, "request failed"), strings.Contains(lower, "upstream"),
		strings.Contains(lower, "network error"):
		return "上游请求失败"
	case strings.Contains(lower, "://"), strings.Contains(body, `"`):
		return "上游请求失败"
	default:
		return body
	}
}
