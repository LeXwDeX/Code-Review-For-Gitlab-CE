package app

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"time"

	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/gitlab"
	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/secret"
	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/store"
	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/workspace"
)

const (
	defaultFeedbackInterval = 6 * time.Hour
	feedbackWindow          = 14 * 24 * time.Hour
	feedbackPageLimit       = 100
	feedbackMaxPages        = 5
)

// FeedbackService implements F8: periodic award and resolve collection.
type FeedbackService struct {
	store     ReviewStore
	gitlab    GitLabClient
	cipher    *secret.Cipher
	git       GitOps
	workspace *workspace.Manager
	logger    *slog.Logger
	now       func() time.Time
}

// FeedbackServiceConfig configures FeedbackService.
type FeedbackServiceConfig struct {
	Store     ReviewStore
	GitLab    GitLabClient
	Cipher    *secret.Cipher
	Git       GitOps
	Workspace *workspace.Manager
	Logger    *slog.Logger
	Now       func() time.Time
}

// NewFeedbackService builds the F8 collector.
func NewFeedbackService(cfg FeedbackServiceConfig) (*FeedbackService, error) {
	if cfg.Store == nil || cfg.GitLab == nil || cfg.Cipher == nil || cfg.Workspace == nil {
		return nil, fmt.Errorf("app: feedback service dependencies are required")
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	return &FeedbackService{
		store:     cfg.Store,
		gitlab:    cfg.GitLab,
		cipher:    cfg.Cipher,
		git:       cfg.Git,
		workspace: cfg.Workspace,
		logger:    logger,
		now:       now,
	}, nil
}

// Start runs the collector immediately and then on every interval until ctx is
// cancelled. It returns when the context is cancelled.
func (s *FeedbackService) Start(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = defaultFeedbackInterval
	}
	s.RunOnce(ctx)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.RunOnce(ctx)
		}
	}
}

// RunOnce performs one collection pass.
func (s *FeedbackService) RunOnce(ctx context.Context) {
	projects := make(map[int64]*store.Project)
	tokens := make(map[int64]string)
	seen := 0
	cutoff := s.now().UTC().Add(-feedbackWindow)
	cursor := ""
	for page := 0; page < feedbackMaxPages; page++ {
		workflows, next, err := s.store.ListWorkflows(ctx, store.WorkflowFilter{Kind: "review", Status: "completed", Since: cutoff}, cursor, feedbackPageLimit)
		if err != nil {
			s.logger.Warn("feedback workflow lookup failed")
			return
		}
		for _, workflow := range workflows {
			if err := ctx.Err(); err != nil {
				return
			}
			seen++
			s.collectWorkflow(ctx, workflow, projects, tokens)
		}
		if len(workflows) < feedbackPageLimit || next == "" {
			break
		}
		cursor = next
	}
	if seen > 0 {
		s.logger.Info("feedback collection finished", "workflows", seen)
	}
}

func (s *FeedbackService) collectWorkflow(ctx context.Context, workflow store.Workflow, projects map[int64]*store.Project, tokens map[int64]string) {
	if workflow.MRIID == nil {
		return
	}
	mrIID := *workflow.MRIID
	project, token, ok := s.project(ctx, workflow.ProjectID, projects, tokens)
	if !ok {
		return
	}
	mr, err := s.gitlab.GetMergeRequest(ctx, token, project.ID, mrIID)
	if err != nil {
		return
	}
	if mr == nil || mr.State == "closed" {
		return
	}
	publications, err := s.store.ListPublications(ctx, workflow.ID)
	if err != nil {
		return
	}
	for _, publication := range publications {
		switch publication.Kind {
		case summaryPublishKind:
			noteID, err := strconv.ParseInt(publication.GitlabID, 10, 64)
			if err != nil || noteID <= 0 {
				continue
			}
			s.collectAwards(ctx, token, publication, mrIID, noteID)
		case discussionPublishKind:
			s.collectDiscussion(ctx, token, publication, workflow, mr, mrIID)
		}
	}
}

func (s *FeedbackService) collectAwards(ctx context.Context, token string, publication store.Publication, mrIID, noteID int64) {
	awards, err := s.gitlab.ListNoteAwards(ctx, token, publication.ProjectID, mrIID, noteID)
	if err != nil {
		return
	}
	for _, award := range awards {
		signal := ""
		switch award.Name {
		case "thumbsup":
			signal = "thumbsup"
		case "thumbsdown":
			signal = "thumbsdown"
		default:
			continue
		}
		userID := award.UserID
		if _, err := s.store.UpsertFeedback(ctx, store.Feedback{
			ProjectID:     publication.ProjectID,
			MRIID:         mrIID,
			PublicationID: publication.ID,
			Signal:        signal,
			GitlabUserID:  &userID,
			ObservedAt:    s.now().UTC(),
		}); err != nil {
			s.logger.Warn("feedback upsert failed", "publication_id", publication.ID)
		}
	}
}

func (s *FeedbackService) collectDiscussion(ctx context.Context, token string, publication store.Publication, workflow store.Workflow, mr *gitlab.MergeRequest, mrIID int64) {
	discussions, err := s.gitlab.ListDiscussions(ctx, token, publication.ProjectID, mrIID)
	if err != nil {
		return
	}
	var notes []gitlab.Note
	for _, discussion := range discussions {
		if discussion.ID != publication.GitlabID {
			continue
		}
		notes = discussion.Notes
		break
	}
	if notes == nil {
		return
	}
	resolved := false
	for _, note := range notes {
		if note.Resolved {
			resolved = true
		}
		s.collectNoteAwards(ctx, token, publication, mrIID, note.ID)
	}
	if !resolved {
		return
	}
	signal := s.resolveSignal(ctx, publication, workflow, mr)
	if signal == "" {
		return
	}
	if _, err := s.store.UpsertFeedback(ctx, store.Feedback{
		ProjectID:     publication.ProjectID,
		MRIID:         mrIID,
		PublicationID: publication.ID,
		Signal:        signal,
		ObservedAt:    s.now().UTC(),
	}); err != nil {
		s.logger.Warn("feedback upsert failed", "publication_id", publication.ID)
	}
}

func (s *FeedbackService) collectNoteAwards(ctx context.Context, token string, publication store.Publication, mrIID, noteID int64) {
	awards, err := s.gitlab.ListNoteAwards(ctx, token, publication.ProjectID, mrIID, noteID)
	if err != nil {
		return
	}
	for _, award := range awards {
		signal := ""
		switch award.Name {
		case "thumbsup":
			signal = "thumbsup"
		case "thumbsdown":
			signal = "thumbsdown"
		default:
			continue
		}
		userID := award.UserID
		if _, err := s.store.UpsertFeedback(ctx, store.Feedback{
			ProjectID:     publication.ProjectID,
			MRIID:         mrIID,
			PublicationID: publication.ID,
			Signal:        signal,
			GitlabUserID:  &userID,
			ObservedAt:    s.now().UTC(),
		}); err != nil {
			s.logger.Warn("feedback upsert failed", "publication_id", publication.ID)
		}
	}
}

// resolveSignal reports resolved_changed when the finding path changed between
// the published head and the current head.
func (s *FeedbackService) resolveSignal(ctx context.Context, publication store.Publication, workflow store.Workflow, mr *gitlab.MergeRequest) string {
	findings, err := s.store.ListFindings(ctx, store.FindingFilter{WorkflowID: workflow.ID})
	if err != nil {
		return "resolved_unchanged"
	}
	path := ""
	for _, finding := range findings {
		if finding.PublishKey == publication.PublishKey {
			path = finding.Path
			break
		}
	}
	if path == "" || mr == nil || mr.SHA == "" || workflow.HeadSHA == "" || s.git == nil {
		return "resolved_unchanged"
	}
	if mr.SHA == workflow.HeadSHA {
		return "resolved_unchanged"
	}
	ws := s.workspace.Get(publication.ProjectID)
	if ws == nil {
		return "resolved_unchanged"
	}
	changed, err := s.git.ChangedFiles(ctx, ws.MirrorDir(), workflow.HeadSHA, mr.SHA)
	if err != nil {
		return "resolved_unchanged"
	}
	for _, file := range changed {
		if file == path {
			return "resolved_changed"
		}
	}
	return "resolved_unchanged"
}

func (s *FeedbackService) project(ctx context.Context, projectID int64, projects map[int64]*store.Project, tokens map[int64]string) (*store.Project, string, bool) {
	if project, ok := projects[projectID]; ok {
		return project, tokens[projectID], project != nil
	}
	project, err := s.store.GetProject(ctx, projectID)
	if err != nil || project == nil || !project.Enabled {
		projects[projectID] = nil
		return nil, "", false
	}
	token, err := s.cipher.Decrypt(project.TokenEnc)
	if err != nil {
		projects[projectID] = nil
		return nil, "", false
	}
	projects[projectID] = project
	tokens[projectID] = string(token)
	return project, tokens[projectID], true
}
