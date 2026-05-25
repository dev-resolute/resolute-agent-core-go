package session

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/resolute-sh/pi-core-agent-go"
)

// JSONLSession is an on-disk session backend using append-only JSONL files.
type JSONLSession struct {
	dir string
	mu  sync.Mutex
}

// NewJSONLSession creates a JSONLSession that stores files in dir.
func NewJSONLSession(dir string) (*JSONLSession, error) {
	if err := os.MkdirAll(dir, 0750); err != nil {
		return nil, fmt.Errorf("creating session dir: %w", err)
	}
	return &JSONLSession{dir: dir}, nil
}

// Create implements SessionRepo.
func (j *JSONLSession) Create(ctx context.Context) (pi.SessionID, error) {
	id := pi.SessionID(NewSessionID())
	f, err := os.Create(j.path(id))
	if err != nil {
		return "", fmt.Errorf("creating session file: %w", err)
	}
	f.Close()
	return id, nil
}

// Append implements SessionRepo.
func (j *JSONLSession) Append(ctx context.Context, id pi.SessionID, msgs ...pi.Message) error {
	j.mu.Lock()
	defer j.mu.Unlock()

	f, err := os.OpenFile(j.path(id), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0640)
	if err != nil {
		return fmt.Errorf("opening session file: %w", err)
	}
	defer f.Close()

	for _, msg := range msgs {
		line, err := json.Marshal(msg)
		if err != nil {
			return fmt.Errorf("marshaling message: %w", err)
		}
		if _, err := f.Write(line); err != nil {
			return fmt.Errorf("writing message: %w", err)
		}
		if _, err := f.WriteString("\n"); err != nil {
			return fmt.Errorf("writing newline: %w", err)
		}
	}
	return f.Sync()
}

// Load implements SessionRepo.
func (j *JSONLSession) Load(ctx context.Context, id pi.SessionID) ([]pi.Message, error) {
	j.mu.Lock()
	defer j.mu.Unlock()

	f, err := os.Open(j.path(id))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("opening session file: %w", err)
	}
	defer f.Close()

	var msgs []pi.Message
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		var msg pi.Message
		if err := json.Unmarshal(scanner.Bytes(), &msg); err != nil {
			continue
		}
		msgs = append(msgs, msg)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("reading session file: %w", err)
	}
	return msgs, nil
}

// List implements SessionRepo.
func (j *JSONLSession) List(ctx context.Context) ([]pi.SessionMeta, error) {
	entries, err := os.ReadDir(j.dir)
	if err != nil {
		return nil, fmt.Errorf("reading session dir: %w", err)
	}
	var out []pi.SessionMeta
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		id := pi.SessionID(entry.Name())
		out = append(out, pi.SessionMeta{
			ID:        id,
			CreatedAt: info.ModTime(),
			UpdatedAt: info.ModTime(),
		})
	}
	return out, nil
}

// AppendBranchSummary implements SessionRepo.
func (j *JSONLSession) AppendBranchSummary(ctx context.Context, id pi.SessionID, summary pi.BranchSummary) error {
	j.mu.Lock()
	defer j.mu.Unlock()

	f, err := os.OpenFile(j.path(id)+".summaries", os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0640)
	if err != nil {
		return fmt.Errorf("opening summaries file: %w", err)
	}
	defer f.Close()

	line, err := json.Marshal(summary)
	if err != nil {
		return fmt.Errorf("marshaling summary: %w", err)
	}
	if _, err := f.Write(line); err != nil {
		return fmt.Errorf("writing summary: %w", err)
	}
	if _, err := f.WriteString("\n"); err != nil {
		return fmt.Errorf("writing newline: %w", err)
	}
	return f.Sync()
}

// LoadBranchSummaries implements SessionRepo.
func (j *JSONLSession) LoadBranchSummaries(ctx context.Context, id pi.SessionID) ([]pi.BranchSummary, error) {
	j.mu.Lock()
	defer j.mu.Unlock()

	f, err := os.Open(j.path(id) + ".summaries")
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("opening summaries file: %w", err)
	}
	defer f.Close()

	var summaries []pi.BranchSummary
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		var s pi.BranchSummary
		if err := json.Unmarshal(scanner.Bytes(), &s); err != nil {
			continue
		}
		summaries = append(summaries, s)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("reading summaries file: %w", err)
	}
	return summaries, nil
}

// Delete implements SessionRepo.
func (j *JSONLSession) Delete(ctx context.Context, id pi.SessionID) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	_ = os.Remove(j.path(id))
	_ = os.Remove(j.path(id) + ".summaries")
	return nil
}

func (j *JSONLSession) path(id pi.SessionID) string {
	return filepath.Join(j.dir, string(id)+".jsonl")
}
