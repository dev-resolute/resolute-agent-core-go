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

// Tool is the generic, compile-time-typed tool struct.
type Tool[P any] struct {
	Name        string
	Description string
	Sequential  bool
	Execute     func(ctx context.Context, params P) (ToolResult, error)
}

// NewTool creates a RegisteredTool from a typed Tool.
func NewTool[P any](t Tool[P]) RegisteredTool {
	return &typedTool[P]{
		name:        t.Name,
		description: t.Description,
		sequential:  t.Sequential,
		execute:     t.Execute,
	}
}

// NewDynamicTool creates a tool from a runtime schema and raw handler.
func NewDynamicTool(name, description string, schema json.RawMessage, execute func(ctx context.Context, callID string, args json.RawMessage) (ToolResult, error)) RegisteredTool {
	return &dynamicTool{
		name:        name,
		description: description,
		schema:      schema,
		execute:     execute,
	}
}

type typedTool[P any] struct {
	name        string
	description string
	sequential  bool
	execute     func(ctx context.Context, params P) (ToolResult, error)
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
	var params P
	if err := json.Unmarshal(args, &params); err != nil {
		return ToolResult{}, fmt.Errorf("unmarshal tool params: %w", err)
	}
	return t.execute(ctx, params)
}

type dynamicTool struct {
	name        string
	description string
	schema      json.RawMessage
	sequential  bool
	execute     func(ctx context.Context, callID string, args json.RawMessage) (ToolResult, error)
}

func (t *dynamicTool) Name() string            { return t.name }
func (t *dynamicTool) Description() string     { return t.description }
func (t *dynamicTool) IsSequential() bool      { return t.sequential }
func (t *dynamicTool) Schema() json.RawMessage { return t.schema }

func (t *dynamicTool) Execute(ctx context.Context, callID string, args json.RawMessage) (ToolResult, error) {
	return t.execute(ctx, callID, args)
}
