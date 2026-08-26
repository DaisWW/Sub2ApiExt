package probe

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	xproxy "golang.org/x/net/proxy"
)

func (p *Prober) clientFor(proxyRawURL string) (*http.Client, error) {
	transport := p.transport
	if strings.TrimSpace(proxyRawURL) != "" {
		var err error
		transport, err = p.proxyTransport(proxyRawURL)
		if err != nil {
			return nil, err
		}
	}
	return &http.Client{
		Transport: transport,
		Timeout:   p.timeout,
		CheckRedirect: func(request *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return fmt.Errorf("too many redirects")
			}
			if err := validateTargetURL(request.URL.String(), p.allowPrivate); err != nil {
				return err
			}
			return validateResolvedHost(request.Context(), request.URL.String(), p.allowPrivate)
		},
	}, nil
}

func (p *Prober) proxyTransport(rawURL string) (*http.Transport, error) {
	parsed, err := parseProxyURL(rawURL)
	if err != nil {
		return nil, err
	}
	transport := p.transport.Clone()
	directDialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	switch strings.ToLower(parsed.Scheme) {
	case "http", "https":
		transport.Proxy = http.ProxyURL(parsed)
		// 代理由管理员显式配置；代理端会自行解析目标，必须视为可信网络边界。
		transport.DialContext = directDialer.DialContext
	case "socks5", "socks5h":
		if err := configureSOCKS5(transport, parsed, directDialer); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("invalid account proxy URL")
	}
	return transport, nil
}

func parseProxyURL(raw string) (*url.URL, error) {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" || parsed.Port() == "" ||
		parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, fmt.Errorf("invalid account proxy URL")
	}
	return parsed, nil
}

func configureSOCKS5(transport *http.Transport, parsed *url.URL, directDialer *net.Dialer) error {
	var auth *xproxy.Auth
	if parsed.User != nil {
		password, _ := parsed.User.Password()
		auth = &xproxy.Auth{User: parsed.User.Username(), Password: password}
	}
	dialer, err := xproxy.SOCKS5("tcp", parsed.Host, auth, directDialer)
	if err != nil {
		return fmt.Errorf("invalid account proxy URL")
	}
	transport.Proxy = nil
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		if contextDialer, ok := dialer.(xproxy.ContextDialer); ok {
			return contextDialer.DialContext(ctx, network, address)
		}
		return dialer.Dial(network, address)
	}
	return nil
}
