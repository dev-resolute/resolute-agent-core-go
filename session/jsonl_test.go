package session

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	pi "github.com/dev-resolute/resolute-agent-core-go"
)

// A usage-stamped message round-trips: the usage body field survives
// persistence and reload verbatim.
func TestJSONLSessionUsageRoundTrip(t *testing.T) {
	ctx := context.Background()
	s, err := NewJSONLSession(t.TempDir())
	if err != nil {
		t.Fatalf("NewJSONLSession: %v", err)
	}

	id, err := s.Create(ctx)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	want := pi.Usage{InputTokens: 100, OutputTokens: 42}
	if err := s.Append(ctx, id, pi.NewText("assistant", "hello").WithUsage(want)); err != nil {
		t.Fatalf("Append: %v", err)
	}

	msgs, err := s.Load(ctx, id)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("loaded %d messages, want 1", len(msgs))
	}
	got := msgs[0].Usage()
	if got == nil || *got != want {
		t.Errorf("Usage() after round trip = %+v, want %+v", got, want)
	}
	if msgs[0].Text() != "hello" {
		t.Errorf("Text() after round trip = %q, want hello", msgs[0].Text())
	}
}

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

// TestJSONLSessionBranchSummaryUsageRoundTrip pins upstream #6671: a
// BranchSummary's Usage pointer must round-trip through the JSONL backend,
// and old-format summary lines persisted before Usage existed (which lack
// the key entirely) must still reload with Usage == nil.
func TestJSONLSessionBranchSummaryUsageRoundTrip(t *testing.T) {
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

	withUsage := pi.BranchSummary{StartIdx: 0, EndIdx: 2, Summary: "has usage", Usage: &pi.Usage{InputTokens: 3, OutputTokens: 4}}
	if err := s.AppendBranchSummary(ctx, id, withUsage); err != nil {
		t.Fatalf("AppendBranchSummary (with usage): %v", err)
	}

	// Simulate an old-format line persisted before Usage existed: no "Usage"
	// key in the JSON at all, not even a null.
	oldFormatLine := `{"StartIdx":2,"EndIdx":4,"Summary":"old format, no usage key","CreatedAt":"0001-01-01T00:00:00Z"}` + "\n"
	summariesPath := filepath.Join(dir, string(id)+".jsonl.summaries")
	f, err := os.OpenFile(summariesPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0640)
	if err != nil {
		t.Fatalf("opening summaries file directly: %v", err)
	}
	if _, err := f.WriteString(oldFormatLine); err != nil {
		t.Fatalf("writing old-format line: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("closing summaries file: %v", err)
	}

	summaries, err := s.LoadBranchSummaries(ctx, id)
	if err != nil {
		t.Fatalf("LoadBranchSummaries: %v", err)
	}
	if len(summaries) != 2 {
		t.Fatalf("expected 2 summaries, got %d", len(summaries))
	}

	got := summaries[0]
	if got.Usage == nil || *got.Usage != *withUsage.Usage {
		t.Errorf("summaries[0].Usage = %+v, want %+v", got.Usage, withUsage.Usage)
	}

	old := summaries[1]
	if old.Usage != nil {
		t.Errorf("summaries[1].Usage = %+v, want nil (old-format line has no Usage key)", old.Usage)
	}
	if old.Summary != "old format, no usage key" {
		t.Errorf("summaries[1].Summary = %q, want the old-format line's summary", old.Summary)
	}
}

// A session file whose last line lacks a trailing newline (externally
// written, crashed writer) is repaired before the next append, so the new
// record never concatenates onto the old one (upstream #8345).
func TestJSONLSessionAppendRepairsMissingTrailingNewline(t *testing.T) {
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

	// Write a record with no trailing newline, simulating a crashed writer.
	raw, _ := json.Marshal(pi.NewText("user", "first"))
	if err := os.WriteFile(filepath.Join(dir, string(id)+".jsonl"), raw, 0640); err != nil {
		t.Fatalf("seed file: %v", err)
	}

	if err := s.Append(ctx, id, pi.NewText("assistant", "second")); err != nil {
		t.Fatalf("Append: %v", err)
	}

	msgs, err := s.Load(ctx, id)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("loaded %d messages, want 2 (repair must keep both records)", len(msgs))
	}
}
