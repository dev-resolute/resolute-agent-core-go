package session

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dev-resolute/resolute-agent-core-go"
)

func TestMemorySessionActiveToolsChange(t *testing.T) {
	ctx := context.Background()
	s := NewMemorySession()
	id, err := s.Create(ctx)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := s.Append(ctx, id, pi.NewActiveToolsChange([]string{"a", "b"})); err != nil {
		t.Fatalf("Append active tools change: %v", err)
	}
	msgs, err := s.Load(ctx, id)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(msgs) != 1 || msgs[0].Type != "active_tools_change" {
		t.Fatalf("expected 1 active_tools_change message, got %+v", msgs)
	}
	names, ok := msgs[0].ActiveToolNames()
	if !ok || len(names) != 2 {
		t.Fatalf("ActiveToolNames = %v ok=%v, want 2 names", names, ok)
	}
}

// TestJSONLSessionActiveToolsChangeGoShape pins the documented Go-native,
// append-only on-disk layout: a flat {"Role","Type","Body"} line, not upstream's
// {type,id,parentId,timestamp} tree. See CONTEXT.md (JSONLSession).
func TestJSONLSessionActiveToolsChangeGoShape(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s, err := NewJSONLSession(dir)
	if err != nil {
		t.Fatalf("NewJSONLSession: %v", err)
	}
	id, err := s.Create(ctx)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := s.Append(ctx, id, pi.NewActiveToolsChange([]string{"a"})); err != nil {
		t.Fatalf("Append active tools change: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dir, string(id)+".jsonl"))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	got := strings.TrimSpace(string(data))
	want := `{"Role":"system","Type":"active_tools_change","Body":{"activeToolNames":["a"]}}`
	if got != want {
		t.Fatalf("on-disk line = %s\nwant %s", got, want)
	}

	msgs, err := s.Load(ctx, id)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	names, ok := msgs[0].ActiveToolNames()
	if !ok || len(names) != 1 || names[0] != "a" {
		t.Fatalf("roundtrip ActiveToolNames = %v ok=%v, want [a]", names, ok)
	}
}
