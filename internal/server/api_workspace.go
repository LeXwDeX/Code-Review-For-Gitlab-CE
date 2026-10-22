package server

import (
	"errors"
	"net/http"
	"strings"

	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/store"
)

func (s *Server) handleGetWorkspace(w http.ResponseWriter, r *http.Request) {
	if !s.requireStore(w, r) {
		return
	}
	projectID, err := parseID(r.PathValue("id"))
	if err != nil {
		validationFailed(w, r, "id", "id must be a positive integer")
		return
	}
	workspace, err := s.store.GetWorkspace(r.Context(), projectID)
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, workspace)
}

func (s *Server) handleRefreshWorkspace(w http.ResponseWriter, r *http.Request) {
	projectID, err := parseID(r.PathValue("id"))
	if err != nil {
		validationFailed(w, r, "id", "id must be a positive integer")
		return
	}
	if s.workspace == nil {
		writeError(w, r, http.StatusServiceUnavailable, "internal", "workspace service is not configured", nil)
		return
	}
	var request struct {
		RegenerateContext bool `json:"regenerate_context,omitempty"`
		Reindex           bool `json:"reindex,omitempty"`
	}
	if err := decodeOptionalJSON(w, r, adminBodyLimit, &request); err != nil {
		decodeRequestError(w, r, err)
		return
	}
	jobID, err := s.workspace.RefreshWorkspace(r.Context(), projectID, request.RegenerateContext, request.Reindex)
	if err != nil {
		s.writeServiceError(w, r, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"job_id": jobID})
}

func (s *Server) handleListContextDocs(w http.ResponseWriter, r *http.Request) {
	if !s.requireStore(w, r) {
		return
	}
	projectID, err := parseID(r.PathValue("id"))
	if err != nil {
		validationFailed(w, r, "id", "id must be a positive integer")
		return
	}
	limit, err := pageLimit(r)
	if err != nil {
		validationFailed(w, r, "limit", err.Error())
		return
	}
	kind := strings.TrimSpace(r.URL.Query().Get("kind"))
	if kind != "" && kind != "agents" && kind != "claude" {
		validationFailed(w, r, "kind", "kind must be agents or claude")
		return
	}
	documents, next, err := s.store.ListContextDocs(r.Context(), store.ContextDocFilter{
		ProjectID: projectID,
		Kind:      kind,
		Cursor:    r.URL.Query().Get("cursor"),
		Limit:     limit,
	})
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	if documents == nil {
		documents = []store.ContextDoc{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"documents": documents, "next_cursor": next})
}

func (s *Server) handleGetCurrentContextDoc(w http.ResponseWriter, r *http.Request) {
	if !s.requireStore(w, r) {
		return
	}
	projectID, err := parseID(r.PathValue("id"))
	if err != nil {
		validationFailed(w, r, "id", "id must be a positive integer")
		return
	}
	kind, err := contextKind(r)
	if err != nil {
		validationFailed(w, r, "kind", err.Error())
		return
	}
	doc, err := s.store.GetCurrentContextDoc(r.Context(), projectID, kind)
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, doc)
}

type contextDocSaveRequest struct {
	Kind    string `json:"kind,omitempty"`
	Content string `json:"content"`
	Locked  *bool  `json:"locked,omitempty"`
}

func (s *Server) handleSaveCurrentContextDoc(w http.ResponseWriter, r *http.Request) {
	if !s.requireStore(w, r) {
		return
	}
	projectID, err := parseID(r.PathValue("id"))
	if err != nil {
		validationFailed(w, r, "id", "id must be a positive integer")
		return
	}
	kind, err := contextKind(r)
	if err != nil {
		validationFailed(w, r, "kind", err.Error())
		return
	}
	var request contextDocSaveRequest
	if err := decodeJSON(w, r, adminBodyLimit, &request); err != nil {
		decodeRequestError(w, r, err)
		return
	}
	if request.Kind != "" && request.Kind != kind {
		validationFailed(w, r, "kind", "body kind must match the kind query parameter")
		return
	}
	if strings.TrimSpace(request.Content) == "" {
		validationFailed(w, r, "content", "content must not be empty")
		return
	}
	current, baseSHA, err := s.currentContextDoc(r, projectID, kind)
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	doc := store.ContextDoc{
		ProjectID: projectID,
		BaseSHA:   baseSHA,
		Kind:      kind,
		Content:   request.Content,
	}
	if current != nil {
		doc.ID = current.ID
		doc.Locked = current.Locked
	}
	if request.Locked != nil {
		doc.Locked = *request.Locked
	}
	if principal, ok := principalFrom(r.Context()); ok && principal.user.ID > 0 {
		userID := principal.user.ID
		doc.UpdatedBy = &userID
	}
	// The schema keeps one row per (project_id, base_sha, kind). A manual edit
	// therefore updates the current version in place instead of inserting a
	// new row; SaveManualContextDoc is the only path allowed to replace a
	// locked document. Changing that history model would require a schema
	// change, which is out of scope for M6.
	saved, err := s.store.SaveManualContextDoc(r.Context(), doc)
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, saved)
}

// currentContextDoc returns the active document for kind plus the base SHA a
// manual edit must target. A missing document falls back to the workspace base
// so the first manual edit can create the version.
func (s *Server) currentContextDoc(r *http.Request, projectID int64, kind string) (*store.ContextDoc, string, error) {
	current, err := s.store.GetCurrentContextDoc(r.Context(), projectID, kind)
	if err == nil {
		return current, current.BaseSHA, nil
	}
	if !errors.Is(err, store.ErrNotFound) {
		return nil, "", err
	}
	workspace, err := s.store.GetWorkspace(r.Context(), projectID)
	if err != nil {
		return nil, "", err
	}
	if strings.TrimSpace(workspace.BaseSHA) == "" {
		return nil, "", store.ErrNotFound
	}
	return nil, workspace.BaseSHA, nil
}

func (s *Server) handleLockContextDoc(w http.ResponseWriter, r *http.Request) {
	if !s.requireStore(w, r) {
		return
	}
	projectID, err := parseID(r.PathValue("id"))
	if err != nil {
		validationFailed(w, r, "id", "id must be a positive integer")
		return
	}
	kind, err := contextKind(r)
	if err != nil {
		validationFailed(w, r, "kind", err.Error())
		return
	}
	var request struct {
		Locked *bool `json:"locked"`
	}
	if err := decodeJSON(w, r, adminBodyLimit, &request); err != nil {
		decodeRequestError(w, r, err)
		return
	}
	if request.Locked == nil {
		validationFailed(w, r, "locked", "locked is required")
		return
	}
	current, _, err := s.currentContextDoc(r, projectID, kind)
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	if current == nil {
		// currentContextDoc only returns a base SHA without a document when the
		// workspace has never generated one; there is nothing to lock yet.
		writeError(w, r, http.StatusNotFound, "not_found", "context document not found", nil)
		return
	}
	var userID *int64
	if principal, ok := principalFrom(r.Context()); ok && principal.user.ID > 0 {
		value := principal.user.ID
		userID = &value
	}
	if err := s.store.LockContextDoc(r.Context(), projectID, current.BaseSHA, kind, *request.Locked, userID); err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
