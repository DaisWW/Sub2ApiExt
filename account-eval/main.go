package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	_ "github.com/lib/pq"
)

func env(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

func main() {
	install := flag.Bool("install-menu", false, "Install the administrator menu via the Sub2API Admin API")
	publicURL := flag.String("public-url", "", "Public Sub2API origin with /account-eval/")
	flag.Parse()
	if err := run(*install, *publicURL); err != nil {
		// Driver/HTTP errors can contain connection strings. Only our public errors are printed.
		log.Print(err.Error())
		os.Exit(1)
	}
}

func run(install bool, publicURL string) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	pg, err := sql.Open("postgres", "connect_timeout=10 options='-c default_transaction_read_only=on'")
	if err != nil {
		return fmt.Errorf("无法配置主服务数据库连接")
	}
	defer pg.Close()
	pg.SetMaxOpenConns(4)
	checkCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	err = pg.PingContext(checkCtx)
	cancel()
	if err != nil {
		return fmt.Errorf("无法连接主服务数据库，请检查运行目录的数据库配置")
	}
	source := &Source{db: pg}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	defer transport.CloseIdleConnections()
	gateway := &Gateway{base: env("EVAL_SUB2API_URL", "http://sub2api:8080"), client: &http.Client{Transport: transport, Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
	if install {
		return installMenu(ctx, source, gateway, publicURL)
	}
	path := env("EVAL_DB_PATH", "/data/eval.db")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return fmt.Errorf("无法创建测评数据目录")
	}
	store, err := openStore(ctx, path)
	if err != nil {
		return fmt.Errorf("无法打开测评数据库")
	}
	defer store.db.Close()
	evaluator := &Evaluator{allowPrivate: env("EVAL_ALLOW_PRIVATE_HOSTS", "false") == "true"}
	engine := &Engine{store: store, load: source.credential, evaluate: evaluator.evaluate, wake: make(chan struct{}, 1)}
	server := &Server{store: store, source: source, engine: engine, gateway: gateway}
	httpServer := &http.Server{Addr: env("EVAL_LISTEN", ":8091"), Handler: server.handler(), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 60 * time.Second}
	engineDone := make(chan error, 1)
	go func() { engineDone <- engine.loop(ctx); stop() }()
	serverDone := make(chan error, 1)
	go func() { serverDone <- httpServer.ListenAndServe() }()
	log.Print("账户测评服务已启动")
	select {
	case <-ctx.Done():
	case err = <-serverDone:
		if !errors.Is(err, http.ErrServerClosed) {
			stop()
			<-engineDone
			return fmt.Errorf("测评 HTTP 服务启动失败")
		}
	}
	stop()
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	_ = httpServer.Shutdown(shutdownCtx)
	if err = <-engineDone; err != nil {
		return fmt.Errorf("测评状态无法保存，已停止发送请求；请检查数据卷及磁盘")
	}
	return nil
}

func installMenu(ctx context.Context, source *Source, gateway *Gateway, publicURL string) error {
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	key, err := source.setting(ctx, "admin_api_key")
	if err != nil {
		return fmt.Errorf("无法读取管理员 API 配置")
	}
	headers := http.Header{}
	if key != "" {
		headers.Set("x-api-key", key)
	} else {
		var credentials struct {
			Email    string `json:"email"`
			Password string `json:"password"`
		}
		if json.NewDecoder(io.LimitReader(os.Stdin, 65536)).Decode(&credentials) != nil || credentials.Email == "" || credentials.Password == "" {
			return fmt.Errorf("尚未配置 Admin API Key；请登录主站后打开 /account-eval/ 并点击“安装管理员入口”")
		}
		var login struct {
			AccessToken string `json:"access_token"`
		}
		if err := gateway.call(ctx, http.MethodPost, "/api/v1/auth/login", credentials, nil, &login); err != nil || login.AccessToken == "" {
			return fmt.Errorf("自动登录不可用；请登录主站后打开 /account-eval/ 并点击“安装管理员入口”")
		}
		headers.Set("Authorization", "Bearer "+login.AccessToken)
	}
	if _, err := gateway.menu(ctx, headers, Menu{URL: publicURL}); err != nil {
		return fmt.Errorf("安装管理员菜单失败：%w", err)
	}
	fmt.Println("管理员测评入口已安装，现有菜单已保留。")
	return nil
}
