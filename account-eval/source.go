package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"sort"
	"strconv"
	"strings"
)

type Source struct{ db *sql.DB }

func (s *Source) catalog(ctx context.Context) ([]Account, []Group, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT a.id,a.name,a.platform,a.type,a.status,
		a.schedulable,a.rate_limit_reset_at,a.temp_unschedulable_until,COALESCE(a.extra->>'privacy_mode',''),
		COALESCE(a.credentials->'model_mapping','{}'),COALESCE(a.extra->'upstream_model_metadata'->'models','{}'),
		COALESCE(json_agg(ag.group_id ORDER BY ag.group_id) FILTER (WHERE g.id IS NOT NULL),'[]')
		FROM accounts a LEFT JOIN account_groups ag ON ag.account_id=a.id
		LEFT JOIN groups g ON g.id=ag.group_id AND g.deleted_at IS NULL
		WHERE a.deleted_at IS NULL GROUP BY a.id ORDER BY a.id`)
	if err != nil {
		return nil, nil, fmt.Errorf("读取账户: %w", err)
	}
	accounts := []Account{}
	for rows.Next() {
		var a Account
		var groups []byte
		var mapping, metadata []byte
		if err := rows.Scan(&a.ID, &a.Name, &a.Platform, &a.Type, &a.Status,
			&a.Schedulable, &a.RateLimitResetAt, &a.TempUnschedulableUntil, &a.PrivacyMode, &mapping, &metadata, &groups); err != nil {
			rows.Close()
			return nil, nil, err
		}
		if err := json.Unmarshal(groups, &a.GroupIDs); err != nil {
			rows.Close()
			return nil, nil, err
		}
		a.Models, a.ModelMetadata, err = accountModelCatalog(mapping, metadata)
		if err != nil {
			rows.Close()
			return nil, nil, fmt.Errorf("读取账户模型目录: %w", err)
		}
		a.Unsupported = supportReason(a.Platform, a.Type)
		accounts = append(accounts, a)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, nil, err
	}
	rows, err = s.db.QueryContext(ctx, `SELECT g.id,g.name,COALESCE(c.model_mapping,'{}'),
		COALESCE((SELECT json_agg(json_build_object('platform',p.platform,'models',p.models))
		FROM channel_model_pricing p WHERE p.channel_id=c.id),'[]'),COALESCE(g.model_pricing,'[]')
		FROM groups g LEFT JOIN channel_groups cg ON cg.group_id=g.id
		LEFT JOIN channels c ON c.id=cg.channel_id AND c.status='active'
		WHERE g.deleted_at IS NULL ORDER BY g.id`)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	groups := []Group{}
	for rows.Next() {
		var g Group
		var mapping, pricing, groupPricing []byte
		if err := rows.Scan(&g.ID, &g.Name, &mapping, &pricing, &groupPricing); err != nil {
			return nil, nil, err
		}
		g.Models, err = channelModelCatalog(mapping, pricing, groupPricing)
		if err != nil {
			return nil, nil, fmt.Errorf("读取渠道模型目录: %w", err)
		}
		groups = append(groups, g)
	}
	return accounts, groups, rows.Err()
}

func exactModelID(id string) bool {
	return strings.TrimSpace(id) != "" && len(id) <= 200 && !strings.ContainsAny(id, "*?[]\r\n")
}

func accountModelCatalog(mappingRaw, metadataRaw []byte) ([]string, map[string]ModelMetadata, error) {
	var mapping map[string]string
	var metadata map[string]ModelMetadata
	if err := json.Unmarshal(mappingRaw, &mapping); err != nil {
		return nil, nil, err
	}
	if err := json.Unmarshal(metadataRaw, &metadata); err != nil {
		return nil, nil, err
	}
	ids := map[string]bool{}
	capabilities := map[string]ModelMetadata{}
	for id, info := range metadata {
		if exactModelID(id) {
			ids[id] = true
			capabilities[id] = info
		}
	}
	for id, target := range mapping {
		if !exactModelID(id) {
			continue
		}
		ids[id] = true
		if info, ok := metadata[target]; ok {
			capabilities[id] = info
		}
	}
	models := make([]string, 0, len(ids))
	for id := range ids {
		models = append(models, id)
	}
	sort.Strings(models)
	return models, capabilities, nil
}

func channelModelCatalog(mappingRaw []byte, pricingRaw ...[]byte) (map[string][]string, error) {
	var mapping map[string]map[string]string
	if err := json.Unmarshal(mappingRaw, &mapping); err != nil {
		return nil, err
	}
	sets := map[string]map[string]bool{}
	add := func(platform, id string) {
		if platform == "" || !exactModelID(id) {
			return
		}
		if sets[platform] == nil {
			sets[platform] = map[string]bool{}
		}
		sets[platform][id] = true
	}
	for platform, models := range mapping {
		for id := range models {
			add(platform, id)
		}
	}
	for _, raw := range pricingRaw {
		var entries []struct {
			Platform string   `json:"platform"`
			Models   []string `json:"models"`
		}
		if err := json.Unmarshal(raw, &entries); err != nil {
			return nil, err
		}
		for _, entry := range entries {
			for _, id := range entry.Models {
				add(entry.Platform, id)
			}
		}
	}
	models := map[string][]string{}
	for platform, ids := range sets {
		for id := range ids {
			models[platform] = append(models[platform], id)
		}
		sort.Strings(models[platform])
	}
	return models, nil
}

func (s *Source) credential(ctx context.Context, id int64) (CredentialAccount, error) {
	var a CredentialAccount
	var raw []byte
	var extra []byte
	var proxyID sql.NullInt64
	var protocol, host, user, password, status sql.NullString
	var port sql.NullInt64
	err := s.db.QueryRowContext(ctx, `SELECT a.id,a.name,a.platform,a.type,a.status,a.credentials,COALESCE(a.extra,'{}'),
		a.proxy_id,p.protocol,p.host,p.port,p.username,p.password,p.status
		FROM accounts a LEFT JOIN proxies p ON p.id=a.proxy_id AND p.deleted_at IS NULL
		AND (p.expires_at IS NULL OR p.expires_at>NOW())
		WHERE a.id=$1 AND a.deleted_at IS NULL`, id).Scan(&a.ID, &a.Name, &a.Platform, &a.Type, &a.Status,
		&raw, &extra, &proxyID, &protocol, &host, &port, &user, &password, &status)
	if err != nil {
		return a, fmt.Errorf("账户已删除或无法读取")
	}
	if err := json.Unmarshal(raw, &a.Credentials); err != nil {
		return a, fmt.Errorf("账户凭据格式错误")
	}
	if err := json.Unmarshal(extra, &a.Extra); err != nil {
		return a, fmt.Errorf("账户扩展资料格式错误")
	}
	if proxyID.Valid {
		if !host.Valid || host.String == "" || status.String != "active" || port.Int64 < 1 || port.Int64 > 65535 {
			a.ProxyError = "账户代理不可用，已停止请求"
		} else {
			u := url.URL{Scheme: strings.ToLower(protocol.String), Host: net.JoinHostPort(host.String, strconv.FormatInt(port.Int64, 10))}
			if user.String != "" {
				u.User = url.UserPassword(user.String, password.String)
			}
			a.ProxyURL = u.String()
		}
	}
	return a, nil
}

func (s *Source) setting(ctx context.Context, key string) (string, error) {
	var value string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key=$1`, key).Scan(&value)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return value, err
}
