package main

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	_ "github.com/lib/pq"
)

func main() {
	logger := newStatusLogger(os.Stderr)
	configPath := os.Getenv("CONFIG_PATH")
	if configPath == "" {
		configPath = "config.json"
	}
	if len(os.Args) > 1 && os.Args[1] == "health" {
		if err := runHealthCheck(configPath, time.Now()); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}

	config, err := loadConfig(configPath)
	if err != nil {
		logger.Fatalf("启动失败: %v", err)
	}
	if err := invalidateSyncHealth(config.StateFile); err != nil {
		logger.Fatalf("启动失败: %v", err)
	}
	db, err := sql.Open("postgres", "")
	if err != nil {
		logger.Fatalf("启动失败: 初始化 PostgreSQL: %v", err)
	}
	defer db.Close()
	pingCtx, cancelPing := context.WithTimeout(context.Background(), 10*time.Second)
	err = db.PingContext(pingCtx)
	cancelPing()
	if err != nil {
		logger.Fatalf("启动失败: 连接 PostgreSQL: %v", err)
	}

	store := StateStore{Path: config.StateFile}
	state, err := store.Load()
	if err != nil {
		logger.Fatalf("启动失败: %v", err)
	}

	client := &http.Client{Timeout: 10 * time.Second}
	source := NewPostgresChannelSource(db)
	syncer := NewSyncer(config, source, client, store, state, logger)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	logger.Printf(
		"rate-sync 已启动: 自动发现=true templates=2 sync_target=%s interval=%s dry_run=%t confirmations=%d upstream_factors=%d usage_bootstrap=%t",
		config.SyncTarget,
		config.Interval,
		config.DryRun,
		config.Confirmations,
		len(config.Factors),
		config.UsageBootstrap,
	)
	run := func() {
		healthy, err := syncer.runCycle(ctx, time.Now())
		finishedAt := time.Now().UTC()
		if err != nil {
			logger.Printf("同步周期失败: %v", err)
			if healthErr := recordSyncHealthFailure(config.StateFile, finishedAt); healthErr != nil {
				logger.Printf("健康状态更新失败: %v", healthErr)
			}
			return
		}
		if config.AdminAPIKey == "" {
			logger.Printf("本轮等待 Admin API Key，健康状态标记为等待")
			if healthErr := writeSyncHealthState(config.StateFile, syncHealth{
				Phase:       syncHealthWaiting,
				LastCycleAt: finishedAt,
			}); healthErr != nil {
				logger.Printf("健康状态更新失败: %v", healthErr)
			}
			return
		}
		if !healthy {
			logger.Printf("本轮没有足够成功证据，健康状态标记为失败")
			if healthErr := recordSyncHealthFailure(config.StateFile, finishedAt); healthErr != nil {
				logger.Printf("健康状态更新失败: %v", healthErr)
			}
			return
		}
		if err := writeSyncHealth(config.StateFile, finishedAt); err != nil {
			logger.Printf("健康状态更新失败: %v", err)
		}
	}

	run()
	ticker := time.NewTicker(config.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			logger.Printf("rate-sync 已停止")
			return
		case <-ticker.C:
			run()
		}
	}
}

func runHealthCheck(configPath string, now time.Time) error {
	config, err := loadConfig(configPath)
	if err != nil {
		return fmt.Errorf("健康检查配置无效: %w", err)
	}
	return checkSyncHealth(config.StateFile, config.Interval, now)
}

func recordSyncHealthFailure(stateFile string, cycleAt time.Time) error {
	health, err := readSyncHealth(stateFile)
	if err != nil {
		health = syncHealth{}
	}
	health.Phase = syncHealthFailed
	health.LastCycleAt = cycleAt.UTC()
	return writeSyncHealthState(stateFile, health)
}
