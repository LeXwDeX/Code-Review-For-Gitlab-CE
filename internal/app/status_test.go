package app

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/gitlab"
	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/review"
	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/store"
)

func testStatusReporter(api *reviewGitLab) *statusReporter {
	return newStatusReporter(api, nil)
}

func statusPhases(updates []string) []string {
	phases := make([]string, 0, len(updates))
	for _, body := range updates {
		remaining := body
		for {
			idx := strings.Index(remaining, "- 阶段：`")
			if idx < 0 {
				break
			}
			remaining = remaining[idx+len("- 阶段：`"):]
			end := strings.IndexByte(remaining, '`')
			if end < 0 {
				break
			}
			phases = append(phases, remaining[:end])
			remaining = remaining[end+1:]
		}
	}
	return phases
}

func (f *reviewGitLab) awardSnapshot(noteID int64) []gitlab.Award {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]gitlab.Award(nil), f.awards[noteID]...)
}

func TestStatusReporterUpsertsSingleNote(t *testing.T) {
	api := newReviewGitLab(reviewTestHead)
	reporter := testStatusReporter(api)

	reporter.report(context.Background(), 42, 7, "token", reviewStatusUpdate{
		State: reviewStatusRunning, HeadSHA: reviewTestHead,
	})
	reporter.report(context.Background(), 42, 7, "token", reviewStatusUpdate{
		State: reviewStatusRunning, HeadSHA: reviewTestHead,
	})
	if got := api.noteCount(); got != 1 {
		t.Fatalf("status notes = %d, want 1", got)
	}
	if len(api.statusUpdates) != 1 {
		t.Fatalf("status writes = %d, want one identical write skipped", len(api.statusUpdates))
	}
	if !strings.HasPrefix(api.notes[0].Body, reviewStatusMarkerLine) {
		t.Fatalf("status marker missing: %q", api.notes[0].Body)
	}

	reporter.report(context.Background(), 42, 7, "token", reviewStatusUpdate{
		State: reviewStatusPublishing, HeadSHA: reviewTestHead,
	})
	if got := api.noteCount(); got != 1 {
		t.Fatalf("status notes after update = %d, want 1", got)
	}
	if len(api.statusUpdates) != 2 || !strings.Contains(api.notes[0].Body, reviewStatusPublishing) {
		t.Fatalf("status update = %q, writes = %d", api.notes[0].Body, len(api.statusUpdates))
	}
}

func TestStatusReporterIgnoresForeignAndFailedNotes(t *testing.T) {
	api := newReviewGitLab(reviewTestHead)
	api.notes = []gitlab.Note{{
		ID: 1, AuthorID: 3, Body: reviewStatusMarkerLine + "\nforeign status",
	}}
	if err := api.UpdateMRNote(context.Background(), "token", 42, 7, 1, "changed"); err != nil {
		t.Fatal(err)
	}
	api.summaryUpdates = 0
	reporter := testStatusReporter(api)
	reporter.report(context.Background(), 42, 7, "token", reviewStatusUpdate{State: reviewStatusRunning})
	if got := api.noteCount(); got != 2 {
		t.Fatalf("notes = %d, want a new bot-owned status note", got)
	}
	if api.summaryUpdates != 0 {
		t.Fatalf("foreign status note was updated %d times", api.summaryUpdates)
	}

	api.statusNoteErr = errors.New("status unavailable")
	reporter.report(context.Background(), 42, 7, "token", reviewStatusUpdate{State: reviewStatusPublishing})
	if len(api.statusUpdates) != 1 {
		t.Fatalf("failed status write changed the note: %d writes", len(api.statusUpdates))
	}
}

func TestStatusReporterReactionIdempotencyAndOwnership(t *testing.T) {
	api := newReviewGitLab(reviewTestHead)
	api.awards[555] = []gitlab.Award{
		{ID: 1, Name: reviewAwardEyes, UserID: 9, Username: "review-bot"},
		{ID: 2, Name: reviewAwardEyes, UserID: 3, Username: "dev"},
	}
	reporter := testStatusReporter(api)
	update := reviewStatusUpdate{
		State:     reviewStatusPass,
		NoteID:    555,
		Reaction:  reviewAwardPass,
		ClearEyes: true,
	}
	reporter.report(context.Background(), 42, 7, "token", update)
	reporter.report(context.Background(), 42, 7, "token", update)

	awards := api.awardSnapshot(555)
	var botEyes, otherEyes, passes int
	for _, award := range awards {
		switch {
		case award.Name == reviewAwardEyes && award.UserID == 9:
			botEyes++
		case award.Name == reviewAwardEyes && award.UserID == 3:
			otherEyes++
		case award.Name == reviewAwardPass && award.UserID == 9:
			passes++
		}
	}
	if botEyes != 0 {
		t.Fatalf("bot eyes were not cleared: %+v", awards)
	}
	if otherEyes != 1 {
		t.Fatalf("another user's eyes were removed: %+v", awards)
	}
	if passes != 1 {
		t.Fatalf("pass reactions = %d, want exactly one: %+v", passes, awards)
	}
}

func TestStatusReporterAwardErrorsAreBestEffort(t *testing.T) {
	api := newReviewGitLab(reviewTestHead)
	api.awardErr = errors.New("award lookup failed")
	reporter := testStatusReporter(api)
	reporter.report(context.Background(), 42, 7, "token", reviewStatusUpdate{
		State: reviewStatusFailed, NoteID: 555, Reaction: reviewAwardFail, ClearEyes: true,
	})
	if got := api.noteCount(); got != 1 {
		t.Fatalf("status note = %d, want one", got)
	}
	if len(api.awardSnapshot(555)) != 0 {
		t.Fatal("failed award lookup still created a reaction")
	}

	api.awardErr = nil
	api.addAwardErr = errors.New("add failed")
	reporter.report(context.Background(), 42, 7, "token", reviewStatusUpdate{
		State: reviewStatusBlocking, NoteID: 555, Reaction: reviewAwardFail,
	})
	if len(api.awardSnapshot(555)) != 0 {
		t.Fatal("failed award creation was ignored")
	}
}

func TestSanitizeStatusDetail(t *testing.T) {
	raw := "node failed:\n\tglpat-secret-token https://gitlab.example.com/private " + strings.Repeat("x", 300)
	got := sanitizeStatusDetail(raw)
	if strings.Contains(got, "glpat-secret-token") || strings.Contains(got, "https://") {
		t.Fatalf("detail leaked sensitive text: %q", got)
	}
	if strings.ContainsAny(got, "\n\t") {
		t.Fatalf("detail contains control characters: %q", got)
	}
	if len([]rune(got)) > reviewStatusDetailRunes {
		t.Fatalf("detail is not bounded: %d runes", len([]rune(got)))
	}
	if !strings.Contains(got, "[redacted]") || !strings.Contains(got, "[url]") {
		t.Fatalf("detail was not redacted: %q", got)
	}
}

func TestReviewStatusLifecycleAndCommandReaction(t *testing.T) {
	f := newReviewFixture(t, reviewTestHead)
	f.service.status = testStatusReporter(f.gitlab)
	job := f.seedReview(t, "job-status-lifecycle")
	var payload map[string]any
	if err := json.Unmarshal(job.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	payload["note_id"] = float64(555)
	job.Payload = mustJSONBody(t, payload)

	if err := f.service.Handle(context.Background(), job); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if got := f.gitlab.noteCount(); got != 2 {
		t.Fatalf("notes = %d, want one summary and one status", got)
	}
	phases := statusPhases(f.gitlab.statusUpdates)
	if len(phases) != 3 || phases[0] != reviewStatusRunning ||
		phases[1] != reviewStatusPublishing || phases[2] != reviewStatusPass {
		t.Fatalf("status phases = %v, want running, publishing, PASS", phases)
	}
	awards := f.gitlab.awardSnapshot(555)
	if len(awards) != 1 || awards[0].Name != reviewAwardPass || awards[0].UserID != f.gitlab.currentUser.ID {
		t.Fatalf("command reaction = %+v", awards)
	}
}

func TestReviewStatusFailureDoesNotBlockReview(t *testing.T) {
	f := newReviewFixture(t, reviewTestHead)
	f.service.status = testStatusReporter(f.gitlab)
	f.gitlab.statusNoteErr = errors.New("status API unavailable")
	job := f.seedReview(t, "job-status-best-effort")

	if err := f.service.Handle(context.Background(), job); err != nil {
		t.Fatalf("status failure changed the review outcome: %v", err)
	}
	if got := f.gitlab.noteCount(); got != 1 {
		t.Fatalf("notes = %d, want only the summary", got)
	}
	if len(f.gitlab.statusUpdates) != 0 {
		t.Fatalf("status failures were recorded as writes: %d", len(f.gitlab.statusUpdates))
	}
	workflow, err := f.db.GetWorkflowByJob(context.Background(), job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if workflow.Verdict == "" {
		t.Fatal("review verdict was not published")
	}
}

func TestReviewStatusRejectsSupersededGeneration(t *testing.T) {
	f := newReviewFixture(t, reviewTestHead)
	f.service.status = testStatusReporter(f.gitlab)
	ctx := context.Background()
	first, err := f.db.EnqueueReviewLatest(ctx, store.ReviewCommand{
		ProjectID: 42, MRIID: 7, HeadSHA: f.head,
		Payload: json.RawMessage(`{"trigger":"command"}`), MaxAttempts: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err := f.db.EnqueueReviewLatest(ctx, store.ReviewCommand{
		ProjectID: 42, MRIID: 7, HeadSHA: f.head,
		Payload: json.RawMessage(`{"trigger":"mr_update"}`), MaxAttempts: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	project, token, err := f.service.projectToken(ctx, 42)
	if err != nil {
		t.Fatal(err)
	}
	f.service.reportStatus(ctx, project, string(token), 7, first.Generation, f.head, reviewStatusUpdate{
		State: reviewStatusRunning, Detail: "old generation",
	})
	if len(f.gitlab.statusUpdates) != 0 {
		t.Fatal("a superseded worker wrote the status note")
	}
	f.service.reportTerminal(ctx, project, string(token), 7, first.Generation, f.head, reviewStatusUpdate{
		State: reviewStatusPass, Reaction: reviewAwardPass, ClearEyes: true,
	})
	f.service.reportSuperseded(ctx, project, string(token), 7, first.Generation, f.head, "")
	if len(f.gitlab.statusUpdates) != 0 {
		t.Fatal("a superseded worker wrote a terminal status")
	}
	f.service.reportStatus(ctx, project, string(token), 7, second.Generation, f.head, reviewStatusUpdate{
		State: reviewStatusRunning, Detail: "new generation",
	})
	if len(f.gitlab.statusUpdates) != 1 || !strings.Contains(f.gitlab.statusUpdates[0], "new generation") {
		t.Fatalf("current generation status = %+v", f.gitlab.statusUpdates)
	}
}

func TestReviewStatusForVerdict(t *testing.T) {
	cases := []struct {
		verdict  string
		state    string
		reaction string
	}{
		{review.VerdictPass, reviewStatusPass, reviewAwardPass},
		{review.VerdictBlocking, reviewStatusBlocking, reviewAwardFail},
		{review.VerdictIncomplete, reviewStatusIncomplete, reviewAwardFail},
		{"", reviewStatusIncomplete, reviewAwardFail},
	}
	for _, tc := range cases {
		state, reaction := reviewStatusForVerdict(tc.verdict)
		if state != tc.state || reaction != tc.reaction {
			t.Errorf("reviewStatusForVerdict(%q) = %q, %q; want %q, %q",
				tc.verdict, state, reaction, tc.state, tc.reaction)
		}
	}
}

func TestReviewStatusAutomaticHookHasNoReaction(t *testing.T) {
	f := newReviewFixture(t, reviewTestHead)
	f.service.status = testStatusReporter(f.gitlab)
	job := f.seedReview(t, "job-status-automatic")
	if err := f.service.Handle(context.Background(), job); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(f.gitlab.awards) != 0 {
		t.Fatalf("automatic review fabricated reactions: %+v", f.gitlab.awards)
	}
	statusNotes := 0
	for _, note := range f.gitlab.notes {
		if strings.HasPrefix(note.Body, reviewStatusMarkerPrefix) {
			statusNotes++
		}
	}
	if statusNotes != 1 {
		t.Fatalf("status notes = %d, want one", statusNotes)
	}
}

func TestStatusReporterDoesNotRegressQueuedOverRunning(t *testing.T) {
	api := newReviewGitLab(reviewTestHead)
	reporter := testStatusReporter(api)
	reporter.report(context.Background(), 42, 7, "token", reviewStatusUpdate{State: reviewStatusRunning})
	reporter.report(context.Background(), 42, 7, "token", reviewStatusUpdate{State: reviewStatusQueued})
	if got := api.noteCount(); got != 1 {
		t.Fatalf("status notes = %d", got)
	}
	if phase := parseStatusPhase(api.notes[0].Body); phase != reviewStatusRunning {
		t.Fatalf("status regressed to %q", phase)
	}
	if len(api.statusUpdates) != 1 {
		t.Fatalf("status writes = %d, want the queued write skipped", len(api.statusUpdates))
	}
}
