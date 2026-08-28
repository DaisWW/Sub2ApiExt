package web

import (
	"embed"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"strconv"
	"strings"

	"github.com/DaisWW/Sub2ApiExt/monitoring/internal/model"
	"github.com/DaisWW/Sub2ApiExt/monitoring/internal/monitor"
	"github.com/DaisWW/Sub2ApiExt/monitoring/internal/store"
)

//go:embed assets
var assets embed.FS

type Server struct {
	service        *monitor.Service
	frameAncestors string
}

func New(service *monitor.Service, frameAncestors ...string) *Server {
	policy := "'self'"
	if len(frameAncestors) > 0 {
		policy = normalizeFrameAncestors(frameAncestors[0])
	}
	return &Server{service: service, frameAncestors: policy}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	staticFS, _ := fs.Sub(assets, "assets")
	mux.Handle("/", spaFiles(staticFS))
	mux.HandleFunc("/healthz", s.health)
	mux.HandleFunc("/api/v1/monitor/dashboard", s.dashboard)
	mux.HandleFunc("/api/v1/monitor/usage-ranking", s.usageRanking)
	mux.HandleFunc("/api/v1/monitor/history", s.history)
	mux.HandleFunc("/api/v1/monitor/alerts", s.alerts)
	mux.HandleFunc("/api/", func(w http.ResponseWriter, _ *http.Request) {
		writeError(w, http.StatusNotFound, "not found")
	})
	return s.withSecurityHeaders(readOnly(mux))
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

func (s *Server) dashboard(w http.ResponseWriter, r *http.Request) {
	dashboard, err := s.service.Dashboard(r.Context())
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "监控面板暂时不可用")
		return
	}
	writeJSON(w, http.StatusOK, dashboard)
}

func (s *Server) usageRanking(w http.ResponseWriter, r *http.Request) {
	period := strings.TrimSpace(r.URL.Query().Get("period"))
	if period == "" {
		period = "24h"
	}
	ranking, err := s.service.UsageRanking(r.Context(), period, boundedQueryInt(r, "limit", 10, 1, 50))
	if err != nil {
		if errors.Is(err, store.ErrInvalidUsagePeriod) {
			writeError(w, http.StatusBadRequest, "period must be 1h, 24h, today, yesterday, 7d, 15d, or 30d")
			return
		}
		writeError(w, http.StatusServiceUnavailable, "用量数据暂时不可用")
		return
	}
	writeJSON(w, http.StatusOK, ranking)
}

func (s *Server) history(w http.ResponseWriter, r *http.Request) {
	key := strings.TrimSpace(r.URL.Query().Get("target"))
	if !validTargetKey(key) {
		writeError(w, http.StatusBadRequest, "target must be account:<id> or group:<id>")
		return
	}
	limit := boundedQueryInt(r, "limit", 240, 1, 1000)
	items, err := s.service.History(r.Context(), key, limit)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "历史数据暂时不可用")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"target": key, "items": items})
}

func (s *Server) alerts(w http.ResponseWriter, r *http.Request) {
	items, err := s.service.Alerts(r.Context(), boundedQueryInt(r, "limit", 50, 1, 200))
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "告警数据暂时不可用")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func readOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			writeError(w, http.StatusMethodNotAllowed, "监控 Web 服务仅提供只读访问")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) withSecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		frameAncestors := s.frameAncestors
		if frameAncestors == "" {
			frameAncestors = "'self'"
		}
		w.Header().Set("Content-Security-Policy", "default-src 'self'; connect-src 'self'; img-src 'self' data:; script-src 'self'; style-src 'self' 'unsafe-inline'; object-src 'none'; base-uri 'none'; frame-ancestors "+frameAncestors)
		if frameAncestors == "'self'" {
			w.Header().Set("X-Frame-Options", "SAMEORIGIN")
		}
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "same-origin")
		next.ServeHTTP(w, r)
	})
}

func normalizeFrameAncestors(value string) string {
	tokens := strings.Fields(value)
	if len(tokens) == 0 {
		return "'self'"
	}
	for _, token := range tokens {
		if token == "*" || strings.ContainsAny(token, ";\r\n") {
			return "'self'"
		}
	}
	return strings.Join(tokens, " ")
}

func validTargetKey(key string) bool {
	kind, value, found := strings.Cut(key, ":")
	if !found || (kind != model.KindAccount && kind != model.KindGroup) {
		return false
	}
	if value == "-1" {
		return kind == model.KindGroup
	}
	if value == "" {
		return false
	}
	for _, char := range value {
		if char < '0' || char > '9' {
			return false
		}
	}
	return len(value) == 1 || value[0] != '0'
}

func queryInt(r *http.Request, name string, fallback int) int {
	value, err := strconv.Atoi(r.URL.Query().Get(name))
	if err != nil || value <= 0 {
		return fallback
	}
	return value
}

func boundedQueryInt(r *http.Request, name string, fallback, minimum, maximum int) int {
	value := queryInt(r, name, fallback)
	if value < minimum {
		return minimum
	}
	if value > maximum {
		return maximum
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
