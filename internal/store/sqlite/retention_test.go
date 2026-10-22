package sqlite

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/store"
)

func insertLLMCallRow(t *testing.T, s *Store, id string, createdAt time.Time) {
	t.Helper()
	_, err := s.db.Exec(`INSERT INTO llm_calls
		(id, workflow_id, node_id, turn, provider, model, latency_ms, created_at)
		VALUES (?, 'wf', 'node', 1, 'provider', 'model', 1, ?)`, id, formatTime(createdAt))
	if err != nil {
		t.Fatalf("insert llm call %s: %v", id, err)
	}
}

func insertDeliveryRow(t *testing.T, s *Store, projectID int64, uuid string, receivedAt time.Time) {
	t.Helper()
	_, err := s.db.Exec(`INSERT INTO webhook_deliveries
		(project_id, uuid, event, outcome, received_at)
		VALUES (?, ?, 'Push Hook', 'ignored', ?)`, projectID, uuid, formatTime(receivedAt))
	if err != nil {
		t.Fatalf("insert delivery %s: %v", uuid, err)
	}
}

func insertJobRow(t *testing.T, s *Store, id, state string, updatedAt time.Time) {
	t.Helper()
	_, err := s.db.Exec(`INSERT INTO jobs
		(id, kind, payload, state, run_after, created_at, updated_at)
		VALUES (?, 'retention.test', '{}', ?, ?, ?, ?)`,
		id, state, formatTime(updatedAt), formatTime(updatedAt), formatTime(updatedAt))
	if err != nil {
		t.Fatalf("insert job %s: %v", id, err)
	}
}

func TestRetentionDeletesExpiredAndKeepsActiveRows(t *testing.T) {
	ctx := context.Background()
	clock := newFakeClock()
	s := openTestStore(t, clock)
	now := clock.Now()
	cutoff := now.Add(-24 * time.Hour)

	insertLLMCallRow(t, s, "llm-old", now.Add(-48*time.Hour))
	insertLLMCallRow(t, s, "llm-fresh", now.Add(-time.Hour))
	insertDeliveryRow(t, s, 1, "delivery-old", now.Add(-48*time.Hour))
	insertDeliveryRow(t, s, 1, "delivery-fresh", now.Add(-time.Hour))
	for _, job := range []struct {
		id    string
		state string
		at    time.Time
	}{
		{id: "job-old-done", state: store.JobDone, at: now.Add(-48 * time.Hour)},
		{id: "job-old-failed", state: store.JobFailed, at: now.Add(-48 * time.Hour)},
		{id: "job-old-cancelled", state: store.JobCancelled, at: now.Add(-48 * time.Hour)},
		{id: "job-old-queued", state: store.JobQueued, at: now.Add(-48 * time.Hour)},
		{id: "job-old-running", state: store.JobRunning, at: now.Add(-48 * time.Hour)},
		{id: "job-fresh-done", state: store.JobDone, at: now.Add(-time.Hour)},
	} {
		insertJobRow(t, s, job.id, job.state, job.at)
	}

	if n, err := s.DeleteLLMCallsBefore(ctx, cutoff); err != nil || n != 1 {
		t.Fatalf("DeleteLLMCallsBefore = %d, %v; want 1", n, err)
	}
	if n, err := s.DeleteDeliveriesBefore(ctx, cutoff); err != nil || n != 1 {
		t.Fatalf("DeleteDeliveriesBefore = %d, %v; want 1", n, err)
	}
	if n, err := s.DeleteTerminalJobsBefore(ctx, cutoff); err != nil || n != 3 {
		t.Fatalf("DeleteTerminalJobsBefore = %d, %v; want 3", n, err)
	}

	if got := rowCount(t, s, `SELECT COUNT(*) FROM llm_calls`); got != 1 {
		t.Errorf("llm_calls = %d, want 1", got)
	}
	if got := rowCount(t, s, `SELECT COUNT(*) FROM llm_calls WHERE id = 'llm-fresh'`); got != 1 {
		t.Errorf("fresh llm call was deleted")
	}
	if got := rowCount(t, s, `SELECT COUNT(*) FROM webhook_deliveries`); got != 1 {
		t.Errorf("webhook_deliveries = %d, want 1", got)
	}
	if got := rowCount(t, s, `SELECT COUNT(*) FROM webhook_deliveries WHERE uuid = 'delivery-fresh'`); got != 1 {
		t.Errorf("fresh delivery was deleted")
	}
	if got := rowCount(t, s, `SELECT COUNT(*) FROM jobs`); got != 3 {
		t.Errorf("jobs = %d, want 3", got)
	}
	for _, id := range []string{"job-old-queued", "job-old-running"} {
		if got := rowCount(t, s, `SELECT COUNT(*) FROM jobs WHERE id = ?`, id); got != 1 {
			t.Errorf("%s was deleted", id)
		}
	}

	if n, err := s.DeleteLLMCallsBefore(ctx, cutoff); err != nil || n != 0 {
		t.Errorf("second DeleteLLMCallsBefore = %d, %v; want 0", n, err)
	}
	if n, err := s.DeleteDeliveriesBefore(ctx, cutoff); err != nil || n != 0 {
		t.Errorf("second DeleteDeliveriesBefore = %d, %v; want 0", n, err)
	}
	if n, err := s.DeleteTerminalJobsBefore(ctx, cutoff); err != nil || n != 0 {
		t.Errorf("second DeleteTerminalJobsBefore = %d, %v; want 0", n, err)
	}

	if _, err := s.DeleteLLMCallsBefore(ctx, time.Time{}); err == nil {
		t.Error("zero llm call cutoff accepted")
	}
	if _, err := s.DeleteDeliveriesBefore(ctx, time.Time{}); err == nil {
		t.Error("zero delivery cutoff accepted")
	}
	if _, err := s.DeleteTerminalJobsBefore(ctx, time.Time{}); err == nil {
		t.Error("zero job cutoff accepted")
	}
	if _, err := s.PruneContextDocs(ctx, 0); err == nil {
		t.Error("zero context versions accepted")
	}
}

func TestRetentionDeleteBatchesAllExpiredRows(t *testing.T) {
	ctx := context.Background()
	clock := newFakeClock()
	s := openTestStore(t, clock)
	expired := clock.Now().Add(-48 * time.Hour)
	cutoff := clock.Now().Add(-24 * time.Hour)
	const rows = retentionBatch + 1

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	llm, err := tx.PrepareContext(ctx, `INSERT INTO llm_calls
		(id, workflow_id, node_id, turn, provider, model, latency_ms, created_at)
		VALUES (?, 'wf', 'node', 1, 'provider', 'model', 1, ?)`)
	if err != nil {
		t.Fatalf("prepare llm: %v", err)
	}
	delivery, err := tx.PrepareContext(ctx, `INSERT INTO webhook_deliveries
		(project_id, uuid, event, outcome, received_at)
		VALUES (1, ?, 'Push Hook', 'ignored', ?)`)
	if err != nil {
		t.Fatalf("prepare delivery: %v", err)
	}
	job, err := tx.PrepareContext(ctx, `INSERT INTO jobs
		(id, kind, payload, state, run_after, created_at, updated_at)
		VALUES (?, 'retention.batch', '{}', 'done', ?, ?, ?)`)
	if err != nil {
		t.Fatalf("prepare job: %v", err)
	}
	stamp := formatTime(expired)
	for i := 0; i < rows; i++ {
		if _, err := llm.ExecContext(ctx, fmt.Sprintf("llm-%03d", i), stamp); err != nil {
			t.Fatalf("insert llm %d: %v", i, err)
		}
		if _, err := delivery.ExecContext(ctx, fmt.Sprintf("uuid-%03d", i), stamp); err != nil {
			t.Fatalf("insert delivery %d: %v", i, err)
		}
		if _, err := job.ExecContext(ctx, fmt.Sprintf("job-%03d", i), stamp, stamp, stamp); err != nil {
			t.Fatalf("insert job %d: %v", i, err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	if n, err := s.DeleteLLMCallsBefore(ctx, cutoff); err != nil || n != rows {
		t.Fatalf("DeleteLLMCallsBefore = %d, %v; want %d", n, err, rows)
	}
	if n, err := s.DeleteDeliveriesBefore(ctx, cutoff); err != nil || n != rows {
		t.Fatalf("DeleteDeliveriesBefore = %d, %v; want %d", n, err, rows)
	}
	if n, err := s.DeleteTerminalJobsBefore(ctx, cutoff); err != nil || n != rows {
		t.Fatalf("DeleteTerminalJobsBefore = %d, %v; want %d", n, err, rows)
	}
	if got := rowCount(t, s, `SELECT COUNT(*) FROM llm_calls`); got != 0 {
		t.Errorf("llm_calls remaining = %d, want 0", got)
	}
	if got := rowCount(t, s, `SELECT COUNT(*) FROM webhook_deliveries`); got != 0 {
		t.Errorf("webhook_deliveries remaining = %d, want 0", got)
	}
	if got := rowCount(t, s, `SELECT COUNT(*) FROM jobs`); got != 0 {
		t.Errorf("jobs remaining = %d, want 0", got)
	}
}

func TestPruneContextDocsKeepsNewestUnlockedAndLocked(t *testing.T) {
	ctx := context.Background()
	clock := newFakeClock()
	s := openTestStore(t, clock)
	mustProject(t, s, 1)
	mustProject(t, s, 2)
	now := clock.Now()

	insert := func(id string, projectID int64, kind string, locked bool, at time.Time) {
		t.Helper()
		if _, err := s.db.Exec(`INSERT INTO context_docs
			(id, project_id, base_sha, kind, source, content, locked, created_at)
			VALUES (?, ?, ?, ?, 'generated', 'content', ?, ?)`,
			id, projectID, id, kind, boolInt(locked), formatTime(at)); err != nil {
			t.Fatalf("insert context doc %s: %v", id, err)
		}
	}
	insert("agents-v0-locked", 1, "agents", true, now.Add(-5*time.Hour))
	for i := 1; i <= 5; i++ {
		insert(fmt.Sprintf("agents-v%d", i), 1, "agents", false, now.Add(time.Duration(i-5)*time.Hour))
	}
	for i := 1; i <= 3; i++ {
		insert(fmt.Sprintf("claude-v%d", i), 1, "claude", false, now.Add(time.Duration(i-4)*time.Hour))
	}
	insert("p2-agents-a", 2, "agents", false, now.Add(-2*time.Hour))
	insert("p2-agents-b", 2, "agents", false, now.Add(-time.Hour))

	if n, err := s.PruneContextDocs(ctx, 2); err != nil || n != 4 {
		t.Fatalf("PruneContextDocs = %d, %v; want 4", n, err)
	}
	if got := rowCount(t, s, `SELECT COUNT(*) FROM context_docs WHERE project_id = 1 AND kind = 'agents'`); got != 3 {
		t.Errorf("project 1 agents = %d, want 3", got)
	}
	if got := rowCount(t, s, `SELECT COUNT(*) FROM context_docs WHERE id = 'agents-v0-locked' AND locked = 1`); got != 1 {
		t.Errorf("locked context document was deleted")
	}
	for _, id := range []string{"agents-v1", "agents-v2", "agents-v3"} {
		if got := rowCount(t, s, `SELECT COUNT(*) FROM context_docs WHERE id = ?`, id); got != 0 {
			t.Errorf("%s was not pruned", id)
		}
	}
	if got := rowCount(t, s, `SELECT COUNT(*) FROM context_docs WHERE project_id = 1 AND kind = 'claude'`); got != 2 {
		t.Errorf("project 1 claude = %d, want 2", got)
	}
	if got := rowCount(t, s, `SELECT COUNT(*) FROM context_docs WHERE project_id = 2`); got != 2 {
		t.Errorf("project 2 context docs = %d, want 2", got)
	}

	if n, err := s.PruneContextDocs(ctx, 2); err != nil || n != 0 {
		t.Fatalf("second PruneContextDocs = %d, %v; want 0", n, err)
	}
}
