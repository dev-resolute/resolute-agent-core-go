package session

import (
	"context"
	"sync"
	"time"

	"github.com/resolute-sh/pi-core-agent-go"
)

// MemorySession is an ephemeral, in-process session backend.
type MemorySession struct {
	mu        sync.Mutex
	sessions  map[pi.SessionID][]pi.Message
	summaries map[pi.SessionID][]pi.BranchSummary
	meta      map[pi.SessionID]pi.SessionMeta
	counter   int
}

// NewMemorySession creates a new MemorySession.
func NewMemorySession() *MemorySession {
	return &MemorySession{
		sessions:  make(map[pi.SessionID][]pi.Message),
		summaries: make(map[pi.SessionID][]pi.BranchSummary),
		meta:      make(map[pi.SessionID]pi.SessionMeta),
	}
}

// Create implements SessionRepo.
func (m *MemorySession) Create(ctx context.Context) (pi.SessionID, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.counter++
	id := pi.SessionID(NewSessionID())
	m.sessions[id] = nil
	m.meta[id] = pi.SessionMeta{
		ID:        id,
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}
	return id, nil
}

// Append implements SessionRepo.
func (m *MemorySession) Append(ctx context.Context, id pi.SessionID, msgs ...pi.Message) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sessions[id] = append(m.sessions[id], msgs...)
	if meta, ok := m.meta[id]; ok {
		meta.UpdatedAt = time.Now()
		m.meta[id] = meta
	}
	return nil
}

// Load implements SessionRepo.
func (m *MemorySession) Load(ctx context.Context, id pi.SessionID) ([]pi.Message, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	msgs, ok := m.sessions[id]
	if !ok {
		return nil, nil
	}
	out := make([]pi.Message, len(msgs))
	copy(out, msgs)
	return out, nil
}

// List implements SessionRepo.
func (m *MemorySession) List(ctx context.Context) ([]pi.SessionMeta, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []pi.SessionMeta
	for _, meta := range m.meta {
		out = append(out, meta)
	}
	return out, nil
}

// AppendBranchSummary implements SessionRepo.
func (m *MemorySession) AppendBranchSummary(ctx context.Context, id pi.SessionID, summary pi.BranchSummary) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.summaries[id] = append(m.summaries[id], summary)
	return nil
}

// LoadBranchSummaries implements SessionRepo.
func (m *MemorySession) LoadBranchSummaries(ctx context.Context, id pi.SessionID) ([]pi.BranchSummary, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]pi.BranchSummary, len(m.summaries[id]))
	copy(out, m.summaries[id])
	return out, nil
}

// Delete implements SessionRepo.
func (m *MemorySession) Delete(ctx context.Context, id pi.SessionID) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.sessions, id)
	delete(m.summaries, id)
	delete(m.meta, id)
	return nil
}
