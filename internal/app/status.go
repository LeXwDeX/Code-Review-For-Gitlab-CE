package app

import (
	"context"
	"io"
	"log/slog"
	"regexp"
	"strings"
	"unicode"

	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/gitlab"
)

const (
	// reviewStatusMarkerPrefix identifies the single status note maintained by
	// this service. It is separate from the summary, finding, and reply markers.
	reviewStatusMarkerPrefix = "<!-- gitlab-ce-review:status"
	reviewStatusMarkerLine   = reviewStatusMarkerPrefix + " v1 -->"
	reviewStatusDetailRunes  = 160

	reviewStatusReceived   = "received"
	reviewStatusQueued     = "queued"
	reviewStatusRunning    = "running"
	reviewStatusPublishing = "publishing"
	reviewStatusPass       = "PASS"
	reviewStatusBlocking   = "BLOCKING"
	reviewStatusIncomplete = "INCOMPLETE"
	reviewStatusFailed     = "failed"
	reviewStatusCancelled  = "cancelled"
	reviewStatusStale      = "stale"

	reviewAwardEyes = "eyes"
	reviewAwardPass = "white_check_mark"
	reviewAwardFail = "x"
)

var (
	statusTokenRE = regexp.MustCompile(`(?i)\b(?:glpat|gloas|glrt|gldt|glcbt|glptt|glft)-[A-Za-z0-9_-]+`)
	statusURLRE   = regexp.MustCompile(`(?i)https?://[^\s]+`)
)

// statusGitLab is the GitLab surface needed to keep one status note and its
// command-source reactions up to date.
type statusGitLab interface {
	CurrentUser(ctx context.Context, token string) (*gitlab.User, error)
	ListMRNotes(ctx context.Context, token string, projectID, iid int64) ([]gitlab.Note, error)
	CreateMRNote(ctx context.Context, token string, projectID, iid int64, body string) (*gitlab.Note, error)
	UpdateMRNote(ctx context.Context, token string, projectID, iid, noteID int64, body string) error
	ListNoteAwards(ctx context.Context, token string, projectID, iid, noteID int64) ([]gitlab.Award, error)
	AddNoteAward(ctx context.Context, token string, projectID, iid, noteID int64, name string) (*gitlab.Award, error)
	DeleteNoteAward(ctx context.Context, token string, projectID, iid, noteID, awardID int64) error
}

// reviewStatusUpdate is one best-effort status write. NoteID is the command
// source note, when the review was triggered by a note. Reaction and ClearEyes
// apply only to that source note; automated merge-request hooks never receive
// a fabricated reaction.
type reviewStatusUpdate struct {
	State     string
	Detail    string
	HeadSHA   string
	NoteID    int64
	Reaction  string
	ClearEyes bool
}

// statusReporter keeps one marker-keyed note per merge request and manages the
// bot's own reactions. Every failure is logged and swallowed: status reporting
// must never fail or retry a review.
type statusReporter struct {
	gitlab statusGitLab
	logger *slog.Logger
}

func newStatusReporter(api statusGitLab, logger *slog.Logger) *statusReporter {
	if api == nil {
		return nil
	}
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &statusReporter{gitlab: api, logger: logger}
}

// report updates the status note and applies terminal reactions. It returns
// no error by design.
func (r *statusReporter) report(ctx context.Context, projectID, mrIID int64, token string, update reviewStatusUpdate) {
	if r == nil || r.gitlab == nil || projectID <= 0 || mrIID <= 0 || token == "" {
		return
	}
	if ctx.Err() != nil {
		return
	}
	botID := int64(0)
	user, err := r.gitlab.CurrentUser(ctx, token)
	if err != nil {
		r.logger.Warn("review status identity lookup failed", "project_id", projectID, "mr_iid", mrIID)
		return
	} else if user != nil {
		botID = user.ID
	}
	if botID <= 0 {
		return
	}

	r.upsertNote(ctx, projectID, mrIID, token, botID, update)

	if update.NoteID <= 0 {
		return
	}
	if update.ClearEyes {
		r.removeAward(ctx, projectID, mrIID, token, update.NoteID, botID, reviewAwardEyes)
	}
	if update.Reaction != "" {
		r.ensureAward(ctx, projectID, mrIID, token, update.NoteID, botID, update.Reaction)
	}
}

func (r *statusReporter) upsertNote(ctx context.Context, projectID, mrIID int64, token string, botID int64, update reviewStatusUpdate) {
	body := reviewStatusBody(update)
	notes, err := r.gitlab.ListMRNotes(ctx, token, projectID, mrIID)
	if err != nil {
		r.logger.Warn("review status note lookup failed", "project_id", projectID, "mr_iid", mrIID)
		return
	}
	for _, note := range notes {
		if !strings.HasPrefix(note.Body, reviewStatusMarkerPrefix) {
			continue
		}
		if botID > 0 && note.AuthorID != botID {
			continue
		}
		if note.Body == body {
			return
		}
		// A queued acknowledgement may race a worker that already started the
		// review. Never move a visible status backwards to received/queued.
		if !statusMayRegress(note.Body, update.State) {
			return
		}
		if err := r.gitlab.UpdateMRNote(ctx, token, projectID, mrIID, note.ID, body); err != nil {
			r.logger.Warn("review status note update failed", "project_id", projectID, "mr_iid", mrIID, "note_id", note.ID)
		}
		return
	}
	if _, err := r.gitlab.CreateMRNote(ctx, token, projectID, mrIID, body); err != nil {
		r.logger.Warn("review status note creation failed", "project_id", projectID, "mr_iid", mrIID)
	}
}

// statusMayRegress reports whether next may replace the previous phase. Only
// the pre-run acknowledgements are monotonic; later phases intentionally allow
// a retry to report running again.
func statusMayRegress(previousBody, next string) bool {
	if next != reviewStatusReceived && next != reviewStatusQueued {
		return true
	}
	switch parseStatusPhase(previousBody) {
	case reviewStatusRunning, reviewStatusPublishing,
		reviewStatusPass, reviewStatusBlocking, reviewStatusIncomplete,
		reviewStatusFailed, reviewStatusCancelled, reviewStatusStale:
		return false
	}
	return true
}

func parseStatusPhase(body string) string {
	const prefix = "- 阶段：`"
	idx := strings.Index(body, prefix)
	if idx < 0 {
		return ""
	}
	rest := body[idx+len(prefix):]
	end := strings.IndexByte(rest, '`')
	if end < 0 {
		return ""
	}
	return rest[:end]
}

func (r *statusReporter) ensureAward(ctx context.Context, projectID, mrIID int64, token string, noteID, botID int64, name string) {
	if botID <= 0 || noteID <= 0 || name == "" {
		return
	}
	awards, err := r.gitlab.ListNoteAwards(ctx, token, projectID, mrIID, noteID)
	if err != nil {
		r.logger.Warn("review status award lookup failed", "project_id", projectID, "mr_iid", mrIID, "note_id", noteID)
		return
	}
	for _, award := range awards {
		if award.UserID == botID && award.Name == name {
			return
		}
	}
	if _, err := r.gitlab.AddNoteAward(ctx, token, projectID, mrIID, noteID, name); err != nil {
		r.logger.Warn("review status award creation failed", "project_id", projectID, "mr_iid", mrIID, "note_id", noteID)
	}
}

func (r *statusReporter) removeAward(ctx context.Context, projectID, mrIID int64, token string, noteID, botID int64, name string) {
	if botID <= 0 || noteID <= 0 || name == "" {
		return
	}
	awards, err := r.gitlab.ListNoteAwards(ctx, token, projectID, mrIID, noteID)
	if err != nil {
		r.logger.Warn("review status award lookup failed", "project_id", projectID, "mr_iid", mrIID, "note_id", noteID)
		return
	}
	for _, award := range awards {
		if award.UserID != botID || award.Name != name {
			continue
		}
		if err := r.gitlab.DeleteNoteAward(ctx, token, projectID, mrIID, noteID, award.ID); err != nil {
			r.logger.Warn("review status award deletion failed", "project_id", projectID, "mr_iid", mrIID, "note_id", noteID)
		}
	}
}

// reviewStatusBody renders a deterministic status note. The first line is the
// hidden marker so repeated reports of the same state are no-ops.
func reviewStatusBody(update reviewStatusUpdate) string {
	var b strings.Builder
	b.WriteString(reviewStatusMarkerLine)
	b.WriteByte('\n')
	b.WriteString("**AI 评审：")
	b.WriteString(reviewStatusLabel(update.State))
	b.WriteString("**\n\n")
	b.WriteString("- 阶段：`")
	b.WriteString(sanitizeStatusDetail(update.State))
	b.WriteString("`\n")
	if head := strings.TrimSpace(update.HeadSHA); head != "" {
		b.WriteString("- 提交：`")
		b.WriteString(shortSHA(head))
		b.WriteString("`\n")
	}
	if detail := sanitizeStatusDetail(update.Detail); detail != "" {
		b.WriteString("- 详情：")
		b.WriteString(detail)
		b.WriteString("\n")
	}
	return b.String()
}

func reviewStatusLabel(state string) string {
	switch state {
	case reviewStatusReceived:
		return "已接收评审请求"
	case reviewStatusQueued:
		return "评审已入队"
	case reviewStatusRunning:
		return "评审运行中"
	case reviewStatusPublishing:
		return "正在发布评审结果"
	case reviewStatusPass:
		return "评审通过（PASS）"
	case reviewStatusBlocking:
		return "发现问题（BLOCKING）"
	case reviewStatusIncomplete:
		return "评审未完成（INCOMPLETE）"
	case reviewStatusFailed:
		return "评审失败"
	case reviewStatusCancelled:
		return "评审已取消"
	case reviewStatusStale:
		return "评审已过期"
	default:
		return sanitizeStatusDetail(state)
	}
}

// sanitizeStatusDetail bounds and redacts untrusted status text. Only a fixed
// set of internal reasons is expected, so URLs and token-like strings are
// replaced rather than risk leaking upstream details or credentials.
func sanitizeStatusDetail(value string) string {
	value = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, value)
	value = strings.Join(strings.Fields(value), " ")
	value = statusTokenRE.ReplaceAllString(value, "[redacted]")
	value = statusURLRE.ReplaceAllString(value, "[url]")
	return truncateRunes(value, reviewStatusDetailRunes)
}

func shortSHA(value string) string {
	if len(value) > 7 {
		return value[:7]
	}
	return value
}
