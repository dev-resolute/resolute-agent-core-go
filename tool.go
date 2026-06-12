package pi

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"

	"github.com/invopop/jsonschema"
)

// ToolResult is the concrete struct returned by a tool's Execute function.
type ToolResult struct {
	Content   string
	Data      json.RawMessage
	IsError   bool
	Terminate bool
}

// RegisteredTool is the internal interface that the agent loop uses to invoke tools.
type RegisteredTool interface {
	Name() string
	Description() string
	Schema() json.RawMessage
	Execute(ctx context.Context, callID string, args json.RawMessage) (ToolResult, error)
	IsSequential() bool
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
	// PrepareArguments is an optional hook that runs on raw args before
	// unmarshalling into P. See PrepareArgumentsFunc for details.
	PrepareArguments PrepareArgumentsFunc
}

// NewTool creates a RegisteredTool from a typed Tool.
func NewTool[P any](t Tool[P]) RegisteredTool {
	return &typedTool[P]{
		name:             t.Name,
		description:      t.Description,
		sequential:       t.Sequential,
		execute:          t.Execute,
		prepareArguments: t.PrepareArguments,
	}
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
	prepared, err := runPrepare(ctx, t.prepareArguments, args)
	if err != nil {
		return ToolResult{}, err
	}
	var params P
	if err := json.Unmarshal(prepared, &params); err != nil {
		return ToolResult{}, fmt.Errorf("unmarshal tool params: %w", err)
	}
	return t.execute(ctx, params)
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
