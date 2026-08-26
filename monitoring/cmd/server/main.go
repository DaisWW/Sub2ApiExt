package main

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/DaisWW/Sub2ApiExt/monitoring/internal/config"
	"github.com/DaisWW/Sub2ApiExt/monitoring/internal/monitor"
	"github.com/DaisWW/Sub2ApiExt/monitoring/internal/store"
	"github.com/DaisWW/Sub2ApiExt/monitoring/internal/web"
	_ "github.com/lib/pq"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)
	cfg, err := config.Load()
	if err != nil {
		logger.Error("invalid monitoring configuration", "error", err)
		os.Exit(1)
	}
	db, err := sql.Open("postgres", cfg.DatabaseURL)
	if err != nil {
		logger.Error("open database failed", "error", err)
		os.Exit(1)
	}
	defer db.Close()
	db.SetMaxOpenConns(8)
	db.SetMaxIdleConns(4)
	db.SetConnMaxIdleTime(5 * time.Minute)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	pingCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	err = db.PingContext(pingCtx)
	cancel()
	if err != nil {
		logger.Error("database unavailable", "error", err)
		os.Exit(1)
	}
	repository := store.New(db)
	if err := repository.EnsureSchema(ctx); err != nil {
		logger.Error("ensure monitoring schema failed", "error", err)
		os.Exit(1)
	}
	service := monitor.New(cfg, repository, logger)
	server := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           web.New(service).Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	go service.Run(ctx)
	go func() {
		logger.Info("monitoring web service started", "addr", cfg.ListenAddr)
		if serveErr := server.ListenAndServe(); serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			logger.Error("monitoring web service stopped unexpectedly", "error", serveErr)
			stop()
		}
	}()
	<-ctx.Done()
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	_ = server.Shutdown(shutdownCtx)
}
