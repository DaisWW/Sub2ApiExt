package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"
)

const templateUsageRatio = "sub2api_usage"

type Syncer struct {
	config            *Config
	source            ChannelSource
	client            *http.Client
	store             StateStore
	state             *State
	logger            *log.Logger
	upstreamClientsMu sync.Mutex
	upstreamClients   map[string]*http.Client
}

type skipError string

func (e skipError) Error() string {
	return string(e)
}

func NewSyncer(config *Config, source ChannelSource, client *http.Client, store StateStore, state *State, logger *log.Logger) *Syncer {
	return &Syncer{
		config:          config,
		source:          source,
		client:          client,
		store:           store,
		state:           state,
		logger:          logger,
		upstreamClients: make(map[string]*http.Client),
	}
}

func (s *Syncer) RunOnce(ctx context.Context, now time.Time) error {
	startedAt := time.Now()
	if err := s.refreshAdminAPIKey(ctx); err != nil {
		return err
	}
	if s.config.AdminAPIKey == "" {
		s.logger.Printf("Admin API Key 尚未配置，本轮等待；配置后将自动开始同步")
		return nil
	}

	s.logger.Printf("开始自动发现并同步价格")
	channels, err := s.source.List(ctx)
	if err != nil {
		return fmt.Errorf("自动发现渠道: %w", err)
	}
	report := newSyncReport(s.syncTarget(), channels)
	stats := s.syncDiscoveredChannels(ctx, channels, now, report)
	if err := s.store.Save(s.state); err != nil {
		return err
	}
	s.logger.Printf(
		"同步检查完成: 可用绑定=%d 已检查=%d 检查正常=%d 暂不自动=%d 失败=%d 耗时=%s",
		len(channels), stats.checked, stats.normal, stats.skipped, stats.failed,
		time.Since(startedAt).Round(time.Millisecond),
	)
	s.logSyncReport(report)
	return nil
}

func (s *Syncer) refreshAdminAPIKey(ctx context.Context) error {
	source, ok := s.source.(adminAPIKeySource)
	if !ok {
		return nil
	}
	adminAPIKey, err := source.AdminAPIKey(ctx)
	if err != nil {
		return err
	}
	s.config.AdminAPIKey = adminAPIKey
	return nil
}

func (s *Syncer) syncDiscoveredChannels(ctx context.Context, channels []Channel, now time.Time, report *syncReport) syncStats {
	handledGroups := make(map[int64]bool)
	if s.syncTarget() == "group" {
		handledGroups = s.syncSingleAccountGroups(ctx, channels, report)
		var usageErr error
		handledGroups, usageErr = s.syncGroupsFromUsage(ctx, channels, now, handledGroups, report)
		if usageErr != nil {
			s.logger.Printf("分组历史成本校准失败，本轮回退到上游探测: %v", usageErr)
		}
	}
	return s.runChannelChecks(ctx, channels, now, handledGroups, report)
}
