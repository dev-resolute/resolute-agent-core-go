// Command providers constructs an agent with Gemini plus the six OpenAI-compatible
// targets (OpenAI, OpenCode Zen, xAI, Mistral, Qwen, z.ai) and routes one prompt by
// "<provider>/<model>" ref. Each provider is registered only when its API key is in
// the environment, so the program compiles and runs with any subset of keys.
//
// Usage:
//
//	MODEL=xai/grok-3-mini XAI_API_KEY=… go run ./examples/providers
//	GEMINI_API_KEY=… go run ./examples/providers   # defaults to gemini/gemini-2.5-flash
package main

import (
	"context"
	"fmt"
	"log"
	"os"

	pi "github.com/dev-resolute/resolute-agent-core-go"
	"github.com/dev-resolute/resolute-agent-core-go/session"
	"github.com/dev-resolute/resolute-llm-go"
	"github.com/dev-resolute/resolute-llm-go/gemini"
	openaicompat "github.com/dev-resolute/resolute-llm-go/openai-compat"
)

func main() {
	providers := registerProviders()
	if len(providers) == 0 {
		log.Fatal("no provider keys set; export e.g. GEMINI_API_KEY and retry")
	}
	for _, p := range providers {
		fmt.Printf("registered: %s\n", p.Name())
	}

	model := os.Getenv("MODEL")
	if model == "" {
		model = "gemini/gemini-2.5-flash"
	}

	agent, err := pi.NewAgent(pi.AgentConfig{
		Providers:    providers,
		DefaultModel: model,
		Session:      session.NewMemorySession(),
	})
	if err != nil {
		log.Fatalf("NewAgent: %v", err)
	}

	stream, err := agent.Prompt(context.Background(),
		pi.NewText("user", "In one sentence, what are you?"), pi.PromptOpts{})
	if err != nil {
		log.Fatalf("prompt %q: %v", model, err)
	}
	for ev := range stream.Events {
		if d, ok := ev.(pi.TextDeltaEvent); ok {
			fmt.Print(d.Delta)
		}
	}
	fmt.Println()
	if res := <-stream.Done; res.Err != nil {
		log.Fatalf("stream: %v", res.Err)
	}
}

// registerProviders maps the seven supported targets onto Gemini (native) and the
// openai-compat adapter, skipping any whose key is absent so the wiring is the only
// thing the example fixes — the caller chooses which to enable via the environment.
func registerProviders() []llm.LLMProvider {
	var providers []llm.LLMProvider

	if key := os.Getenv("GEMINI_API_KEY"); key != "" {
		p, err := gemini.New(gemini.Config{APIKey: key})
		if err != nil {
			log.Fatalf("gemini: %v", err)
		}
		providers = append(providers, p)
	}
	if key := os.Getenv("OPENAI_API_KEY"); key != "" {
		p, err := openaicompat.New(openaicompat.Config{Name: "openai", BaseURL: "https://api.openai.com/v1", APIKey: key})
		if err != nil {
			log.Fatalf("openai: %v", err)
		}
		providers = append(providers, p)
	}
	if key := os.Getenv("OPENCODE_API_KEY"); key != "" {
		p, err := openaicompat.New(openaicompat.Config{
			Name:    "opencode",
			BaseURL: "https://opencode.ai/zen/go/v1",
			APIKey:  key,
			Compat: openaicompat.Compat{
				ThinkingFormat: openaicompat.ThinkingDeepSeek,
				RequiresReasoningContentOnAssistantMessages: true,
				MaxTokens: 8000,
			},
		})
		if err != nil {
			log.Fatalf("opencode: %v", err)
		}
		providers = append(providers, p)
	}

	for _, t := range []struct {
		name string
		env  string
		ctor func(openaicompat.TargetConfig) (llm.LLMProvider, error)
	}{
		{"xai", "XAI_API_KEY", openaicompat.XAI},
		{"mistral", "MISTRAL_API_KEY", openaicompat.Mistral},
		{"qwen", "DASHSCOPE_API_KEY", openaicompat.Qwen},
		{"zai", "ZAI_API_KEY", openaicompat.ZAI},
	} {
		if key := os.Getenv(t.env); key != "" {
			p, err := t.ctor(openaicompat.TargetConfig{APIKey: key})
			if err != nil {
				log.Fatalf("%s: %v", t.name, err)
			}
			providers = append(providers, p)
		}
	}

	return providers
}
