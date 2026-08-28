package main

import (
	"database/sql"
	"log/slog"
	"net/http"
	"os"

	"github.com/DaisWW/Sub2ApiExt/monitoring/internal/config"
	"github.com/DaisWW/Sub2ApiExt/monitoring/internal/monitor"
	"github.com/DaisWW/Sub2ApiExt/monitoring/internal/store"
	"github.com/DaisWW/Sub2ApiExt/monitoring/internal/web"
	_ "github.com/lib/pq"
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
	service := monitor.New(cfg, store.New(db), slog.New(slog.NewTextHandler(os.Stdout, nil)))
	if err := http.ListenAndServe(":8091", web.New(service, cfg.FrameAncestors).Handler()); err != nil {
		panic(err)
	}
}
