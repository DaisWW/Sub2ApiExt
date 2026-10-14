package main

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
)

func TestModelCatalogAliasesAndWildcardRules(t *testing.T) {
	models, metadata, err := accountModelCatalog([]byte(`{"alias":"gpt-6.1-sol","gpt-*":"target","blank":""}`), []byte(`{"gpt-6.1-sol":{"reasoning":true,"supported_reasoning_levels":["high","max"],"secret":"hidden"}}`))
	if err != nil || !reflect.DeepEqual(models, []string{"alias", "blank", "gpt-6.1-sol"}) || !reflect.DeepEqual(metadata["alias"].SupportedReasoningLevels, []string{"high", "max"}) {
		t.Fatalf("account models: %v %v %v", models, metadata, err)
	}
	_, metadata, err = accountModelCatalog([]byte(`{"alias":"m","m":"target"}`), []byte(`{"m":{"supported_reasoning_levels":["high"]},"target":{"supported_reasoning_levels":["max"]}}`))
	if err != nil || !reflect.DeepEqual(metadata["alias"].SupportedReasoningLevels, []string{"high"}) || !reflect.DeepEqual(metadata["m"].SupportedReasoningLevels, []string{"max"}) {
		t.Fatal("alias capabilities were chained instead of mapped once")
	}
	channels, err := channelModelCatalog([]byte(`{"openai":{"alias":"target","gpt-*":"target"}}`), []byte(`[{"platform":"openai","models":["alias","priced","gpt-*"]},{"platform":"anthropic","models":["claude"]}]`), []byte(`null`))
	if err != nil || !reflect.DeepEqual(channels["openai"], []string{"alias", "priced"}) || !reflect.DeepEqual(channels["anthropic"], []string{"claude"}) {
		t.Fatalf("channel models: %v %v", channels, err)
	}
	models, metadata, err = accountModelCatalog([]byte(`null`), []byte(`null`))
	if err != nil || len(models) != 0 || len(metadata) != 0 {
		t.Fatalf("null catalog: %v %v %v", models, metadata, err)
	}
}

func TestReasoningMaxRequestsAndKnownCapabilities(t *testing.T) {
	for _, platform := range []string{"openai", "openai_compatible", "grok", "xai"} {
		for _, protocol := range []string{"chat", "responses"} {
			t.Run(platform+"/"+protocol, func(t *testing.T) {
				a := CredentialAccount{Account: Account{Platform: platform, Type: "apikey"}, Credentials: map[string]any{"api_key": "fixture-key"}}
				s := Settings{Models: map[string]string{platform: "fixture-model"}, Protocols: map[string]string{platform: protocol}, Reasoning: "max"}
				req, _, _, err := buildRequest(context.Background(), a, s, "full question")
				if err != nil {
					t.Fatal(err)
				}
				defer req.Body.Close()
				var body map[string]any
				if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
					t.Fatal(err)
				}
				if platform == "openai" || platform == "openai_compatible" {
					effort := body["reasoning_effort"]
					if protocol == "responses" {
						effort = obj(body["reasoning"])["effort"]
					}
					if effort != "max" {
						t.Fatalf("max was changed or omitted: %v", body)
					}
				} else if body["reasoning"] != nil || body["reasoning_effort"] != nil {
					t.Fatal("OpenAI effort sent to another platform")
				}
			})
		}
	}
	accounts := []Account{{ID: 1, Platform: "openai", ModelMetadata: map[string]ModelMetadata{"m": {SupportedReasoningLevels: []string{"high", "max"}}}}, {ID: 2, Platform: "openai", ModelMetadata: map[string]ModelMetadata{"m": {SupportedReasoningLevels: []string{"high"}}}}}
	s := Settings{Models: map[string]string{"openai": "m"}, Reasoning: "max", Concurrency: 2, Timeout: 30}
	if err := s.validate(accounts[:1]); err != nil {
		t.Fatal(err)
	}
	if err := s.validate(accounts); err == nil {
		t.Fatal("accepted effort outside the selected account capabilities")
	}
	s.Models["openai"] = " m "
	if err := s.validate(accounts); err == nil {
		t.Fatal("whitespace bypassed model capability validation")
	}
	s.Models["openai"] = "m"
	s.Reasoning = "high"
	if err := s.validate(accounts); err != nil {
		t.Fatal(err)
	}
	noReasoning := false
	accounts[0].ModelMetadata["m"] = ModelMetadata{Reasoning: &noReasoning}
	if err := s.validate(accounts[:1]); err == nil {
		t.Fatal("accepted effort for a model without reasoning")
	}
	s.Reasoning = ""
	if err := s.validate(accounts); err != nil {
		t.Fatal(err)
	}
}
