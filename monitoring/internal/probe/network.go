package probe

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"
)

var nonPublicNetworks = parseCIDRs([]string{
	"0.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8", "169.254.0.0/16",
	"192.0.0.0/24", "192.0.2.0/24", "192.88.99.0/24", "198.18.0.0/15",
	"198.51.100.0/24", "203.0.113.0/24", "224.0.0.0/4", "240.0.0.0/4",
	"100::/64", "2001:2::/48", "2001:10::/28", "2001:20::/28",
	"2001:db8::/32", "2002::/16", "3fff::/20", "fc00::/7", "fe80::/10", "ff00::/8",
})

func parseCIDRs(values []string) []*net.IPNet {
	networks := make([]*net.IPNet, 0, len(values))
	for _, value := range values {
		_, network, err := net.ParseCIDR(value)
		if err == nil {
			networks = append(networks, network)
		}
	}
	return networks
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
		addresses, err := resolvePublicAddresses(ctx, host)
		if err != nil {
			return nil, err
		}
		return dialer.DialContext(ctx, network, net.JoinHostPort(addresses[0].IP.String(), port))
	}
}

func validateResolvedHost(ctx context.Context, raw string, allowPrivate bool) error {
	if allowPrivate {
		return nil
	}
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Hostname() == "" {
		return fmt.Errorf("invalid URL")
	}
	_, err = resolvePublicAddresses(ctx, parsed.Hostname())
	return err
}

func resolvePublicAddresses(ctx context.Context, host string) ([]net.IPAddr, error) {
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
	return addresses, nil
}

func isPublicIP(ip net.IP) bool {
	if ip == nil {
		return false
	}
	if ipv4 := ip.To4(); ipv4 != nil {
		ip = ipv4
	}
	if !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsUnspecified() {
		return false
	}
	for _, network := range nonPublicNetworks {
		if network.Contains(ip) {
			return false
		}
	}
	return true
}

// validateTargetURL 校验 HTTP(S) URL 及字面量私网地址；域名解析结果由调用方另行校验。
func validateTargetURL(raw string, allowPrivate bool) error {
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
	if ip := net.ParseIP(host); ip != nil && !isPublicIP(ip) {
		return fmt.Errorf("private host is disabled")
	}
	return nil
}
