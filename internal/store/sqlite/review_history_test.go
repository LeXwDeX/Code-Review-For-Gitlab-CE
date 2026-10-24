package sqlite

import (
	"context"
	"testing"
	"time"

	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/store"
)

func publishedFinding(id, severity, decision string) store.Finding {
	return store.Finding{
		ID:         id,
		OriginNode: "synthesize",
		Severity:   severity,
		Category:   "logic",
		Title:      "finding " + id,
		Body:       "body " + id,
		Decision:   decision,
	}
}

func mustPublication(t *testing.T, s *Store, projectID, mrIID int64, workflowID, key string) {
	t.Helper()
	if _, err := s.UpsertPublication(context.Background(), store.Publication{
		ProjectID:   projectID,
		MRIID:       mrIID,
		Kind:        "summary",
		PublishKey:  key,
		WorkflowID:  workflowID,
		Fingerprint: "fp-" + key,
	}); err != nil {
		t.Fatalf("UpsertPublication: %v", err)
	}
}

// HasPublishedSevereFinding looks only at the most recently published review
// and only at findings that were actually published or marked uncertain.
func TestHasPublishedSevereFindingUsesLatestPublishedReview(t *testing.T) {
	ctx := context.Background()
	clock := newFakeClock()
	s := openTestStore(t, clock)
	mustProject(t, s, 42)
	headA := reviewHead("a")
	headB := reviewHead("b")

	first, err := s.EnqueueReviewLatest(ctx, store.ReviewCommand{
		ProjectID: 42, MRIID: 7, HeadSHA: headA,
	})
	if err != nil {
		t.Fatalf("enqueue first: %v", err)
	}
	wfA, _, err := s.CreateReviewWorkflow(ctx, reviewWorkflowRequest(first.JobID, first.Generation, 42, 7, headA))
	if err != nil {
		t.Fatalf("create first: %v", err)
	}
	finishWorkflow(t, s, wfA, 1, clock.Now())
	mustPublication(t, s, 42, 7, wfA, "summary")
	if err := s.ReplaceFindings(ctx, wfA, []store.Finding{publishedFinding("f-a", "P1", "publish")}); err != nil {
		t.Fatalf("ReplaceFindings: %v", err)
	}
	severe, err := s.HasPublishedSevereFinding(ctx, 42, 7)
	if err != nil || !severe {
		t.Fatalf("severe = %v, %v, want true", severe, err)
	}

	// Dropped findings were refuted or dismissed; they do not force a full
	// review. Marked findings are unresolved and do.
	if err := s.ReplaceFindings(ctx, wfA, []store.Finding{publishedFinding("f-a", "P0", "drop")}); err != nil {
		t.Fatalf("ReplaceFindings drop: %v", err)
	}
	if severe, err = s.HasPublishedSevereFinding(ctx, 42, 7); err != nil || severe {
		t.Fatalf("dropped finding severe = %v, %v, want false", severe, err)
	}
	if err := s.ReplaceFindings(ctx, wfA, []store.Finding{publishedFinding("f-a", "P0", "mark")}); err != nil {
		t.Fatalf("ReplaceFindings mark: %v", err)
	}
	if severe, err = s.HasPublishedSevereFinding(ctx, 42, 7); err != nil || !severe {
		t.Fatalf("marked finding severe = %v, %v, want true", severe, err)
	}

	clock.Advance(time.Second)
	second, err := s.EnqueueReviewLatest(ctx, store.ReviewCommand{
		ProjectID: 42, MRIID: 7, HeadSHA: headB,
	})
	if err != nil {
		t.Fatalf("enqueue second: %v", err)
	}
	wfB, _, err := s.CreateReviewWorkflow(ctx, reviewWorkflowRequest(second.JobID, second.Generation, 42, 7, headB))
	if err != nil {
		t.Fatalf("create second: %v", err)
	}
	finishWorkflow(t, s, wfB, 1, clock.Now())
	mustPublication(t, s, 42, 7, wfB, "summary")
	if err := s.ReplaceFindings(ctx, wfB, []store.Finding{publishedFinding("f-b", "P2", "publish")}); err != nil {
		t.Fatalf("ReplaceFindings second: %v", err)
	}
	// The older workflow still holds P0, but the latest published review is
	// clean apart from a P2, so incremental review stays allowed.
	if severe, err = s.HasPublishedSevereFinding(ctx, 42, 7); err != nil || severe {
		t.Fatalf("latest review severe = %v, %v, want false", severe, err)
	}
}

func TestHasPublishedSevereFindingWithoutPublication(t *testing.T) {
	s := openTestStore(t, newFakeClock())
	mustProject(t, s, 42)
	severe, err := s.HasPublishedSevereFinding(context.Background(), 42, 7)
	if err != nil || severe {
		t.Fatalf("severe = %v, %v, want false", severe, err)
	}
}
