package pi

import (
	"context"
	"errors"
	"testing"

	"github.com/resolute-sh/pi-llm-go"
)

// TestSessionIDAndTransportFlowToLLMRequest verifies that SessionID and
// Transport reach LLMRequest on every turn, for both fresh and resumed
// sessions, and for both zero-value (auto) and explicit transports.
func TestSessionIDAndTransportFlowToLLMRequest(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		transport     llm.TransportPreference
		resumed       bool
		wantTransport llm.TransportPreference
	}{
		{
			name:          "default transport auto, fresh session",
			transport:     llm.TransportAuto,
			resumed:       false,
			wantTransport: llm.TransportAuto,
		},
		{
			name:          "explicit SSE transport, fresh session",
			transport:     llm.TransportSSE,
			resumed:       false,
			wantTransport: llm.TransportSSE,
		},
		{
			name:          "default transport auto, resumed session",
			transport:     llm.TransportAuto,
			resumed:       true,
			wantTransport: llm.TransportAuto,
		},
		{
			name:          "explicit SSE transport, resumed session",
			transport:     llm.TransportSSE,
			resumed:       true,
			wantTransport: llm.TransportSSE,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			provider := newRecordingProvider("test")
			a, err := NewAgent(AgentConfig{
				Providers:    []llm.LLMProvider{provider},
				DefaultModel: "test/model",
				Transport:    tt.transport,
			})
			if err != nil {
				t.Fatalf("NewAgent: %v", err)
			}

			runOnePrompt(t, a)
			firstSID := a.State().SessionID

			if tt.resumed {
				ctx := context.Background()
				stream, err := a.Prompt(ctx, NewText("user", "hi again"), PromptOpts{SessionID: firstSID})
				if err != nil {
					t.Fatalf("Prompt (resumed): %v", err)
				}
				_, result := drain(t, stream)
				if result.Err != nil {
					t.Fatalf("prompt error (resumed): %v", result.Err)
				}
			}

			req := provider.capturedReq()

			if req.SessionID == "" {
				t.Errorf("LLMRequest.SessionID is empty, want non-empty session id")
			}
			if req.SessionID != string(firstSID) {
				t.Errorf("LLMRequest.SessionID = %q, want %q", req.SessionID, string(firstSID))
			}
			if req.Transport != tt.wantTransport {
				t.Errorf("LLMRequest.Transport = %v, want %v", req.Transport, tt.wantTransport)
			}
		})
	}
}

// TestErrTransportUnsupportedPropagation verifies that a provider returning
// ErrTransportUnsupported delivers it through PromptResult.Err unchanged and
// is never retried — transport mismatch is a permanent configuration error,
// not a transient failure. The error arrives via stream.Done (not via an
// in-stream LLMErrorEvent), so the agent loop terminates on the first turn.
func TestErrTransportUnsupportedPropagation(t *testing.T) {
	t.Parallel()

	provider := newRecordingProvider("test")
	provider.streamErr = llm.ErrTransportUnsupported

	a, err := NewAgent(AgentConfig{
		Providers:    []llm.LLMProvider{provider},
		DefaultModel: "test/model",
		Transport:    llm.TransportWebSocket,
	})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}

	ctx := context.Background()
	stream, err := a.Prompt(ctx, NewText("user", "hi"), PromptOpts{})
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	_, result := drain(t, stream)
	if !errors.Is(result.Err, llm.ErrTransportUnsupported) {
		t.Errorf("result.Err = %v; want errors.Is(err, ErrTransportUnsupported) = true", result.Err)
	}
	if req := provider.capturedReq(); req.Transport != llm.TransportWebSocket {
		t.Errorf("LLMRequest.Transport = %v, want TransportWebSocket", req.Transport)
	}
	if n := provider.streamCallCount(); n != 1 {
		t.Errorf("Stream called %d time(s), want 1 (no retry on transport error)", n)
	}
}
