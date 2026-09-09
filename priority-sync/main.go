package main

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	_ "github.com/lib/pq"
)

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
		ReplaceAttr: func(_ []string, attr slog.Attr) slog.Attr {
			if attr.Key == slog.TimeKey {
				return slog.Attr{}
			}
			return attr
		},
	}))
	slog.SetDefault(logger)
	config, err := LoadConfig()
	if err != nil {
		logger.Error("优先级服务配置无效", "error", err)
		os.Exit(1)
	}
	if len(os.Args) > 1 && os.Args[1] == "health" {
		if err := healthCheck(config); err != nil {
			logger.Error("优先级服务健康检查失败", "error", err)
			os.Exit(1)
		}
		return
	}

	db, err := sql.Open("postgres", config.DatabaseURL)
	if err != nil {
		logger.Error("打开 PostgreSQL 失败", "error", err)
		os.Exit(1)
	}
	defer db.Close()
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(2)
	db.SetConnMaxIdleTime(5 * time.Minute)
	pingContext, cancelPing := context.WithTimeout(context.Background(), 15*time.Second)
	err = db.PingContext(pingContext)
	cancelPing()
	if err != nil {
		logger.Error("PostgreSQL 不可用", "error", err)
		os.Exit(1)
	}

	state, err := loadState(config.StateFile)
	if err != nil {
		logger.Error("读取优先级状态失败", "error", err)
		os.Exit(1)
	}
	source := NewMetricsStore(db)
	client := &http.Client{Timeout: 10 * time.Second}
	runner := NewRunner(config, source, client, state, logger)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	logger.Info("priority-sync 已启动",
		"interval", config.Interval.String(),
		"window", config.Window.String(),
		"dry_run", config.DryRun,
		"confirmations", config.Confirmations,
	)
	run := func() {
		cycleContext, cancel := context.WithTimeout(ctx, config.Interval)
		defer cancel()
		if err := runner.RunOnce(cycleContext, time.Now().UTC()); err != nil {
			logger.Error("优先级策略周期失败", "error", err)
		}
	}
	run()
	ticker := time.NewTicker(config.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			logger.Info("priority-sync 已停止")
			return
		case <-ticker.C:
			run()
		}
	}
}

func healthCheck(config Config) error {
	info, err := os.Stat(config.ReportFile)
	if err != nil {
		return fmt.Errorf("报告文件不可用: %w", err)
	}
	maxAge := config.Interval * 3
	if maxAge < 2*time.Minute {
		maxAge = 2 * time.Minute
	}
	if time.Since(info.ModTime()) > maxAge {
		return fmt.Errorf("报告已超过 %s 未更新", maxAge)
	}
	return nil
}
