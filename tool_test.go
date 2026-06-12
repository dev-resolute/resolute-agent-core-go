package pi

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/resolute-sh/pi-llm-go"
)

func TestToolSchemaReflection(t *testing.T) {
	type AddParams struct {
		A int `json:"a" jsonschema:"description=First number to add"`
		B int `json:"b" jsonschema:"description=Second number to add"`
	}

	tool := NewTool(Tool[AddParams]{
		Name:        "add",
		Description: "Add two numbers",
		Execute: func(ctx context.Context, p AddParams) (ToolResult, error) {
			return ToolResult{Content: "ok"}, nil
		},
	})

	schema := tool.Schema()
	if len(schema) == 0 {
		t.Fatal("expected non-empty schema")
	}

	var s map[string]any
	if err := json.Unmarshal(schema, &s); err != nil {
		t.Fatalf("invalid JSON schema: %v", err)
	}

	if s["type"] != "object" {
		t.Fatalf("expected type object, got %v", s["type"])
	}

	props, ok := s["properties"].(map[string]any)
	if !ok {
		t.Fatalf("expected properties map, got %T", s["properties"])
	}
	if _, ok := props["a"]; !ok {
		t.Fatal("expected property 'a'")
	}
	if _, ok := props["b"]; !ok {
		t.Fatal("expected property 'b'")
	}
}

func TestToolSchemaNestedStruct(t *testing.T) {
	type Nested struct {
		Query string `json:"query" jsonschema:"description=Search query"`
	}

	tool := NewTool(Tool[Nested]{
		Name:        "search",
		Description: "Search the database",
		Execute: func(ctx context.Context, p Nested) (ToolResult, error) {
			return ToolResult{Content: p.Query}, nil
		},
	})

	schema := tool.Schema()
	var s map[string]any
	if err := json.Unmarshal(schema, &s); err != nil {
		t.Fatalf("invalid JSON schema: %v", err)
	}

	props, ok := s["properties"].(map[string]any)
	if !ok {
		t.Fatal("expected properties")
	}
	if _, ok := props["query"]; !ok {
		t.Fatal("expected property 'query'")
	}
}

func TestPrepareArgumentsTypedTool(t *testing.T) {
	t.Parallel()

	type CurrentParams struct {
		Value string `json:"value"`
	}

	makeOldArgs := func(old string) json.RawMessage {
		b, _ := json.Marshal(map[string]string{"legacy_value": old})
		return b
	}
	makeNewArgs := func(val string) json.RawMessage {
		b, _ := json.Marshal(CurrentParams{Value: val})
		return b
	}

	tests := []struct {
		name            string
		prepareArgs     PrepareArgumentsFunc
		inputArgs       json.RawMessage
		wantContent     string
		wantErr         bool
		wantErrContains string
	}{
		{
			name:        "nil hook is a no-op",
			prepareArgs: nil,
			inputArgs:   makeNewArgs("hello"),
			wantContent: "hello",
		},
		{
			name: "shims outdated shape to current shape",
			prepareArgs: func(_ context.Context, raw json.RawMessage) (json.RawMessage, error) {
				var legacy struct {
					LegacyValue string `json:"legacy_value"`
				}
				if err := json.Unmarshal(raw, &legacy); err != nil || legacy.LegacyValue == "" {
					return raw, nil
				}
				return json.Marshal(CurrentParams{Value: legacy.LegacyValue})
			},
			inputArgs:   makeOldArgs("shimmed"),
			wantContent: "shimmed",
		},
		{
			name: "prepare error propagates as execute error",
			prepareArgs: func(_ context.Context, _ json.RawMessage) (json.RawMessage, error) {
				return nil, errors.New("shim failed")
			},
			inputArgs:       makeNewArgs("irrelevant"),
			wantErr:         true,
			wantErrContains: "shim failed",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tool := NewTool(Tool[CurrentParams]{
				Name:             "typed",
				Description:      "typed test tool",
				PrepareArguments: tt.prepareArgs,
				Execute: func(_ context.Context, p CurrentParams) (ToolResult, error) {
					return ToolResult{Content: p.Value}, nil
				},
			})

			// given
			ctx := context.Background()

			// when
			result, err := tool.Execute(ctx, "call-1", tt.inputArgs)

			// then
			if tt.wantErr {
				if err == nil {
					t.Fatalf("Execute(%s) error = nil, want error containing %q", tt.name, tt.wantErrContains)
				}
				if tt.wantErrContains != "" && !strings.Contains(err.Error(), tt.wantErrContains) {
					t.Errorf("Execute(%s) error = %q, want to contain %q", tt.name, err.Error(), tt.wantErrContains)
				}
				return
			}
			if err != nil {
				t.Fatalf("Execute(%s) unexpected error: %v", tt.name, err)
			}
			if result.Content != tt.wantContent {
				t.Errorf("Execute(%s) content = %q, want %q", tt.name, result.Content, tt.wantContent)
			}
		})
	}
}

// TestPrepareArgumentsErrorYieldsToolErrorResult is the end-to-end criterion for
// AGENT-4: when a tool's PrepareArguments hook returns an error the prompt loop
// must (a) surface a tool_result with IsError=true and the "prepare arguments"
// wrap in its content, (b) not treat the error as fatal (PromptResult.Err==nil),
// and (c) continue the loop (provider receives a second call).
//
// The tool-call turn auto-continues (Fix A), so the second provider call needs
// no external nudge: turn 1 emits the broken tool call, turn 2 is text-only.
func TestPrepareArgumentsErrorYieldsToolErrorResult(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32

	provider := &stubProvider{
		name: "test",
		emit: func(events chan<- llm.LLMEvent) {
			n := calls.Add(1)
			if n == 1 {
				events <- llm.ToolCallStartEvent{CallID: "c1", ToolName: "broken", Args: []byte("{}")}
				events <- llm.ToolCallEndEvent{CallID: "c1"}
				events <- llm.MessageEndEvent{}
			} else {
				events <- llm.TextDeltaEvent{Delta: "done"}
				events <- llm.MessageEndEvent{}
			}
		},
	}

	brokenTool := NewTool(Tool[struct{}]{
		Name:        "broken",
		Description: "tool whose PrepareArguments always fails",
		PrepareArguments: func(_ context.Context, _ json.RawMessage) (json.RawMessage, error) {
			return nil, errors.New("injected prepare error")
		},
		Execute: func(_ context.Context, _ struct{}) (ToolResult, error) {
			return ToolResult{Content: "unreachable"}, nil
		},
	})

	a, err := NewAgent(AgentConfig{
		Providers:    []llm.LLMProvider{provider},
		DefaultModel: "test/model",
		Tools:        []RegisteredTool{brokenTool},
	})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}

	stream, err := a.Prompt(context.Background(), NewText("user", "go"), PromptOpts{})
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}

	_, result := drain(t, stream)

	// (b) prompt continued — PrepareArguments error is not fatal
	if result.Err != nil {
		t.Fatalf("PromptResult.Err = %v, want nil", result.Err)
	}

	// (a) transcript contains a tool_result with IsError=true whose content includes
	// the "prepare arguments" wrap injected by typedTool.Execute.
	var foundErrResult bool
	for _, msg := range result.Messages {
		if msg.Type != "tool_result" {
			continue
		}
		_, _, content, _, isError, ok := msg.ToolResult()
		if !ok {
			continue
		}
		if isError && strings.Contains(content, "prepare arguments") {
			foundErrResult = true
			break
		}
	}
	if !foundErrResult {
		t.Fatalf("no tool_result with IsError=true containing %q found in transcript", "prepare arguments")
	}

	// (c) loop continued past the tool error — provider received a second call
	if got := calls.Load(); got != 2 {
		t.Errorf("provider.Stream() called %d times, want 2", got)
	}
}

func TestPrepareArgumentsDynamicTool(t *testing.T) {
	t.Parallel()

	makeArgs := func(key, val string) json.RawMessage {
		b, _ := json.Marshal(map[string]string{key: val})
		return b
	}

	tests := []struct {
		name        string
		opts        []DynamicToolOption
		inputArgs   json.RawMessage
		wantSeen    string // value the handler should see under "value"
		wantErr     bool
		wantErrText string
	}{
		{
			name:      "nil hook is a no-op",
			opts:      nil,
			inputArgs: makeArgs("value", "direct"),
			wantSeen:  "direct",
		},
		{
			name: "shims raw args before handler",
			opts: []DynamicToolOption{
				WithPrepareArguments(func(_ context.Context, raw json.RawMessage) (json.RawMessage, error) {
					var m map[string]string
					if err := json.Unmarshal(raw, &m); err != nil {
						return raw, nil
					}
					if v, ok := m["old_key"]; ok {
						return json.Marshal(map[string]string{"value": v})
					}
					return raw, nil
				}),
			},
			inputArgs: makeArgs("old_key", "migrated"),
			wantSeen:  "migrated",
		},
		{
			name: "prepare error propagates as execute error",
			opts: []DynamicToolOption{
				WithPrepareArguments(func(_ context.Context, _ json.RawMessage) (json.RawMessage, error) {
					return nil, errors.New("dynamic shim failed")
				}),
			},
			inputArgs:   makeArgs("value", "irrelevant"),
			wantErr:     true,
			wantErrText: "dynamic shim failed",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var seen string
			schema := json.RawMessage(`{"type":"object","properties":{"value":{"type":"string"}}}`)
			tool := NewDynamicTool("dyn", "dynamic test tool", schema, func(_ context.Context, _ string, args json.RawMessage) (ToolResult, error) {
				var m map[string]string
				if err := json.Unmarshal(args, &m); err != nil {
					return ToolResult{}, err
				}
				seen = m["value"]
				return ToolResult{Content: seen}, nil
			}, tt.opts...)

			// given
			ctx := context.Background()

			// when
			result, err := tool.Execute(ctx, "call-2", tt.inputArgs)

			// then
			if tt.wantErr {
				if err == nil {
					t.Fatalf("Execute(%s) error = nil, want error containing %q", tt.name, tt.wantErrText)
				}
				if tt.wantErrText != "" && !strings.Contains(err.Error(), tt.wantErrText) {
					t.Errorf("Execute(%s) error = %q, want to contain %q", tt.name, err.Error(), tt.wantErrText)
				}
				return
			}
			if err != nil {
				t.Fatalf("Execute(%s) unexpected error: %v", tt.name, err)
			}
			if result.Content != tt.wantSeen {
				t.Errorf("Execute(%s) content = %q, want %q", tt.name, result.Content, tt.wantSeen)
			}
		})
	}
}
