package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

func (s *PostgresChannelSource) AdminAPIKey(ctx context.Context) (string, error) {
	var key string
	if err := s.db.QueryRowContext(ctx, adminAPIKeySQL).Scan(&key); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", nil
		}
		return "", fmt.Errorf("查询 Admin API Key: %w", err)
	}
	return strings.TrimSpace(key), nil
}

func (s *PostgresChannelSource) List(ctx context.Context) ([]Channel, error) {
	rows, err := s.db.QueryContext(ctx, discoverChannelsSQL)
	if err != nil {
		return nil, fmt.Errorf("查询可用渠道: %w", err)
	}
	defer rows.Close()

	var channels []Channel
	for rows.Next() {
		channel, err := scanChannel(rows)
		if err != nil {
			return nil, err
		}
		channels = append(channels, channel)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历可用渠道: %w", err)
	}
	return channels, nil
}

func scanChannel(rows *sql.Rows) (Channel, error) {
	var channel Channel
	var daily, weekly, monthly sql.NullFloat64
	var proxyProtocol, proxyHost, proxyUsername, proxyPassword sql.NullString
	var proxyPort sql.NullInt64
	if err := rows.Scan(
		&channel.AccountID,
		&channel.AccountName,
		&channel.AccountRateMultiplier,
		&channel.BaseURL,
		&channel.APIKey,
		&channel.Group.ID,
		&channel.Group.Name,
		&channel.Group.RateMultiplier,
		&daily,
		&weekly,
		&monthly,
		&proxyProtocol,
		&proxyHost,
		&proxyPort,
		&proxyUsername,
		&proxyPassword,
	); err != nil {
		return Channel{}, fmt.Errorf("读取可用渠道: %w", err)
	}
	proxyURL, err := buildProxyURL(proxyProtocol, proxyHost, proxyPort, proxyUsername, proxyPassword)
	if err != nil {
		return Channel{}, fmt.Errorf("读取账号 %d 代理: %w", channel.AccountID, err)
	}
	channel.ProxyURL = proxyURL
	channel.Group.DailyLimitUSD = nullableFloat(daily)
	channel.Group.WeeklyLimitUSD = nullableFloat(weekly)
	channel.Group.MonthlyLimitUSD = nullableFloat(monthly)
	return channel, nil
}
