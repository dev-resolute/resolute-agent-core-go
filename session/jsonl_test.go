package session

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/dev-resolute/resolute-agent-core-go"
)

func TestJSONLSessionCRUD(t *testing.T) {
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

	msg := pi.NewText("user", "hello")
	if err := s.Append(ctx, id, msg); err != nil {
		t.Fatalf("Append: %v", err)
	}

	msgs, err := s.Load(ctx, id)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message, got %d", len(msgs))
	}

	// Verify file exists
	path := filepath.Join(dir, string(id)+".jsonl")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("expected file to exist: %v", err)
	}
}
