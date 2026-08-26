package probe

import (
	"sort"
	"strings"

	"github.com/DaisWW/Sub2ApiExt/monitoring/internal/model"
)

// selectModel 优先使用显式监控模型，其次复用近期成功模型和稳定映射目标，
// 最后才回退到平台默认值。
func selectModel(account model.Account, defaultModel string) string {
	if explicit := credentialString(account.Credentials, "monitor_model", "model"); explicit != "" {
		return explicit
	}
	if recent := strings.TrimSpace(account.RecentModel); recent != "" {
		return mapModel(account.Credentials, recent)
	}
	if mapped := firstMappedModel(account.Credentials); mapped != "" {
		return mapped
	}
	platform := strings.ToLower(strings.TrimSpace(account.Platform))
	switch platform {
	case "anthropic", "claude":
		return "claude-sonnet-4-5-20250929"
	case "gemini", "antigravity":
		return "gemini-2.0-flash"
	case "grok", "xai":
		return "grok-4.5"
	default:
		if platform == "openai" && isOAuthAccount(account.Type) {
			return "gpt-5.4"
		}
		if fallback := strings.TrimSpace(defaultModel); fallback != "" {
			return fallback
		}
		return "ping"
	}
}

func mapModel(credentials map[string]any, requested string) string {
	mapping := stringMapping(credentials)
	if len(mapping) == 0 {
		return requested
	}
	if target, ok := mapping[requested]; ok && strings.TrimSpace(target) != "" {
		return strings.TrimSpace(target)
	}
	bestPattern := ""
	for pattern := range mapping {
		if !wildcardMatch(pattern, requested) {
			continue
		}
		if len(pattern) > len(bestPattern) || (len(pattern) == len(bestPattern) && pattern < bestPattern) {
			bestPattern = pattern
		}
	}
	if bestPattern != "" && strings.TrimSpace(mapping[bestPattern]) != "" {
		return strings.TrimSpace(mapping[bestPattern])
	}
	return requested
}

func firstMappedModel(credentials map[string]any) string {
	mapping := stringMapping(credentials)
	if len(mapping) == 0 {
		return ""
	}
	keys := make([]string, 0, len(mapping))
	for key := range mapping {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if target := strings.TrimSpace(mapping[key]); target != "" && target != "*" {
			return target
		}
	}
	return ""
}

func stringMapping(credentials map[string]any) map[string]string {
	if credentials == nil {
		return nil
	}
	raw, ok := credentials["model_mapping"]
	if !ok {
		return nil
	}
	mapping := make(map[string]string)
	switch typed := raw.(type) {
	case map[string]any:
		for key, value := range typed {
			if target, ok := value.(string); ok {
				mapping[strings.TrimSpace(key)] = target
			}
		}
	case map[string]string:
		for key, target := range typed {
			mapping[strings.TrimSpace(key)] = target
		}
	}
	return mapping
}

func wildcardMatch(pattern, value string) bool {
	if strings.HasSuffix(pattern, "*") {
		return strings.HasPrefix(value, strings.TrimSuffix(pattern, "*"))
	}
	return pattern == value
}
