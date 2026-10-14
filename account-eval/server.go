package main

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

//go:embed web/*
var assets embed.FS

type AccountSource interface {
	catalog(context.Context) ([]Account, []Group, error)
}

type Server struct {
	store   *Store
	source  AccountSource
	engine  *Engine
	gateway *Gateway
}

func jsonReply(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func bad(w http.ResponseWriter, status int, message string) {
	jsonReply(w, status, map[string]string{"error": message})
}
func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		bad(w, 415, "请使用 JSON 请求")
		return false
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4*1024*1024)
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if d.Decode(v) != nil || d.Decode(&struct{}{}) != io.EOF {
		bad(w, 400, "请求格式无效或超过 4MB")
		return false
	}
	return true
}

func (s *Server) handler() http.Handler {
	api := http.NewServeMux()
	api.HandleFunc("GET /api/catalog", func(w http.ResponseWriter, r *http.Request) {
		a, g, err := s.source.catalog(r.Context())
		if err != nil {
			bad(w, 503, "无法读取主服务账户资料")
			return
		}
		jsonReply(w, 200, map[string]any{"accounts": a, "groups": g})
	})
	api.HandleFunc("GET /api/suites", func(w http.ResponseWriter, r *http.Request) {
		v, err := s.store.suites(r.Context())
		if err != nil {
			bad(w, 500, "无法读取题库")
			return
		}
		jsonReply(w, 200, v)
	})
	api.HandleFunc("POST /api/suites", s.saveSuite)
	api.HandleFunc("DELETE /api/suites/{id}", func(w http.ResponseWriter, r *http.Request) {
		_, err := s.store.db.ExecContext(r.Context(), `DELETE FROM suites WHERE id=?`, r.PathValue("id"))
		if err != nil {
			bad(w, 500, "删除失败")
			return
		}
		jsonReply(w, 200, map[string]bool{"ok": true})
	})
	api.HandleFunc("GET /api/runs", func(w http.ResponseWriter, r *http.Request) {
		v, err := s.store.runs(r.Context())
		if err != nil {
			bad(w, 500, "无法读取任务")
			return
		}
		jsonReply(w, 200, v)
	})
	api.HandleFunc("POST /api/runs", s.createRun)
	api.HandleFunc("GET /api/runs/{id}", s.runDetail)
	api.HandleFunc("POST /api/runs/{id}/cancel", func(w http.ResponseWriter, r *http.Request) {
		if err := s.engine.cancelRun(r.Context(), r.PathValue("id")); err != nil {
			bad(w, 409, "任务不存在或已经结束")
			return
		}
		jsonReply(w, 200, map[string]bool{"ok": true})
	})
	api.HandleFunc("POST /api/runs/{id}/retry", s.retryRun)
	api.HandleFunc("GET /api/results/{id}", func(w http.ResponseWriter, r *http.Request) {
		id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
		v, err := s.store.result(r.Context(), id)
		if err != nil {
			bad(w, 404, "结果不存在")
			return
		}
		jsonReply(w, 200, v)
	})
	api.HandleFunc("PATCH /api/results/{id}", s.mark)
	api.HandleFunc("POST /api/icon", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			SVG string `json:"svg"`
		}
		if !decode(w, r, &req) {
			return
		}
		svg, err := cleanSVG(req.SVG)
		if err != nil {
			bad(w, 400, err.Error())
			return
		}
		jsonReply(w, 200, map[string]string{"svg": svg})
	})
	api.HandleFunc("POST /api/menu", func(w http.ResponseWriter, r *http.Request) {
		var desired struct {
			Label string `json:"label"`
			URL   string `json:"url"`
			Icon  string `json:"icon_svg"`
		}
		if !decode(w, r, &desired) {
			return
		}
		v, err := s.gateway.menu(r.Context(), r.Header, Menu{Label: desired.Label, URL: desired.URL, Icon: desired.Icon})
		if err != nil {
			bad(w, 400, err.Error())
			return
		}
		jsonReply(w, 200, v)
	})
	sub := http.NewServeMux()
	sub.Handle("/api/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
		defer cancel()
		r = r.WithContext(ctx)
		if err := s.gateway.admin(ctx, r.Header); err != nil {
			status := 401
			if errors.Is(err, errForbidden) {
				status = 403
			}
			bad(w, status, err.Error())
			return
		}
		api.ServeHTTP(w, r)
	}))
	files, _ := fs.Sub(assets, "web")
	sub.Handle("/", http.FileServer(http.FS(files)))
	root := http.NewServeMux()
	root.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if s.store.db.PingContext(ctx) != nil {
			w.WriteHeader(503)
			return
		}
		w.WriteHeader(200)
	})
	root.HandleFunc("/account-eval", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/account-eval/", http.StatusPermanentRedirect)
	})
	root.Handle("/account-eval/", http.StripPrefix("/account-eval", sub))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self' 'unsafe-inline'; style-src 'self' 'unsafe-inline'; img-src 'self' data: blob:; frame-src 'self' blob:; connect-src 'self'; object-src 'none'; base-uri 'none'; frame-ancestors 'self'")
		root.ServeHTTP(w, r)
	})
}

var validID = regexp.MustCompile(`^[a-f0-9]{32}$`)

func (s *Server) saveSuite(w http.ResponseWriter, r *http.Request) {
	var v Suite
	if !decode(w, r, &v) {
		return
	}
	if err := v.validate(); err != nil {
		bad(w, 400, err.Error())
		return
	}
	if v.ID == "" {
		v.ID = newID()
	}
	if !validID.MatchString(v.ID) {
		bad(w, 400, "题组 ID 无效")
		return
	}
	if err := s.store.saveSuite(r.Context(), v); err != nil {
		bad(w, 500, "保存题组失败")
		return
	}
	jsonReply(w, 200, v)
}

func (s *Server) createRun(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name       string   `json:"name"`
		SuiteID    string   `json:"suite_id"`
		AccountIDs []int64  `json:"account_ids"`
		Settings   Settings `json:"settings"`
	}
	if !decode(w, r, &req) {
		return
	}
	suites, err := s.store.suites(r.Context())
	if err != nil {
		bad(w, 500, "无法读取题库")
		return
	}
	var suite Suite
	for _, v := range suites {
		if v.ID == req.SuiteID {
			suite = v
			break
		}
	}
	if suite.ID == "" {
		bad(w, 400, "请先保存题组")
		return
	}
	all, _, err := s.source.catalog(r.Context())
	if err != nil {
		bad(w, 503, "无法读取账户")
		return
	}
	ids := map[int64]bool{}
	for _, id := range req.AccountIDs {
		ids[id] = true
	}
	accounts := []Account{}
	for _, a := range all {
		if ids[a.ID] {
			accounts = append(accounts, a)
			delete(ids, a.ID)
		}
	}
	if len(ids) > 0 || len(accounts) == 0 {
		bad(w, 400, "请选择有效账户，账户列表变化后请刷新")
		return
	}
	if err := req.Settings.validate(accounts); err != nil {
		bad(w, 400, err.Error())
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		name = suite.Name
	}
	if len(name) > 200 {
		bad(w, 400, "任务名称过长")
		return
	}
	run := Run{ID: newID(), Name: name, Suite: suite, Accounts: accounts, Settings: req.Settings, Status: "queued", CreatedAt: time.Now().UTC()}
	cells := []Result{}
	for _, a := range accounts {
		for q := range suite.Questions {
			cells = append(cells, Result{AccountID: a.ID, Question: q})
		}
	}
	run.Total = len(cells)
	if err := s.store.createRun(r.Context(), run, cells); err != nil {
		bad(w, 500, "创建任务失败")
		return
	}
	s.engine.notify()
	jsonReply(w, 201, run)
}

func (s *Server) runDetail(w http.ResponseWriter, r *http.Request) {
	v, err := s.store.run(r.Context(), r.PathValue("id"))
	if err != nil {
		bad(w, 404, "任务不存在")
		return
	}
	results, err := s.store.results(r.Context(), v.ID, false)
	if err != nil {
		bad(w, 500, "无法读取结果")
		return
	}
	v.Total = len(results)
	v.Done = 0
	for _, c := range results {
		if c.Status != "queued" && c.Status != "running" {
			v.Done++
		}
	}
	jsonReply(w, 200, map[string]any{"run": v, "results": results})
}

func (s *Server) retryRun(w http.ResponseWriter, r *http.Request) {
	old, err := s.store.run(r.Context(), r.PathValue("id"))
	if err != nil {
		bad(w, 404, "任务不存在")
		return
	}
	if old.Status == "queued" || old.Status == "running" || old.Status == "cancelling" {
		bad(w, 409, "请等待任务结束后重试")
		return
	}
	results, err := s.store.results(r.Context(), old.ID, false)
	if err != nil {
		bad(w, 500, "无法读取失败记录")
		return
	}
	cells := []Result{}
	ids := map[int64]bool{}
	for _, c := range results {
		if c.Status != "completed" {
			cells = append(cells, Result{AccountID: c.AccountID, Question: c.Question})
			ids[c.AccountID] = true
		}
	}
	if len(cells) == 0 {
		bad(w, 400, "没有需要重试的项目")
		return
	}
	old.ParentID = old.ID
	old.ID = newID()
	old.Status = "queued"
	old.CreatedAt = time.Now().UTC()
	old.Name += " · 重试"
	old.Total = len(cells)
	old.Done = 0
	accounts := []Account{}
	for _, a := range old.Accounts {
		if ids[a.ID] {
			accounts = append(accounts, a)
		}
	}
	old.Accounts = accounts
	if err := s.store.createRun(r.Context(), old, cells); err != nil {
		bad(w, 500, "创建重试任务失败")
		return
	}
	s.engine.notify()
	jsonReply(w, 201, old)
}

func (s *Server) mark(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Mark string `json:"mark"`
	}
	if !decode(w, r, &req) {
		return
	}
	if req.Mark != "" && req.Mark != "pass" && req.Mark != "fail" && req.Mark != "review" {
		bad(w, 400, "标记无效")
		return
	}
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	res, err := s.store.db.ExecContext(r.Context(), `UPDATE results SET mark=? WHERE id=?`, req.Mark, id)
	if err != nil {
		bad(w, 500, "保存标记失败")
		return
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		bad(w, 404, "结果不存在")
		return
	}
	jsonReply(w, 200, map[string]bool{"ok": true})
}
