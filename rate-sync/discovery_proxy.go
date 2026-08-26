package main

import (
	"database/sql"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
)

func buildProxyURL(protocol, host sql.NullString, port sql.NullInt64, username, password sql.NullString) (string, error) {
	if !protocol.Valid && !host.Valid && !port.Valid {
		return "", nil
	}
	proxyProtocol := strings.ToLower(strings.TrimSpace(protocol.String))
	proxyHost := strings.TrimSpace(host.String)
	if proxyProtocol == "" || proxyHost == "" || !port.Valid || port.Int64 < 1 || port.Int64 > 65535 {
		return "", fmt.Errorf("代理配置不完整")
	}
	if proxyProtocol != "http" && proxyProtocol != "https" {
		return "", fmt.Errorf("暂不支持代理协议 %q", proxyProtocol)
	}
	proxyURL := &url.URL{
		Scheme: proxyProtocol,
		Host:   net.JoinHostPort(proxyHost, strconv.FormatInt(port.Int64, 10)),
	}
	if username.Valid && password.Valid && username.String != "" && password.String != "" {
		proxyURL.User = url.UserPassword(username.String, password.String)
	}
	return proxyURL.String(), nil
}

func nullableFloat(value sql.NullFloat64) *float64 {
	if !value.Valid {
		return nil
	}
	result := value.Float64
	return &result
}
