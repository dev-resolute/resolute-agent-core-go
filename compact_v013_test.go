package resolute

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/dev-resolute/resolute-llm-go"
)

// lengthStopProvider answers every Stream call with text and a Length stop
// reason, simulating a summarization call truncated at the output limit.
type lengthStopProvider struct{}

func (*lengthStopProvider) Name() string { return "test" }
func (*lengthStopProvider) Capabilities(string) llm.ProviderCapabilities {
	return llm.ProviderCapabilities{Streaming: true}
}
func (*lengthStopProvider) Stream(ctx context.Context, req llm.LLMRequest) llm.EventStream {
	events := make(chan llm.LLMEvent, 2)
	done := make(chan llm.StreamResult, 1)
	events <- llm.TextDeltaEvent{Delta: "partial summary"}
	events <- llm.MessageEndEvent{StopReason: llm.StopReasonLength}
	close(events)
	done <- llm.StreamResult{}
	close(done)
	return llm.NewEventStream(events, done)
}

// A summary truncated at the output token limit is rejected — never
// persisted (upstream #7048).
func TestCompactRejectsTruncatedSummary(t *testing.T) {
	repo, sid := buildCompactableSession(t)
	a, err := NewAgent(AgentConfig{
		Providers:        []llm.LLMProvider{&lengthStopProvider{}},
		DefaultModel:     "test/m",
		Session:          repo,
		KeepRecentTokens: 50,
		ConvertToLLM:     DefaultConvertToLLM,
	})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}

	_, err = a.Compact(context.Background(), CompactOpts{SessionID: sid})
	if !errors.Is(err, ErrSummaryTruncated) {
		t.Fatalf("Compact err = %v, want ErrSummaryTruncated", err)
	}

	summaries, lerr := repo.LoadBranchSummaries(context.Background(), sid)
	if lerr != nil {
		t.Fatalf("LoadBranchSummaries: %v", lerr)
	}
	if len(summaries) != 0 {
		t.Fatalf("persisted %d summaries, want 0 (truncated summary must not persist)", len(summaries))
	}
}

// A truncated summary is not retried even when a retry policy is configured
// (deterministic failure).
func TestTruncatedSummaryNotRetried(t *testing.T) {
	p := &lengthStopProviderCounted{}
	a, err := NewAgent(AgentConfig{
		Providers:    []llm.LLMProvider{p},
		DefaultModel: "test/m",
		ConvertToLLM: DefaultConvertToLLM,
		SummarizationRetry: SummarizationRetryPolicy{
			MaxRetries: 3,
			BaseDelay:  1,
		},
	})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}
	_, _, err = a.summarizeWithLLM(context.Background(), p, "m", []Message{NewText("user", "hi")})
	if !errors.Is(err, ErrSummaryTruncated) {
		t.Fatalf("err = %v, want ErrSummaryTruncated", err)
	}
	if p.calls != 1 {
		t.Errorf("provider calls = %d, want 1 (no retry on truncation)", p.calls)
	}
}

type lengthStopProviderCounted struct {
	lengthStopProvider
	calls int
}

func (p *lengthStopProviderCounted) Stream(ctx context.Context, req llm.LLMRequest) llm.EventStream {
	p.calls++
	return p.lengthStopProvider.Stream(ctx, req)
}

// CompactionSettings.ForModel resolves per-model budget overrides with the
// base settings as fallback (upstream 0.86.0 compaction.modelOverrides).
func TestCompactionSettingsForModel(t *testing.T) {
	base := CompactionSettings{
		Enabled:          true,
		ReserveTokens:    16384,
		KeepRecentTokens: 20000,
		ModelOverrides: map[string]CompactionBudget{
			"gemini/gemini-3.1-pro": {ReserveTokens: 32768},
			"mistral-medium":        {KeepRecentTokens: 8000},
		},
	}

	full := base.ForModel("gemini/gemini-3.1-pro")
	if full.ReserveTokens != 32768 || full.KeepRecentTokens != 20000 {
		t.Errorf("full-ref override = %+v, want Reserve 32768 / KeepRecent 20000", full)
	}

	bare := base.ForModel("mistral/mistral-medium")
	if bare.ReserveTokens != 16384 || bare.KeepRecentTokens != 8000 {
		t.Errorf("bare-id override = %+v, want Reserve 16384 / KeepRecent 8000", bare)
	}

	none := base.ForModel("other/model")
	if none.ReserveTokens != 16384 || none.KeepRecentTokens != 20000 {
		t.Errorf("unmatched model = %+v, want base budgets", none)
	}

	empty := base.ForModel("")
	if empty.ReserveTokens != 16384 || empty.KeepRecentTokens != 20000 {
		t.Errorf("empty model = %+v, want base budgets", empty)
	}
}

// Compact applies the per-model override from AgentConfig.
func TestCompactUsesModelOverride(t *testing.T) {
	// With KeepRecentTokens 50 the session compacts; an override raising it
	// past the session size leaves nothing to compact.
	repo, sid := buildCompactableSession(t)
	a, err := NewAgent(AgentConfig{
		Providers:        []llm.LLMProvider{&usageEmittingProvider{texts: []string{"summary"}}},
		DefaultModel:     "test/m",
		Session:          repo,
		KeepRecentTokens: 50,
		CompactionModelOverrides: map[string]CompactionBudget{
			"test/m": {KeepRecentTokens: 1 << 30},
		},
		ConvertToLLM: DefaultConvertToLLM,
	})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}
	res, err := a.Compact(context.Background(), CompactOpts{SessionID: sid})
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if res.RemovedCount != 0 {
		t.Errorf("RemovedCount = %d, want 0 (override raised KeepRecentTokens past session size)", res.RemovedCount)
	}
}

// Registration rejects tools with empty or invalid parameter schemas
// (upstream #9300).
func TestValidateToolConfigRejectsBadSchema(t *testing.T) {
	good := NewDynamicTool("good", "d", []byte(`{"type":"object"}`), func(context.Context, string, json.RawMessage) (ToolResult, error) {
		return ToolResult{}, nil
	})
	empty := NewDynamicTool("empty", "d", nil, func(context.Context, string, json.RawMessage) (ToolResult, error) {
		return ToolResult{}, nil
	})
	invalid := NewDynamicTool("invalid", "d", []byte(`{not json`), func(context.Context, string, json.RawMessage) (ToolResult, error) {
		return ToolResult{}, nil
	})

	if err := validateToolConfig([]RegisteredTool{good}, nil); err != nil {
		t.Errorf("good tool rejected: %v", err)
	}
	for _, bad := range []RegisteredTool{empty, invalid} {
		if err := validateToolConfig([]RegisteredTool{bad}, nil); !errors.Is(err, ErrInvalidToolSchema) {
			t.Errorf("tool %q: err = %v, want ErrInvalidToolSchema", bad.Name(), err)
		}
	}

	if _, err := NewAgent(AgentConfig{
		Providers:    []llm.LLMProvider{&lengthStopProvider{}},
		DefaultModel: "test/m",
		Tools:        []RegisteredTool{empty},
		ConvertToLLM: DefaultConvertToLLM,
	}); !errors.Is(err, ErrInvalidToolSchema) {
		t.Errorf("NewAgent err = %v, want ErrInvalidToolSchema", err)
	}

	ag, err := NewAgent(AgentConfig{
		Providers:    []llm.LLMProvider{&lengthStopProvider{}},
		DefaultModel: "test/m",
		ConvertToLLM: DefaultConvertToLLM,
	})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}
	if err := ag.SetTools([]RegisteredTool{empty}); !errors.Is(err, ErrInvalidToolSchema) {
		t.Errorf("SetTools err = %v, want ErrInvalidToolSchema", err)
	}
}

// ConstrainedSampling on a typed tool reaches the prompt loop's ToolDef
// conversion through the capability assertion.
func TestConstrainedSamplingCapability(t *testing.T) {
	var withCS RegisteredTool = NewTool(Tool[struct{}]{
		Name:                "t",
		Description:         "d",
		ConstrainedSampling: &llm.ConstrainedSampling{Strict: llm.StrictPrefer},
		Execute:             func(context.Context, struct{}) (ToolResult, error) { return ToolResult{}, nil },
	})
	st, ok := withCS.(constrainedSamplingTool)
	if !ok {
		t.Fatal("typed tool does not implement constrainedSamplingTool")
	}
	if st.ToolConstrainedSampling() == nil || st.ToolConstrainedSampling().Strict != llm.StrictPrefer {
		t.Errorf("sampling = %+v, want prefer", st.ToolConstrainedSampling())
	}

	plain := NewTool(Tool[struct{}]{
		Name:    "p",
		Execute: func(context.Context, struct{}) (ToolResult, error) { return ToolResult{}, nil },
	})
	st2, ok := plain.(constrainedSamplingTool)
	if !ok || st2.ToolConstrainedSampling() != nil {
		t.Errorf("plain tool sampling = %+v (ok=%v), want nil", st2, ok)
	}

	dyn := NewDynamicTool("d", "d", []byte(`{}`),
		func(context.Context, string, json.RawMessage) (ToolResult, error) { return ToolResult{}, nil },
		WithConstrainedSampling(&llm.ConstrainedSampling{Strict: llm.StrictRequire}))
	st3, ok := dyn.(constrainedSamplingTool)
	if !ok || st3.ToolConstrainedSampling().Strict != llm.StrictRequire {
		t.Errorf("dynamic tool sampling = %+v (ok=%v), want require", st3, ok)
	}
}

// sanity: the truncation sentinel message names the limit.
func TestErrSummaryTruncatedMessage(t *testing.T) {
	if !strings.Contains(ErrSummaryTruncated.Error(), "output token limit") {
		t.Errorf("message = %q, want mention of output token limit", ErrSummaryTruncated)
	}
}
