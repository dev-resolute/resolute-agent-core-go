package session

import (
	"context"
	"testing"

	"github.com/resolute-sh/pi-core-agent-go"
)

func TestMemorySessionCRUD(t *testing.T) {
	ctx := context.Background()
	s := NewMemorySession()

	id, err := s.Create(ctx)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if id == "" {
		t.Fatal("expected non-empty session id")
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
	if msgs[0].Text() != "hello" {
		t.Fatalf("expected 'hello', got %q", msgs[0].Text())
	}

	metas, err := s.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(metas) != 1 {
		t.Fatalf("expected 1 session, got %d", len(metas))
	}

	if err := s.Delete(ctx, id); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	msgs, err = s.Load(ctx, id)
	if err != nil {
		t.Fatalf("Load after delete: %v", err)
	}
	if msgs != nil {
		t.Fatalf("expected nil after delete, got %d messages", len(msgs))
	}
}

func TestMemorySessionBranchSummary(t *testing.T) {
	ctx := context.Background()
	s := NewMemorySession()

	id, _ := s.Create(ctx)
	summary := pi.BranchSummary{StartIdx: 0, EndIdx: 2, Summary: "summary text"}
	if err := s.AppendBranchSummary(ctx, id, summary); err != nil {
		t.Fatalf("AppendBranchSummary: %v", err)
	}

	summaries, err := s.LoadBranchSummaries(ctx, id)
	if err != nil {
		t.Fatalf("LoadBranchSummaries: %v", err)
	}
	if len(summaries) != 1 {
		t.Fatalf("expected 1 summary, got %d", len(summaries))
	}
}
