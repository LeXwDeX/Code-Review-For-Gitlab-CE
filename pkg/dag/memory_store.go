package dag

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
)

// MemoryEventStore is an in-memory EventStore for tests and embedders. Append
// runs the projector on a candidate event log before committing, so an invalid
// batch never becomes visible.
type MemoryEventStore struct {
	mu     sync.Mutex
	events map[string][]Event
}

// NewMemoryEventStore returns an empty store.
func NewMemoryEventStore() *MemoryEventStore {
	return &MemoryEventStore{events: make(map[string][]Event)}
}

// Append implements EventStore.
func (s *MemoryEventStore) Append(ctx context.Context, wfID string, expectRev int64, evs ...Event) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	existing := s.events[wfID]
	if expectRev != int64(len(existing)) {
		return 0, fmt.Errorf("%w: workflow %s has revision %d, append expects %d", ErrStaleRevision, wfID, len(existing), expectRev)
	}
	if len(evs) == 0 {
		return expectRev, nil
	}
	candidate := make([]Event, 0, len(existing)+len(evs))
	candidate = append(candidate, copyEvents(existing)...)
	for _, ev := range evs {
		next := ev
		next.Seq = int64(len(candidate) + 1)
		next.Payload = append(json.RawMessage(nil), ev.Payload...)
		if next.Payload == nil {
			next.Payload = json.RawMessage("{}")
		}
		candidate = append(candidate, next)
	}
	if _, err := Project(candidate); err != nil {
		return 0, err
	}
	s.events[wfID] = candidate
	return int64(len(candidate)), nil
}

// Load implements EventStore. An unknown workflow yields no events and no error;
// Engine maps that to ErrNotFound.
func (s *MemoryEventStore) Load(ctx context.Context, wfID string) ([]Event, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return copyEvents(s.events[wfID]), nil
}

// ListUnfinished implements EventStore.
func (s *MemoryEventStore) ListUnfinished(ctx context.Context) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	ids := make([]string, 0, len(s.events))
	for id := range s.events {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	unfinished := make([]string, 0, len(ids))
	for _, id := range ids {
		wf, err := Project(s.events[id])
		if err != nil {
			return nil, fmt.Errorf("dag: list unfinished %s: %w", id, err)
		}
		if !wf.Terminal() {
			unfinished = append(unfinished, id)
		}
	}
	return unfinished, nil
}

func copyEvents(in []Event) []Event {
	if in == nil {
		return nil
	}
	out := make([]Event, len(in))
	for i, ev := range in {
		out[i] = ev
		out[i].Payload = append(json.RawMessage(nil), ev.Payload...)
	}
	return out
}
