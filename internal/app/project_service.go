package app

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/config"
	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/gitlab"
	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/secret"
	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/store"
)

// ProjectStore is the persistence surface needed by ProjectService.
type ProjectStore interface {
	RegisterProject(ctx context.Context, project store.Project) (string, error)
	GetProject(ctx context.Context, id int64) (*store.Project, error)
	ListProjects(ctx context.Context) ([]store.Project, error)
	ListProjectsFiltered(ctx context.Context, filter store.ProjectFilter) ([]store.Project, error)
	UpdateProject(ctx context.Context, id int64, update store.ProjectUpdate) (*store.Project, error)
	UpdateProjectToken(ctx context.Context, id int64, update store.ProjectTokenUpdate) error
	UpdateProjectHook(ctx context.Context, id int64, hookID *int64, secretEnc []byte) error
	DeleteProject(ctx context.Context, id int64) error
}

// RegisterInput is an authenticated registration request.
type RegisterInput struct {
	Project     string
	AccessToken string
	Profile     string
	AutoWebhook bool
	Settings    json.RawMessage
}

// Registration is the successful registration response. WebhookSecret is set
// only when the webhook was not created automatically and must be shown once.
type Registration struct {
	Project       *store.Project
	JobID         string
	WebhookSecret string
}

// ProjectListFilter selects projects for the admin list endpoint.
type ProjectListFilter struct {
	Query   string
	Enabled *bool
}

// UpdateProjectInput changes only the provided project fields.
type UpdateProjectInput struct {
	Profile  *string
	Settings *json.RawMessage
	Enabled  *bool
}

// WebhookRegistration is the outcome of (re)creating a project webhook. The
// webhook secret itself is never returned again: it stays in project storage.
type WebhookRegistration struct {
	Project *store.Project
	HookID  int64
}

// ProjectService registers projects and validates their GitLab credentials.
type ProjectService struct {
	store     ProjectStore
	gitlab    gitlab.API
	cipher    *secret.Cipher
	profiles  map[string]config.ProfileConfig
	publicURL string
	hookURL   string
	workspace string
	logger    *slog.Logger
	now       func() time.Time

	mu sync.Mutex
}

// ProjectServiceConfig configures ProjectService.
type ProjectServiceConfig struct {
	Store     ProjectStore
	GitLab    gitlab.API
	Cipher    *secret.Cipher
	Profiles  map[string]config.ProfileConfig
	PublicURL string
	// Workspace is the workspace root shared with workspace.Manager. Project
	// directories and uploaded skill directories live below it.
	Workspace string
	Logger    *slog.Logger
	Now       func() time.Time
}

// NewProjectService builds the registration service.
func NewProjectService(cfg ProjectServiceConfig) (*ProjectService, error) {
	if cfg.Store == nil || cfg.GitLab == nil || cfg.Cipher == nil {
		return nil, errors.New("app: project service dependencies are required")
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	publicURL := strings.TrimRight(cfg.PublicURL, "/")
	hookURL := ""
	if parsed, err := url.Parse(publicURL); err == nil && parsed.Host != "" {
		parsed.RawQuery = ""
		parsed.ForceQuery = false
		parsed.Fragment = ""
		hookURL = parsed.JoinPath("webhooks", "gitlab").String()
	}
	return &ProjectService{
		store:     cfg.Store,
		gitlab:    cfg.GitLab,
		cipher:    cfg.Cipher,
		profiles:  cfg.Profiles,
		publicURL: publicURL,
		hookURL:   hookURL,
		workspace: strings.TrimSpace(cfg.Workspace),
		logger:    logger,
		now:       now,
	}, nil
}

// Register validates the project token and scopes, optionally creates a
// webhook, then atomically stores the project and onboarding job.
func (s *ProjectService) Register(ctx context.Context, in RegisterInput) (*Registration, error) {
	ref, err := gitlab.ParseProjectRef(in.Project)
	if err != nil {
		return nil, validationError("invalid project", Detail{Field: "project", Message: "must be a positive numeric ID or namespace/path"})
	}
	if strings.TrimSpace(in.AccessToken) == "" {
		return nil, validationError("access token is required", Detail{Field: "token", Message: "must not be empty"})
	}
	profile := in.Profile
	if profile == "" {
		profile = "default"
	}
	if _, ok := s.profiles[profile]; !ok {
		return nil, validationError("unknown profile", Detail{Field: "profile", Message: "profile is not defined"})
	}
	canonicalSettings, err := ParseSettings(in.Settings, s.profiles)
	if err != nil {
		return nil, err
	}
	if in.AutoWebhook {
		if err := s.validatePublicURL(); err != nil {
			return nil, err
		}
	}

	project, err := s.gitlab.GetProject(ctx, in.AccessToken, projectRefString(ref))
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, err
		}
		s.logger.Warn("GitLab project lookup failed", "status", gitlab.StatusOf(err))
		return nil, upstreamError(gitlab.StatusOf(err))
	}
	if project.ID <= 0 || project.PathWithNamespace == "" || project.WebURL == "" || project.DefaultBranch == "" {
		return nil, upstreamError(0)
	}

	info, err := s.gitlab.GetTokenInfo(ctx, in.AccessToken)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, err
		}
		s.logger.Warn("GitLab token lookup failed", "status", gitlab.StatusOf(err))
		return nil, upstreamError(gitlab.StatusOf(err))
	}
	if err := validateTokenInfo(info, s.now()); err != nil {
		return nil, err
	}

	tokenEnc, err := s.cipher.Encrypt([]byte(in.AccessToken))
	if err != nil {
		return nil, internalError()
	}
	webhookSecret, err := newWebhookSecret()
	if err != nil {
		return nil, internalError()
	}
	secretEnc, err := s.cipher.Encrypt([]byte(webhookSecret))
	if err != nil {
		return nil, internalError()
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if _, err := s.store.GetProject(ctx, project.ID); err == nil {
		return nil, conflictError("project is already registered")
	} else if !errors.Is(err, store.ErrNotFound) {
		return nil, internalError()
	}

	var hookID *int64
	if in.AutoWebhook {
		created, err := s.gitlab.CreateProjectHook(ctx, in.AccessToken, project.ID, gitlab.HookSpec{
			URL:                   s.hookURL,
			Secret:                webhookSecret,
			PushEvents:            true,
			MergeRequestsEvents:   true,
			NoteEvents:            true,
			EnableSSLVerification: true,
		})
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return nil, err
			}
			s.logger.Warn("GitLab webhook creation failed", "project_id", project.ID, "status", gitlab.StatusOf(err))
			return nil, upstreamError(gitlab.StatusOf(err))
		}
		hookID = &created
	}

	stored := store.Project{
		ID:                project.ID,
		PathWithNamespace: project.PathWithNamespace,
		WebURL:            project.WebURL,
		DefaultBranch:     project.DefaultBranch,
		TokenEnc:          tokenEnc,
		TokenExpiresAt:    info.ExpiresAt,
		TokenScopes:       append([]string(nil), info.Scopes...),
		WebhookSecretEnc:  secretEnc,
		HookID:            hookID,
		Profile:           profile,
		Settings:          canonicalSettings,
		Enabled:           true,
	}
	jobID, err := s.store.RegisterProject(ctx, stored)
	if err != nil {
		if hookID != nil {
			if delErr := s.gitlab.DeleteProjectHook(context.WithoutCancel(ctx), in.AccessToken, project.ID, *hookID); delErr != nil {
				s.logger.Warn("webhook compensation failed", "project_id", project.ID)
				return nil, &Error{Code: "internal", Message: "registration failed and webhook cleanup failed", Compensate: true}
			}
		}
		if errors.Is(err, store.ErrConflict) {
			return nil, conflictError("project is already registered")
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, err
		}
		s.logger.Warn("project registration persistence failed", "project_id", project.ID)
		return nil, internalError()
	}

	saved, err := s.store.GetProject(ctx, project.ID)
	if err != nil {
		// The registration committed. Report it; a follow-up read is diagnostic.
		s.logger.Warn("registered project read-back failed", "project_id", project.ID)
		saved = &stored
	}
	s.logger.Info("project registered", "project_id", project.ID, "job_id", jobID)

	result := &Registration{Project: saved, JobID: jobID}
	if !in.AutoWebhook {
		result.WebhookSecret = webhookSecret
	}
	return result, nil
}

// List returns registered projects. Encrypted fields are never serialized.
func (s *ProjectService) List(ctx context.Context) ([]store.Project, error) {
	projects, err := s.store.ListProjects(ctx)
	if err != nil {
		return nil, internalError()
	}
	return projects, nil
}

// ListFiltered returns projects matching the admin list selector.
func (s *ProjectService) ListFiltered(ctx context.Context, filter ProjectListFilter) ([]store.Project, error) {
	query := strings.TrimSpace(filter.Query)
	if len(query) > 200 {
		return nil, validationError("query is too long", Detail{Field: "q", Message: "must be at most 200 bytes"})
	}
	projects, err := s.store.ListProjectsFiltered(ctx, store.ProjectFilter{Query: query, Enabled: filter.Enabled})
	if err != nil {
		return nil, internalError()
	}
	return projects, nil
}

// Update changes the profile, settings or enabled state of a project.
func (s *ProjectService) Update(ctx context.Context, id int64, in UpdateProjectInput) (*store.Project, error) {
	if id <= 0 {
		return nil, validationError("invalid project id", Detail{Field: "id", Message: "must be positive"})
	}
	if in.Profile == nil && in.Settings == nil && in.Enabled == nil {
		return nil, validationError("at least one project field is required",
			Detail{Field: "body", Message: "provide profile, settings or enabled"})
	}
	update := store.ProjectUpdate{Enabled: in.Enabled}
	if in.Profile != nil {
		profile := strings.TrimSpace(*in.Profile)
		if _, ok := s.profiles[profile]; !ok {
			return nil, validationError("unknown profile", Detail{Field: "profile", Message: "profile is not defined"})
		}
		update.Profile = &profile
	}
	if in.Settings != nil {
		canonical, err := ParseSettings(*in.Settings, s.profiles)
		if err != nil {
			return nil, err
		}
		update.Settings = canonical
	}
	project, err := s.store.UpdateProject(ctx, id, update)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, notFoundError("project not registered")
		}
		s.logger.Warn("project update failed", "project_id", id)
		return nil, internalError()
	}
	return project, nil
}

// RotateToken revalidates access and scopes, then re-encrypts the new token.
func (s *ProjectService) RotateToken(ctx context.Context, id int64, token string) (*store.Project, error) {
	if id <= 0 {
		return nil, validationError("invalid project id", Detail{Field: "id", Message: "must be positive"})
	}
	if _, err := s.Get(ctx, id); err != nil {
		return nil, err
	}
	if strings.TrimSpace(token) == "" {
		return nil, validationError("access token is required", Detail{Field: "token", Message: "must not be empty"})
	}
	project, err := s.gitlab.GetProject(ctx, token, fmt.Sprintf("%d", id))
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, err
		}
		s.logger.Warn("GitLab project lookup failed during token rotation", "project_id", id, "status", gitlab.StatusOf(err))
		return nil, upstreamError(gitlab.StatusOf(err))
	}
	if project.ID != id {
		return nil, upstreamError(0)
	}
	info, err := s.gitlab.GetTokenInfo(ctx, token)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, err
		}
		s.logger.Warn("GitLab token lookup failed during rotation", "project_id", id, "status", gitlab.StatusOf(err))
		return nil, upstreamError(gitlab.StatusOf(err))
	}
	if err := validateTokenInfo(info, s.now()); err != nil {
		return nil, err
	}
	tokenEnc, err := s.cipher.Encrypt([]byte(token))
	if err != nil {
		return nil, internalError()
	}
	if err := s.store.UpdateProjectToken(ctx, id, store.ProjectTokenUpdate{
		TokenEnc:  tokenEnc,
		ExpiresAt: info.ExpiresAt,
		Scopes:    append([]string(nil), info.Scopes...),
	}); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, notFoundError("project not registered")
		}
		s.logger.Warn("project token persistence failed", "project_id", id)
		return nil, internalError()
	}
	return s.Get(ctx, id)
}

// CreateWebhook creates a fresh GitLab webhook for a registered project and
// updates its stored hook ID and secret. The previous hook is removed on a
// best-effort basis; the secret is never returned.
func (s *ProjectService) CreateWebhook(ctx context.Context, id int64) (*WebhookRegistration, error) {
	if id <= 0 {
		return nil, validationError("invalid project id", Detail{Field: "id", Message: "must be positive"})
	}
	if err := s.validatePublicURL(); err != nil {
		return nil, err
	}
	project, err := s.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	token, err := s.cipher.Decrypt(project.TokenEnc)
	if err != nil {
		return nil, internalError()
	}
	webhookSecret, err := newWebhookSecret()
	if err != nil {
		return nil, internalError()
	}
	created, err := s.gitlab.CreateProjectHook(ctx, string(token), id, gitlab.HookSpec{
		URL:                   s.hookURL,
		Secret:                webhookSecret,
		PushEvents:            true,
		MergeRequestsEvents:   true,
		NoteEvents:            true,
		EnableSSLVerification: true,
	})
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, err
		}
		s.logger.Warn("GitLab webhook creation failed", "project_id", id, "status", gitlab.StatusOf(err))
		return nil, upstreamError(gitlab.StatusOf(err))
	}
	secretEnc, err := s.cipher.Encrypt([]byte(webhookSecret))
	if err != nil {
		return nil, internalError()
	}
	if err := s.store.UpdateProjectHook(ctx, id, &created, secretEnc); err != nil {
		if delErr := s.gitlab.DeleteProjectHook(context.WithoutCancel(ctx), string(token), id, created); delErr != nil {
			s.logger.Warn("webhook compensation failed", "project_id", id)
			return nil, &Error{Code: "internal", Message: "webhook update failed and cleanup failed", Compensate: true}
		}
		return nil, internalError()
	}
	if project.HookID != nil && *project.HookID != created {
		if delErr := s.gitlab.DeleteProjectHook(context.WithoutCancel(ctx), string(token), id, *project.HookID); delErr != nil {
			s.logger.Warn("previous webhook removal failed", "project_id", id, "hook_id", *project.HookID)
		}
	}
	saved, err := s.Get(ctx, id)
	if err != nil {
		// The update committed; report it with the hook ID.
		s.logger.Warn("project read-back after webhook update failed", "project_id", id)
		saved = project
	}
	return &WebhookRegistration{Project: saved, HookID: created}, nil
}

// Delete removes the service-created webhook, the project workspace and the
// project-scoped extension rows, then deletes the project registration.
func (s *ProjectService) Delete(ctx context.Context, id int64) error {
	if id <= 0 {
		return validationError("invalid project id", Detail{Field: "id", Message: "must be positive"})
	}
	project, err := s.Get(ctx, id)
	if err != nil {
		return err
	}
	if project.HookID != nil {
		if token, decErr := s.cipher.Decrypt(project.TokenEnc); decErr != nil {
			s.logger.Warn("project token decrypt failed during delete", "project_id", id)
		} else if delErr := s.gitlab.DeleteProjectHook(context.WithoutCancel(ctx), string(token), id, *project.HookID); delErr != nil {
			s.logger.Warn("webhook removal failed during project delete", "project_id", id, "status", gitlab.StatusOf(delErr))
		}
	}
	if err := s.removeProjectData(id); err != nil {
		s.logger.Warn("project workspace cleanup failed", "project_id", id)
		return internalError()
	}
	if err := s.store.DeleteProject(ctx, id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return notFoundError("project not registered")
		}
		s.logger.Warn("project delete failed", "project_id", id)
		return internalError()
	}
	return nil
}

// removeProjectData removes the project workspace and its managed skill
// directory. It refuses to follow symlinked workspace roots.
func (s *ProjectService) removeProjectData(id int64) error {
	root := strings.TrimSpace(s.workspace)
	if root == "" {
		return nil
	}
	if !filepath.IsAbs(root) {
		return errors.New("app: workspace root is not absolute")
	}
	for _, path := range []string{
		filepath.Join(root, strconv.FormatInt(id, 10)),
		projectSkillsContainer(root, id),
	} {
		if err := removePath(path); err != nil {
			return err
		}
	}
	return nil
}

func removePath(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return os.Remove(path)
	}
	return os.RemoveAll(path)
}

// Get returns one registered project.
func (s *ProjectService) Get(ctx context.Context, id int64) (*store.Project, error) {
	if id <= 0 {
		return nil, validationError("invalid project id", Detail{Field: "id", Message: "must be positive"})
	}
	project, err := s.store.GetProject(ctx, id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, notFoundError("project not registered")
		}
		return nil, internalError()
	}
	return project, nil
}

func (s *ProjectService) validatePublicURL() error {
	if s.publicURL == "" {
		return validationError("server.public_url is required for auto_webhook", Detail{Field: "auto_webhook", Message: "public_url is not configured"})
	}
	u, err := url.Parse(s.publicURL)
	if err != nil || u.Host == "" || u.User != nil ||
		(u.Scheme != "http" && u.Scheme != "https") ||
		u.RawQuery != "" || u.Fragment != "" {
		return validationError("server.public_url is invalid", Detail{Field: "auto_webhook", Message: "public_url is not usable"})
	}
	return nil
}

func validateTokenInfo(info *gitlab.TokenInfo, now time.Time) error {
	if info == nil {
		return upstreamError(0)
	}
	if !info.Active || info.Revoked {
		return validationError("access token is not active", Detail{Field: "token", Message: "must be active and not revoked"})
	}
	if info.ExpiresAt != nil {
		// GitLab returns a date, not a timestamp: a token remains valid through
		// the whole UTC day named by expires_at.
		if now.UTC().Format("2006-01-02") > info.ExpiresAt.UTC().Format("2006-01-02") {
			return validationError("access token is expired", Detail{Field: "token", Message: "must not be expired"})
		}
	}
	scopes := make(map[string]bool, len(info.Scopes))
	for _, scope := range info.Scopes {
		scopes[scope] = true
	}
	var missing []Detail
	if !scopes["api"] {
		missing = append(missing, Detail{Field: "token.scopes", Message: "api scope is required"})
	}
	if !scopes["read_repository"] {
		missing = append(missing, Detail{Field: "token.scopes", Message: "read_repository scope is required"})
	}
	if len(missing) > 0 {
		return validationError("access token scopes are insufficient", missing...)
	}
	return nil
}

func projectRefString(ref gitlab.ProjectRef) string {
	if ref.ID > 0 {
		return fmt.Sprintf("%d", ref.ID)
	}
	return ref.Path
}

func newWebhookSecret() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("app: generate webhook secret: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}
