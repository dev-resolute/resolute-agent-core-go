package pi

import (
	"context"
	"encoding/json"
	"testing"
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
