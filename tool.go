package pi

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"

	"github.com/dev-resolute/resolute-llm-go"
	"github.com/invopop/jsonschema"
)

// ToolResult is the concrete struct returned by a tool's Execute function.
type ToolResult struct {
	Content   string
	Data      json.RawMessage
	IsError   bool
	Terminate bool
	// Images carries optional image parts of the result (flows to llm.ToolResultContent.Images).
	Images []llm.ImageContent
}

// RegisteredTool is the internal interface that the agent loop uses to invoke tools.
type RegisteredTool interface {
	Name() string
	Description() string
	Schema() json.RawMessage
	Execute(ctx context.Context, callID string, args json.RawMessage) (ToolResult, error)
	IsSequential() bool
}

// streamingTool is an optional capability a RegisteredTool may implement to
// stream partial results during execution. The execution site type-asserts
// for it; tools built from Tool.Execute (rather than Tool.ExecuteStream) do
// not implement it, so the assertion structurally distinguishes the two.
type streamingTool interface {
	ExecuteStream(ctx context.Context, callID string, args json.RawMessage, emit func(ToolResult)) (ToolResult, error)
}

// PrepareArgumentsFunc transforms raw LLM-supplied arguments before
// unmarshalling into P. A typical use case is shimming a deprecated argument
// shape — e.g. migrating a legacy_value key to the current value key —
// without requiring callers to update their prompts. Returning an error
// surfaces as a tool error result; the prompt continues.
type PrepareArgumentsFunc func(ctx context.Context, raw json.RawMessage) (json.RawMessage, error)

// Tool is the generic, compile-time-typed tool struct.
type Tool[P any] struct {
	Name        string
	Description string
	Sequential  bool
	Execute     func(ctx context.Context, params P) (ToolResult, error)
	// ExecuteStream is the streaming alternative to Execute: it may call emit
	// zero or more times with partial results before returning the final
	// ToolResult. Exactly one of Execute or ExecuteStream must be set;
	// NewTool panics otherwise. Partial results are ephemeral — the loop
	// forwards each emit as a ToolUpdateEvent and never persists them to the
	// transcript.
	ExecuteStream func(ctx context.Context, params P, emit func(ToolResult)) (ToolResult, error)
	// PrepareArguments is an optional hook that runs on raw args before
	// unmarshalling into P. See PrepareArgumentsFunc for details.
	PrepareArguments PrepareArgumentsFunc
}

// NewTool creates a RegisteredTool from a typed Tool. Exactly one of
// Execute or ExecuteStream must be set; NewTool panics otherwise, since
// this is API misuse rather than a runtime condition.
func NewTool[P any](t Tool[P]) RegisteredTool {
	if (t.Execute == nil) == (t.ExecuteStream == nil) {
		panic("pi.NewTool: exactly one of Execute or ExecuteStream must be set (tool " + t.Name + ")")
	}
	base := typedTool[P]{
		name:             t.Name,
		description:      t.Description,
		sequential:       t.Sequential,
		execute:          t.Execute,
		prepareArguments: t.PrepareArguments,
	}
	if t.ExecuteStream != nil {
		return &streamingTypedTool[P]{typedTool: base, executeStream: t.ExecuteStream}
	}
	return &base
}

// DynamicToolOption configures an optional capability on a dynamic tool.
type DynamicToolOption func(*dynamicTool)

// WithPrepareArguments attaches a PrepareArgumentsFunc hook to a dynamic tool.
// The hook transforms raw LLM-supplied arguments before they reach the
// handler. Returning an error surfaces as a tool error result; the prompt
// continues.
func WithPrepareArguments(fn PrepareArgumentsFunc) DynamicToolOption {
	return func(t *dynamicTool) { t.prepareArguments = fn }
}

// NewDynamicTool creates a tool from a runtime schema and raw handler.
// Optional DynamicToolOption values (e.g. WithPrepareArguments) may be
// appended; existing callers that pass none are unaffected.
func NewDynamicTool(name, description string, schema json.RawMessage, execute func(ctx context.Context, callID string, args json.RawMessage) (ToolResult, error), opts ...DynamicToolOption) RegisteredTool {
	dt := &dynamicTool{
		name:        name,
		description: description,
		schema:      schema,
		execute:     execute,
	}
	for _, o := range opts {
		o(dt)
	}
	return dt
}

type typedTool[P any] struct {
	name             string
	description      string
	sequential       bool
	execute          func(ctx context.Context, params P) (ToolResult, error)
	prepareArguments PrepareArgumentsFunc
}

func (t *typedTool[P]) Name() string        { return t.name }
func (t *typedTool[P]) Description() string { return t.description }
func (t *typedTool[P]) IsSequential() bool  { return t.sequential }

func (t *typedTool[P]) Schema() json.RawMessage {
	var p P
	// Handle pointer types by dereferencing.
	v := reflect.ValueOf(p)
	if v.Kind() == reflect.Ptr {
		if v.IsNil() {
			v = reflect.New(v.Type().Elem())
		}
		p = v.Interface().(P)
	}

	r := &jsonschema.Reflector{
		Anonymous:      true,
		DoNotReference: true,
	}
	schema := r.Reflect(p)
	data, err := json.Marshal(schema)
	if err != nil {
		return json.RawMessage(`{"type":"object","properties":{},"required":[]}`)
	}
	return data
}

func (t *typedTool[P]) Execute(ctx context.Context, callID string, args json.RawMessage) (ToolResult, error) {
	params, err := t.prepareParams(ctx, args)
	if err != nil {
		return ToolResult{}, err
	}
	return t.execute(ctx, params)
}

// prepareParams runs the PrepareArguments hook (if any) and unmarshals the
// result into P. Shared by typedTool.Execute and streamingTypedTool.ExecuteStream
// so both entry points apply identical argument handling.
func (t *typedTool[P]) prepareParams(ctx context.Context, args json.RawMessage) (P, error) {
	var params P
	prepared, err := runPrepare(ctx, t.prepareArguments, args)
	if err != nil {
		return params, err
	}
	if err := json.Unmarshal(prepared, &params); err != nil {
		return params, fmt.Errorf("unmarshal tool params: %w", err)
	}
	return params, nil
}

// streamingTypedTool is a typedTool[P] whose spec set ExecuteStream instead
// of Execute. Embedding typedTool[P] reuses Name/Description/Schema/
// IsSequential and the prepare/unmarshal pipeline; ExecuteStream is the only
// addition. This is what makes the streamingTool capability structurally
// present only on tools built from ExecuteStream — a plain typedTool[P]
// never gains an ExecuteStream method.
type streamingTypedTool[P any] struct {
	typedTool[P]
	executeStream func(ctx context.Context, params P, emit func(ToolResult)) (ToolResult, error)
}

// ExecuteStream implements the unexported streamingTool capability interface.
func (t *streamingTypedTool[P]) ExecuteStream(ctx context.Context, callID string, args json.RawMessage, emit func(ToolResult)) (ToolResult, error) {
	params, err := t.prepareParams(ctx, args)
	if err != nil {
		return ToolResult{}, err
	}
	return t.executeStream(ctx, params, emit)
}

type dynamicTool struct {
	name             string
	description      string
	schema           json.RawMessage
	sequential       bool
	execute          func(ctx context.Context, callID string, args json.RawMessage) (ToolResult, error)
	prepareArguments PrepareArgumentsFunc
}

func (t *dynamicTool) Name() string            { return t.name }
func (t *dynamicTool) Description() string     { return t.description }
func (t *dynamicTool) IsSequential() bool      { return t.sequential }
func (t *dynamicTool) Schema() json.RawMessage { return t.schema }

func (t *dynamicTool) Execute(ctx context.Context, callID string, args json.RawMessage) (ToolResult, error) {
	prepared, err := runPrepare(ctx, t.prepareArguments, args)
	if err != nil {
		return ToolResult{}, err
	}
	return t.execute(ctx, callID, prepared)
}

// runPrepare invokes hook on raw when non-nil and wraps any error with the
// "prepare arguments" prefix expected by callers. Returns raw unchanged when
// hook is nil.
func runPrepare(ctx context.Context, hook PrepareArgumentsFunc, raw json.RawMessage) (json.RawMessage, error) {
	if hook == nil {
		return raw, nil
	}
	prepared, err := hook(ctx, raw)
	if err != nil {
		return nil, fmt.Errorf("prepare arguments: %w", err)
	}
	return prepared, nil
}
