package dag

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestMemoryStoreAppendAndLoad(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryEventStore()
	if events, err := store.Load(ctx, "missing"); err != nil || len(events) != 0 {
		t.Fatalf("load missing = %v, %v", events, err)
	}
	ev := created(t, node("a"))
	rev, err := store.Append(ctx, "wf", 0, ev)
	if err != nil || rev != 1 {
		t.Fatalf("append = %d, %v", rev, err)
	}
	if _, err := store.Append(ctx, "wf", 0, ev); !errors.Is(err, ErrStaleRevision) {
		t.Fatalf("stale append error = %v", err)
	}
	rev, err = store.Append(ctx, "wf", 1)
	if err != nil || rev != 1 {
		t.Fatalf("empty append = %d, %v", rev, err)
	}
	events, err := store.Load(ctx, "wf")
	if err != nil || len(events) != 1 || events[0].Seq != 1 {
		t.Fatalf("load = %+v, %v", events, err)
	}
	events[0].Payload[0] = 'X'
	again, _ := store.Load(ctx, "wf")
	if again[0].Payload[0] == 'X' {
		t.Fatal("Load must return a copy")
	}
}

func TestMemoryStoreRejectsInvalidBatchWithoutCommit(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryEventStore()
	createdEv := created(t, node("a"))
	rev, err := store.Append(ctx, "wf", 0, createdEv)
	if err != nil {
		t.Fatal(err)
	}
	before, _ := store.Load(ctx, "wf")
	invalid := nodeEvent(t, EventNodeCompleted, "a", 1)
	if _, err := store.Append(ctx, "wf", rev, invalid); err == nil {
		t.Fatal("invalid transition accepted")
	}
	after, _ := store.Load(ctx, "wf")
	if len(after) != len(before) {
		t.Fatal("invalid batch changed the event log")
	}
	if _, err := store.Append(ctx, "wf", rev, nodeEvent(t, EventNodeQueued, "a", 2)); !errors.Is(err, ErrStaleAttempt) {
		t.Fatalf("stale attempt error = %v", err)
	}
	after, _ = store.Load(ctx, "wf")
	if len(after) != len(before) {
		t.Fatal("stale attempt changed the event log")
	}
}

func TestMemoryStoreListUnfinishedAndCorruption(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryEventStore()
	for _, id := range []string{"b", "a"} {
		if _, err := store.Append(ctx, id, 0, created(t, node("n"))); err != nil {
			t.Fatal(err)
		}
	}
	finished, _ := store.Append(ctx, "c", 0, created(t, node("n")))
	_ = finished
	if _, err := store.Append(ctx, "c", 1,
		mustEv(t, EventWorkflowStarted, struct{}{}, testNow),
		nodeEvent(t, EventNodeSkipped, "n", 1),
		mustEv(t, EventWorkflowCancelled, WorkflowTerminalPayload{}, testNow),
	); err != nil {
		t.Fatal(err)
	}
	ids, err := store.ListUnfinished(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 2 || ids[0] != "a" || ids[1] != "b" {
		t.Fatalf("ids = %v", ids)
	}
	store.mu.Lock()
	store.events["bad"] = []Event{{Type: EventNodeQueued, Seq: 1, At: time.Now()}}
	store.mu.Unlock()
	if _, err := store.ListUnfinished(ctx); err == nil {
		t.Fatal("corrupt log ignored")
	}
}

func TestMemoryStoreContext(t *testing.T) {
	store := NewMemoryEventStore()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := store.Append(ctx, "wf", 0, Event{Type: EventWorkflowCreated}); !errors.Is(err, context.Canceled) {
		t.Fatalf("append = %v", err)
	}
	if _, err := store.Load(ctx, "wf"); !errors.Is(err, context.Canceled) {
		t.Fatalf("load = %v", err)
	}
	if _, err := store.ListUnfinished(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("list = %v", err)
	}
}
