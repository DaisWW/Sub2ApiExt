package probe

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/DaisWW/Sub2ApiExt/monitoring/internal/model"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func TestSelectModelPrefersExplicitMonitorModel(t *testing.T) {
	account := model.Account{
		Platform:    "openai",
		RecentModel: "recent-model",
		Credentials: map[string]any{
			"monitor_model": "configured-model",
			"model_mapping": map[string]any{"recent-model": "mapped-model"},
		},
	}
	if got := selectModel(account, "default-model"); got != "configured-model" {
		t.Fatalf("got %q, want configured model", got)
	}
}

func TestSelectModelMapsRecentModel(t *testing.T) {
	account := model.Account{
		Platform:    "openai",
		RecentModel: "gpt-alias",
		Credentials: map[string]any{"model_mapping": map[string]any{
			"gpt-*":  "gpt-fallback",
			"gpt-a*": "gpt-specific",
		}},
	}
	if got := selectModel(account, "default-model"); got != "gpt-specific" {
		t.Fatalf("got %q, want longest wildcard mapping", got)
	}
}

func TestSelectModelUsesStableMappingTargetWithoutHistory(t *testing.T) {
	account := model.Account{
		Platform: "openai",
		Credentials: map[string]any{"model_mapping": map[string]string{
			"z-model": "z-upstream",
			"a-model": "a-upstream",
		}},
	}
	if got := selectModel(account, "default-model"); got != "a-upstream" {
		t.Fatalf("got %q, want first stable mapping target", got)
	}
}

func TestSelectModelFallsBackByPlatform(t *testing.T) {
	anthropic := model.Account{Platform: "anthropic"}
	if got := selectModel(anthropic, "default-model"); got != "claude-sonnet-4-5-20250929" {
		t.Fatalf("got %q, want anthropic fallback", got)
	}
	openAI := model.Account{Platform: "openai"}
	if got := selectModel(openAI, "default-model"); got != "default-model" {
		t.Fatalf("got %q, want configured default", got)
	}
	grok := model.Account{Platform: "grok"}
	if got := selectModel(grok, "default-model"); got != "grok-4.5" {
		t.Fatalf("got %q, want grok fallback", got)
	}
	oauth := model.Account{Platform: "openai", Type: "oauth"}
	if got := selectModel(oauth, "default-model"); got != "gpt-5.4" {
		t.Fatalf("got %q, want OAuth fallback", got)
	}
}

func TestBuildRequestUsesOpenAICompatibleGrokEndpoint(t *testing.T) {
	request, err := buildRequest(context.Background(), model.Account{Platform: "grok"}, "https://example.com/v1", "secret", "grok-4.5")
	if err != nil {
		t.Fatal(err)
	}
	if got := request.URL.String(); got != "https://example.com/v1/chat/completions" {
		t.Fatalf("got endpoint %q", got)
	}
	if got := request.Header.Get("Authorization"); got != "Bearer secret" {
		t.Fatalf("got authorization %q", got)
	}
	body, err := io.ReadAll(request.Body)
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatal(err)
	}
	if payload["model"] != "grok-4.5" {
		t.Fatalf("got payload model %#v", payload["model"])
	}
}

func TestBuildRequestKeepsGeminiAPIKeyOutOfURL(t *testing.T) {
	for _, accountType := range []string{"api_key", "apikey", ""} {
		request, err := buildRequest(context.Background(), model.Account{Platform: "gemini", Type: accountType}, "https://example.com", "secret", "gemini-2.0-flash")
		if err != nil {
			t.Fatal(err)
		}
		if got := request.URL.String(); got != "https://example.com/v1beta/models/gemini-2.0-flash:generateContent" {
			t.Fatalf("type %q: got endpoint %q", accountType, got)
		}
		if got := request.Header.Get("x-goog-api-key"); got != "secret" {
			t.Fatalf("type %q: got API key header %q", accountType, got)
		}
	}
}

func TestBuildRequestUsesBearerForGeminiOAuth(t *testing.T) {
	request, err := buildRequest(context.Background(), model.Account{Platform: "gemini", Type: "oauth"}, "https://example.com", "secret", "gemini-2.0-flash")
	if err != nil {
		t.Fatal(err)
	}
	if got := request.Header.Get("Authorization"); got != "Bearer secret" {
		t.Fatalf("got authorization %q", got)
	}
	if got := request.Header.Get("x-goog-api-key"); got != "" {
		t.Fatalf("OAuth request should not set API key header, got %q", got)
	}
}

func TestBuildRequestUsesOpenAIOAuthProtocol(t *testing.T) {
	account := model.Account{Platform: "openai", Type: "oauth", ChatGPTAccount: "account-123"}
	request, err := buildRequest(context.Background(), account, "https://unused.example.com", "secret", "gpt-5.4")
	if err != nil {
		t.Fatal(err)
	}
	if got := request.URL.String(); got != openAIOAuthEndpoint {
		t.Fatalf("got endpoint %q", got)
	}
	if got := request.Header.Get("Accept"); got != "text/event-stream" {
		t.Fatalf("got Accept %q", got)
	}
	if got := request.Header.Get("Authorization"); got != "Bearer secret" {
		t.Fatalf("got authorization %q", got)
	}
	if got := request.Header.Get("chatgpt-account-id"); got != "account-123" {
		t.Fatalf("got ChatGPT account header %q", got)
	}
	var payload map[string]any
	if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	if payload["stream"] != true || payload["store"] != false {
		t.Fatalf("unexpected OAuth payload flags: %#v", payload)
	}
	input, ok := payload["input"].([]any)
	if !ok || len(input) != 1 {
		t.Fatalf("OAuth input must use Responses API structure: %#v", payload["input"])
	}
	if _, exists := payload["max_output_tokens"]; exists {
		t.Fatal("OAuth probe must not use an artificially small output limit")
	}
}

func TestBuildRequestUsesAnthropicOAuthProtocol(t *testing.T) {
	request, err := buildRequest(context.Background(), model.Account{Platform: "anthropic", Type: "setup-token"}, "https://unused.example.com", "secret", "claude-sonnet-4-5-20250929")
	if err != nil {
		t.Fatal(err)
	}
	if got := request.URL.String(); got != anthropicOAuthEndpoint {
		t.Fatalf("got endpoint %q", got)
	}
	if got := request.Header.Get("Authorization"); got != "Bearer secret" {
		t.Fatalf("got authorization %q", got)
	}
	if got := request.Header.Get("x-api-key"); got != "" {
		t.Fatalf("OAuth request should not set API key header, got %q", got)
	}
	if got := request.Header.Get("anthropic-beta"); !strings.Contains(got, "oauth-2025-04-20") {
		t.Fatalf("OAuth beta header is missing: %q", got)
	}
}

func TestSupportsAccountRejectsProtocolsRequiringDedicatedSigners(t *testing.T) {
	unsupported := []model.Account{
		{Platform: "anthropic", Type: "bedrock"},
		{Platform: "gemini", Type: "service_account"},
		{Platform: "gemini", Type: "oauth", Credentials: map[string]any{"project_id": "project"}},
		{Platform: "antigravity", Type: "oauth"},
	}
	for _, account := range unsupported {
		if SupportsAccount(account) {
			t.Fatalf("unexpected active probe support for %s/%s", account.Platform, account.Type)
		}
	}
}

func TestAccountBaseURLAppendsAntigravityGatewayPrefix(t *testing.T) {
	account := model.Account{
		Platform:    "antigravity",
		Type:        "api_key",
		Credentials: map[string]any{"base_url": "https://gateway.example.com/"},
	}
	if got := accountBaseURL(account); got != "https://gateway.example.com/antigravity" {
		t.Fatalf("accountBaseURL() = %q", got)
	}
}

func TestNetworkErrorMessageDoesNotEchoRequestDetails(t *testing.T) {
	message := networkErrorMessage(fmt.Errorf("Post https://example.com/?key=secret: dial tcp: i/o timeout"))
	if message != "上游请求超时" {
		t.Fatalf("got %q", message)
	}
}

func TestResponseMessageUsesStatusOnly(t *testing.T) {
	message := responseMessage(502)
	if message != "上游返回 HTTP 502" {
		t.Fatalf("got %q", message)
	}
}

func TestDoRequestRecordsLatencyOnNetworkError(t *testing.T) {
	prober := New(Config{Timeout: time.Second})
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("dial tcp: connection refused")
	})}
	request := httptest.NewRequest(http.MethodPost, "https://example.com/v1/chat/completions", nil)
	result := prober.doRequest(client, request, model.ProbeResult{CheckedAt: time.Now().UTC()})
	if result.Status != model.StatusError {
		t.Fatalf("got status %q", result.Status)
	}
	if result.LatencyMs == nil || *result.LatencyMs < 1 {
		t.Fatalf("network error should include latency, got %+v", result.LatencyMs)
	}
}

func TestClientForAcceptsConfiguredPrivateProxy(t *testing.T) {
	prober := New(Config{Timeout: time.Second})
	if _, err := prober.clientFor("http://127.0.0.1:8080"); err != nil {
		t.Fatalf("configured proxy should be allowed: %v", err)
	}
	if _, err := prober.clientFor("socks5://user:password@127.0.0.1:1080"); err != nil {
		t.Fatalf("SOCKS5 proxy should be supported: %v", err)
	}
}

func TestProbeRejectsPrivateTargetEvenWithProxy(t *testing.T) {
	prober := New(Config{Timeout: time.Second})
	result := prober.Probe(context.Background(), model.Account{
		ID:          1,
		Platform:    "openai",
		Type:        "apikey",
		Status:      "active",
		Schedulable: true,
		Credentials: map[string]any{"api_key": "secret", "base_url": "http://127.0.0.1:9000"},
		ProxyURL:    "http://127.0.0.1:8080",
	})
	if result.Status != model.StatusError || result.ErrorClass != "configuration" {
		t.Fatalf("private target should fail before proxy execution: %+v", result)
	}
}
