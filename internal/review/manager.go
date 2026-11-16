package review

import (
	"context"
	"sync"
	"time"
)

type job struct{ cancel context.CancelFunc }
type Manager struct {
	mu      sync.Mutex
	active  map[string]*job
	seen    map[string]time.Time
	sem     chan struct{}
	closed  bool
	workers sync.WaitGroup
}

func NewManager(concurrency int) *Manager {
	if concurrency < 1 {
		concurrency = 1
	}
	return &Manager{active: map[string]*job{}, seen: map[string]time.Time{}, sem: make(chan struct{}, concurrency)}
}

// Start suppresses duplicate delivery IDs and cancels previous work for the
// same PR before scheduling the newer relevant event.
func (m *Manager) Start(parent context.Context, key, delivery string, fn func(context.Context)) bool {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return false
	}
	now := time.Now()
	for id, t := range m.seen {
		if now.Sub(t) > 24*time.Hour {
			delete(m.seen, id)
		}
	}
	if delivery != "" {
		if _, ok := m.seen[delivery]; ok {
			m.mu.Unlock()
			return false
		}
		m.seen[delivery] = now
	}
	if old := m.active[key]; old != nil {
		old.cancel()
	}
	ctx, cancel := context.WithCancel(parent)
	j := &job{cancel: cancel}
	m.active[key] = j
	m.workers.Add(1)
	m.mu.Unlock()
	go func() {
		defer m.workers.Done()
		defer func() {
			m.mu.Lock()
			if m.active[key] == j {
				delete(m.active, key)
			}
			m.mu.Unlock()
			cancel()
		}()
		select {
		case m.sem <- struct{}{}:
		case <-ctx.Done():
			return
		}
		defer func() { <-m.sem }()
		fn(ctx)
	}()
	return true
}

// Shutdown prevents new work, cancels active reviews, and waits for workers to
// finish or for ctx to expire.
func (m *Manager) Shutdown(ctx context.Context) error {
	m.mu.Lock()
	m.closed = true
	for _, active := range m.active {
		active.cancel()
	}
	m.mu.Unlock()

	done := make(chan struct{})
	go func() {
		m.workers.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
