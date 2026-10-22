package sqlite

import (
	"bytes"
	"compress/gzip"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/llm"
	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/store"
)

var _ llm.Recorder = (*Store)(nil)

const maxLLMPayload = 1 << 20

// ---------------------------------------------------------------------------
// LLM call audit

// RecordLLMCall persists one provider attempt. Payloads are gzipped and
// bounded; provider credentials never appear in llm.Request or CallRecord.
func (s *Store) RecordLLMCall(ctx context.Context, call llm.CallRecord) error {
	if strings.TrimSpace(call.Meta.WorkflowID) == "" || strings.TrimSpace(call.Meta.NodeID) == "" {
		return errors.New("sqlite: llm call workflow id and node id are required")
	}
	if strings.TrimSpace(call.Provider) == "" || strings.TrimSpace(call.Model) == "" {
		return errors.New("sqlite: llm call provider and model are required")
	}
	var requestValue, responseValue any
	if call.Request != nil {
		requestValue = call.Request
	}
	if call.Response != nil {
		responseValue = call.Response
	}
	request, err := encodeGzipJSON(requestValue)
	if err != nil {
		return err
	}
	response, err := encodeGzipJSON(responseValue)
	if err != nil {
		return err
	}
	var (
		inputTokens  int64
		outputTokens int64
		cacheRead    int64
		cacheWrite   int64
		stopReason   string
		requestID    string
	)
	if call.Response != nil {
		inputTokens = call.Response.Usage.InputTokens
		outputTokens = call.Response.Usage.OutputTokens
		cacheRead = call.Response.Usage.CacheReadTokens
		cacheWrite = call.Response.Usage.CacheWriteTokens
		stopReason = string(call.Response.StopReason)
		requestID = call.Response.RequestID
	}
	id, err := newID()
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO llm_calls
		(id, workflow_id, node_id, turn, provider, model, request, response,
		 input_tokens, output_tokens, cache_read, cache_write, latency_ms,
		 stop_reason, error_kind, request_id, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, call.Meta.WorkflowID, call.Meta.NodeID, call.Meta.Turn,
		call.Provider, call.Model, request, response, inputTokens, outputTokens,
		cacheRead, cacheWrite, call.Duration.Milliseconds(), nullString(stopReason),
		nullString(llmErrorKind(call.Err)), nullString(requestID),
		formatTime(s.now().UTC()))
	if err != nil {
		return fmt.Errorf("sqlite: record llm call: %w", err)
	}
	return nil
}

// RecordCall implements llm.Recorder. Audit failure must not fail the model
// call, so the error is deliberately dropped.
func (s *Store) RecordCall(ctx context.Context, call llm.CallRecord) {
	_ = s.RecordLLMCall(ctx, call)
}

func llmErrorKind(err error) string {
	if err == nil {
		return ""
	}
	var classified *llm.Error
	if errors.As(err, &classified) {
		return string(classified.Kind)
	}
	return "error"
}

func encodeGzipJSON(value any) ([]byte, error) {
	if value == nil {
		return nil, nil
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("sqlite: encode llm payload: %w", err)
	}
	if len(raw) > maxLLMPayload {
		raw, err = json.Marshal(map[string]any{"truncated": true, "bytes": len(raw)})
		if err != nil {
			return nil, fmt.Errorf("sqlite: encode truncated llm payload: %w", err)
		}
	}
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(raw); err != nil {
		return nil, fmt.Errorf("sqlite: gzip llm payload: %w", err)
	}
	if err := zw.Close(); err != nil {
		return nil, fmt.Errorf("sqlite: gzip llm payload: %w", err)
	}
	return buf.Bytes(), nil
}

func decodeGzipJSON(data []byte) (json.RawMessage, error) {
	if len(data) == 0 {
		return nil, nil
	}
	zr, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("sqlite: open llm payload: %w", err)
	}
	defer zr.Close()
	raw, err := io.ReadAll(io.LimitReader(zr, maxLLMPayload+1))
	if err != nil {
		return nil, fmt.Errorf("sqlite: read llm payload: %w", err)
	}
	if len(raw) > maxLLMPayload {
		return nil, errors.New("sqlite: llm payload exceeds stored bound")
	}
	if !json.Valid(raw) {
		return nil, errors.New("sqlite: llm payload is not valid JSON")
	}
	return json.RawMessage(raw), nil
}

// ListLLMCalls returns audit rows newest first. Payloads are decompressed only
// when requested.
func (s *Store) ListLLMCalls(ctx context.Context, filter store.LLMCallFilter) ([]store.LLMCall, string, error) {
	limit := normalizeLimit(filter.Limit)
	where := []string{"1 = 1"}
	args := make([]any, 0, 4)
	if filter.WorkflowID != "" {
		where = append(where, "workflow_id = ?")
		args = append(args, filter.WorkflowID)
	}
	if filter.NodeID != "" {
		where = append(where, "node_id = ?")
		args = append(args, filter.NodeID)
	}
	if filter.Cursor != "" {
		created, id, err := decodeCursor(filter.Cursor, store.ErrInvalidCursor)
		if err != nil {
			return nil, "", err
		}
		where = append(where, "(created_at < ? OR (created_at = ? AND id < ?))")
		args = append(args, created, created, id)
	}
	payload := "NULL, NULL"
	if filter.IncludePayload {
		payload = "request, response"
	}
	query := `SELECT id, workflow_id, node_id, turn, provider, model, ` + payload + `,
		input_tokens, output_tokens, cache_read, cache_write, latency_ms,
		stop_reason, error_kind, request_id, created_at
		FROM llm_calls WHERE ` + strings.Join(where, " AND ") + `
		ORDER BY created_at DESC, id DESC LIMIT ?`
	args = append(args, limit+1)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, "", fmt.Errorf("sqlite: list llm calls: %w", err)
	}
	defer rows.Close()
	calls := make([]store.LLMCall, 0, limit)
	for rows.Next() {
		var (
			call       store.LLMCall
			request    []byte
			response   []byte
			stopReason sql.NullString
			errorKind  sql.NullString
			requestID  sql.NullString
			createdAt  string
		)
		if err := rows.Scan(&call.ID, &call.WorkflowID, &call.NodeID, &call.Turn,
			&call.Provider, &call.Model, &request, &response, &call.InputTokens,
			&call.OutputTokens, &call.CacheRead, &call.CacheWrite, &call.LatencyMS,
			&stopReason, &errorKind, &requestID, &createdAt); err != nil {
			return nil, "", fmt.Errorf("sqlite: scan llm call: %w", err)
		}
		if filter.IncludePayload {
			if call.Request, err = decodeGzipJSON(request); err != nil {
				return nil, "", fmt.Errorf("sqlite: decode llm request %s: %w", call.ID, err)
			}
			if call.Response, err = decodeGzipJSON(response); err != nil {
				return nil, "", fmt.Errorf("sqlite: decode llm response %s: %w", call.ID, err)
			}
		}
		call.StopReason = stopReason.String
		call.ErrorKind = errorKind.String
		call.RequestID = requestID.String
		if call.CreatedAt, err = parseTime(createdAt); err != nil {
			return nil, "", err
		}
		calls = append(calls, call)
	}
	if err := rows.Err(); err != nil {
		return nil, "", fmt.Errorf("sqlite: iterate llm calls: %w", err)
	}
	next := ""
	if len(calls) > limit {
		calls = calls[:limit]
		last := calls[len(calls)-1]
		next = encodeCursor(last.CreatedAt, last.ID)
	}
	return calls, next, nil
}

// ClearLLMPayloads removes stored request and response bodies older than the
// cutoff while preserving token and latency aggregates.
func (s *Store) ClearLLMPayloads(ctx context.Context, olderThan time.Time) (int64, error) {
	if olderThan.IsZero() {
		return 0, errors.New("sqlite: retention cutoff is required")
	}
	res, err := s.db.ExecContext(ctx, `UPDATE llm_calls
		SET request = NULL, response = NULL
		WHERE created_at < ? AND (request IS NOT NULL OR response IS NOT NULL)`,
		formatTime(olderThan.UTC()))
	if err != nil {
		return 0, fmt.Errorf("sqlite: clear llm payloads: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("sqlite: clear llm payloads: %w", err)
	}
	return n, nil
}

// ---------------------------------------------------------------------------
// Workspace and context documents

const workspaceColumns = `project_id, state, base_sha, index_sha, context_sha,
	last_refresh_at, last_error, updated_at`

func scanWorkspace(row rowScanner) (*store.Workspace, error) {
	var (
		ws            store.Workspace
		baseSHA       sql.NullString
		indexSHA      sql.NullString
		contextSHA    sql.NullString
		lastRefreshAt sql.NullString
		lastError     sql.NullString
		updatedAt     string
	)
	if err := row.Scan(&ws.ProjectID, &ws.State, &baseSHA, &indexSHA, &contextSHA,
		&lastRefreshAt, &lastError, &updatedAt); err != nil {
		return nil, err
	}
	ws.BaseSHA = baseSHA.String
	ws.IndexSHA = indexSHA.String
	ws.ContextSHA = contextSHA.String
	ws.LastError = lastError.String
	if lastRefreshAt.Valid {
		t, err := parseTime(lastRefreshAt.String)
		if err != nil {
			return nil, err
		}
		ws.LastRefreshAt = &t
	}
	var err error
	if ws.UpdatedAt, err = parseTime(updatedAt); err != nil {
		return nil, err
	}
	return &ws, nil
}

func validWorkspaceState(state string) bool {
	switch state {
	case "new", "onboarding", "ready", "refreshing", "error":
		return true
	default:
		return false
	}
}

// GetWorkspace returns one workspace state row.
func (s *Store) GetWorkspace(ctx context.Context, projectID int64) (*store.Workspace, error) {
	if projectID <= 0 {
		return nil, errors.New("sqlite: project id is required")
	}
	ws, err := scanWorkspace(s.db.QueryRowContext(ctx,
		`SELECT `+workspaceColumns+` FROM workspaces WHERE project_id = ?`, projectID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: workspace for project %d", store.ErrNotFound, projectID)
	}
	if err != nil {
		return nil, fmt.Errorf("sqlite: get workspace: %w", err)
	}
	return ws, nil
}

// SaveWorkspace inserts or replaces the workspace state row.
func (s *Store) SaveWorkspace(ctx context.Context, ws store.Workspace) (*store.Workspace, error) {
	if ws.ProjectID <= 0 {
		return nil, errors.New("sqlite: workspace project id is required")
	}
	if !validWorkspaceState(ws.State) {
		return nil, fmt.Errorf("sqlite: invalid workspace state %q", ws.State)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("sqlite: begin save workspace: %w", err)
	}
	defer tx.Rollback()
	if err := requireProjectTx(ctx, tx, ws.ProjectID); err != nil {
		return nil, err
	}
	if ws.UpdatedAt.IsZero() {
		ws.UpdatedAt = s.now().UTC()
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO workspaces
		(project_id, state, base_sha, index_sha, context_sha, last_refresh_at,
		 last_error, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (project_id) DO UPDATE SET
			state = excluded.state,
			base_sha = excluded.base_sha,
			index_sha = excluded.index_sha,
			context_sha = excluded.context_sha,
			last_refresh_at = excluded.last_refresh_at,
			last_error = excluded.last_error,
			updated_at = excluded.updated_at`,
		ws.ProjectID, ws.State, nullString(ws.BaseSHA), nullString(ws.IndexSHA),
		nullString(ws.ContextSHA), nullableTime(ws.LastRefreshAt),
		nullString(ws.LastError), formatTime(ws.UpdatedAt.UTC())); err != nil {
		return nil, fmt.Errorf("sqlite: save workspace: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("sqlite: commit save workspace: %w", err)
	}
	return s.GetWorkspace(ctx, ws.ProjectID)
}

// SetActiveContext points the workspace at an existing context document
// version.
func (s *Store) SetActiveContext(ctx context.Context, projectID int64, baseSHA string) error {
	if projectID <= 0 || strings.TrimSpace(baseSHA) == "" {
		return errors.New("sqlite: project id and base sha are required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sqlite: begin active context: %w", err)
	}
	defer tx.Rollback()
	var one int
	err = tx.QueryRowContext(ctx,
		`SELECT 1 FROM context_docs WHERE project_id = ? AND base_sha = ? LIMIT 1`,
		projectID, strings.TrimSpace(baseSHA)).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: context document %s for project %d", store.ErrNotFound, baseSHA, projectID)
	}
	if err != nil {
		return fmt.Errorf("sqlite: check context document: %w", err)
	}
	res, err := tx.ExecContext(ctx, `UPDATE workspaces SET context_sha = ?, updated_at = ?
		WHERE project_id = ?`, strings.TrimSpace(baseSHA), formatTime(s.now().UTC()), projectID)
	if err != nil {
		return fmt.Errorf("sqlite: set active context: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return fmt.Errorf("sqlite: set active context: %w", err)
	} else if n != 1 {
		return fmt.Errorf("%w: workspace for project %d", store.ErrNotFound, projectID)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("sqlite: commit active context: %w", err)
	}
	return nil
}

const contextDocColumns = `id, project_id, base_sha, kind, source, content, locked,
	model, workflow_id, created_at, updated_by`

func validContextKind(kind string) bool {
	return kind == "agents" || kind == "claude"
}

func scanContextDoc(row rowScanner) (*store.ContextDoc, error) {
	var (
		doc        store.ContextDoc
		locked     int
		model      sql.NullString
		workflowID sql.NullString
		createdAt  string
		updatedBy  sql.NullInt64
	)
	if err := row.Scan(&doc.ID, &doc.ProjectID, &doc.BaseSHA, &doc.Kind, &doc.Source,
		&doc.Content, &locked, &model, &workflowID, &createdAt, &updatedBy); err != nil {
		return nil, err
	}
	doc.Locked = locked != 0
	doc.Model = model.String
	doc.WorkflowID = workflowID.String
	if updatedBy.Valid {
		value := updatedBy.Int64
		doc.UpdatedBy = &value
	}
	var err error
	if doc.CreatedAt, err = parseTime(createdAt); err != nil {
		return nil, err
	}
	return &doc, nil
}

func (s *Store) workspaceRefs(ctx context.Context, projectID int64) (baseSHA, contextSHA string, err error) {
	var base, contextRef sql.NullString
	err = s.db.QueryRowContext(ctx,
		`SELECT base_sha, context_sha FROM workspaces WHERE project_id = ?`,
		projectID).Scan(&base, &contextRef)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", nil
	}
	if err != nil {
		return "", "", fmt.Errorf("sqlite: read workspace refs: %w", err)
	}
	return base.String, contextRef.String, nil
}

func applyContextFlags(doc *store.ContextDoc, baseSHA, contextSHA string) {
	doc.Stale = baseSHA != "" && doc.BaseSHA != baseSHA
	doc.Active = contextSHA != "" && doc.BaseSHA == contextSHA
}

// GetContextDoc returns one document version.
func (s *Store) GetContextDoc(ctx context.Context, projectID int64, baseSHA, kind string) (*store.ContextDoc, error) {
	if projectID <= 0 || strings.TrimSpace(baseSHA) == "" || !validContextKind(kind) {
		return nil, errors.New("sqlite: project id, base sha and kind are required")
	}
	doc, err := scanContextDoc(s.db.QueryRowContext(ctx,
		`SELECT `+contextDocColumns+` FROM context_docs
		 WHERE project_id = ? AND base_sha = ? AND kind = ?`, projectID, baseSHA, kind))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: context %s %s for project %d", store.ErrNotFound, kind, baseSHA, projectID)
	}
	if err != nil {
		return nil, fmt.Errorf("sqlite: get context document: %w", err)
	}
	wsBase, wsContext, err := s.workspaceRefs(ctx, projectID)
	if err != nil {
		return nil, err
	}
	applyContextFlags(doc, wsBase, wsContext)
	return doc, nil
}

// GetCurrentContextDoc returns the workspace-active version, or the newest
// stored version when no active reference was set.
func (s *Store) GetCurrentContextDoc(ctx context.Context, projectID int64, kind string) (*store.ContextDoc, error) {
	if projectID <= 0 || !validContextKind(kind) {
		return nil, errors.New("sqlite: project id and kind are required")
	}
	wsBase, wsContext, err := s.workspaceRefs(ctx, projectID)
	if err != nil {
		return nil, err
	}
	var doc *store.ContextDoc
	if wsContext != "" {
		doc, err = scanContextDoc(s.db.QueryRowContext(ctx,
			`SELECT `+contextDocColumns+` FROM context_docs
			 WHERE project_id = ? AND base_sha = ? AND kind = ?`, projectID, wsContext, kind))
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("sqlite: get current context document: %w", err)
		}
	}
	if doc == nil {
		doc, err = scanContextDoc(s.db.QueryRowContext(ctx,
			`SELECT `+contextDocColumns+` FROM context_docs
			 WHERE project_id = ? AND kind = ?
			 ORDER BY created_at DESC, id DESC LIMIT 1`, projectID, kind))
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("%w: context %s for project %d", store.ErrNotFound, kind, projectID)
		}
		if err != nil {
			return nil, fmt.Errorf("sqlite: get current context document: %w", err)
		}
	}
	applyContextFlags(doc, wsBase, wsContext)
	return doc, nil
}

// ListContextDocs returns document versions newest first.
func (s *Store) ListContextDocs(ctx context.Context, filter store.ContextDocFilter) ([]store.ContextDoc, string, error) {
	if filter.ProjectID <= 0 {
		return nil, "", errors.New("sqlite: project id is required")
	}
	if filter.Kind != "" && !validContextKind(filter.Kind) {
		return nil, "", fmt.Errorf("sqlite: invalid context kind %q", filter.Kind)
	}
	limit := normalizeLimit(filter.Limit)
	where := []string{"project_id = ?"}
	args := []any{filter.ProjectID}
	if filter.Kind != "" {
		where = append(where, "kind = ?")
		args = append(args, filter.Kind)
	}
	if filter.Cursor != "" {
		created, id, err := decodeCursor(filter.Cursor, store.ErrInvalidCursor)
		if err != nil {
			return nil, "", err
		}
		where = append(where, "(created_at < ? OR (created_at = ? AND id < ?))")
		args = append(args, created, created, id)
	}
	args = append(args, limit+1)
	rows, err := s.db.QueryContext(ctx, `SELECT `+contextDocColumns+` FROM context_docs
		WHERE `+strings.Join(where, " AND ")+`
		ORDER BY created_at DESC, id DESC LIMIT ?`, args...)
	if err != nil {
		return nil, "", fmt.Errorf("sqlite: list context documents: %w", err)
	}
	defer rows.Close()
	docs := make([]store.ContextDoc, 0, limit)
	for rows.Next() {
		doc, err := scanContextDoc(rows)
		if err != nil {
			return nil, "", fmt.Errorf("sqlite: scan context document: %w", err)
		}
		docs = append(docs, *doc)
	}
	if err := rows.Err(); err != nil {
		return nil, "", fmt.Errorf("sqlite: iterate context documents: %w", err)
	}
	wsBase, wsContext, err := s.workspaceRefs(ctx, filter.ProjectID)
	if err != nil {
		return nil, "", err
	}
	for i := range docs {
		applyContextFlags(&docs[i], wsBase, wsContext)
	}
	next := ""
	if len(docs) > limit {
		docs = docs[:limit]
		last := docs[len(docs)-1]
		next = encodeCursor(last.CreatedAt, last.ID)
	}
	return docs, next, nil
}

// SaveContextDoc stores a generated or refreshed document. A locked row is
// never overwritten; SaveManualContextDoc is the explicit manual override.
func (s *Store) SaveContextDoc(ctx context.Context, doc store.ContextDoc) (*store.ContextDoc, error) {
	return s.saveContextDoc(ctx, doc, false)
}

// SaveManualContextDoc stores an explicit administrator edit. This is the only
// path allowed to replace a locked document.
func (s *Store) SaveManualContextDoc(ctx context.Context, doc store.ContextDoc) (*store.ContextDoc, error) {
	return s.saveContextDoc(ctx, doc, true)
}

func (s *Store) saveContextDoc(ctx context.Context, doc store.ContextDoc, manual bool) (*store.ContextDoc, error) {
	if doc.ProjectID <= 0 || strings.TrimSpace(doc.BaseSHA) == "" {
		return nil, errors.New("sqlite: context document project id and base sha are required")
	}
	if !validContextKind(doc.Kind) {
		return nil, fmt.Errorf("sqlite: invalid context kind %q", doc.Kind)
	}
	if strings.TrimSpace(doc.Content) == "" {
		return nil, errors.New("sqlite: context document content is required")
	}
	doc.BaseSHA = strings.TrimSpace(doc.BaseSHA)
	if manual {
		doc.Source = "manual"
	} else {
		doc.Source = "generated"
	}
	now := s.now().UTC()
	if doc.ID == "" {
		id, err := newID()
		if err != nil {
			return nil, err
		}
		doc.ID = id
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("sqlite: begin save context document: %w", err)
	}
	defer tx.Rollback()
	if err := requireProjectTx(ctx, tx, doc.ProjectID); err != nil {
		return nil, err
	}

	var (
		existingID      string
		existingLocked  int
		existingCreated string
	)
	err = tx.QueryRowContext(ctx, `SELECT id, locked, created_at FROM context_docs
		WHERE project_id = ? AND base_sha = ? AND kind = ?`,
		doc.ProjectID, doc.BaseSHA, doc.Kind).Scan(&existingID, &existingLocked, &existingCreated)
	switch {
	case err == nil:
		if !manual && existingLocked != 0 {
			return nil, fmt.Errorf("%w: context %s %s for project %d",
				store.ErrContextLocked, doc.Kind, doc.BaseSHA, doc.ProjectID)
		}
		locked := existingLocked
		if manual {
			locked = boolInt(doc.Locked)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE context_docs SET
			source = ?, content = ?, locked = ?, model = ?, workflow_id = ?, updated_by = ?
			WHERE id = ?`, doc.Source, doc.Content, locked, nullString(doc.Model),
			nullString(doc.WorkflowID), nullableInt(doc.UpdatedBy), existingID); err != nil {
			return nil, fmt.Errorf("sqlite: update context document: %w", err)
		}
		doc.ID = existingID
		if doc.CreatedAt, err = parseTime(existingCreated); err != nil {
			return nil, err
		}
		doc.Locked = locked != 0
	case errors.Is(err, sql.ErrNoRows):
		doc.CreatedAt = now
		locked := boolInt(doc.Locked)
		if !manual {
			// A generated version starts unlocked; only an explicit manual save
			// may create a locked version.
			locked = 0
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO context_docs
			(id, project_id, base_sha, kind, source, content, locked, model,
			 workflow_id, created_at, updated_by)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			doc.ID, doc.ProjectID, doc.BaseSHA, doc.Kind, doc.Source, doc.Content,
			locked, nullString(doc.Model), nullString(doc.WorkflowID),
			formatTime(doc.CreatedAt), nullableInt(doc.UpdatedBy)); err != nil {
			if isConstraintErr(err) {
				return nil, fmt.Errorf("%w: context document already exists", store.ErrConflict)
			}
			return nil, fmt.Errorf("sqlite: insert context document: %w", err)
		}
	default:
		return nil, fmt.Errorf("sqlite: read context document: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("sqlite: commit context document: %w", err)
	}
	return s.GetContextDoc(ctx, doc.ProjectID, doc.BaseSHA, doc.Kind)
}

// LockContextDoc toggles the lock flag without touching content.
func (s *Store) LockContextDoc(ctx context.Context, projectID int64, baseSHA, kind string, locked bool, userID *int64) error {
	if projectID <= 0 || strings.TrimSpace(baseSHA) == "" || !validContextKind(kind) {
		return errors.New("sqlite: project id, base sha and kind are required")
	}
	res, err := s.db.ExecContext(ctx, `UPDATE context_docs
		SET locked = ?, updated_by = COALESCE(?, updated_by)
		WHERE project_id = ? AND base_sha = ? AND kind = ?`,
		boolInt(locked), nullableInt(userID), projectID, baseSHA, kind)
	if err != nil {
		return fmt.Errorf("sqlite: lock context document: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return fmt.Errorf("sqlite: lock context document: %w", err)
	} else if n != 1 {
		return fmt.Errorf("%w: context %s %s for project %d", store.ErrNotFound, kind, baseSHA, projectID)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Feedback

const feedbackColumns = `id, project_id, mr_iid, publication_id, finding_id,
	signal, gitlab_user_id, observed_at`

func scanFeedback(row rowScanner) (*store.Feedback, error) {
	var (
		f          store.Feedback
		findingID  sql.NullString
		gitlabUser sql.NullInt64
		observedAt string
	)
	if err := row.Scan(&f.ID, &f.ProjectID, &f.MRIID, &f.PublicationID, &findingID,
		&f.Signal, &gitlabUser, &observedAt); err != nil {
		return nil, err
	}
	f.FindingID = findingID.String
	if gitlabUser.Valid {
		value := gitlabUser.Int64
		f.GitlabUserID = &value
	}
	var err error
	if f.ObservedAt, err = parseTime(observedAt); err != nil {
		return nil, err
	}
	return &f, nil
}

// UpsertFeedback records a signal idempotently. A nil GitLab user is anonymous
// and is matched with IS NULL, because SQLite unique indexes treat NULLs as
// distinct.
func (s *Store) UpsertFeedback(ctx context.Context, f store.Feedback) (*store.Feedback, error) {
	if f.ProjectID <= 0 || f.MRIID <= 0 {
		return nil, errors.New("sqlite: feedback project id and mr iid are required")
	}
	if strings.TrimSpace(f.PublicationID) == "" || strings.TrimSpace(f.Signal) == "" {
		return nil, errors.New("sqlite: feedback publication id and signal are required")
	}
	now := s.now().UTC()
	if f.ObservedAt.IsZero() {
		f.ObservedAt = now
	} else {
		f.ObservedAt = f.ObservedAt.UTC()
	}

	match := `publication_id = ? AND signal = ? AND gitlab_user_id IS NULL`
	matchArgs := []any{f.PublicationID, f.Signal}
	if f.GitlabUserID != nil {
		match = `publication_id = ? AND signal = ? AND gitlab_user_id = ?`
		matchArgs = append(matchArgs, *f.GitlabUserID)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("sqlite: begin feedback: %w", err)
	}
	defer tx.Rollback()
	var existingID string
	err = tx.QueryRowContext(ctx,
		`SELECT id FROM feedback WHERE `+match, matchArgs...).Scan(&existingID)
	switch {
	case err == nil:
		if _, err := tx.ExecContext(ctx, `UPDATE feedback
			SET finding_id = ?, observed_at = ? WHERE id = ?`,
			nullString(f.FindingID), formatTime(f.ObservedAt), existingID); err != nil {
			return nil, fmt.Errorf("sqlite: update feedback: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return nil, fmt.Errorf("sqlite: commit feedback: %w", err)
		}
		return s.getFeedbackByID(ctx, existingID)
	case errors.Is(err, sql.ErrNoRows):
		if f.ID == "" {
			id, err := newID()
			if err != nil {
				return nil, err
			}
			f.ID = id
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO feedback
			(id, project_id, mr_iid, publication_id, finding_id, signal,
			 gitlab_user_id, observed_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			f.ID, f.ProjectID, f.MRIID, f.PublicationID, nullString(f.FindingID),
			f.Signal, nullableInt(f.GitlabUserID), formatTime(f.ObservedAt))
		if err != nil {
			if isConstraintErr(err) {
				return nil, fmt.Errorf("%w: feedback already exists", store.ErrConflict)
			}
			return nil, fmt.Errorf("sqlite: insert feedback: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return nil, fmt.Errorf("sqlite: commit feedback: %w", err)
		}
		return &f, nil
	default:
		return nil, fmt.Errorf("sqlite: read feedback: %w", err)
	}
}

func (s *Store) getFeedbackByID(ctx context.Context, id string) (*store.Feedback, error) {
	f, err := scanFeedback(s.db.QueryRowContext(ctx,
		`SELECT `+feedbackColumns+` FROM feedback WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: feedback %s", store.ErrNotFound, id)
	}
	if err != nil {
		return nil, fmt.Errorf("sqlite: get feedback: %w", err)
	}
	return f, nil
}

func feedbackWhere(filter store.FeedbackFilter) ([]string, []any) {
	where := []string{"1 = 1"}
	args := make([]any, 0, 8)
	if filter.ProjectID != nil {
		where = append(where, "project_id = ?")
		args = append(args, *filter.ProjectID)
	}
	if filter.MRIID != nil {
		where = append(where, "mr_iid = ?")
		args = append(args, *filter.MRIID)
	}
	if filter.PublicationID != "" {
		where = append(where, "publication_id = ?")
		args = append(args, filter.PublicationID)
	}
	if filter.Signal != "" {
		where = append(where, "signal = ?")
		args = append(args, filter.Signal)
	}
	return where, args
}

// ListFeedback returns feedback newest first.
func (s *Store) ListFeedback(ctx context.Context, filter store.FeedbackFilter) ([]store.Feedback, string, error) {
	where, args := feedbackWhere(filter)
	limit := normalizeLimit(filter.Limit)
	if filter.Cursor != "" {
		observed, id, err := decodeCursor(filter.Cursor, store.ErrInvalidCursor)
		if err != nil {
			return nil, "", err
		}
		where = append(where, "(observed_at < ? OR (observed_at = ? AND id < ?))")
		args = append(args, observed, observed, id)
	}
	args = append(args, limit+1)
	rows, err := s.db.QueryContext(ctx, `SELECT `+feedbackColumns+` FROM feedback
		WHERE `+strings.Join(where, " AND ")+`
		ORDER BY observed_at DESC, id DESC LIMIT ?`, args...)
	if err != nil {
		return nil, "", fmt.Errorf("sqlite: list feedback: %w", err)
	}
	defer rows.Close()
	items := make([]store.Feedback, 0, limit)
	for rows.Next() {
		f, err := scanFeedback(rows)
		if err != nil {
			return nil, "", fmt.Errorf("sqlite: scan feedback: %w", err)
		}
		items = append(items, *f)
	}
	if err := rows.Err(); err != nil {
		return nil, "", fmt.Errorf("sqlite: iterate feedback: %w", err)
	}
	next := ""
	if len(items) > limit {
		items = items[:limit]
		last := items[len(items)-1]
		next = encodeCursor(last.ObservedAt, last.ID)
	}
	return items, next, nil
}

// FeedbackTotals returns counts by signal.
func (s *Store) FeedbackTotals(ctx context.Context, filter store.FeedbackFilter) (store.FeedbackTotals, error) {
	where, args := feedbackWhere(filter)
	totals := store.FeedbackTotals{BySignal: make(map[string]int64)}
	rows, err := s.db.QueryContext(ctx, `SELECT signal, COUNT(*) FROM feedback
		WHERE `+strings.Join(where, " AND ")+` GROUP BY signal ORDER BY signal`, args...)
	if err != nil {
		return totals, fmt.Errorf("sqlite: feedback totals: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			signal string
			count  int64
		)
		if err := rows.Scan(&signal, &count); err != nil {
			return totals, fmt.Errorf("sqlite: scan feedback total: %w", err)
		}
		totals.BySignal[signal] = count
		totals.Total += count
	}
	if err := rows.Err(); err != nil {
		return totals, fmt.Errorf("sqlite: iterate feedback totals: %w", err)
	}
	return totals, nil
}

// ---------------------------------------------------------------------------
// Aggregates for the M6 API

func statsConditions(filter store.StatsFilter, alias string) ([]string, []any) {
	conditions := make([]string, 0, 3)
	args := make([]any, 0, 3)
	if !filter.From.IsZero() {
		conditions = append(conditions, alias+".created_at >= ?")
		args = append(args, formatTime(filter.From.UTC()))
	}
	if !filter.To.IsZero() {
		conditions = append(conditions, alias+".created_at < ?")
		args = append(args, formatTime(filter.To.UTC()))
	}
	if filter.ProjectID != nil {
		conditions = append(conditions, alias+".project_id = ?")
		args = append(args, *filter.ProjectID)
	}
	return conditions, args
}

// Stats aggregates workflow outcomes, findings decisions, costs and per-model
// audit usage. Unknown denominators stay nil.
func (s *Store) Stats(ctx context.Context, filter store.StatsFilter) (store.Stats, error) {
	stats := store.Stats{
		ByProject: make([]store.ProjectStats, 0),
		ByModel:   make([]store.ModelStats, 0),
		Decisions: make(map[string]int64),
	}
	if !filter.From.IsZero() && !filter.To.IsZero() && !filter.From.Before(filter.To) {
		return stats, errors.New("sqlite: stats from must be before to")
	}
	conditions, args := statsConditions(filter, "w")
	where := ""
	if len(conditions) > 0 {
		where = "WHERE " + strings.Join(conditions, " AND ")
	}

	projectIndex := make(map[int64]int)
	durations := make([]float64, 0)
	rows, err := s.db.QueryContext(ctx, `SELECT
		w.project_id, w.verdict, w.started_at, w.finished_at, w.cost_micros
		FROM workflows w `+where, args...)
	if err != nil {
		return stats, fmt.Errorf("sqlite: stats workflows: %w", err)
	}
	for rows.Next() {
		var (
			projectID int64
			verdict   sql.NullString
			started   sql.NullString
			finished  sql.NullString
			cost      int64
		)
		if err := rows.Scan(&projectID, &verdict, &started, &finished, &cost); err != nil {
			rows.Close()
			return stats, fmt.Errorf("sqlite: scan stats workflow: %w", err)
		}
		stats.Runs++
		stats.CostMicros += cost
		index, ok := projectIndex[projectID]
		if !ok {
			index = len(stats.ByProject)
			projectIndex[projectID] = index
			stats.ByProject = append(stats.ByProject, store.ProjectStats{ProjectID: projectID})
		}
		stats.ByProject[index].Runs++
		stats.ByProject[index].CostMicros += cost
		if verdict.String == "BLOCKING" {
			stats.Blocking++
			stats.ByProject[index].Blocking++
		}
		if started.Valid && finished.Valid {
			from, err := parseTime(started.String)
			if err != nil {
				rows.Close()
				return stats, err
			}
			to, err := parseTime(finished.String)
			if err != nil {
				rows.Close()
				return stats, err
			}
			if !to.Before(from) {
				durations = append(durations, float64(to.Sub(from).Milliseconds()))
			}
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return stats, fmt.Errorf("sqlite: iterate stats workflows: %w", err)
	}
	rows.Close()

	if stats.Runs > 0 {
		ratio := float64(stats.Blocking) / float64(stats.Runs)
		stats.BlockingRatio = &ratio
	}
	if len(durations) > 0 {
		var sum float64
		for _, value := range durations {
			sum += value
		}
		avg := sum / float64(len(durations))
		stats.AvgDurationMS = &avg
		p95 := percentile95(durations)
		stats.P95DurationMS = &p95
	}

	decisionRows, err := s.db.QueryContext(ctx, `SELECT
		COALESCE(f.decision, ''), w.project_id, COUNT(*)
		FROM findings f JOIN workflows w ON w.id = f.workflow_id `+where+`
		GROUP BY COALESCE(f.decision, ''), w.project_id`, args...)
	if err != nil {
		return stats, fmt.Errorf("sqlite: stats findings: %w", err)
	}
	var decisionTotal int64
	for decisionRows.Next() {
		var (
			decision  string
			projectID int64
			count     int64
		)
		if err := decisionRows.Scan(&decision, &projectID, &count); err != nil {
			decisionRows.Close()
			return stats, fmt.Errorf("sqlite: scan stats finding: %w", err)
		}
		if decision == "" {
			continue
		}
		stats.Decisions[decision] += count
		decisionTotal += count
		index, ok := projectIndex[projectID]
		if !ok {
			index = len(stats.ByProject)
			projectIndex[projectID] = index
			stats.ByProject = append(stats.ByProject, store.ProjectStats{ProjectID: projectID})
		}
		if decision == "publish" || decision == "mark" {
			stats.Published += count
			stats.ByProject[index].Published += count
		}
	}
	if err := decisionRows.Err(); err != nil {
		decisionRows.Close()
		return stats, fmt.Errorf("sqlite: iterate stats findings: %w", err)
	}
	decisionRows.Close()
	if decisionTotal > 0 {
		ratio := float64(stats.Decisions["drop"]) / float64(decisionTotal)
		stats.OverturnedRatio = &ratio
	}

	modelIndex := make(map[string]int)
	modelRows, err := s.db.QueryContext(ctx, `SELECT
		c.provider, c.model, COUNT(*),
		COALESCE(SUM(CASE WHEN c.error_kind IS NULL OR c.error_kind = '' THEN 0 ELSE 1 END), 0),
		COALESCE(SUM(c.input_tokens), 0), COALESCE(SUM(c.output_tokens), 0),
		COALESCE(AVG(c.latency_ms), 0)
		FROM llm_calls c JOIN workflows w ON w.id = c.workflow_id `+where+`
		GROUP BY c.provider, c.model ORDER BY c.provider, c.model`, args...)
	if err != nil {
		return stats, fmt.Errorf("sqlite: stats llm calls: %w", err)
	}
	for modelRows.Next() {
		var (
			entry      store.ModelStats
			avgLatency float64
		)
		if err := modelRows.Scan(&entry.Provider, &entry.Model, &entry.Calls,
			&entry.Errors, &entry.InputTokens, &entry.OutputTokens, &avgLatency); err != nil {
			modelRows.Close()
			return stats, fmt.Errorf("sqlite: scan stats llm call: %w", err)
		}
		entry.AvgLatencyMS = &avgLatency
		modelIndex[entry.Provider+"\x00"+entry.Model] = len(stats.ByModel)
		stats.ByModel = append(stats.ByModel, entry)
	}
	if err := modelRows.Err(); err != nil {
		modelRows.Close()
		return stats, fmt.Errorf("sqlite: iterate stats llm calls: %w", err)
	}
	modelRows.Close()

	latencyRows, err := s.db.QueryContext(ctx, `SELECT c.provider, c.model, c.latency_ms
		FROM llm_calls c JOIN workflows w ON w.id = c.workflow_id `+where+`
		ORDER BY c.provider, c.model, c.latency_ms`, args...)
	if err != nil {
		return stats, fmt.Errorf("sqlite: stats latencies: %w", err)
	}
	latencies := make(map[string][]float64)
	for latencyRows.Next() {
		var (
			provider string
			model    string
			latency  float64
		)
		if err := latencyRows.Scan(&provider, &model, &latency); err != nil {
			latencyRows.Close()
			return stats, fmt.Errorf("sqlite: scan latency: %w", err)
		}
		key := provider + "\x00" + model
		latencies[key] = append(latencies[key], latency)
	}
	if err := latencyRows.Err(); err != nil {
		latencyRows.Close()
		return stats, fmt.Errorf("sqlite: iterate latencies: %w", err)
	}
	latencyRows.Close()
	for i := range stats.ByModel {
		entry := &stats.ByModel[i]
		if entry.Calls > 0 {
			rate := float64(entry.Errors) / float64(entry.Calls)
			entry.ErrorRate = &rate
		}
		if values := latencies[entry.Provider+"\x00"+entry.Model]; len(values) > 0 {
			p50 := percentile(values, 50)
			entry.P50LatencyMS = &p50
		}
	}

	costRows, err := s.db.QueryContext(ctx, `SELECT
		n.provider, n.model, COALESCE(SUM(n.cost_micros), 0)
		FROM workflow_nodes n JOIN workflows w ON w.id = n.workflow_id `+where+`
		AND n.provider IS NOT NULL AND n.provider <> ''
		AND n.model IS NOT NULL AND n.model <> ''
		GROUP BY n.provider, n.model`, args...)
	if err != nil {
		return stats, fmt.Errorf("sqlite: stats model costs: %w", err)
	}
	for costRows.Next() {
		var (
			provider string
			model    string
			cost     int64
		)
		if err := costRows.Scan(&provider, &model, &cost); err != nil {
			costRows.Close()
			return stats, fmt.Errorf("sqlite: scan model cost: %w", err)
		}
		key := provider + "\x00" + model
		index, ok := modelIndex[key]
		if !ok {
			index = len(stats.ByModel)
			modelIndex[key] = index
			stats.ByModel = append(stats.ByModel, store.ModelStats{Provider: provider, Model: model})
		}
		stats.ByModel[index].CostMicros += cost
	}
	if err := costRows.Err(); err != nil {
		costRows.Close()
		return stats, fmt.Errorf("sqlite: iterate model costs: %w", err)
	}
	costRows.Close()

	sortProjectStats(stats.ByProject)
	return stats, nil
}

func percentile95(values []float64) float64 {
	return percentile(values, 95)
}

func percentile(values []float64, p int) float64 {
	sorted := append([]float64(nil), values...)
	for i := 1; i < len(sorted); i++ {
		for j := i; j > 0 && sorted[j] < sorted[j-1]; j-- {
			sorted[j], sorted[j-1] = sorted[j-1], sorted[j]
		}
	}
	rank := (p*len(sorted) + 99) / 100
	if rank < 1 {
		rank = 1
	}
	return sorted[rank-1]
}

func sortProjectStats(items []store.ProjectStats) {
	for i := 1; i < len(items); i++ {
		for j := i; j > 0 && items[j].ProjectID < items[j-1].ProjectID; j-- {
			items[j], items[j-1] = items[j-1], items[j]
		}
	}
}
