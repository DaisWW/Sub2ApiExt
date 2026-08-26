package main

import (
	"context"
	"database/sql"
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

	config, err := loadConfig(configPath)
	if err != nil {
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
		"rate-sync 已启动: 自动发现=true templates=2 sync_target=%s interval=%s dry_run=%t confirmations=%d factors=%d sync_hosts=%d usage_bootstrap=%t",
		config.SyncTarget,
		config.Interval,
		config.DryRun,
		config.Confirmations,
		len(config.Factors),
		len(config.SyncHosts),
		config.UsageBootstrap,
	)
	run := func() {
		if err := syncer.RunOnce(ctx, time.Now()); err != nil {
			logger.Printf("同步周期失败: %v", err)
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
