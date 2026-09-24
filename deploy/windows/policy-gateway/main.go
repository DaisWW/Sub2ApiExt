package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/signal"
	"regexp"
	"strings"
	"syscall"
	"time"
)

const defaultConfigPath = "/etc/sub2api-policy/config.json"

type config struct {
	Enabled      bool         `json:"enabled"`
	Rules        []ruleConfig `json:"rules"`
	MaxBodyBytes int64        `json:"max_body_bytes"`
}

type ruleConfig struct {
	Name                    string   `json:"name"`
	Enabled                 bool     `json:"enabled"`
	Action                  string   `json:"action"`
	ClientUserAgentPatterns []string `json:"client_user_agent_patterns"`
	ModelPatterns           []string `json:"model_patterns"`
	RejectStatus            int      `json:"reject_status"`
}

type requestModel struct {
	Model string `json:"model"`
}

type policyHandler struct {
	proxy        *httputil.ReverseProxy
	rules        []compiledRule
	maxBodyBytes int64
}

type compiledRule struct {
	action         string
	clientPatterns []*regexp.Regexp
	modelPatterns  []*regexp.Regexp
	status         int
}

func main() {
	cfg := loadConfig()
	if !cfg.Enabled {
		log.Fatal("policy gateway is disabled")
	}

	upstream := strings.TrimSpace(os.Getenv("POLICY_UPSTREAM"))
	if upstream == "" {
		upstream = "http://sub2api:8080"
	}
	target, err := url.Parse(upstream)
	if err != nil || (target.Scheme != "http" && target.Scheme != "https") || target.Host == "" || target.User != nil {
		log.Fatal("invalid POLICY_UPSTREAM")
	}

	rules := compileRules(cfg)
	maxBodyBytes := cfg.MaxBodyBytes
	if maxBodyBytes <= 0 {
		maxBodyBytes = 64 * 1024 * 1024
	}

	proxy := httputil.NewSingleHostReverseProxy(target)
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil // The upstream is a Docker service on the internal network.
	defer transport.CloseIdleConnections()
	proxy.Transport = transport
	proxy.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, err error) {
		log.Printf("upstream proxy failed: %T", err)
		http.Error(w, "upstream unavailable", http.StatusBadGateway)
	}
	handler := &policyHandler{
		proxy:        proxy,
		rules:        rules,
		maxBodyBytes: maxBodyBytes,
	}

	listen := strings.TrimSpace(os.Getenv("POLICY_LISTEN"))
	if listen == "" {
		listen = ":8080"
	}
	server := &http.Server{
		Addr:              listen,
		Handler:           handler,
		ReadHeaderTimeout: 15 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-stop
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			log.Printf("shutdown failed: %v", err)
		}
	}()

	log.Printf("policy gateway listening on %s, upstream %s", listen, target.Host)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}

func loadConfig() config {
	path := strings.TrimSpace(os.Getenv("POLICY_CONFIG"))
	if path == "" {
		path = defaultConfigPath
	}
	body, err := os.ReadFile(path)
	if err != nil {
		log.Fatalf("read policy config: %v", err)
	}
	var cfg config
	if err := json.Unmarshal(body, &cfg); err != nil {
		log.Fatalf("parse policy config: %v", err)
	}
	return cfg
}

func compileRules(cfg config) []compiledRule {
	rules := make([]compiledRule, 0, len(cfg.Rules))
	for index, rule := range cfg.Rules {
		if !rule.Enabled {
			continue
		}
		name := strings.TrimSpace(rule.Name)
		if name == "" {
			name = fmt.Sprintf("rule-%d", index+1)
		}
		action := strings.ToLower(strings.TrimSpace(rule.Action))
		if action == "" {
			action = "deny"
		}
		if action != "deny" && action != "allow_models" {
			log.Fatalf("policy rule %q has unsupported action %q", name, rule.Action)
		}
		if len(rule.ClientUserAgentPatterns) == 0 || len(rule.ModelPatterns) == 0 {
			log.Fatalf("policy rule %q must define client_user_agent_patterns and model_patterns", name)
		}
		status := rule.RejectStatus
		if status < 400 || status > 599 {
			status = http.StatusForbidden
		}
		rules = append(rules, compiledRule{
			action:         action,
			clientPatterns: compilePatterns(name+" client_user_agent_patterns", rule.ClientUserAgentPatterns),
			modelPatterns:  compilePatterns(name+" model_patterns", rule.ModelPatterns),
			status:         status,
		})
	}
	if len(rules) == 0 {
		log.Fatal("policy config has no enabled rules")
	}
	return rules
}

func compilePatterns(name string, values []string) []*regexp.Regexp {
	patterns := make([]*regexp.Regexp, 0, len(values))
	for _, value := range values {
		pattern, err := regexp.Compile(value)
		if err != nil {
			log.Fatalf("invalid %s pattern %q: %v", name, value, err)
		}
		patterns = append(patterns, pattern)
	}
	return patterns
}

func (h *policyHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/health" {
		w.WriteHeader(http.StatusOK)
		return
	}
	userAgent := r.Header.Get("User-Agent")
	if !h.matchesAnyClient(userAgent) || r.Body == nil {
		h.proxy.ServeHTTP(w, r)
		return
	}

	originalBody := r.Body
	body, err := io.ReadAll(io.LimitReader(originalBody, h.maxBodyBytes+1))
	_ = originalBody.Close()
	if err != nil {
		http.Error(w, "failed to read request body", http.StatusBadRequest)
		return
	}
	if int64(len(body)) > h.maxBodyBytes {
		http.Error(w, "request body is too large for policy inspection", http.StatusRequestEntityTooLarge)
		return
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	r.ContentLength = int64(len(body))
	if len(body) == 0 {
		h.proxy.ServeHTTP(w, r)
		return
	}

	var parsed requestModel
	parseErr := json.Unmarshal(body, &parsed)
	model := strings.TrimSpace(parsed.Model)
	if parseErr != nil {
		if rule, ok := h.firstAllowModelsRule(userAgent); ok {
			writeRejected(w, rule.status, "request model is required by client policy")
			return
		}
		h.proxy.ServeHTTP(w, r)
		return
	}
	if rule, blocked := h.blockingRule(userAgent, model); blocked {
		writeRejected(w, rule.status, rejectionMessage(rule.action, model))
		return
	}
	h.proxy.ServeHTTP(w, r)
}

func (h *policyHandler) matchesAnyClient(userAgent string) bool {
	for _, rule := range h.rules {
		if matchesAny(rule.clientPatterns, userAgent) {
			return true
		}
	}
	return false
}

func (h *policyHandler) firstAllowModelsRule(userAgent string) (compiledRule, bool) {
	for _, rule := range h.rules {
		if rule.action == "allow_models" && matchesAny(rule.clientPatterns, userAgent) {
			return rule, true
		}
	}
	return compiledRule{}, false
}

func (h *policyHandler) blockingRule(userAgent, model string) (compiledRule, bool) {
	for _, rule := range h.rules {
		if !matchesAny(rule.clientPatterns, userAgent) {
			continue
		}
		switch rule.action {
		case "deny":
			if matchesAny(rule.modelPatterns, model) {
				return rule, true
			}
		case "allow_models":
			if !matchesAny(rule.modelPatterns, model) {
				return rule, true
			}
		}
	}
	return compiledRule{}, false
}

func matchesAny(patterns []*regexp.Regexp, value string) bool {
	for _, pattern := range patterns {
		if pattern.MatchString(value) {
			return true
		}
	}
	return false
}

func rejectionMessage(action, model string) string {
	if action == "allow_models" {
		if model == "" {
			return "request model is not allowed by client policy"
		}
		return fmt.Sprintf("client is only allowed to use configured models; model %q is not allowed", model)
	}
	return fmt.Sprintf("this client is not allowed to use model %q", model)
}

func writeRejected(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, `{"error":{"type":"permission_error","code":"client_model_policy_blocked","message":`)
	encoded, _ := json.Marshal(message)
	_, _ = w.Write(encoded)
	_, _ = io.WriteString(w, "}}")
}
