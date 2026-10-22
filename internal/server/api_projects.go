package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/app"
	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/store"
)

type registerProjectRequest struct {
	Project     string          `json:"project"`
	Token       string          `json:"token"`
	Profile     string          `json:"profile,omitempty"`
	AutoWebhook bool            `json:"auto_webhook,omitempty"`
	Settings    json.RawMessage `json:"settings,omitempty"`
}

type registerProjectResponse struct {
	Project       *store.Project `json:"project"`
	JobID         string         `json:"job_id"`
	WebhookSecret string         `json:"webhook_secret,omitempty"`
}

type updateProjectRequest struct {
	Profile  *string          `json:"profile,omitempty"`
	Settings *json.RawMessage `json:"settings,omitempty"`
	Enabled  *bool            `json:"enabled,omitempty"`
}

type rotateProjectTokenRequest struct {
	Token string `json:"token"`
}

type projectWebhookResponse struct {
	Project *store.Project `json:"project"`
	HookID  int64          `json:"hook_id"`
}

func (s *Server) handleRegisterProject(w http.ResponseWriter, r *http.Request) {
	var request registerProjectRequest
	if err := decodeJSON(w, r, adminBodyLimit, &request); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeError(w, r, http.StatusBadRequest, "bad_request", "request body is too large", nil)
			return
		}
		writeError(w, r, http.StatusBadRequest, "validation_failed", "invalid JSON request body", nil)
		return
	}
	registration, err := s.projects.Register(r.Context(), app.RegisterInput{
		Project:     request.Project,
		AccessToken: request.Token,
		Profile:     request.Profile,
		AutoWebhook: request.AutoWebhook,
		Settings:    request.Settings,
	})
	if err != nil {
		writeAppError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, registerProjectResponse{
		Project:       registration.Project,
		JobID:         registration.JobID,
		WebhookSecret: registration.WebhookSecret,
	})
}

func (s *Server) handleListProjects(w http.ResponseWriter, r *http.Request) {
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	if len(query) > 200 {
		validationFailed(w, r, "q", "q must be at most 200 bytes")
		return
	}
	var enabled *bool
	switch raw := strings.TrimSpace(r.URL.Query().Get("enabled")); raw {
	case "":
	case "true", "1":
		value := true
		enabled = &value
	case "false", "0":
		value := false
		enabled = &value
	default:
		validationFailed(w, r, "enabled", "enabled must be true or false")
		return
	}
	projects, err := s.projects.ListFiltered(r.Context(), app.ProjectListFilter{Query: query, Enabled: enabled})
	if err != nil {
		writeAppError(w, r, err)
		return
	}
	if projects == nil {
		projects = []store.Project{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"projects": projects})
}

func (s *Server) handleGetProject(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r.PathValue("id"))
	if err != nil {
		validationFailed(w, r, "id", "id must be a positive integer")
		return
	}
	project, err := s.projects.Get(r.Context(), id)
	if err != nil {
		writeAppError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, project)
}

func (s *Server) handleUpdateProject(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r.PathValue("id"))
	if err != nil {
		validationFailed(w, r, "id", "id must be a positive integer")
		return
	}
	var request updateProjectRequest
	if err := decodeJSON(w, r, adminBodyLimit, &request); err != nil {
		decodeRequestError(w, r, err)
		return
	}
	if request.Profile == nil && request.Settings == nil && request.Enabled == nil {
		validationFailed(w, r, "body", "at least one of profile, settings or enabled is required")
		return
	}
	project, err := s.projects.Update(r.Context(), id, app.UpdateProjectInput{
		Profile:  request.Profile,
		Settings: request.Settings,
		Enabled:  request.Enabled,
	})
	if err != nil {
		writeAppError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, project)
}

func (s *Server) handleRotateProjectToken(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r.PathValue("id"))
	if err != nil {
		validationFailed(w, r, "id", "id must be a positive integer")
		return
	}
	var request rotateProjectTokenRequest
	if err := decodeJSON(w, r, adminBodyLimit, &request); err != nil {
		decodeRequestError(w, r, err)
		return
	}
	project, err := s.projects.RotateToken(r.Context(), id, request.Token)
	if err != nil {
		writeAppError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, project)
}

func (s *Server) handleCreateProjectWebhook(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r.PathValue("id"))
	if err != nil {
		validationFailed(w, r, "id", "id must be a positive integer")
		return
	}
	registration, err := s.projects.CreateWebhook(r.Context(), id)
	if err != nil {
		writeAppError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, projectWebhookResponse{
		Project: registration.Project,
		HookID:  registration.HookID,
	})
}

func (s *Server) handleDeleteProject(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r.PathValue("id"))
	if err != nil {
		validationFailed(w, r, "id", "id must be a positive integer")
		return
	}
	if err := s.projects.Delete(r.Context(), id); err != nil {
		writeAppError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
