package main

import (
	"context"
	"database/sql"
	"time"
)

type ChannelSource interface {
	List(context.Context) ([]Channel, error)
}

// GroupUsageStats 是按分组汇总的近期成本，用于计算多账号分组的共享倍率。
type GroupUsageStats struct {
	GroupID      int64
	Requests     int64
	StandardCost float64
	AccountCost  float64
}

type GroupUsageWindowStats struct {
	Window time.Duration
	GroupUsageStats
}

// GroupUsageAccountStats 是按账号汇总的成功请求成本。
// BaseCost 不含账号倍率，账号倍率变化后可以立即重新计算历史权重。
type GroupUsageAccountStats struct {
	GroupID            int64
	AccountID          int64
	Requests           int64
	StandardCost       float64
	BaseCost           float64
	CurrentAccountRate float64
}

type groupUsageSource interface {
	ListGroupUsageStats(context.Context, time.Time, time.Time) ([]GroupUsageStats, error)
}

type groupUsageWindowSource interface {
	ListGroupUsageStatsByWindows(context.Context, time.Time, []time.Duration) ([]GroupUsageWindowStats, error)
}

type groupUsageIncrementalSource interface {
	LatestGroupUsageID(context.Context) (int64, error)
	ListGroupUsageSince(context.Context, map[int64]int64, int64) ([]GroupUsageAccountStats, error)
	ListGroupUsageAccounts(context.Context, time.Time, time.Time, int64, []int64) ([]GroupUsageAccountStats, error)
}

type adminAPIKeySource interface {
	AdminAPIKey(context.Context) (string, error)
}

type PostgresChannelSource struct {
	db *sql.DB
}

type Channel struct {
	AccountID             int64
	AccountName           string
	AccountRateMultiplier float64
	BaseURL               string
	APIKey                string
	ProxyURL              string
	Group                 sub2APIGroup
}

type sub2APIGroup struct {
	ID              int64
	Name            string
	RateMultiplier  float64
	DailyLimitUSD   *float64
	WeeklyLimitUSD  *float64
	MonthlyLimitUSD *float64
}

func NewPostgresChannelSource(db *sql.DB) *PostgresChannelSource {
	return &PostgresChannelSource{db: db}
}
