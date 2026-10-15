package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"path"
	"sort"
	"strings"
	"time"

	xproxy "golang.org/x/net/proxy"
)

type Evaluator struct{ allowPrivate bool }

func credentialString(c map[string]any, keys ...string) string {
	for _, key := range keys {
		if v, ok := c[key].(string); ok && strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func mappedModel(c map[string]any, model string) string {
	m, _ := c["model_mapping"].(map[string]any)
	if v, ok := m[model].(string); ok && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	keys := []string{}
	for k := range m {
		if strings.Contains(k, "*") {
			keys = append(keys, k)
		}
	}
	sort.Slice(keys, func(i, j int) bool {
		if len(keys[i]) == len(keys[j]) {
			return keys[i] < keys[j]
		}
		return len(keys[i]) > len(keys[j])
	})
	for _, k := range keys {
		if ok, _ := path.Match(k, model); ok {
			if v, ok := m[k].(string); ok && strings.TrimSpace(v) != "" {
				return strings.TrimSpace(v)
			}
		}
	}
	return model
}

func endpoint(base, suffix string) (string, error) {
	u, err := url.Parse(strings.TrimRight(base, "/"))
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("账户上游地址无效")
	}
	if strings.HasSuffix(u.Path, suffix) {
		return u.String(), nil
	}
	if strings.HasSuffix(u.Path, "/v1") && strings.HasPrefix(suffix, "/v1/") {
		suffix = strings.TrimPrefix(suffix, "/v1")
	}
	if strings.HasSuffix(u.Path, "/v1beta") && strings.HasPrefix(suffix, "/v1beta/") {
		suffix = strings.TrimPrefix(suffix, "/v1beta")
	}
	u.Path += suffix
	return u.String(), nil
}

func buildRequest(ctx context.Context, a CredentialAccount, s Settings, prompt string) (*http.Request, string, string, error) {
	if reason := supportReason(a.Platform, a.Type); reason != "" {
		return nil, "", "", fmt.Errorf("%s", reason)
	}
	if a.ProxyError != "" {
		return nil, "", "", fmt.Errorf("%s", a.ProxyError)
	}
	oauth := a.Type == "oauth" || a.Type == "setup-token" || a.Type == "setup_token"
	if oauth && a.Platform == "openai" {
		mode := strings.ToLower(credentialString(a.Credentials, "auth_mode", "openai_auth_mode"))
		if mode != "" && mode != "oauth" {
			return nil, "", "", fmt.Errorf("暂不支持此 OpenAI 认证模式")
		}
	}
	token := credentialString(a.Credentials, "api_key", "key")
	if oauth {
		token = credentialString(a.Credentials, "access_token", "token")
	}
	if token == "" {
		return nil, "", "", fmt.Errorf("账户缺少可用凭据")
	}
	model := mappedModel(a.Credentials, strings.TrimSpace(s.Models[a.Platform]))
	if model == "" {
		return nil, "", "", fmt.Errorf("账户平台已变化，请重新选择模型")
	}
	base := credentialString(a.Credentials, "base_url", "endpoint")
	protocol := ""
	var payload map[string]any
	headers := map[string]string{"Content-Type": "application/json", "Accept": "text/event-stream", "User-Agent": "Sub2ApiExt-account-eval/1.0"}
	var target string
	var err error
	switch a.Platform {
	case "openai", "openai_compatible", "grok", "xai":
		if base == "" {
			base = "https://api.openai.com"
			if a.Platform == "grok" || a.Platform == "xai" {
				base = "https://api.x.ai"
			}
		}
		protocol = s.Protocols[a.Platform]
		if protocol == "" || protocol == "auto" {
			protocol = "responses"
			if a.Platform != "openai" || credentialString(a.Credentials, "api_protocol") == "chat_completions" {
				protocol = "chat"
			}
			if a.Platform == "openai" {
				switch str(a.Extra["openai_responses_mode"]) {
				case "force_chat_completions":
					protocol = "chat"
				case "force_responses":
					protocol = "responses"
				default:
					if a.Extra["openai_responses_supported"] == false {
						protocol = "chat"
					}
				}
			}
		}
		if oauth {
			protocol = "responses"
		}
		headers["Authorization"] = "Bearer " + token
		if protocol == "chat" {
			target, err = endpoint(base, "/v1/chat/completions")
			payload = map[string]any{"model": model, "messages": []map[string]string{{"role": "user", "content": prompt}}, "stream": true, "stream_options": map[string]bool{"include_usage": true}}
			if s.MaxTokens > 0 {
				if a.Platform == "openai" {
					payload["max_completion_tokens"] = s.MaxTokens
				} else {
					payload["max_tokens"] = s.MaxTokens
				}
			}
			if s.Reasoning != "" && (a.Platform == "openai" || a.Platform == "openai_compatible") {
				payload["reasoning_effort"] = s.Reasoning
			}
		} else {
			target, err = endpoint(base, "/v1/responses")
			payload = map[string]any{"model": model, "input": []map[string]any{{"role": "user", "content": []map[string]string{{"type": "input_text", "text": prompt}}}}, "stream": true, "store": false}
			if s.MaxTokens > 0 {
				payload["max_output_tokens"] = s.MaxTokens
			}
			if s.Reasoning != "" && (a.Platform == "openai" || a.Platform == "openai_compatible") {
				payload["reasoning"] = map[string]string{"effort": s.Reasoning}
			}
			if oauth {
				target = "https://chatgpt.com/backend-api/codex/responses"
				// The Codex OAuth endpoint rejects max_output_tokens.
				if s.MaxTokens > 0 {
					return nil, model, protocol, fmt.Errorf("Codex OAuth 不支持输出 Token 上限，请设为 0")
				}
				payload["instructions"] = "You are a helpful assistant."
				headers["OpenAI-Beta"] = "responses=experimental"
				headers["Originator"] = "codex_cli_rs"
				headers["User-Agent"] = "codex_cli_rs/0.134.0"
				if ua := credentialString(a.Credentials, "user_agent"); ua != "" {
					headers["User-Agent"] = ua
				}
				if id := credentialString(a.Credentials, "chatgpt_account_id", "account_id"); id != "" {
					headers["ChatGPT-Account-ID"] = id
				}
			}
		}
	case "anthropic", "claude":
		protocol = "anthropic"
		if base == "" {
			base = "https://api.anthropic.com"
		}
		if oauth {
			base = "https://api.anthropic.com"
			headers["Authorization"] = "Bearer " + token
			headers["anthropic-beta"] = "claude-code-20250219,oauth-2025-04-20,interleaved-thinking-2025-05-14,fine-grained-tool-streaming-2025-05-14"
			headers["User-Agent"] = "claude-cli/2.1.161 (external, cli)"
			headers["x-app"] = "cli"
		} else {
			headers["x-api-key"] = token
		}
		headers["anthropic-version"] = "2023-06-01"
		target, err = endpoint(base, "/v1/messages")
		limit := s.MaxTokens
		if limit == 0 {
			limit = 16384
		}
		payload = map[string]any{"model": model, "max_tokens": limit, "stream": true, "messages": []map[string]string{{"role": "user", "content": prompt}}}
		if oauth {
			payload["system"] = "You are Claude Code, Anthropic's official CLI for Claude."
		}
	case "gemini":
		protocol = "gemini"
		if base == "" {
			base = "https://generativelanguage.googleapis.com"
		}
		target, err = endpoint(base, "/v1beta/models/"+url.PathEscape(model)+":generateContent")
		headers["x-goog-api-key"] = token
		headers["Accept"] = "application/json"
		payload = map[string]any{"contents": []map[string]any{{"role": "user", "parts": []map[string]string{{"text": prompt}}}}}
		if s.MaxTokens > 0 {
			payload["generationConfig"] = map[string]int{"maxOutputTokens": s.MaxTokens}
		}
	}
	if err != nil {
		return nil, model, protocol, err
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, model, protocol, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return nil, model, protocol, fmt.Errorf("无法构造账户请求")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	if overrides, ok := a.Credentials["header_overrides"].(map[string]any); ok {
		for key, value := range overrides {
			if v, ok := value.(string); ok && !strings.ContainsAny(key+v, "\r\n") {
				req.Header.Set(key, v)
			}
		}
	}
	return req, model, protocol, nil
}

func upstreamClient(ctx context.Context, target, proxyRaw string, allowPrivate bool) (*http.Client, func(), error) {
	if err := validateTargetURL(target, allowPrivate); err != nil {
		return nil, nil, fmt.Errorf("上游地址不在允许范围")
	}
	if err := validateResolvedHost(ctx, target, allowPrivate); err != nil {
		return nil, nil, fmt.Errorf("上游 DNS 解析失败或指向私网")
	}
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.Proxy = nil
	t.DialContext = safeDialContext(allowPrivate)
	t.ResponseHeaderTimeout = 45 * time.Second
	if proxyRaw != "" {
		u, err := url.Parse(proxyRaw)
		if err != nil || u.Hostname() == "" || u.Port() == "" || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
			return nil, nil, fmt.Errorf("账户代理地址无效")
		}
		direct := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
		switch u.Scheme {
		case "http", "https":
			t.Proxy = http.ProxyURL(u)
			t.DialContext = direct.DialContext
		case "socks5", "socks5h":
			var auth *xproxy.Auth
			if u.User != nil {
				p, _ := u.User.Password()
				auth = &xproxy.Auth{User: u.User.Username(), Password: p}
			}
			d, err := xproxy.SOCKS5("tcp", u.Host, auth, direct)
			if err != nil {
				return nil, nil, fmt.Errorf("无法使用账户代理")
			}
			cd, ok := d.(xproxy.ContextDialer)
			if !ok {
				return nil, nil, fmt.Errorf("代理不支持取消请求")
			}
			t.DialContext = cd.DialContext
		default:
			return nil, nil, fmt.Errorf("不支持的账户代理协议")
		}
	}
	return &http.Client{Transport: t, CheckRedirect: func(*http.Request, []*http.Request) error { return fmt.Errorf("禁止携带账户凭据重定向") }}, t.CloseIdleConnections, nil
}

func (e *Evaluator) evaluate(ctx context.Context, a CredentialAccount, s Settings, q Question) Result {
	r := Result{Status: "failed"}
	start := time.Now()
	req, model, protocol, err := buildRequest(ctx, a, s, q.Prompt)
	r.Model = model
	if err != nil {
		r.Error = err.Error()
		return r
	}
	client, closeClient, err := upstreamClient(ctx, req.URL.String(), a.ProxyURL, e.allowPrivate)
	if err != nil {
		r.Error = err.Error()
		return r
	}
	defer closeClient()
	resp, err := client.Do(req)
	if err != nil {
		r.Error = "上游连接失败：" + upstreamErrorReason(err)
		if ctx.Err() != nil {
			r.Error = requestContextError(ctx, s.Timeout)
		}
		r.DurationMS = time.Since(start).Milliseconds()
		return r
	}
	defer resp.Body.Close()
	r.FirstByteMS = time.Since(start).Milliseconds()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		r.Error = fmt.Sprintf("上游返回 HTTP %d", resp.StatusCode)
		switch resp.StatusCode {
		case http.StatusTooManyRequests:
			r.Error += "（上游限流，请稍后重试失败项）"
		case http.StatusBadGateway, http.StatusServiceUnavailable:
			r.Error += "（上游服务暂不可用，可重试失败项）"
		case http.StatusGatewayTimeout:
			r.Error += "（上游网关等待超时，可重试失败项）"
		}
		r.DurationMS = time.Since(start).Milliseconds()
		return r
	}
	readUpstreamAnswer(resp, protocol, &r)
	if ctx.Err() != nil && r.Status != "completed" {
		r.Status = "incomplete"
		r.Error = requestContextError(ctx, s.Timeout)
		if r.Answer != "" {
			r.Error += "；已保留收到的部分回答"
		}
	}
	r.DurationMS = time.Since(start).Milliseconds()
	// Never retain an upstream echo of a known secret.
	authorization := req.Header.Get("Authorization")
	secrets := []string{authorization, strings.TrimPrefix(authorization, "Bearer "), req.Header.Get("X-Api-Key"), req.Header.Get("X-Goog-Api-Key"), req.Header.Get("Cookie")}
	for k, v := range a.Credentials {
		k = strings.ToLower(k)
		if strings.Contains(k, "token") || strings.Contains(k, "key") || strings.Contains(k, "secret") || strings.Contains(k, "password") {
			if secret, ok := v.(string); ok {
				secrets = append(secrets, secret)
			}
		}
	}
	if overrides, ok := a.Credentials["header_overrides"].(map[string]any); ok {
		for key := range overrides {
			secrets = append(secrets, req.Header.Get(key))
		}
	}
	for _, cookie := range req.Cookies() {
		secrets = append(secrets, cookie.Value)
	}
	if proxy, err := url.Parse(a.ProxyURL); err == nil && proxy.User != nil {
		if password, ok := proxy.User.Password(); ok {
			secrets = append(secrets, password, proxy.User.String())
		}
	}
	// Redact longer values first so a shorter token cannot expose the suffix of another key.
	sort.Slice(secrets, func(i, j int) bool { return len(secrets[i]) > len(secrets[j]) })
	for _, secret := range secrets {
		if len(secret) >= 4 {
			r.Answer = strings.ReplaceAll(r.Answer, secret, "[REDACTED]")
		}
	}
	if q.Format == "html" {
		r.HTML = extractHTML(r.Answer)
	}
	return r
}

func requestContextError(ctx context.Context, timeout int) string {
	if ctx.Err() == context.DeadlineExceeded {
		if timeout > 0 {
			return fmt.Sprintf("单题超时（%d 秒），可在高级参数中调高超时", timeout)
		}
		return "请求超时"
	}
	return "请求已取消"
}
