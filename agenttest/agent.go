// Package agenttest provides test helpers for pi-core-agent-go.
package agenttest

import (
	"testing"

	"github.com/resolute-sh/pi-llm-go"
	"github.com/resolute-sh/pi-llm-go/mock"
	"github.com/resolute-sh/pi-core-agent-go"
	"github.com/resolute-sh/pi-core-agent-go/session"
)

// Opts carries options for NewAgent.
type Opts struct {
	Provider     llm.LLMProvider
	Tools        []pi.RegisteredTool
	Hooks        pi.Hooks
	SystemPrompt string
	Session      pi.SessionRepo
}

// NewAgent creates an Agent with sensible test defaults and registers t.Cleanup.
func NewAgent(t *testing.T, opts Opts) *pi.Agent {
	t.Helper()

	provider := opts.Provider
	if provider == nil {
		provider = mock.New("mock")
	}

	s := opts.Session
	if s == nil {
		s = session.NewMemorySession()
	}

	cfg := pi.AgentConfig{
		Providers:    []llm.LLMProvider{provider},
		DefaultModel: "mock/test",
		SystemPrompt: opts.SystemPrompt,
		Tools:        opts.Tools,
		Hooks:        opts.Hooks,
		Session:      s,
	}

	agent, err := pi.NewAgent(cfg)
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}

	return agent
}
