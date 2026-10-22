package app

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"

	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/config"
	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/gitlab"
	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/jobs"
	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/secret"
	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/store"
)

const developerAccess = 30

// reviewEnqueuer is implemented by stores that assign review generations in
// the enqueue transaction. Fakes without it keep the M0 behavior.
type reviewEnqueuer interface {
	EnqueueReviewLatest(ctx context.Context, command store.ReviewCommand) (store.ReviewEnqueueResult, error)
}

// CommandStore is the persistence surface needed by CommandWorker.
type CommandStore interface {
	GetProject(ctx context.Context, id int64) (*store.Project, error)
	Enqueue(ctx context.Context, job store.Job) (string, bool, error)
	EnqueueLatest(ctx context.Context, job store.Job) (string, bool, error)
}

// CommandWorker revalidates `/review` note commands and only then enqueues a
// review run or cancel. It never replies to GitLab in M0.
type CommandWorker struct {
	store        CommandStore
	gitlab       gitlab.API
	cipher       *secret.Cipher
	profiles     map[string]config.ProfileConfig
	botUsernames map[string]bool
	logger       *slog.Logger
}

// CommandWorkerConfig configures CommandWorker.
type CommandWorkerConfig struct {
	Store        CommandStore
	GitLab       gitlab.API
	Cipher       *secret.Cipher
	Profiles     map[string]config.ProfileConfig
	BotUsernames []string
	Logger       *slog.Logger
}

// NewCommandWorker builds the bounded M0 command handler.
func NewCommandWorker(cfg CommandWorkerConfig) (*CommandWorker, error) {
	if cfg.Store == nil || cfg.GitLab == nil || cfg.Cipher == nil {
		return nil, errors.New("app: command worker dependencies are required")
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	bots := make(map[string]bool, len(cfg.BotUsernames))
	for _, name := range cfg.BotUsernames {
		if name != "" {
			bots[name] = true
		}
	}
	return &CommandWorker{
		store:        cfg.Store,
		gitlab:       cfg.GitLab,
		cipher:       cfg.Cipher,
		profiles:     cfg.Profiles,
		botUsernames: bots,
		logger:       logger,
	}, nil
}

type commandPayload struct {
	ProjectID      int64  `json:"project_id"`
	MRIID          int64  `json:"mr_iid"`
	NoteID         int64  `json:"note_id"`
	AuthorID       int64  `json:"author_id"`
	AuthorUsername string `json:"author_username"`
	AuthorBot      bool   `json:"author_bot"`
	Command        string `json:"command"`
	Profile        string `json:"profile"`
	Full           bool   `json:"full"`
	Trigger        string `json:"trigger"`
}

// Handle implements jobs.Handler for review.command. Returning nil settles the
// job as done; returning an error lets the worker retry with backoff.
func (w *CommandWorker) Handle(ctx context.Context, job *jobs.Job) error {
	if job == nil || job.Kind != "review.command" {
		return nil
	}
	var payload commandPayload
	dec := json.NewDecoder(strings.NewReader(string(job.Payload)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&payload); err != nil {
		return nil
	}
	if payload.ProjectID <= 0 || payload.MRIID <= 0 || payload.NoteID <= 0 ||
		payload.AuthorID <= 0 || (payload.Command != commandRun && payload.Command != commandCancel) {
		return nil
	}

	project, err := w.store.GetProject(ctx, payload.ProjectID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		return err
	}
	if project == nil || !project.Enabled {
		return nil
	}
	if payload.AuthorBot || w.botUsernames[payload.AuthorUsername] {
		return nil
	}
	token, err := w.cipher.Decrypt(project.TokenEnc)
	if err != nil {
		w.logger.Warn("command worker could not decrypt project token", "project_id", project.ID)
		return errors.New("app: project token unavailable")
	}

	current, err := w.gitlab.CurrentUser(ctx, string(token))
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return err
		}
		return errors.New("app: cannot verify bot identity")
	}
	if current != nil && (current.ID == payload.AuthorID || (payload.AuthorUsername != "" && current.Username == payload.AuthorUsername)) {
		return nil
	}

	mr, err := w.gitlab.GetMergeRequest(ctx, string(token), project.ID, payload.MRIID)
	if err != nil {
		if gitlab.IsNotFound(err) {
			return nil
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return err
		}
		return errors.New("app: cannot read merge request")
	}
	if mr == nil || !isOpenState(mr.State) {
		return nil
	}
	if mr.TargetProjectID > 0 && mr.TargetProjectID != project.ID {
		return nil
	}

	access, err := w.gitlab.GetMemberAccess(ctx, string(token), project.ID, payload.AuthorID)
	if err != nil {
		if gitlab.IsNotFound(err) {
			return nil
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return err
		}
		return errors.New("app: cannot verify command author access")
	}
	if access < developerAccess {
		return nil
	}

	if payload.Command == commandRun {
		sha := strings.TrimSpace(mr.SHA)
		if !shaRE.MatchString(sha) || isZeroSHA(sha) {
			return errors.New("app: merge request head SHA is unavailable")
		}
	}

	if payload.Command == commandCancel {
		commandPayload := map[string]any{"project_id": project.ID, "mr_iid": payload.MRIID, "trigger": "command", "note_id": payload.NoteID, "author_id": payload.AuthorID}
		if enqueuer, ok := w.store.(reviewEnqueuer); ok {
			result, err := enqueuer.EnqueueReviewLatest(ctx, store.ReviewCommand{
				ProjectID:   project.ID,
				MRIID:       payload.MRIID,
				Cancel:      true,
				Payload:     mustJSON(commandPayload),
				MaxAttempts: 3,
			})
			if err != nil {
				return err
			}
			w.logAccepted(project.ID, payload, result.JobID, result.Duplicate)
			return nil
		}
		jobID, duplicate, err := w.store.Enqueue(ctx, store.Job{
			Kind:        "review.cancel",
			Key:         reviewCancelKey(project.ID, payload.MRIID),
			Payload:     mustJSON(commandPayload),
			MaxAttempts: 3,
		})
		if err != nil {
			return err
		}
		w.logAccepted(project.ID, payload, jobID, duplicate)
		return nil
	}

	profile := payload.Profile
	if _, ok := w.profiles[profile]; !ok {
		profile = project.Profile
	}
	if _, ok := w.profiles[profile]; !ok {
		profile = "default"
	}
	runPayload := map[string]any{
		"project_id": project.ID,
		"mr_iid":     payload.MRIID,
		"head_sha":   strings.ToLower(strings.TrimSpace(mr.SHA)),
		"profile":    profile,
		"trigger":    "command",
		"full":       payload.Full,
	}
	if enqueuer, ok := w.store.(reviewEnqueuer); ok {
		result, err := enqueuer.EnqueueReviewLatest(ctx, store.ReviewCommand{
			ProjectID:   project.ID,
			MRIID:       payload.MRIID,
			HeadSHA:     strings.ToLower(strings.TrimSpace(mr.SHA)),
			Payload:     mustJSON(runPayload),
			MaxAttempts: 3,
		})
		if err != nil {
			return err
		}
		w.logAccepted(project.ID, payload, result.JobID, result.Duplicate)
		return nil
	}
	jobID, duplicate, err := w.store.EnqueueLatest(ctx, store.Job{
		Kind:        "review.run",
		Key:         reviewRunKey(project.ID, payload.MRIID),
		Payload:     mustJSON(runPayload),
		MaxAttempts: 3,
	})
	if err != nil {
		return err
	}
	w.logAccepted(project.ID, payload, jobID, duplicate)
	return nil
}

func (w *CommandWorker) logAccepted(projectID int64, payload commandPayload, jobID string, duplicate bool) {
	w.logger.Info("review command accepted",
		"project_id", projectID,
		"mr_iid", payload.MRIID,
		"command", payload.Command,
		"job_id", jobID,
		"duplicate", duplicate,
	)
}

func mustJSON(value any) json.RawMessage {
	encoded, err := json.Marshal(value)
	if err != nil {
		return json.RawMessage(`{}`)
	}
	return encoded
}
