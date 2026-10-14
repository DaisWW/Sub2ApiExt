package main

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
)

type Question struct {
	Name   string `json:"name"`
	Prompt string `json:"prompt"`
	Format string `json:"format"`
}

type Suite struct {
	ID        string     `json:"id"`
	Name      string     `json:"name"`
	Questions []Question `json:"questions"`
}

type Account struct {
	ID                     int64                    `json:"id"`
	Name                   string                   `json:"name"`
	Platform               string                   `json:"platform"`
	Type                   string                   `json:"type"`
	Status                 string                   `json:"status"`
	GroupIDs               []int64                  `json:"group_ids"`
	Unsupported            string                   `json:"unsupported,omitempty"`
	Schedulable            bool                     `json:"schedulable"`
	RateLimitResetAt       *time.Time               `json:"rate_limit_reset_at,omitempty"`
	TempUnschedulableUntil *time.Time               `json:"temp_unschedulable_until,omitempty"`
	PrivacyMode            string                   `json:"privacy_mode"`
	Models                 []string                 `json:"models,omitempty"`
	ModelMetadata          map[string]ModelMetadata `json:"model_metadata,omitempty"`
}

type ModelMetadata struct {
	Reasoning                *bool    `json:"reasoning,omitempty"`
	SupportedReasoningLevels []string `json:"supported_reasoning_levels,omitempty"`
}

type Group struct {
	ID     int64               `json:"id"`
	Name   string              `json:"name"`
	Models map[string][]string `json:"models,omitempty"`
}

// Secrets only exist while making an upstream request; they cannot be serialized.
type CredentialAccount struct {
	Account
	Credentials map[string]any `json:"-"`
	Extra       map[string]any `json:"-"`
	ProxyURL    string         `json:"-"`
	ProxyError  string         `json:"-"`
}

type Settings struct {
	Models      map[string]string `json:"models"`
	Protocols   map[string]string `json:"protocols"`
	Reasoning   string            `json:"reasoning"`
	Concurrency int               `json:"concurrency"`
	Timeout     int               `json:"timeout_seconds"`
	MaxTokens   int               `json:"max_tokens"`
}

type Run struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Suite     Suite     `json:"suite"`
	Accounts  []Account `json:"accounts"`
	Settings  Settings  `json:"settings"`
	Status    string    `json:"status"`
	CreatedAt time.Time `json:"created_at"`
	ParentID  string    `json:"parent_id,omitempty"`
	Total     int       `json:"total"`
	Done      int       `json:"done"`
}

type Result struct {
	ID          int64  `json:"id"`
	RunID       string `json:"run_id"`
	AccountID   int64  `json:"account_id"`
	Question    int    `json:"question"`
	Status      string `json:"status"`
	Model       string `json:"model"`
	Answer      string `json:"answer,omitempty"`
	HTML        string `json:"html,omitempty"`
	Error       string `json:"error,omitempty"`
	DurationMS  int64  `json:"duration_ms"`
	FirstByteMS int64  `json:"first_byte_ms"`
	InputTokens int64  `json:"input_tokens"`
	OutTokens   int64  `json:"output_tokens"`
	Mark        string `json:"mark"`
}

func newID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}

func (s *Suite) validate() error {
	s.Name = strings.TrimSpace(s.Name)
	if s.Name == "" || len(s.Name) > 200 || len(s.Questions) == 0 || len(s.Questions) > 100 {
		return fmt.Errorf("请填写题组名称及 1–100 道题")
	}
	for i := range s.Questions {
		q := &s.Questions[i]
		if strings.TrimSpace(q.Name) == "" || strings.TrimSpace(q.Prompt) == "" || len(q.Prompt) > 200000 {
			return fmt.Errorf("第 %d 题需填写名称和问题，问题最多 200KB", i+1)
		}
		if q.Format != "html" && q.Format != "text" && q.Format != "json" {
			return fmt.Errorf("第 %d 题的展示格式无效", i+1)
		}
	}
	return nil
}

func (s Settings) validate(accounts []Account) error {
	if s.Concurrency < 1 || s.Concurrency > 32 || s.Timeout < 5 || s.Timeout > 3600 || s.MaxTokens < 0 {
		return fmt.Errorf("并发需为 1–32，超时需为 5–3600 秒，输出上限为 0 或正整数")
	}
	for _, a := range accounts {
		if a.Unsupported != "" {
			continue
		}
		if strings.TrimSpace(s.Models[a.Platform]) == "" {
			return fmt.Errorf("请为 %s 配置模型", a.Platform)
		}
		p := s.Protocols[a.Platform]
		if p != "" && p != "auto" && p != "responses" && p != "chat" {
			return fmt.Errorf("%s 的请求协议无效", a.Platform)
		}
		if s.Reasoning != "" && (a.Platform == "openai" || a.Platform == "openai_compatible") {
			info := a.ModelMetadata[strings.TrimSpace(s.Models[a.Platform])]
			if info.Reasoning != nil && !*info.Reasoning {
				return fmt.Errorf("账户 #%d 的模型不支持推理强度，请使用模型默认", a.ID)
			}
			if len(info.SupportedReasoningLevels) > 0 {
				supported := false
				for _, level := range info.SupportedReasoningLevels {
					if level == s.Reasoning {
						supported = true
						break
					}
				}
				if !supported {
					return fmt.Errorf("账户 #%d 的模型不支持推理强度 %s", a.ID, s.Reasoning)
				}
			}
		}
	}
	return nil
}

func supportReason(platform, kind string) string {
	api := kind == "api_key" || kind == "apikey" || kind == ""
	oauth := kind == "oauth" || kind == "setup-token" || kind == "setup_token"
	switch platform {
	case "openai", "anthropic", "claude":
		if api || oauth {
			return ""
		}
	case "openai_compatible", "grok", "xai", "gemini":
		if api {
			return ""
		}
	}
	return "暂不支持此账户协议"
}
