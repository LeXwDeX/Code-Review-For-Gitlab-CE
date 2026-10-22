package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/config"
	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/gitlab"
	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/jobs"
	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/repo"
	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/review"
	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/secret"
	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/store"
	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/store/sqlite"
	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/workspace"
	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/pkg/dag"
	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/prompts"
	"go.yaml.in/yaml/v3"
)

const (
	defaultCancelPoll     = 2 * time.Second
	defaultCancelWait     = 45 * time.Second
	defaultReviewMaxFiles = 200
)

// ReviewStore is the persistence surface needed by review orchestration.
type ReviewStore interface {
	dag.EventStore
	GetProject(ctx context.Context, id int64) (*store.Project, error)
	GetReviewControl(ctx context.Context, projectID, mrIID int64) (*store.ReviewControl, error)
	FindActiveReview(ctx context.Context, projectID, mrIID int64) (*store.Workflow, error)
	CreateReviewWorkflow(ctx context.Context, req sqlite.ReviewWorkflowRequest) (string, bool, error)
	DetachWorkflowJob(ctx context.Context, workflowID, jobID string) error
	GetWorkflowByJob(ctx context.Context, jobID string) (*store.Workflow, error)
	GetWorkflow(ctx context.Context, id string) (*store.Workflow, error)
	GetWorkflowDossier(ctx context.Context, workflowID string) (*store.WorkflowDossier, error)
	SaveWorkflowDossier(ctx context.Context, dossier store.WorkflowDossier) (*store.WorkflowDossier, error)
	SetWorkflowPublicationState(ctx context.Context, workflowID, state string) error
	SetWorkflowVerdict(ctx context.Context, id, verdict string) error
	ListPendingPublications(ctx context.Context, limit int) ([]store.PendingPublication, error)
	ListWorkflows(ctx context.Context, filter store.WorkflowFilter, cursor string, limit int) ([]store.Workflow, string, error)
	LastPublishedHead(ctx context.Context, projectID, mrIID int64) (string, error)
	GetPublication(ctx context.Context, projectID, mrIID int64, kind, publishKey string) (*store.Publication, error)
	ListPublications(ctx context.Context, workflowID string) ([]store.Publication, error)
	UpsertPublication(ctx context.Context, p store.Publication) (*store.Publication, error)
	ReplaceFindings(ctx context.Context, workflowID string, findings []store.Finding) error
	ListFindings(ctx context.Context, filter store.FindingFilter) ([]store.Finding, error)
	GetWorkspace(ctx context.Context, projectID int64) (*store.Workspace, error)
	SaveWorkspace(ctx context.Context, ws store.Workspace) (*store.Workspace, error)
	GetCurrentContextDoc(ctx context.Context, projectID int64, kind string) (*store.ContextDoc, error)
	SaveContextDoc(ctx context.Context, doc store.ContextDoc) (*store.ContextDoc, error)
	SetActiveContext(ctx context.Context, projectID int64, baseSHA string) error
	UpsertFeedback(ctx context.Context, f store.Feedback) (*store.Feedback, error)
	ListFeedback(ctx context.Context, filter store.FeedbackFilter) ([]store.Feedback, string, error)
}

// ReviewService implements F1/F2: MR review orchestration. It owns workflow
// creation, execution, and the decision to publish.
type ReviewService struct {
	store          ReviewStore
	gitlab         GitLabClient
	cipher         *secret.Cipher
	workspace      *workspace.Manager
	git            GitOps
	engine         *dag.Engine
	executors      *executorRegistry
	profiles       map[string]config.ProfileConfig
	blocks         map[string]config.BlockBinding
	reviewDefaults config.ProjectSettings
	prompts        prompts.Bundle
	clients        review.ClientProvider
	agentRun       review.AgentRunner
	tools          review.ToolProvider
	skills         review.SkillProvider
	providerNames  []string
	cost           review.CostFunc
	publisher      *Publisher
	publicURL      string
	logger         *slog.Logger
	now            func() time.Time
	cancelPoll     time.Duration
	cancelWait     time.Duration
}

// ReviewServiceConfig configures ReviewService.
type ReviewServiceConfig struct {
	Store            ReviewStore
	GitLab           GitLabClient
	Cipher           *secret.Cipher
	Workspace        *workspace.Manager
	Git              GitOps
	Engine           *dag.Engine
	Executors        *executorRegistry
	Profiles         map[string]config.ProfileConfig
	Blocks           map[string]config.BlockBinding
	ReviewDefaults   config.ProjectSettings
	Prompts          prompts.Bundle
	Clients          review.ClientProvider
	AgentRun         review.AgentRunner
	Tools            review.ToolProvider
	Skills           review.SkillProvider
	Cost             review.CostFunc
	Publisher        *Publisher
	AllowedProviders []string
	PublicURL        string
	Logger           *slog.Logger
	Now              func() time.Time
	CancelPoll       time.Duration
	CancelWait       time.Duration
}

// NewReviewService validates dependencies and builds the F1/F2 service.
func NewReviewService(cfg ReviewServiceConfig) (*ReviewService, error) {
	if cfg.Store == nil || cfg.GitLab == nil || cfg.Cipher == nil ||
		cfg.Workspace == nil || cfg.Git == nil || cfg.Prompts.Files == nil {
		return nil, errors.New("app: review service dependencies are required")
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	registry := cfg.Executors
	if registry == nil {
		registry = newExecutorRegistry()
	}
	engine := cfg.Engine
	if engine == nil {
		engine = dag.NewEngine(cfg.Store, registry, registry, dag.Options{})
	}
	poll := cfg.CancelPoll
	if poll <= 0 {
		poll = defaultCancelPoll
	}
	wait := cfg.CancelWait
	if wait <= 0 {
		wait = defaultCancelWait
	}
	return &ReviewService{
		store:          cfg.Store,
		gitlab:         cfg.GitLab,
		cipher:         cfg.Cipher,
		workspace:      cfg.Workspace,
		git:            cfg.Git,
		engine:         engine,
		executors:      registry,
		profiles:       cfg.Profiles,
		blocks:         cfg.Blocks,
		reviewDefaults: cfg.ReviewDefaults,
		prompts:        cfg.Prompts,
		clients:        cfg.Clients,
		agentRun:       cfg.AgentRun,
		tools:          cfg.Tools,
		skills:         cfg.Skills,
		providerNames:  cfg.AllowedProviders,
		publisher:      cfg.Publisher,
		publicURL:      strings.TrimRight(cfg.PublicURL, "/"),
		logger:         logger,
		now:            now,
		cancelPoll:     poll,
		cancelWait:     wait,
	}, nil
}

// Handle dispatches review jobs. Permanent failures are logged and settled.
func (s *ReviewService) Handle(ctx context.Context, job *jobs.Job) error {
	if job == nil {
		return nil
	}
	var err error
	switch job.Kind {
	case "review.run":
		err = s.RunReview(ctx, job)
	case "review.cancel":
		err = s.CancelReview(ctx, job)
	case "review.publish":
		err = s.RepublishReview(ctx, job)
	default:
		return nil
	}
	if err == nil || IsPermanentJob(err) {
		if err != nil {
			s.logger.Info("review job settled without retry", "job_id", job.ID, "kind", job.Kind, "reason", err.Error())
		}
		return nil
	}
	return err
}

type reviewRunPayload struct {
	ProjectID  int64  `json:"project_id"`
	MRIID      int64  `json:"mr_iid"`
	HeadSHA    string `json:"head_sha"`
	Generation int64  `json:"generation"`
	Profile    string `json:"profile"`
	Trigger    string `json:"trigger"`
	Full       bool   `json:"full"`
}

type reviewCancelPayload struct {
	ProjectID  int64  `json:"project_id"`
	MRIID      int64  `json:"mr_iid"`
	Generation int64  `json:"generation"`
	Trigger    string `json:"trigger"`
}

type reviewPublishPayload struct {
	WorkflowID string `json:"workflow_id"`
}

func decodeReviewPayload(raw json.RawMessage, target any) error {
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	if err := dec.Decode(target); err != nil {
		return fmt.Errorf("%w: invalid review payload", ErrPermanentJob)
	}
	return nil
}

// RunReview executes one review.run job.
func (s *ReviewService) RunReview(ctx context.Context, job *jobs.Job) error {
	var payload reviewRunPayload
	if err := decodeReviewPayload(job.Payload, &payload); err != nil {
		return err
	}
	if payload.ProjectID <= 0 || payload.MRIID <= 0 || payload.Generation <= 0 ||
		!shaRE.MatchString(payload.HeadSHA) {
		return PermanentJob(fmt.Errorf("invalid review.run payload"))
	}
	payload.HeadSHA = strings.ToLower(payload.HeadSHA)

	project, token, err := s.projectToken(ctx, payload.ProjectID)
	if err != nil {
		return err
	}
	control, err := s.currentControl(ctx, payload.ProjectID, payload.MRIID, payload.Generation, payload.HeadSHA)
	if err != nil {
		return err
	}
	workflow, storedDossier, resumable, err := s.resumableWorkflow(ctx, job.ID, payload.Generation, payload.HeadSHA)
	if err != nil {
		return err
	}
	if !resumable {
		if err := s.cancelActive(ctx, control.ProjectID, control.MRIID, payload.Generation); err != nil {
			return err
		}
	}

	inputs, err := s.collectInputs(ctx, project, token, control.MRIID, payload.HeadSHA)
	if err != nil {
		return err
	}

	cfg := s.resolveConfig(project, inputs.RepoDocs)
	configHash := reviewConfigHash(cfg)
	promptHash := s.prompts.Hash
	if workflow != nil {
		changed, err := dossierConfigChanged(workflow, storedDossier, configHash, promptHash)
		if err != nil {
			return err
		}
		if changed {
			// A successor generation may already own this MR. Re-check before
			// cancelling or detaching so a stale worker never touches it.
			if _, err := s.currentControl(ctx, project.ID, control.MRIID, payload.Generation, payload.HeadSHA); err != nil {
				return err
			}
			// The stored dossier was built for another configuration or prompt
			// bundle. Keep it for audit, cancel this workflow only, and free the
			// job mapping so this retry can record a fresh dossier.
			if resumable {
				if err := s.cancelWorkflow(ctx, workflow.ID); err != nil {
					return err
				}
			}
			if err := s.store.DetachWorkflowJob(ctx, workflow.ID, job.ID); err != nil &&
				!errors.Is(err, store.ErrNotFound) {
				return err
			}
			s.logger.Info("review workflow superseded by configuration or prompt change",
				"workflow_id", workflow.ID, "job_id", job.ID)
			resumable = false
		}
	}
	files, ignored, summary, warnings := buildReviewFiles(inputs.Diffs, cfg.Settings.Ignore, cfg.Settings.MaxFiles, cfg.Settings.MaxDiffLines)
	if len(files) == 0 {
		// Everything is ignored: publish an informational summary without a
		// workflow. Re-check the generation because collection takes time.
		if _, err := s.currentControl(ctx, project.ID, control.MRIID, payload.Generation, payload.HeadSHA); err != nil {
			return err
		}
		return s.publishEmpty(ctx, project, token, control, inputs, cfg)
	}
	cfg.Warnings = append(cfg.Warnings, warnings...)

	head, cleanup, err := s.prepareRun(ctx, project, token, control.MRIID, job.ID, payload.HeadSHA)
	if err != nil {
		return err
	}
	defer cleanup()

	profileName, profileCfg := s.profileFor(project, payload.Profile, cfg.Profile, inputs.MR.Labels)
	profile := review.Profile{Name: profileName, Config: profileCfg}
	contextDoc := s.contextDoc(ctx, project.ID)

	trigger := payload.Trigger
	if trigger == "" {
		trigger = "api"
	}
	incremental := s.incrementalRange(ctx, project.ID, payload, control, payload.HeadSHA, files)
	dossier, err := review.BuildDossier(review.DossierInput{
		Project: review.ProjectRef{ID: project.ID, Path: project.PathWithNamespace, WebURL: project.WebURL},
		MR:      mrMeta(inputs.MR),
		DiffVersion: gitlab.Version{
			ID:       inputs.Version.ID,
			BaseSHA:  inputs.Version.BaseSHA,
			StartSHA: inputs.Version.StartSHA,
			HeadSHA:  inputs.Version.HeadSHA,
		},
		DiffSummary: summary,
		Files:       files,
		Ignored:     ignored,
		Pipeline:    inputs.Pipeline,
		Issues:      inputs.Issues,
		RepoDocs:    inputs.RepoDocs,
		ContextDocs: contextDoc,
		CustomRules: cfg.Settings.CustomRules,
		Incremental: incremental,
		HeadDir:     head,
		BaseDir:     s.workspace.Get(project.ID).BaseDir(),
		Metadata: map[string]string{
			"trigger":     trigger,
			"base_sha":    inputs.Version.BaseSHA,
			"config_hash": configHash,
			"prompt_hash": promptHash,
		},
		GeneratedAt: s.now().UTC(),
	})
	if err != nil {
		return PermanentJob(fmt.Errorf("build dossier: %w", err))
	}
	dossierData, err := review.MarshalDossier(dossier)
	if err != nil {
		return PermanentJob(fmt.Errorf("marshal dossier: %w", err))
	}
	dossierHash, err := review.DossierHash(dossier)
	if err != nil {
		return PermanentJob(fmt.Errorf("hash dossier: %w", err))
	}

	graphCfg := review.GraphConfig{AllowedProviders: s.allowedProviders()}
	spec, err := review.BuildGraph(profile, dossier, graphCfg)
	if err != nil {
		return PermanentJob(fmt.Errorf("build review graph: %w", err))
	}

	meta := map[string]string{
		"trigger":     trigger,
		"base_sha":    inputs.Version.BaseSHA,
		"fingerprint": reviewFingerprint(dossierHash, configHash, promptHash),
		"config_hash": configHash,
		"prompt_hash": promptHash,
		"profile":     profileName,
	}
	wfID, reused, err := s.store.CreateReviewWorkflow(ctx, sqlite.ReviewWorkflowRequest{
		JobID:              job.ID,
		Spec:               spec,
		Meta:               meta,
		DossierVersion:     review.DossierSchemaVersion,
		DossierHash:        dossierHash,
		DossierData:        dossierData,
		ExpectedGeneration: payload.Generation,
		ProjectID:          project.ID,
		MRIID:              control.MRIID,
		HeadSHA:            payload.HeadSHA,
	})
	if err != nil {
		if errors.Is(err, store.ErrSuperseded) {
			s.logger.Info("review workflow superseded before creation", "project_id", project.ID, "mr_iid", payload.MRIID)
			return nil
		}
		return err
	}

	// A push may land between CreateReviewWorkflow and the event append.
	if _, err := s.currentControl(ctx, project.ID, control.MRIID, payload.Generation, payload.HeadSHA); err != nil {
		if IsPermanentJob(err) {
			_ = s.engine.Cancel(context.WithoutCancel(ctx), wfID, "superseded")
			return nil
		}
		return err
	}

	if reused || resumable {
		// A retried job can reuse a workflow left with running nodes by a
		// crash; restart those nodes before Engine.Run rejects the graph.
		if err := s.engine.Recover(ctx); err != nil {
			return err
		}
	}

	diffIndex := gitlab.BuildDiffIndex(gitlab.Version{
		BaseSHA:  inputs.Version.BaseSHA,
		StartSHA: inputs.Version.StartSHA,
		HeadSHA:  inputs.Version.HeadSHA,
	}, inputs.Diffs)
	executor, err := review.NewExecutor(review.ExecutorDeps{
		Dossier:  dossier,
		Profile:  profile,
		Graph:    graphCfg,
		Blocks:   s.blocks,
		Clients:  s.clients,
		Prompts:  s.prompts,
		AgentRun: s.agentRun,
		Snapshot: engineSnapshot(s.engine),
		Tools:    s.tools,
		Skills:   s.skills,
		Locate:   locateFunc(diffIndex),
		Cost:     s.cost,
	})
	if err != nil {
		return PermanentJob(fmt.Errorf("build review executor: %w", err))
	}
	s.executors.Register(wfID, executor)
	defer s.executors.Unregister(wfID)

	superseded, runErr := s.runWithControl(ctx, project.ID, control.MRIID, wfID, payload.Generation, payload.HeadSHA)
	if runErr != nil {
		return runErr
	}
	if superseded {
		s.logger.Info("review superseded while running", "workflow_id", wfID)
		return nil
	}

	snapshot, err := s.engine.Snapshot(ctx, wfID)
	if err != nil {
		return err
	}
	switch snapshot.Status {
	case dag.WorkflowCompleted:
		return s.publish(ctx, wfID)
	case dag.WorkflowFailed:
		if cfg.Settings.PostFailureNote {
			return s.publishFailure(ctx, wfID, snapshot)
		}
		return nil
	default:
		return nil
	}
}

// CancelReview executes one review.cancel job. It never publishes.
func (s *ReviewService) CancelReview(ctx context.Context, job *jobs.Job) error {
	var payload reviewCancelPayload
	if err := decodeReviewPayload(job.Payload, &payload); err != nil {
		return err
	}
	if payload.ProjectID <= 0 || payload.MRIID <= 0 {
		return PermanentJob(fmt.Errorf("invalid review.cancel payload"))
	}
	control, err := s.store.GetReviewControl(ctx, payload.ProjectID, payload.MRIID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return PermanentJob(fmt.Errorf("review control is missing"))
		}
		return err
	}
	if !control.Cancelled {
		// A newer review.run replaced this cancel; do not touch it.
		return nil
	}
	active, err := s.store.FindActiveReview(ctx, payload.ProjectID, payload.MRIID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		return err
	}
	if err := s.engine.Cancel(ctx, active.ID, "user_cancelled"); err != nil {
		if errors.Is(err, dag.ErrNotFound) {
			return nil
		}
		return err
	}
	if err := s.waitTerminal(ctx, active.ID); err != nil {
		return err
	}
	return nil
}

// RepublishReview executes one review.publish job. It delegates to the
// publisher so retries and stale-fingerprint handling stay in one place.
func (s *ReviewService) RepublishReview(ctx context.Context, job *jobs.Job) error {
	var payload reviewPublishPayload
	if err := decodeReviewPayload(job.Payload, &payload); err != nil {
		return err
	}
	if strings.TrimSpace(payload.WorkflowID) == "" {
		return PermanentJob(fmt.Errorf("invalid review.publish payload"))
	}
	if s.publisher == nil {
		return errors.New("app: review publisher is not configured")
	}
	return s.publisher.Republish(ctx, payload.WorkflowID)
}

func (s *ReviewService) projectToken(ctx context.Context, projectID int64) (*store.Project, string, error) {
	project, err := s.store.GetProject(ctx, projectID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, "", PermanentJob(fmt.Errorf("project %d is not registered", projectID))
		}
		return nil, "", err
	}
	if project == nil || !project.Enabled {
		return nil, "", PermanentJob(fmt.Errorf("project %d is disabled", projectID))
	}
	token, err := s.cipher.Decrypt(project.TokenEnc)
	if err != nil {
		return nil, "", PermanentJob(fmt.Errorf("project token is unavailable"))
	}
	return project, string(token), nil
}

// resumableWorkflow returns the workflow this job is mapped to and reports
// whether a retry can resume it. A matching workflow is returned even when it
// is terminal, so the caller can compare the stored configuration and prompt
// hashes before deciding to reuse or rebuild it.
func (s *ReviewService) resumableWorkflow(ctx context.Context, jobID string, generation int64, head string) (*store.Workflow, *store.WorkflowDossier, bool, error) {
	workflow, err := s.store.GetWorkflowByJob(ctx, jobID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, nil, false, nil
		}
		return nil, nil, false, err
	}
	if workflow == nil || !strings.EqualFold(workflow.HeadSHA, head) {
		return nil, nil, false, nil
	}
	dossier, err := s.store.GetWorkflowDossier(ctx, workflow.ID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return workflow, nil, false, nil
		}
		return nil, nil, false, err
	}
	if dossier.Generation != generation {
		return nil, nil, false, nil
	}
	return workflow, dossier, !workflowTerminal(workflow.Status), nil
}

// dossierConfigChanged compares the hashes stored with a dossier against the
// hashes this retry would use now. Rows written before the dossier metadata
// existed fall back to the workflow fingerprint, which binds the dossier,
// configuration and prompt hashes together.
func dossierConfigChanged(workflow *store.Workflow, dossier *store.WorkflowDossier, configHash, promptHash string) (bool, error) {
	if dossier == nil {
		return false, nil
	}
	var stored review.Dossier
	if err := json.Unmarshal(dossier.Data, &stored); err != nil {
		return false, fmt.Errorf("decode dossier: %w", err)
	}
	storedConfig := strings.TrimSpace(stored.Metadata["config_hash"])
	storedPrompt := strings.TrimSpace(stored.Metadata["prompt_hash"])
	if storedConfig != "" && storedPrompt != "" {
		return storedConfig != configHash || storedPrompt != promptHash, nil
	}
	if workflow.Fingerprint == "" {
		return false, nil
	}
	return workflow.Fingerprint != reviewFingerprint(dossier.Hash, configHash, promptHash), nil
}

func workflowTerminal(status string) bool {
	switch status {
	case string(dag.WorkflowCompleted), string(dag.WorkflowFailed), string(dag.WorkflowCancelled):
		return true
	}
	return false
}

// currentControl verifies the persisted generation before any work.
func (s *ReviewService) currentControl(ctx context.Context, projectID, mrIID, generation int64, head string) (*store.ReviewControl, error) {
	control, err := s.store.GetReviewControl(ctx, projectID, mrIID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, PermanentJob(fmt.Errorf("review control is missing"))
		}
		return nil, err
	}
	if control.Cancelled || control.Generation != generation || control.HeadSHA != head {
		return nil, PermanentJob(fmt.Errorf("%w: generation %d head %s is not current", ErrReviewSuperseded, generation, head))
	}
	return control, nil
}

// cancelActive cancels the MR's active review when the control still belongs
// to this worker's generation and the active workflow is not newer. A stale
// worker never cancels a newer generation; a new review still cancels the
// previous generation.
func (s *ReviewService) cancelActive(ctx context.Context, projectID, mrIID, generation int64) error {
	control, err := s.store.GetReviewControl(ctx, projectID, mrIID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		return err
	}
	if control.Cancelled || control.Generation != generation {
		return nil
	}
	active, err := s.store.FindActiveReview(ctx, projectID, mrIID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		return err
	}
	dossier, err := s.store.GetWorkflowDossier(ctx, active.ID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		return err
	}
	if dossier.Generation > generation {
		return nil
	}
	return s.cancelWorkflow(ctx, active.ID)
}

// cancelWorkflow cancels one workflow and waits for its durable terminal state.
func (s *ReviewService) cancelWorkflow(ctx context.Context, workflowID string) error {
	if err := s.engine.Cancel(ctx, workflowID, "superseded"); err != nil {
		if errors.Is(err, dag.ErrNotFound) {
			return nil
		}
		return err
	}
	if err := s.waitTerminal(ctx, workflowID); err != nil {
		if errors.Is(err, dag.ErrNotFound) {
			return nil
		}
		return err
	}
	return nil
}

func (s *ReviewService) waitTerminal(ctx context.Context, workflowID string) error {
	deadline := time.Now().Add(s.cancelWait)
	for {
		snapshot, err := s.engine.Snapshot(ctx, workflowID)
		if err != nil {
			return err
		}
		if snapshot.Terminal() {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("app: workflow %s did not reach a terminal state", workflowID)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(s.cancelPoll):
		}
	}
}

// runWithControl runs a workflow and watches the persisted review generation.
// A superseded run is cancelled through the engine and never published.
func (s *ReviewService) runWithControl(ctx context.Context, projectID, mrIID int64, workflowID string, generation int64, head string) (bool, error) {
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- s.engine.Run(runCtx, workflowID) }()

	ticker := time.NewTicker(s.cancelPoll)
	defer ticker.Stop()
	superseded := false
	for {
		select {
		case err := <-done:
			if err != nil {
				if errors.Is(err, context.Canceled) || ctx.Err() != nil {
					return superseded, nil
				}
				var transition *dag.TransitionError
				if errors.As(err, &transition) {
					return superseded, nil
				}
				return superseded, err
			}
			return superseded, nil
		case <-ctx.Done():
			cancel()
			<-done
			return superseded, nil
		case <-ticker.C:
			control, err := s.store.GetReviewControl(ctx, projectID, mrIID)
			if err != nil {
				continue
			}
			if control.Cancelled || control.Generation != generation || control.HeadSHA != head {
				if !superseded {
					superseded = true
					if err := s.engine.Cancel(context.WithoutCancel(ctx), workflowID, "superseded"); err != nil && !errors.Is(err, dag.ErrNotFound) {
						s.logger.Warn("review cancel failed", "workflow_id", workflowID)
					}
				}
			}
		}
	}
}

func (s *ReviewService) publish(ctx context.Context, workflowID string) error {
	if s.publisher == nil {
		return errors.New("app: review publisher is not configured")
	}
	return s.publisher.Publish(ctx, workflowID)
}

func (s *ReviewService) publishFailure(ctx context.Context, workflowID string, snapshot dag.WorkflowView) error {
	if s.publisher == nil {
		return errors.New("app: review publisher is not configured")
	}
	return s.publisher.PublishFailure(ctx, workflowID, failureReason(snapshot))
}

func failureReason(snapshot dag.WorkflowView) string {
	for _, id := range snapshot.Order {
		node := snapshot.Nodes[id]
		if node.Status == dag.NodeFailed {
			if strings.TrimSpace(node.Error) != "" {
				return truncateRunes(node.Error, 500)
			}
			return "node " + id + " failed"
		}
	}
	if strings.TrimSpace(snapshot.Error) != "" {
		return truncateRunes(snapshot.Error, 500)
	}
	return "workflow failed"
}

// reviewInputs is the sanitized GitLab input for one review.
type reviewInputs struct {
	MR       *gitlab.MergeRequest
	Version  *gitlab.Version
	Diffs    []gitlab.FileDiff
	Pipeline *review.PipelineSummary
	Issues   []review.IssueRef
	RepoDocs review.RepoDocs
}

func (s *ReviewService) collectInputs(ctx context.Context, project *store.Project, token string, mrIID int64, head string) (*reviewInputs, error) {
	mr, err := s.gitlab.GetMergeRequest(ctx, token, project.ID, mrIID)
	if err != nil {
		if gitlab.IsNotFound(err) {
			return nil, PermanentJob(fmt.Errorf("%w: merge request not found", ErrMRClosed))
		}
		return nil, err
	}
	if mr == nil || !isOpenState(mr.State) {
		return nil, PermanentJob(fmt.Errorf("%w: state %s", ErrMRClosed, mrState(mr)))
	}
	if mr.TargetProjectID > 0 && mr.TargetProjectID != project.ID {
		return nil, PermanentJob(fmt.Errorf("%w: target project mismatch", ErrReviewSuperseded))
	}
	if !strings.EqualFold(strings.TrimSpace(mr.SHA), head) {
		return nil, PermanentJob(fmt.Errorf("%w: merge request head changed", ErrReviewSuperseded))
	}
	if strings.TrimSpace(mr.TargetBranch) == "" {
		return nil, PermanentJob(fmt.Errorf("merge request target branch is empty"))
	}

	version, err := s.gitlab.LatestMRVersion(ctx, token, project.ID, mrIID)
	if err != nil {
		return nil, err
	}
	if version == nil || !strings.EqualFold(strings.TrimSpace(version.HeadSHA), head) {
		return nil, PermanentJob(fmt.Errorf("%w: diff version head changed", ErrReviewSuperseded))
	}
	diffs, err := s.gitlab.ListMRDiffs(ctx, token, project.ID, mrIID)
	if err != nil {
		return nil, err
	}
	inputs := &reviewInputs{MR: mr, Version: version, Diffs: diffs}
	inputs.Pipeline = s.collectPipeline(ctx, token, project.ID, mrIID)
	inputs.Issues = s.collectIssues(ctx, token, project.ID, mrIID)
	inputs.RepoDocs = review.RepoDocs{
		GitLabReviewConfig: s.rawDoc(ctx, token, project.ID, ".gitlab-review.yml", mr.TargetBranch),
		AGENTS:             s.rawDoc(ctx, token, project.ID, "AGENTS.md", mr.TargetBranch),
		CLAUDE:             s.rawDoc(ctx, token, project.ID, "CLAUDE.md", mr.TargetBranch),
	}
	return inputs, nil
}

func mrState(mr *gitlab.MergeRequest) string {
	if mr == nil {
		return "missing"
	}
	return mr.State
}

func (s *ReviewService) collectPipeline(ctx context.Context, token string, projectID, mrIID int64) *review.PipelineSummary {
	pipelines, err := s.gitlab.ListMRPipelines(ctx, token, projectID, mrIID)
	if err != nil {
		s.logger.Warn("pipeline lookup failed", "project_id", projectID, "mr_iid", mrIID)
		return nil
	}
	var latest *gitlab.Pipeline
	for i := range pipelines {
		if latest == nil || pipelines[i].ID > latest.ID {
			latest = &pipelines[i]
		}
	}
	if latest == nil {
		return nil
	}
	summary := &review.PipelineSummary{
		ID:     latest.ID,
		Status: latest.Status,
		Ref:    latest.Ref,
		SHA:    latest.SHA,
		WebURL: latest.WebURL,
	}
	if latest.Status != "failed" {
		return summary
	}
	jobs, err := s.gitlab.ListPipelineJobs(ctx, token, projectID, latest.ID)
	if err != nil {
		s.logger.Warn("pipeline jobs lookup failed", "project_id", projectID, "pipeline_id", latest.ID)
		return summary
	}
	for _, job := range jobs {
		if job.Status != "failed" || job.AllowFailure {
			continue
		}
		summary.FailedJobs = append(summary.FailedJobs, review.JobSummary{
			Name:          job.Name,
			Stage:         job.Stage,
			Status:        job.Status,
			FailureReason: job.FailureReason,
			WebURL:        job.WebURL,
		})
	}
	return summary
}

func (s *ReviewService) collectIssues(ctx context.Context, token string, projectID, mrIID int64) []review.IssueRef {
	issues, err := s.gitlab.ListClosingIssues(ctx, token, projectID, mrIID)
	if err != nil {
		s.logger.Warn("closing issues lookup failed", "project_id", projectID, "mr_iid", mrIID)
		return nil
	}
	out := make([]review.IssueRef, 0, len(issues))
	for _, issue := range issues {
		out = append(out, review.IssueRef{
			IID:    issue.IID,
			Title:  issue.Title,
			State:  issue.State,
			WebURL: issue.WebURL,
		})
	}
	return out
}

func (s *ReviewService) rawDoc(ctx context.Context, token string, projectID int64, path, ref string) string {
	data, err := s.gitlab.GetRawFile(ctx, token, projectID, path, ref)
	if err != nil {
		if !gitlab.IsNotFound(err) {
			s.logger.Warn("repository document lookup failed", "project_id", projectID, "path", path)
		}
		return ""
	}
	return string(data)
}

func mrMeta(mr *gitlab.MergeRequest) review.MRMeta {
	out := review.MRMeta{
		IID:          mr.IID,
		Title:        mr.Title,
		Description:  mr.Description,
		SourceBranch: mr.SourceBranch,
		TargetBranch: mr.TargetBranch,
		Draft:        mr.Draft,
		Labels:       append([]string(nil), mr.Labels...),
		WebURL:       mr.WebURL,
	}
	if mr.Author != nil {
		out.Author = mr.Author.Username
	}
	return out
}

// effectiveReviewConfig is the merged platform, project, and repo view.
type effectiveReviewConfig struct {
	Settings config.ProjectSettings
	Profile  string
	RepoDocs review.RepoDocs
	Warnings []string
}

type repoReviewConfig struct {
	Version  int      `yaml:"version"`
	Language string   `yaml:"language"`
	Profile  string   `yaml:"profile"`
	Ignore   []string `yaml:"ignore"`
	Rules    []string `yaml:"rules"`
	Review   struct {
		SkipDraft     *bool    `yaml:"skip_draft"`
		LabelsExclude []string `yaml:"labels_exclude"`
	} `yaml:"review"`
}

func (s *ReviewService) resolveConfig(project *store.Project, docs review.RepoDocs) effectiveReviewConfig {
	return resolveReviewConfig(project, docs, s.reviewDefaults, s.profiles)
}

func resolveReviewConfig(project *store.Project, docs review.RepoDocs, defaults config.ProjectSettings, profiles map[string]config.ProfileConfig) effectiveReviewConfig {
	settings, err := ResolveSettings(project.Settings, defaults)
	out := effectiveReviewConfig{Settings: settings, RepoDocs: docs}
	if err != nil {
		out.Settings = cloneSettings(defaults)
		out.Warnings = append(out.Warnings, "stored project settings are invalid; platform defaults are used")
	}
	// Normalize once so the review service and publisher hash the same value.
	if out.Settings.MaxFiles <= 0 {
		out.Settings.MaxFiles = defaultReviewMaxFiles
	}
	if !out.Settings.AllowRepoConfig || strings.TrimSpace(docs.GitLabReviewConfig) == "" {
		return out
	}
	var repo repoReviewConfig
	if err := yaml.Unmarshal([]byte(docs.GitLabReviewConfig), &repo); err != nil {
		out.Warnings = append(out.Warnings, ".gitlab-review.yml is invalid and was ignored")
		return out
	}
	if strings.TrimSpace(repo.Language) != "" {
		out.Settings.Language = repo.Language
	}
	if strings.TrimSpace(repo.Profile) != "" {
		if _, ok := profiles[repo.Profile]; ok {
			out.Profile = repo.Profile
		} else {
			out.Warnings = append(out.Warnings, "repository profile is not configured and was ignored")
		}
	}
	out.Settings.Ignore = appendUnique(out.Settings.Ignore, repo.Ignore)
	out.Settings.CustomRules = appendUnique(out.Settings.CustomRules, repo.Rules)
	if repo.Review.SkipDraft != nil {
		out.Settings.SkipDraft = *repo.Review.SkipDraft
	}
	out.Settings.LabelsExclude = appendUnique(out.Settings.LabelsExclude, repo.Review.LabelsExclude)
	return out
}

func reviewConfigHash(cfg effectiveReviewConfig) string {
	payload, err := json.Marshal(struct {
		Settings config.ProjectSettings `json:"settings"`
		Profile  string                 `json:"profile"`
		RepoDocs review.RepoDocs        `json:"repo_docs"`
	}{cfg.Settings, cfg.Profile, cfg.RepoDocs})
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}

// reviewFingerprint binds a workflow to the exact dossier, configuration, and
// prompt bundle used to produce it. Format:
// sha256("<dossier hash>\n<config hash>\n<prompt hash>").
func reviewFingerprint(dossierHash, configHash, promptHash string) string {
	return sha256Hex(dossierHash + "\n" + configHash + "\n" + promptHash)
}

func (s *ReviewService) profileFor(project *store.Project, requested, repoProfile string, labels []string) (string, config.ProfileConfig) {
	candidates := []string{requested}
	if strings.TrimSpace(requested) == "" {
		candidates = append(candidates, repoProfile)
	}
	candidates = append(candidates, ProfileForLabels(labels, project.Profile, s.reviewDefaults), "default")
	seen := make(map[string]struct{}, len(candidates))
	for _, name := range candidates {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		if cfg, ok := s.profiles[name]; ok {
			return name, cfg
		}
	}
	return "custom", config.ProfileConfig{}
}

// allowedProviders returns the provider allowlist used by graph validation.
func (s *ReviewService) allowedProviders() []string {
	return append([]string(nil), s.providerNames...)
}

func (s *ReviewService) contextDoc(ctx context.Context, projectID int64) string {
	doc, err := s.store.GetCurrentContextDoc(ctx, projectID, "agents")
	if err != nil {
		return ""
	}
	return doc.Content
}

func (s *ReviewService) incrementalRange(ctx context.Context, projectID int64, payload reviewRunPayload, control *store.ReviewControl, head string, files []review.FileChange) *review.IncrementalRange {
	if payload.Trigger != "mr_update" || payload.Full {
		return &review.IncrementalRange{Head: head, Full: true, Reason: "full review"}
	}
	last, err := s.store.LastPublishedHead(ctx, projectID, control.MRIID)
	if err != nil || last == "" || strings.EqualFold(last, head) {
		return &review.IncrementalRange{Head: head, Full: true, Reason: "no previous reviewed head"}
	}
	ws := s.workspace.Get(projectID)
	if ws == nil {
		return nil
	}
	ancestor, err := s.git.IsAncestor(ctx, ws.MirrorDir(), last, head)
	if err != nil {
		s.logger.Warn("incremental ancestry check failed", "project_id", projectID, "mr_iid", control.MRIID)
		return &review.IncrementalRange{Head: head, LastHead: last, Full: true, Reason: "ancestry check failed"}
	}
	paths := make([]string, 0, len(files))
	for _, file := range files {
		paths = append(paths, file.Path)
	}
	scope, err := review.ComputeIncrementalScope(review.IncrementalInput{
		LastHead:   last,
		Head:       head,
		MRFiles:    paths,
		IsAncestor: ancestor,
		LoadChanged: func(from, to string) ([]string, error) {
			return s.git.ChangedFiles(ctx, ws.MirrorDir(), from, to)
		},
	})
	if err != nil {
		return &review.IncrementalRange{Head: head, LastHead: last, Full: true, Reason: "incremental scope failed"}
	}
	return &review.IncrementalRange{
		LastHead: last,
		Head:     head,
		Files:    scope.Files,
		Full:     scope.Full,
		Reason:   scope.Reason,
	}
}

// prepareRun fetches the MR head and creates a detached worktree keyed by the
// retried job ID. The returned cleanup removes the worktree and releases the
// workspace read lock in every path.
func (s *ReviewService) prepareRun(ctx context.Context, project *store.Project, token string, mrIID int64, runID, head string) (string, func(), error) {
	ws := s.workspace.Get(project.ID)
	if ws == nil {
		return "", nil, PermanentJob(fmt.Errorf("workspace is unavailable"))
	}
	unlock, err := ws.RLock(ctx)
	if err != nil {
		return "", nil, err
	}
	var once sync.Once
	release := func() { once.Do(unlock) }
	fail := func(err error) (string, func(), error) {
		release()
		return "", nil, err
	}
	remote := trimProjectWebURL(project.WebURL)
	if remote == "" {
		return fail(PermanentJob(fmt.Errorf("project web url is empty")))
	}
	auth := repo.Auth{Token: token}
	if err := s.git.EnsureMirror(ctx, ws.MirrorDir(), remote, auth); err != nil {
		return fail(err)
	}
	ref := fmt.Sprintf("refs/merge-requests/%d/head", mrIID)
	refspec := fmt.Sprintf("+%s:%s", ref, ref)
	if err := s.git.Fetch(ctx, ws.MirrorDir(), auth, refspec); err != nil {
		return fail(err)
	}
	resolved, err := s.git.Resolve(ctx, ws.MirrorDir(), ref)
	if err != nil {
		return fail(err)
	}
	if !strings.EqualFold(resolved, head) {
		// The mirror may lag behind the API; retry and re-check the generation.
		return fail(fmt.Errorf("app: merge request ref does not match the job head"))
	}

	runsDir := filepath.Join(ws.Root, "runs")
	if err := os.MkdirAll(runsDir, 0o700); err != nil {
		return fail(fmt.Errorf("app: create run directory: %w", err))
	}
	if info, err := os.Lstat(runsDir); err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fail(fmt.Errorf("app: run directory is unsafe"))
	}
	runDir := filepath.Join(runsDir, runID)
	switch info, err := os.Lstat(runDir); {
	case err == nil:
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return fail(fmt.Errorf("app: run path is not a directory"))
		}
		current, resolveErr := s.git.Resolve(ctx, runDir, "HEAD")
		if resolveErr == nil && strings.EqualFold(current, head) {
			break
		}
		// A missing or interrupted worktree is rebuilt from the mirror.
		if err := removeRunDir(runDir); err != nil {
			return fail(err)
		}
		if err := s.git.AddWorktree(ctx, ws.MirrorDir(), runDir, head); err != nil {
			return fail(err)
		}
	case errors.Is(err, os.ErrNotExist):
		if err := s.git.AddWorktree(ctx, ws.MirrorDir(), runDir, head); err != nil {
			return fail(err)
		}
	default:
		return fail(err)
	}

	cleanup := func() {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		if err := s.git.RemoveWorktree(cleanupCtx, ws.MirrorDir(), runDir); err != nil {
			s.logger.Warn("run worktree removal failed", "project_id", project.ID, "run_id", runID)
		}
		_ = os.RemoveAll(runDir)
		release()
	}
	return runDir, cleanup, nil
}

// publishEmpty publishes the "nothing to review" summary without a workflow.
func (s *ReviewService) publishEmpty(ctx context.Context, project *store.Project, token string, control *store.ReviewControl, inputs *reviewInputs, cfg effectiveReviewConfig) error {
	if s.publisher == nil {
		return errors.New("app: review publisher is not configured")
	}
	profileName, _ := s.profileFor(project, "", cfg.Profile, inputs.MR.Labels)
	fingerprint := sha256Hex(strings.Join([]string{
		inputs.Version.HeadSHA, reviewConfigHash(cfg), profileName,
	}, "\n"))
	body, err := review.RenderSummary(review.SummaryInput{
		Verdict:     review.VerdictPass,
		Summary:     "无需评审：所有变更文件均被忽略规则过滤。",
		Fingerprint: fingerprint,
		HeadSHA:     inputs.Version.HeadSHA,
		Profile:     profileName,
		Warnings:    append(cfg.Warnings, "no reviewable files after ignore rules"),
	})
	if err != nil {
		return PermanentJob(fmt.Errorf("render empty summary: %w", err))
	}
	return s.publisher.PublishStandaloneSummary(ctx, project.ID, control.MRIID, token, fingerprint, body)
}

func sha256Hex(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

// removeRunDir clears an interrupted worktree before rebuilding it.
func removeRunDir(dir string) error {
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("app: remove interrupted run worktree: %w", err)
	}
	return nil
}

func truncateRunes(value string, max int) string {
	runes := []rune(value)
	if len(runes) <= max {
		return value
	}
	return string(runes[:max])
}

// ignoreMatcher compiles GitLab-style glob ignore patterns.
type ignoreMatcher struct {
	patterns []*regexp.Regexp
	basename []*regexp.Regexp
}

func newIgnoreMatcher(patterns []string) *ignoreMatcher {
	out := &ignoreMatcher{}
	for _, raw := range patterns {
		pattern := strings.TrimSpace(raw)
		if pattern == "" || strings.HasPrefix(pattern, "!") {
			continue
		}
		if strings.HasSuffix(pattern, "/") {
			pattern += "**"
		}
		re, err := compileGlob(pattern)
		if err != nil {
			continue
		}
		out.patterns = append(out.patterns, re)
		if !strings.Contains(pattern, "/") {
			if re, err := compileGlob("**/" + pattern); err == nil {
				out.basename = append(out.basename, re)
			}
		}
	}
	return out
}

func (m *ignoreMatcher) Match(path string) bool {
	if m == nil {
		return false
	}
	for _, re := range m.patterns {
		if re.MatchString(path) {
			return true
		}
	}
	for _, re := range m.basename {
		if re.MatchString(path) {
			return true
		}
	}
	return false
}

func compileGlob(pattern string) (*regexp.Regexp, error) {
	var b strings.Builder
	b.WriteString("^")
	for i := 0; i < len(pattern); i++ {
		switch pattern[i] {
		case '*':
			if i+1 < len(pattern) && pattern[i+1] == '*' {
				i++
				if i+1 < len(pattern) && pattern[i+1] == '/' {
					i++
					b.WriteString("(?:.*/)?")
				} else {
					b.WriteString(".*")
				}
			} else {
				b.WriteString("[^/]*")
			}
		case '?':
			b.WriteString("[^/]")
		default:
			b.WriteString(regexp.QuoteMeta(string(pattern[i])))
		}
	}
	b.WriteString("$")
	return regexp.Compile(b.String())
}

// buildReviewFiles filters diffs, applies ignore rules and bounds, and counts
// lines. Invalid repository paths are skipped rather than failing the review.
func buildReviewFiles(diffs []gitlab.FileDiff, ignores []string, maxFiles, maxDiffLines int) ([]review.FileChange, []string, review.DiffSummary, []string) {
	matcher := newIgnoreMatcher(ignores)
	files := make([]review.FileChange, 0, len(diffs))
	ignored := make([]string, 0)
	warnings := make([]string, 0)
	summary := review.DiffSummary{}
	lines := 0
	for _, diff := range diffs {
		filePath := strings.TrimSpace(diff.NewPath)
		if filePath == "" {
			filePath = strings.TrimSpace(diff.OldPath)
		}
		if !validRepoPath(filePath) {
			warnings = append(warnings, "skipped a changed file with an invalid path")
			continue
		}
		if matcher.Match(filePath) {
			ignored = append(ignored, filePath)
			continue
		}
		if maxFiles > 0 && len(files) >= maxFiles {
			summary.Truncated = true
			continue
		}
		additions, deletions := countDiffLines(diff.Diff)
		if maxDiffLines > 0 && lines > 0 && lines+additions+deletions > maxDiffLines {
			summary.Truncated = true
			continue
		}
		status := "modified"
		switch {
		case diff.NewFile:
			status = "added"
		case diff.DeletedFile:
			status = "deleted"
		case diff.RenamedFile:
			status = "renamed"
		}
		oldPath := diff.OldPath
		if !validRepoPath(oldPath) {
			oldPath = ""
		}
		files = append(files, review.FileChange{
			Path:      filePath,
			OldPath:   oldPath,
			Status:    status,
			Diff:      diff.Diff,
			Additions: additions,
			Deletions: deletions,
			Generated: diff.GeneratedFile,
		})
		lines += additions + deletions
		summary.Files++
		summary.Additions += additions
		summary.Deletions += deletions
	}
	sort.Strings(ignored)
	return files, ignored, summary, warnings
}

func validRepoPath(value string) bool {
	if value == "" || len(value) > review.MaxPathBytes {
		return false
	}
	if strings.ContainsAny(value, "\\\x00\r\n") || strings.HasPrefix(value, "/") {
		return false
	}
	cleaned := path.Clean(value)
	if cleaned != value || cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return false
	}
	for _, segment := range strings.Split(value, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return false
		}
	}
	return true
}

func countDiffLines(diff string) (int, int) {
	additions, deletions := 0, 0
	for _, line := range strings.Split(diff, "\n") {
		switch {
		case strings.HasPrefix(line, "+++") || strings.HasPrefix(line, "---"):
			continue
		case strings.HasPrefix(line, "+"):
			additions++
		case strings.HasPrefix(line, "-"):
			deletions++
		}
	}
	return additions, deletions
}

// Recover rebuilds executors for unfinished review workflows and re-runs them,
// then publishes completed workflows whose publication is still pending.
func (s *ReviewService) Recover(ctx context.Context) error {
	var errs []error
	// Running nodes left by a crash must restart before Engine.Run sees them.
	if err := s.engine.Recover(ctx); err != nil {
		errs = append(errs, err)
	}
	for _, status := range []string{"running", "pending"} {
		attempted := make(map[string]struct{})
		for page := 0; page < 20; page++ {
			workflows, _, err := s.store.ListWorkflows(ctx, store.WorkflowFilter{Kind: "review", Status: status}, "", 50)
			if err != nil {
				errs = append(errs, err)
				break
			}
			var pending []store.Workflow
			for _, workflow := range workflows {
				if _, seen := attempted[workflow.ID]; seen {
					continue
				}
				attempted[workflow.ID] = struct{}{}
				pending = append(pending, workflow)
			}
			if len(pending) == 0 {
				break
			}
			for _, workflow := range pending {
				if err := s.recoverWorkflow(ctx, workflow); err != nil {
					errs = append(errs, fmt.Errorf("app: recover workflow %s: %w", workflow.ID, err))
				}
			}
			if len(workflows) < 50 {
				break
			}
		}
	}
	if err := s.PublishPending(ctx); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

func (s *ReviewService) recoverWorkflow(ctx context.Context, workflow store.Workflow) error {
	if workflow.MRIID == nil || s.publisher == nil {
		return nil
	}
	record, err := s.store.GetWorkflowDossier(ctx, workflow.ID)
	if err != nil {
		return err
	}
	var dossier review.Dossier
	if err := json.Unmarshal(record.Data, &dossier); err != nil {
		return fmt.Errorf("decode dossier: %w", err)
	}
	if err := review.ValidateDossier(&dossier); err != nil {
		return err
	}
	project, token, err := s.projectToken(ctx, workflow.ProjectID)
	if err != nil {
		return err
	}
	if _, err := s.currentControl(ctx, project.ID, *workflow.MRIID, record.Generation, dossier.DiffVersion.HeadSHA); err != nil {
		if IsPermanentJob(err) {
			return s.engine.Cancel(context.WithoutCancel(ctx), workflow.ID, "superseded")
		}
		return err
	}
	headDir, cleanup, err := s.recoverRun(ctx, project, token, *workflow.MRIID, record.JobID, workflow.ID, dossier)
	if err != nil {
		return err
	}
	if cleanup != nil {
		defer cleanup()
	}
	profileName, profileCfg := s.profileFor(project, workflow.Profile, "", nil)
	profile := review.Profile{Name: profileName, Config: profileCfg}
	diffs := dossierDiffs(&dossier)
	index := gitlab.BuildDiffIndex(dossier.DiffVersion, diffs)
	resolved := dossier
	resolved.HeadDir = headDir
	resolved.BaseDir = s.workspace.Get(project.ID).BaseDir()
	executor, err := review.NewExecutor(review.ExecutorDeps{
		Dossier:  &resolved,
		Profile:  profile,
		Graph:    review.GraphConfig{AllowedProviders: s.allowedProviders()},
		Blocks:   s.blocks,
		Clients:  s.clients,
		Prompts:  s.prompts,
		AgentRun: s.agentRun,
		Snapshot: engineSnapshot(s.engine),
		Tools:    s.tools,
		Skills:   s.skills,
		Locate:   locateFunc(index),
		Cost:     s.cost,
	})
	if err != nil {
		return err
	}
	s.executors.Register(workflow.ID, executor)
	defer s.executors.Unregister(workflow.ID)
	superseded, err := s.runWithControl(ctx, project.ID, *workflow.MRIID, workflow.ID, record.Generation, dossier.DiffVersion.HeadSHA)
	if err != nil {
		return err
	}
	if superseded {
		return nil
	}
	snapshot, err := s.engine.Snapshot(ctx, workflow.ID)
	if err != nil {
		return err
	}
	cfg := s.resolveConfig(project, dossier.RepoDocs)
	switch snapshot.Status {
	case dag.WorkflowCompleted:
		return s.publish(ctx, workflow.ID)
	case dag.WorkflowFailed:
		if cfg.Settings.PostFailureNote {
			return s.publishFailure(ctx, workflow.ID, snapshot)
		}
	}
	return nil
}

func (s *ReviewService) recoverRun(ctx context.Context, project *store.Project, token string, mrIID int64, jobID, workflowID string, dossier review.Dossier) (string, func(), error) {
	ws := s.workspace.Get(project.ID)
	if dossier.HeadDir != "" && ws != nil && runDirOwned(ws, dossier.HeadDir) {
		info, statErr := os.Lstat(dossier.HeadDir)
		switch {
		case statErr == nil && info.Mode()&os.ModeSymlink != 0:
			return "", nil, fmt.Errorf("app: review worktree path is a symlink")
		case statErr == nil && !info.IsDir():
			return "", nil, fmt.Errorf("app: review worktree path is not a directory")
		case statErr == nil:
			head, resolveErr := s.git.Resolve(ctx, dossier.HeadDir, "HEAD")
			if resolveErr == nil && strings.EqualFold(head, dossier.DiffVersion.HeadSHA) {
				unlock, err := ws.RLock(ctx)
				if err != nil {
					return "", nil, err
				}
				return dossier.HeadDir, unlock, nil
			}
		case !errors.Is(statErr, os.ErrNotExist):
			return "", nil, statErr
		}
		// A stale, unreadable, or missing worktree is rebuilt from the mirror.
		if err := ctx.Err(); err != nil {
			return "", nil, err
		}
		if err := s.git.RemoveWorktree(ctx, ws.MirrorDir(), dossier.HeadDir); err != nil {
			return "", nil, fmt.Errorf("app: remove stale review worktree: %w", err)
		}
		if err := removeRunDir(dossier.HeadDir); err != nil {
			return "", nil, err
		}
	}
	runID := jobID
	if runID == "" {
		runID = workflowID
	}
	return s.prepareRun(ctx, project, token, mrIID, runID, dossier.DiffVersion.HeadSHA)
}

// runDirOwned reports whether dir is a direct child of the workspace runs
// directory, so recovery never deletes a path outside its workspace.
func runDirOwned(ws *workspace.Workspace, dir string) bool {
	if ws == nil || ws.Root == "" || dir == "" || !filepath.IsAbs(dir) {
		return false
	}
	runsDir := filepath.Join(ws.Root, "runs")
	if info, err := os.Lstat(runsDir); err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return false
	}
	rel, err := filepath.Rel(runsDir, filepath.Clean(dir))
	if err != nil || rel == "." || rel == ".." || strings.ContainsRune(rel, filepath.Separator) {
		return false
	}
	return true
}

// PublishPending retries completed workflows whose publication state is still
// pending. It is safe to call at startup and after a crash.
func (s *ReviewService) PublishPending(ctx context.Context) error {
	if s.publisher == nil {
		return nil
	}
	var errs []error
	for round := 0; round < 3; round++ {
		pending, err := s.store.ListPendingPublications(ctx, 50)
		if err != nil {
			return err
		}
		if len(pending) == 0 {
			return errors.Join(errs...)
		}
		progress := false
		for _, item := range pending {
			if err := s.publisher.Publish(ctx, item.WorkflowID); err != nil {
				if IsPermanentJob(err) {
					progress = true
					continue
				}
				errs = append(errs, err)
				continue
			}
			progress = true
		}
		if !progress {
			break
		}
	}
	return errors.Join(errs...)
}

func dossierDiffs(d *review.Dossier) []gitlab.FileDiff {
	out := make([]gitlab.FileDiff, 0, len(d.Files))
	for _, file := range d.Files {
		diff := gitlab.FileDiff{
			OldPath: file.OldPath,
			NewPath: file.Path,
			Diff:    file.Diff,
		}
		switch file.Status {
		case "added", "new":
			diff.NewFile = true
		case "deleted":
			diff.DeletedFile = true
		case "renamed":
			diff.RenamedFile = true
		}
		out = append(out, diff)
	}
	return out
}
