package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
)

const responseLimit = 1024 * 1024

func (s *Syncer) fetchUpstreamJSON(ctx context.Context, channel *Channel, path, rawQuery string, target any) (int, error) {
	return s.fetchUpstreamJSONWithLimit(ctx, channel, path, rawQuery, responseLimit, target)
}

func (s *Syncer) fetchUpstreamJSONWithLimit(ctx context.Context, channel *Channel, path, rawQuery string, limit int64, target any) (int, error) {
	endpoint, err := upstreamEndpoint(channel.BaseURL, path, rawQuery)
	if err != nil {
		return 0, err
	}
	req, err := newUpstreamRequest(ctx, endpoint, channel.APIKey)
	if err != nil {
		return 0, err
	}
	proxyURLs, err := s.upstreamProxyURLs(channel)
	if err != nil {
		return 0, fmt.Errorf("配置上游代理: %w", err)
	}
	for index, proxyURL := range proxyURLs {
		client, err := s.upstreamClientForProxy(proxyURL)
		if err != nil {
			return 0, fmt.Errorf("配置上游代理: %w", err)
		}
		status, requestErr := executeUpstreamRequest(ctx, client, req, limit, target)
		if requestErr == nil {
			if index > 0 && s.logger != nil {
				s.logger.Printf("[%s] 备用代理 %s 请求成功", channelLabel(channel), proxyLabel(proxyURL))
			}
			return status, nil
		}
		if status != 0 {
			return status, requestErr
		}
		if !s.canRetryProxy(ctx, requestErr, index, len(proxyURLs)) {
			return 0, fmt.Errorf("请求上游（代理 %s）: %w", proxyLabel(proxyURL), requestErr)
		}
		if s.logger != nil {
			s.logger.Printf(
				"[%s] 上游请求网络失败，切换代理 %s -> %s: %v",
				channelLabel(channel), proxyLabel(proxyURL), proxyLabel(proxyURLs[index+1]), requestErr,
			)
		}
	}
	return 0, fmt.Errorf("请求上游失败: 没有可用代理")
}

func newUpstreamRequest(ctx context.Context, endpoint, apiKey string) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("创建上游请求: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "sub2api-rate-sync/0.2.0")
	return req, nil
}

func executeUpstreamRequest(ctx context.Context, client *http.Client, req *http.Request, limit int64, target any) (int, error) {
	resp, err := client.Do(req.Clone(ctx))
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return resp.StatusCode, nil
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, limit)).Decode(target); err != nil {
		return resp.StatusCode, fmt.Errorf("解析上游响应: %w", err)
	}
	return resp.StatusCode, nil
}

func (s *Syncer) canRetryProxy(ctx context.Context, err error, index, total int) bool {
	return index+1 < total && isRetryableNetworkError(err) && ctx.Err() == nil
}

func (s *Syncer) upstreamClient(channel *Channel) (*http.Client, error) {
	proxyURLs, err := s.upstreamProxyURLs(channel)
	if err != nil {
		return nil, err
	}
	return s.upstreamClientForProxy(proxyURLs[0])
}

func (s *Syncer) upstreamProxyURLs(channel *Channel) ([]string, error) {
	channelProxyURL := ""
	if channel != nil {
		channelProxyURL = strings.TrimSpace(channel.ProxyURL)
	}
	globalProxyURL := ""
	if s.config != nil {
		globalProxyURL = strings.TrimSpace(s.config.ProxyURL)
	}

	proxyURLs := make([]string, 0, 2)
	switch {
	case channelProxyURL != "":
		proxyURLs = append(proxyURLs, channelProxyURL)
		if globalProxyURL != "" {
			proxyURLs = append(proxyURLs, globalProxyURL)
		}
	case globalProxyURL != "":
		proxyURLs = append(proxyURLs, globalProxyURL)
	case s.config == nil || len(s.config.ProxyFallbackURLs) == 0:
		proxyURLs = append(proxyURLs, "")
	}
	if s.config != nil {
		proxyURLs = append(proxyURLs, s.config.ProxyFallbackURLs...)
	}
	return normalizeProxyURLs(proxyURLs)
}

func normalizeProxyURLs(values []string) ([]string, error) {
	result := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, proxyURL := range values {
		proxyURL = strings.TrimSpace(proxyURL)
		if proxyURL != "" {
			if err := validateProxyURL(proxyURL, "proxy_url"); err != nil {
				return nil, err
			}
			proxyURL = strings.TrimRight(proxyURL, "/")
		}
		if _, exists := seen[proxyURL]; exists {
			continue
		}
		seen[proxyURL] = struct{}{}
		result = append(result, proxyURL)
	}
	if len(result) == 0 {
		result = append(result, "")
	}
	return result, nil
}

func (s *Syncer) upstreamClientForProxy(proxyURL string) (*http.Client, error) {
	baseClient := s.client
	if baseClient == nil {
		baseClient = http.DefaultClient
	}
	if proxyURL == "" {
		return baseClient, nil
	}

	parsed, err := url.Parse(proxyURL)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return nil, fmt.Errorf("代理 URL 无效")
	}
	cacheKey := parsed.String()

	s.upstreamClientsMu.Lock()
	defer s.upstreamClientsMu.Unlock()
	if s.upstreamClients == nil {
		s.upstreamClients = make(map[string]*http.Client)
	}
	if client, ok := s.upstreamClients[cacheKey]; ok {
		return client, nil
	}

	transport := http.DefaultTransport.(*http.Transport).Clone()
	if baseTransport, ok := baseClient.Transport.(*http.Transport); ok && baseTransport != nil {
		transport = baseTransport.Clone()
	}
	transport.Proxy = http.ProxyURL(parsed)
	client := &http.Client{
		Transport:     transport,
		Timeout:       baseClient.Timeout,
		CheckRedirect: baseClient.CheckRedirect,
		Jar:           baseClient.Jar,
	}
	s.upstreamClients[cacheKey] = client
	return client, nil
}

func isRetryableNetworkError(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, io.EOF) {
		return true
	}
	var netErr net.Error
	return errors.As(err, &netErr)
}

func proxyLabel(proxyURL string) string {
	proxyURL = strings.TrimSpace(proxyURL)
	if proxyURL == "" {
		return "直连"
	}
	parsed, err := url.Parse(proxyURL)
	if err == nil && parsed.Host != "" {
		return parsed.Scheme + "://" + parsed.Host
	}
	return proxyURL
}

func upstreamEndpoint(baseURL, path, rawQuery string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return "", fmt.Errorf("账号 base_url 必须是有效的 http/https URL")
	}
	return (&url.URL{Scheme: parsed.Scheme, Host: parsed.Host, Path: path, RawQuery: rawQuery}).String(), nil
}
