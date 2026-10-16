package access

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

var (
	ErrUnauthenticated = errors.New("请在主站重新登录后查看监控")
	ErrUnavailable     = errors.New("主站权限验证暂时不可用")
)

// Authorizer delegates both identity and group policy to Sub2API. It does not
// decode JWTs, cache grants, or accept a browser-supplied user/group identifier.
type Authorizer struct {
	base   string
	client *http.Client
}

func NewAuthorizer(base string) (*Authorizer, error) {
	u, err := url.Parse(base)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" ||
		u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return nil, errors.New("MONITORING_SUB2API_URL must be an http(s) origin without credentials")
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil // The configured main service is on the trusted Docker network.
	return &Authorizer{
		base: strings.TrimRight(base, "/"),
		client: &http.Client{
			Timeout:       8 * time.Second,
			Transport:     transport,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}, nil
}

func (a *Authorizer) Close() { a.client.CloseIdleConnections() }

func (a *Authorizer) Resolve(ctx context.Context, headers http.Header) (Scope, error) {
	value := headers.Get("Authorization")
	if value == "" {
		return Public(), nil
	}
	parts := strings.Fields(value)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		return Scope{}, ErrUnauthenticated
	}
	var user struct {
		ID     int64  `json:"id"`
		Role   string `json:"role"`
		Status string `json:"status"`
	}
	if err := a.get(ctx, "/api/v1/auth/me", headers, &user); err != nil {
		return Scope{}, err
	}
	if user.ID <= 0 || user.Status != "active" || user.Role == "" {
		return Scope{}, ErrUnauthenticated
	}
	if user.Role == "admin" {
		return Administrator(), nil
	}
	var groups []struct {
		ID int64 `json:"id"`
	}
	if err := a.get(ctx, "/api/v1/groups/available", headers, &groups); err != nil {
		return Scope{}, err
	}
	ids := make([]int64, 0, len(groups))
	for _, group := range groups {
		if group.ID <= 0 {
			return Scope{}, ErrUnavailable
		}
		ids = append(ids, group.ID)
	}
	return Groups(ids), nil
}

func (a *Authorizer) get(ctx context.Context, path string, headers http.Header, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.base+path, nil)
	if err != nil {
		return ErrUnavailable
	}
	// Preserve the session binding used by the main service, as account-eval does.
	for _, name := range []string{"Authorization", "User-Agent", "X-Forwarded-For", "X-Real-Ip"} {
		if value := headers.Get(name); value != "" {
			req.Header.Set(name, value)
		}
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return ErrUnavailable
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return ErrUnauthenticated
	}
	if resp.StatusCode != http.StatusOK {
		return ErrUnavailable
	}
	var envelope struct {
		Code int             `json:"code"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&envelope); err != nil ||
		envelope.Code != 0 || len(envelope.Data) == 0 || string(envelope.Data) == "null" {
		return ErrUnavailable
	}
	if err := json.Unmarshal(envelope.Data, out); err != nil {
		return ErrUnavailable
	}
	return nil
}
