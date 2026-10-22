package workspace

import (
	"context"
	"sync"
)

// ctxRWMutex is a writer-preferring reader/writer lock whose waits honor a
// context. It uses no goroutines, so a waiter that leaves cannot leak work.
type ctxRWMutex struct {
	mu      sync.Mutex
	readers int
	writer  bool
	writers int
	changed chan struct{}
}

func newCtxRWMutex() *ctxRWMutex {
	return &ctxRWMutex{changed: make(chan struct{})}
}

func (m *ctxRWMutex) RLock(ctx context.Context) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	for {
		m.mu.Lock()
		if !m.writer && m.writers == 0 {
			m.readers++
			m.mu.Unlock()
			return m.rUnlock, nil
		}
		changed := m.changed
		m.mu.Unlock()

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-changed:
		}
	}
}

func (m *ctxRWMutex) rUnlock() {
	m.mu.Lock()
	m.readers--
	changed := m.readers == 0
	if changed {
		m.broadcastLocked()
	}
	m.mu.Unlock()
}

func (m *ctxRWMutex) Lock(ctx context.Context) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.Lock()
	m.writers++
	m.mu.Unlock()

	for {
		m.mu.Lock()
		if !m.writer && m.readers == 0 {
			m.writer = true
			m.writers--
			m.mu.Unlock()
			return m.unlock, nil
		}
		changed := m.changed
		m.mu.Unlock()

		select {
		case <-ctx.Done():
			m.mu.Lock()
			m.writers--
			m.broadcastLocked()
			m.mu.Unlock()
			return nil, ctx.Err()
		case <-changed:
		}
	}
}

func (m *ctxRWMutex) unlock() {
	m.mu.Lock()
	if !m.writer {
		m.mu.Unlock()
		panic("workspace: unlock of unlocked write lock")
	}
	m.writer = false
	m.broadcastLocked()
	m.mu.Unlock()
}

func (m *ctxRWMutex) broadcastLocked() {
	close(m.changed)
	m.changed = make(chan struct{})
}
