package pi

import (
	"errors"
	"testing"

	"github.com/dev-resolute/resolute-llm-go"
	"github.com/dev-resolute/resolute-llm-go/mock"
	openaicompat "github.com/dev-resolute/resolute-llm-go/openai-compat"
)

// TestRegistryResolvesNamedCompatProviders proves the generic provider registry
// routes a "<provider>/<model>" ref to the right provider when Gemini and all four
// LLM-10 compat targets (each with a distinct Name) coexist in one agent.
func TestRegistryResolvesNamedCompatProviders(t *testing.T) {
	xai, err := openaicompat.XAI(openaicompat.TargetConfig{APIKey: "test"})
	if err != nil {
		t.Fatalf("XAI: %v", err)
	}
	mistral, err := openaicompat.Mistral(openaicompat.TargetConfig{APIKey: "test"})
	if err != nil {
		t.Fatalf("Mistral: %v", err)
	}
	qwen, err := openaicompat.Qwen(openaicompat.TargetConfig{APIKey: "test"})
	if err != nil {
		t.Fatalf("Qwen: %v", err)
	}
	zai, err := openaicompat.ZAI(openaicompat.TargetConfig{APIKey: "test"})
	if err != nil {
		t.Fatalf("ZAI: %v", err)
	}

	a, err := NewAgent(AgentConfig{
		Providers:    []llm.LLMProvider{mock.New("gemini"), xai, mistral, qwen, zai},
		DefaultModel: "gemini/gemini-2.5-flash",
	})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}

	tests := []struct {
		ref          string
		wantProvider string
		wantModel    string
	}{
		{ref: "gemini/gemini-2.5-flash", wantProvider: "gemini", wantModel: "gemini-2.5-flash"},
		{ref: "xai/grok-4", wantProvider: "xai", wantModel: "grok-4"},
		{ref: "mistral/mistral-large-latest", wantProvider: "mistral", wantModel: "mistral-large-latest"},
		{ref: "qwen/qwen-plus", wantProvider: "qwen", wantModel: "qwen-plus"},
		{ref: "zai/glm-4.6", wantProvider: "zai", wantModel: "glm-4.6"},
	}
	for _, tt := range tests {
		t.Run(tt.ref, func(t *testing.T) {
			// given a provider/model ref
			// when it is parsed and resolved against the registry
			name, model, err := parseModelRef(tt.ref)
			if err != nil {
				t.Fatalf("parseModelRef(%q): %v", tt.ref, err)
			}
			if name != tt.wantProvider || model != tt.wantModel {
				t.Fatalf("parseModelRef(%q) = (%q, %q), want (%q, %q)", tt.ref, name, model, tt.wantProvider, tt.wantModel)
			}

			// then the registry returns the provider whose Name matches the prefix
			p, err := a.providerByName(name)
			if err != nil {
				t.Fatalf("providerByName(%q): %v", name, err)
			}
			if p.Name() != tt.wantProvider {
				t.Errorf("providerByName(%q).Name() = %q, want %q", name, p.Name(), tt.wantProvider)
			}
		})
	}

	// and an unregistered provider name is rejected
	if _, err := a.providerByName("unknown"); !errors.Is(err, ErrInvalidModelRef) {
		t.Errorf("providerByName(unknown) err = %v, want errors.Is ErrInvalidModelRef", err)
	}
}
