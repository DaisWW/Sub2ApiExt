package probe

import (
	"context"
	"encoding/json"
	"io"
	"testing"

	"github.com/DaisWW/Sub2ApiExt/monitoring/internal/model"
)

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
	if got := selectModel(anthropic, "default-model"); got != "claude-3-5-haiku-latest" {
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
