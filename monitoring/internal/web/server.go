package web

import (
	"embed"
	"encoding/json"
	"io/fs"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/DaisWW/Sub2ApiExt/monitoring/internal/monitor"
)

//go:embed assets/*
var assets embed.FS

type Server struct {
	service *monitor.Service
	token   string
	log     *slog.Logger
}

func New(service *monitor.Service, token string, logger *slog.Logger) *Server {
	if logger == nil {
		logger = slog.Default()
	}
	return &Server{service: service, token: strings.TrimSpace(token), log: logger}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	staticFS, _ := fs.Sub(assets, "assets")
	mux.Handle("/", spaFiles(staticFS))
	mux.HandleFunc("/healthz", s.health)
	mux.HandleFunc("/api/v1/monitor/config", s.config)
	mux.HandleFunc("/api/v1/monitor/dashboard", s.dashboard)
	mux.HandleFunc("/api/v1/monitor/history", s.history)
	mux.HandleFunc("/api/v1/monitor/alerts", s.alerts)
	mux.HandleFunc("/api/v1/monitor/probe", s.probe)
	mux.HandleFunc("/api/v1/monitor/alerts/", s.acknowledge)
	return s.withSecurityHeaders(s.withAuth(mux))
}

func spaFiles(files fs.FS) http.Handler {
	server := http.FileServer(http.FS(files))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/")
		if path == "" {
			path = "index.html"
		}
		if _, err := fs.Stat(files, path); err != nil {
			content, readErr := fs.ReadFile(files, "index.html")
			if readErr != nil {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write(content)
			return
		}
		server.ServeHTTP(w, r)
	})
}

func (s *Server) health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "service": "sub2api-monitoring"})
}

func (s *Server) config(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"auth_required": s.token != "", "interval_seconds": int(s.service.Interval() / time.Second)})
}

func (s *Server) dashboard(w http.ResponseWriter, r *http.Request) {
	dashboard, err := s.service.DashboardWindow(r.Context(), queryInt(r, "window", 0))
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, dashboard)
}

func (s *Server) history(w http.ResponseWriter, r *http.Request) {
	key := strings.TrimSpace(r.URL.Query().Get("target"))
	if !validTargetKey(key) {
		writeError(w, http.StatusBadRequest, "target must be account:<id> or group:<id>")
		return
	}
	days := queryInt(r, "days", 7)
	limit := queryInt(r, "limit", 240)
	items, err := s.service.History(r.Context(), key, days, limit)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"target": key, "items": items})
}

func (s *Server) alerts(w http.ResponseWriter, r *http.Request) {
	items, err := s.service.Alerts(r.Context(), r.URL.Query().Get("unacknowledged") == "true", queryInt(r, "limit", 50))
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (s *Server) probe(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if !s.service.Trigger() {
		writeError(w, http.StatusConflict, "a probe cycle is already running")
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"accepted": true})
}

func (s *Server) acknowledge(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(parts) != 6 || parts[5] != "ack" {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	id, err := strconv.ParseInt(parts[4], 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusBadRequest, "invalid alert id")
		return
	}
	if err := s.service.AcknowledgeAlert(r.Context(), id); err != nil {
		writeError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"acknowledged": true})
}

func (s *Server) withAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		protectedAPI := strings.HasPrefix(r.URL.Path, "/api/v1/monitor/") && r.URL.Path != "/api/v1/monitor/config"
		if s.token != "" && protectedAPI {
			authorization := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
			if authorization != s.token {
				writeError(w, http.StatusUnauthorized, "monitoring API token required")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) withSecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "same-origin")
		next.ServeHTTP(w, r)
	})
}

func validTargetKey(key string) bool {
	if !(strings.HasPrefix(key, "account:") || strings.HasPrefix(key, "group:")) {
		return false
	}
	value := strings.TrimPrefix(strings.TrimPrefix(key, "account:"), "group:")
	if value == "-1" {
		return true
	}
	if value == "" {
		return false
	}
	for _, char := range value {
		if char < '0' || char > '9' {
			return false
		}
	}
	return true
}

func queryInt(r *http.Request, name string, fallback int) int {
	value, err := strconv.Atoi(r.URL.Query().Get(name))
	if err != nil || value <= 0 {
		return fallback
	}
	return value
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}
