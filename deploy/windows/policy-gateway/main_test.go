package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"testing"
)

func TestClientModelRules(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read forwarded body: %v", err)
			return
		}
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write(body)
	}))
	defer upstream.Close()

	target, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	handler := &policyHandler{
		proxy:        httputil.NewSingleHostReverseProxy(target),
		maxBodyBytes: 1024,
		rules: compileRules(config{Rules: []ruleConfig{
			{
				Enabled: true, Action: "deny",
				ClientUserAgentPatterns: []string{"^claude-cli/"},
				ModelPatterns:           []string{"^gpt-"},
			},
			{
				Enabled: true, Action: "allow_models",
				ClientUserAgentPatterns: []string{"^claude-cli/"},
				ModelPatterns:           []string{"^claude-"},
			},
			{
				Enabled: true, Action: "deny",
				ClientUserAgentPatterns: []string{"^other-cli/"},
				ModelPatterns:           []string{"^gpt-"},
			},
		}}),
	}

	tests := []struct {
		name   string
		agent  string
		body   string
		status int
	}{
		{"claude code with Claude", "claude-cli/1", `{"model":"claude-sonnet"}`, http.StatusAccepted},
		{"claude code with GPT", "claude-cli/1", `{"model":"gpt-6"}`, http.StatusForbidden},
		{"claude code with other model", "claude-cli/1", `{"model":"gemini-3"}`, http.StatusForbidden},
		{"claude code missing model", "claude-cli/1", `{}`, http.StatusForbidden},
		{"claude code invalid JSON", "claude-cli/1", `{`, http.StatusForbidden},
		{"other client with GPT", "other-cli/1", `{"model":"gpt-6"}`, http.StatusForbidden},
		{"other client with Claude", "other-cli/1", `{"model":"claude-sonnet"}`, http.StatusAccepted},
		{"unmatched client with GPT", "ordinary/1", `{"model":"gpt-6"}`, http.StatusAccepted},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(test.body))
			request.Header.Set("User-Agent", test.agent)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != test.status {
				t.Fatalf("status = %d, want %d: %s", response.Code, test.status, response.Body.String())
			}
			if test.status == http.StatusAccepted && response.Body.String() != test.body {
				t.Fatalf("forwarded body = %q, want %q", response.Body.String(), test.body)
			}
		})
	}
}
