package app

import "sync"

// Lifecycle records whether the background worker has started and is still
// running. Readiness must not report true before start or after stop.
type Lifecycle struct {
	mu      sync.RWMutex
	started bool
}

// MarkStarted declares the worker running.
func (l *Lifecycle) MarkStarted() {
	l.mu.Lock()
	l.started = true
	l.mu.Unlock()
}

// MarkStopped declares the worker stopped.
func (l *Lifecycle) MarkStopped() {
	l.mu.Lock()
	l.started = false
	l.mu.Unlock()
}

// Ready reports the current worker state.
func (l *Lifecycle) Ready() bool {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.started
}
