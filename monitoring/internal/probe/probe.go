package probe

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/DaisWW/Sub2ApiExt/monitoring/internal/model"
)

type Config struct {
	Timeout          time.Duration
	DefaultModel     string
	AllowPrivateHost bool
}

type Prober struct {
	transport    *http.Transport
	timeout      time.Duration
	defaultModel string
	allowPrivate bool
}

func New(cfg Config) *Prober {
	transport := &http.Transport{
		Proxy:               http.ProxyFromEnvironment,
		TLSClientConfig:     &tls.Config{MinVersion: tls.VersionTLS12},
		MaxIdleConns:        32,
		MaxIdleConnsPerHost: 8,
		IdleConnTimeout:     60 * time.Second,
		DialContext:         safeDialContext(cfg.AllowPrivateHost),
	}
	return &Prober{
		transport:    transport,
		timeout:      cfg.Timeout,
		defaultModel: cfg.DefaultModel,
		allowPrivate: cfg.AllowPrivateHost,
	}
}

func (p *Prober) Probe(ctx context.Context, account model.Account) model.ProbeResult {
	result := model.ProbeResult{
		TargetKey: model.TargetKey(model.KindAccount, account.ID),
		Kind:      model.KindAccount,
		EntityID:  account.ID,
		CheckedAt: time.Now().UTC(),
		Source:    "probe",
	}
	if account.Status != "error" && (account.Status != "active" || !account.Schedulable) {
		result.Status = model.StatusDisabled
		result.ErrorClass = "account_disabled"
		result.Message = "account is not schedulable"
		return result
	}
	if account.ProxyError != "" {
		result.Status = model.StatusError
		result.ErrorClass = "proxy"
		result.Message = account.ProxyError
		return result
	}
	token := credentialString(account.Credentials, "api_key", "access_token", "token", "key", "session_key")
	if token == "" {
		result.Status = model.StatusError
		result.ErrorClass = "missing_credential"
		result.Message = "no supported credential found"
		return result
	}
	baseURL := credentialString(account.Credentials, "base_url", "endpoint")
	if baseURL == "" {
		baseURL = defaultBaseURL(account.Platform)
	}
	if err := ValidateBaseURL(baseURL, p.allowPrivate); err != nil {
		result.Status = model.StatusError
		result.ErrorClass = "configuration"
		result.Message = err.Error()
		return result
	}
	modelName := selectModel(account, p.defaultModel)
	request, err := buildRequest(ctx, account, baseURL, token, modelName)
	if err != nil {
		result.Status = model.StatusError
		result.ErrorClass = "configuration"
		result.Message = err.Error()
		return result
	}
	client, err := p.clientFor(account.ProxyURL)
	if err != nil {
		result.Status = model.StatusError
		result.ErrorClass = "proxy"
		result.Message = err.Error()
		return result
	}
	return p.doRequest(client, request, result)
}

// selectModel follows the same intent as gateway routing: an explicit monitor
// model wins, otherwise reuse a model that recently worked for this account.
// Mapping targets are upstream model IDs, so mapped values are sent directly.
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
		return "claude-3-5-haiku-latest"
	case "gemini", "antigravity":
		return "gemini-2.0-flash"
	case "grok", "xai":
		return "grok-4.5"
	default:
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

func (p *Prober) doRequest(client *http.Client, request *http.Request, result model.ProbeResult) model.ProbeResult {
	start := time.Now()
	var firstByte time.Time
	trace := &httptrace.ClientTrace{GotFirstResponseByte: func() { firstByte = time.Now() }}
	request = request.WithContext(httptrace.WithClientTrace(request.Context(), trace))
	response, err := client.Do(request)
	if err != nil {
		result.Status = model.StatusError
		result.ErrorClass = classifyNetworkError(err)
		result.Message = trimMessage(err.Error())
		return result
	}
	defer response.Body.Close()
	body, readErr := io.ReadAll(io.LimitReader(response.Body, 64*1024))
	total := durationMs(time.Since(start))
	result.LatencyMs = &total
	if !firstByte.IsZero() {
		value := durationMs(firstByte.Sub(start))
		result.FirstByteMs = &value
	}
	statusCode := response.StatusCode
	result.StatusCode = &statusCode
	if readErr != nil {
		result.Status = model.StatusError
		result.ErrorClass = "read"
		result.Message = trimMessage(readErr.Error())
		return result
	}
	if response.StatusCode >= 200 && response.StatusCode < 300 {
		result.Status = model.StatusOperational
		return result
	}
	result.Status = model.StatusFailed
	result.ErrorClass = "upstream"
	result.Message = responseMessage(response.StatusCode, body)
	return result
}

func (p *Prober) clientFor(proxyRawURL string) (*http.Client, error) {
	transport := p.transport
	if strings.TrimSpace(proxyRawURL) != "" {
		parsed, err := url.Parse(proxyRawURL)
		if err != nil || parsed.Scheme == "" || parsed.Host == "" {
			return nil, fmt.Errorf("invalid account proxy URL")
		}
		transport = p.transport.Clone()
		transport.Proxy = http.ProxyURL(parsed)
		transport.DialContext = (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext
	}
	return &http.Client{Transport: transport, Timeout: p.timeout}, nil
}

func safeDialContext(allowPrivate bool) func(context.Context, string, string) (net.Conn, error) {
	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	if allowPrivate {
		return dialer.DialContext
	}
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, err
		}
		addresses, err := net.DefaultResolver.LookupIPAddr(ctx, host)
		if err != nil {
			return nil, fmt.Errorf("resolve upstream host: %w", err)
		}
		if len(addresses) == 0 {
			return nil, fmt.Errorf("upstream host has no IP address")
		}
		for _, address := range addresses {
			if !isPublicIP(address.IP) {
				return nil, fmt.Errorf("upstream host resolves to a private address")
			}
		}
		return dialer.DialContext(ctx, network, net.JoinHostPort(addresses[0].IP.String(), port))
	}
}

func isPublicIP(ip net.IP) bool {
	return ip != nil && ip.IsGlobalUnicast() && !ip.IsPrivate() && !ip.IsLoopback() && !ip.IsLinkLocalUnicast()
}

func buildRequest(ctx context.Context, account model.Account, baseURL, token, modelName string) (*http.Request, error) {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	parsed, err := url.Parse(baseURL)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return nil, fmt.Errorf("invalid base URL")
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, fmt.Errorf("base URL must use http or https")
	}
	var endpoint string
	var payload any
	var headers = map[string]string{"Content-Type": "application/json", "Accept": "application/json"}
	platform := strings.ToLower(strings.TrimSpace(account.Platform))
	switch platform {
	case "openai", "openai_compatible", "codex", "grok", "xai":
		endpoint = appendEndpoint(baseURL, "/v1/chat/completions")
		payload = map[string]any{
			"model":      modelName,
			"messages":   []map[string]string{{"role": "user", "content": "ping"}},
			"max_tokens": 1,
			"stream":     false,
		}
		headers["Authorization"] = "Bearer " + token
	case "anthropic", "claude":
		endpoint = appendEndpoint(baseURL, "/v1/messages")
		payload = map[string]any{
			"model":      modelName,
			"max_tokens": 1,
			"messages":   []map[string]string{{"role": "user", "content": "ping"}},
		}
		headers["x-api-key"] = token
		headers["anthropic-version"] = "2023-06-01"
	case "gemini", "antigravity":
		endpoint = appendEndpoint(baseURL, "/v1beta/models/"+url.PathEscape(modelName)+":generateContent")
		payload = map[string]any{"contents": []map[string]any{{"parts": []map[string]string{{"text": "ping"}}}}}
		if account.Type == "api_key" || account.Type == "" {
			separator := "?"
			if strings.Contains(endpoint, "?") {
				separator = "&"
			}
			endpoint += separator + "key=" + url.QueryEscape(token)
		} else {
			headers["Authorization"] = "Bearer " + token
		}
	default:
		endpoint = baseURL
		headers["Authorization"] = "Bearer " + token
		payload = map[string]string{"model": modelName, "prompt": "ping"}
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	for key, value := range headers {
		request.Header.Set(key, value)
	}
	if account.Type == "cookie" {
		request.Header.Set("Cookie", token)
	}
	return request, nil
}

func appendEndpoint(base, endpoint string) string {
	parsed, err := url.Parse(base)
	if err != nil {
		return strings.TrimRight(base, "/") + endpoint
	}
	basePath := strings.TrimRight(parsed.Path, "/")
	endpointPath := endpoint
	if strings.HasSuffix(basePath, "/v1") && strings.HasPrefix(endpointPath, "/v1/") {
		endpointPath = strings.TrimPrefix(endpointPath, "/v1")
	}
	parsed.Path = strings.TrimRight(basePath, "/") + endpointPath
	parsed.RawQuery = ""
	return parsed.String()
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
			switch typed := value.(type) {
			case string:
				if value := strings.TrimSpace(typed); value != "" {
					return value
				}
			}
		}
	}
	return ""
}

func durationMs(duration time.Duration) int {
	value := int(duration / time.Millisecond)
	if value < 1 {
		return 1
	}
	return value
}

func classifyNetworkError(err error) string {
	if errorsIsTimeout(err) {
		return "timeout"
	}
	return "network"
}

func errorsIsTimeout(err error) bool {
	if err == nil {
		return false
	}
	if timeout, ok := err.(interface{ Timeout() bool }); ok && timeout.Timeout() {
		return true
	}
	return strings.Contains(strings.ToLower(err.Error()), "timeout")
}

func responseMessage(status int, body []byte) string {
	message := strings.TrimSpace(string(body))
	if message == "" {
		return fmt.Sprintf("upstream returned HTTP %d", status)
	}
	return trimMessage(fmt.Sprintf("HTTP %d: %s", status, message))
}

func trimMessage(value string) string {
	value = strings.Join(strings.Fields(value), " ")
	if len(value) > 300 {
		return value[:300]
	}
	return value
}

// ValidateBaseURL is used by callers before executing a request. It rejects
// local/private destinations by default, while allowing an explicit opt-in for
// installations that intentionally route through an internal endpoint.
func ValidateBaseURL(raw string, allowPrivate bool) error {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Hostname() == "" {
		return fmt.Errorf("invalid URL")
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return fmt.Errorf("unsupported URL scheme")
	}
	if allowPrivate {
		return nil
	}
	host := strings.ToLower(parsed.Hostname())
	if host == "localhost" || strings.HasSuffix(host, ".local") {
		return fmt.Errorf("private host is disabled")
	}
	if ip := net.ParseIP(host); ip != nil && (ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast()) {
		return fmt.Errorf("private host is disabled")
	}
	return nil
}
