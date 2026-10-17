package main

import (
	"context"
	"crypto/tls"
	"database/sql"
	"log/slog"
	"net/http"
	"os"

	"github.com/DaisWW/Sub2ApiExt/monitoring/internal/config"
	"github.com/DaisWW/Sub2ApiExt/monitoring/internal/monitor"
	"github.com/DaisWW/Sub2ApiExt/monitoring/internal/store"
	"github.com/DaisWW/Sub2ApiExt/monitoring/internal/web"
	_ "github.com/lib/pq"
	"github.com/redis/go-redis/v9"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		panic(err)
	}
	db, err := sql.Open("postgres", cfg.DatabaseURL)
	if err != nil {
		panic(err)
	}
	defer db.Close()
	var redisClient *redis.Client
	if cfg.RedisAddr != "" {
		redisOptions := &redis.Options{Addr: cfg.RedisAddr, Password: cfg.RedisPassword, DB: cfg.RedisDB}
		if cfg.RedisTLS {
			redisOptions.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12}
		}
		redisClient = redis.NewClient(redisOptions)
		defer redisClient.Close()
	}
	repository := store.New(db, redisClient)
	repository.SetConcurrencySlotTTL(cfg.ConcurrencySlotTTL)
	if err := repository.EnsureSchema(context.Background()); err != nil {
		panic(err)
	}
	service := monitor.New(cfg, repository, slog.New(slog.NewTextHandler(os.Stdout, nil)))
	if err := http.ListenAndServe(":8091", web.New(service, cfg.FrameAncestors).Handler()); err != nil {
		panic(err)
	}
}
