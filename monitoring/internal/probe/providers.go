package probe

import (
	"net/url"
	"strings"

	"github.com/DaisWW/Sub2ApiExt/monitoring/internal/model"
)

func requestSpecFor(account model.Account, baseURL, token, modelName string) requestSpec {
	platform := strings.ToLower(strings.TrimSpace(account.Platform))
	switch platform {
	case "openai", "openai_compatible", "codex", "grok", "xai":
		return openAIRequestSpec(account, baseURL, token, modelName)
	case "anthropic", "claude":
		return anthropicRequestSpec(account, baseURL, token, modelName)
	case "gemini", "antigravity":
		return geminiRequestSpec(account, baseURL, token, modelName)
	default:
		return requestSpec{
			endpoint: baseURL,
			payload:  map[string]string{"model": modelName, "prompt": "ping"},
			headers:  authenticatedHeaders("Bearer " + token),
		}
	}
}

func openAIRequestSpec(account model.Account, baseURL, token, modelName string) requestSpec {
	headers := authenticatedHeaders("Bearer " + token)
	if isOAuthAccount(account.Type) && strings.EqualFold(strings.TrimSpace(account.Platform), "openai") {
		headers["Accept"] = "text/event-stream"
		if account.ChatGPTAccount != "" {
			headers["chatgpt-account-id"] = account.ChatGPTAccount
		}
		return requestSpec{
			endpoint: openAIOAuthEndpoint,
			payload: map[string]any{
				"model":        modelName,
				"instructions": "Reply with exactly pong.",
				"input": []map[string]any{{
					"role":    "user",
					"content": []map[string]string{{"type": "input_text", "text": "ping"}},
				}},
				"stream": true,
				"store":  false,
			},
			headers: headers,
		}
	}
	return requestSpec{
		endpoint: appendEndpoint(baseURL, "/v1/chat/completions"),
		payload: map[string]any{
			"model":      modelName,
			"messages":   []map[string]string{{"role": "user", "content": "ping"}},
			"max_tokens": 1,
			"stream":     false,
		},
		headers: headers,
	}
}

func anthropicRequestSpec(account model.Account, baseURL, token, modelName string) requestSpec {
	headers := jsonHeaders()
	spec := requestSpec{
		endpoint: appendEndpoint(baseURL, "/v1/messages"),
		payload: map[string]any{
			"model":      modelName,
			"max_tokens": 1,
			"messages":   []map[string]string{{"role": "user", "content": "ping"}},
			"stream":     isOAuthAccount(account.Type),
		},
		headers: headers,
	}
	if isAPIKeyAccount(account.Type) {
		headers["x-api-key"] = token
	} else {
		spec.endpoint = anthropicOAuthEndpoint
		headers["Authorization"] = "Bearer " + token
		headers["Accept"] = "text/event-stream"
		headers["anthropic-beta"] = "claude-code-20250219,oauth-2025-04-20,interleaved-thinking-2025-05-14,fine-grained-tool-streaming-2025-05-14"
		headers["User-Agent"] = "claude-cli/2.1.161 (external, cli)"
		headers["X-App"] = "cli"
		headers["Anthropic-Dangerous-Direct-Browser-Access"] = "true"
	}
	headers["anthropic-version"] = "2023-06-01"
	return spec
}

func geminiRequestSpec(account model.Account, baseURL, token, modelName string) requestSpec {
	headers := jsonHeaders()
	if isAPIKeyAccount(account.Type) {
		headers["x-goog-api-key"] = token
	} else {
		headers["Authorization"] = "Bearer " + token
	}
	return requestSpec{
		endpoint: appendEndpoint(baseURL, "/v1beta/models/"+url.PathEscape(modelName)+":generateContent"),
		payload:  map[string]any{"contents": []map[string]any{{"parts": []map[string]string{{"text": "ping"}}}}},
		headers:  headers,
	}
}

func jsonHeaders() map[string]string {
	return map[string]string{"Content-Type": "application/json", "Accept": "application/json"}
}

func authenticatedHeaders(authorization string) map[string]string {
	headers := jsonHeaders()
	headers["Authorization"] = authorization
	return headers
}

func isAPIKeyAccount(accountType string) bool {
	switch strings.ToLower(strings.TrimSpace(accountType)) {
	case "", "api_key", "apikey":
		return true
	default:
		return false
	}
}

func isOAuthAccount(accountType string) bool {
	switch strings.ToLower(strings.TrimSpace(accountType)) {
	case "oauth", "setup-token", "setup_token":
		return true
	default:
		return false
	}
}

// SupportsAccount 是主动探测协议的统一准入判断。不支持的账户仍可展示真实请求历史。
func SupportsAccount(account model.Account) bool {
	platform := strings.ToLower(strings.TrimSpace(account.Platform))
	switch platform {
	case "openai":
		return isAPIKeyAccount(account.Type) || isOAuthAccount(account.Type)
	case "openai_compatible", "codex", "grok", "xai":
		return isAPIKeyAccount(account.Type)
	case "anthropic", "claude":
		return isAPIKeyAccount(account.Type) || isOAuthAccount(account.Type)
	case "gemini":
		if isAPIKeyAccount(account.Type) {
			return true
		}
		return isOAuthAccount(account.Type) && credentialString(account.Credentials, "project_id") == ""
	case "antigravity":
		return isAPIKeyAccount(account.Type) && credentialString(account.Credentials, "base_url", "endpoint") != ""
	default:
		return false
	}
}

func accountBaseURL(account model.Account) string {
	baseURL := credentialString(account.Credentials, "base_url", "endpoint")
	if baseURL == "" {
		return defaultBaseURL(account.Platform)
	}
	if strings.EqualFold(strings.TrimSpace(account.Platform), "antigravity") && isAPIKeyAccount(account.Type) {
		return strings.TrimRight(baseURL, "/") + "/antigravity"
	}
	return baseURL
}

func defaultBaseURL(platform string) string {
	switch strings.ToLower(platform) {
	case "anthropic", "claude":
		return "https://api.anthropic.com"
	case "gemini", "antigravity":
		return "https://generativelanguage.googleapis.com"
	case "grok", "xai":
		return "https://api.x.ai"
	default:
		return "https://api.openai.com"
	}
}

func credentialString(credentials map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, ok := credentials[key]; ok {
			if typed, ok := value.(string); ok {
				if value := strings.TrimSpace(typed); value != "" {
					return value
				}
			}
		}
	}
	return ""
}
