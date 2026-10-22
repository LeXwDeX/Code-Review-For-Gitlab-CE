package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/store"
)

// ListProjectsFiltered returns projects matching the admin list selector.
// Query matches the path or web URL case-insensitively.
func (s *Store) ListProjectsFiltered(ctx context.Context, filter store.ProjectFilter) ([]store.Project, error) {
	where := []string{"1 = 1"}
	args := make([]any, 0, 3)
	if query := strings.TrimSpace(filter.Query); query != "" {
		where = append(where,
			"(instr(lower(path_with_namespace), lower(?)) > 0 OR instr(lower(web_url), lower(?)) > 0)")
		args = append(args, query, query)
	}
	if filter.Enabled != nil {
		where = append(where, "enabled = ?")
		if *filter.Enabled {
			args = append(args, 1)
		} else {
			args = append(args, 0)
		}
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+projectColumns+` FROM projects WHERE `+strings.Join(where, " AND ")+` ORDER BY id`, args...)
	if err != nil {
		return nil, fmt.Errorf("sqlite: list projects: %w", err)
	}
	defer rows.Close()
	projects := make([]store.Project, 0)
	for rows.Next() {
		project, err := scanProject(rows)
		if err != nil {
			return nil, fmt.Errorf("sqlite: scan project: %w", err)
		}
		projects = append(projects, *project)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite: list projects: %w", err)
	}
	return projects, nil
}

// UpdateProject replaces the provided profile, settings and enabled fields.
func (s *Store) UpdateProject(ctx context.Context, id int64, update store.ProjectUpdate) (*store.Project, error) {
	if id <= 0 {
		return nil, errors.New("sqlite: project id is required")
	}
	sets := make([]string, 0, 4)
	args := make([]any, 0, 5)
	if update.Profile != nil {
		if strings.TrimSpace(*update.Profile) == "" {
			return nil, errors.New("sqlite: project profile is required")
		}
		sets = append(sets, "profile = ?")
		args = append(args, *update.Profile)
	}
	if len(update.Settings) > 0 {
		if !json.Valid(update.Settings) {
			return nil, errors.New("sqlite: project settings is not valid JSON")
		}
		sets = append(sets, "settings = ?")
		args = append(args, string(update.Settings))
	}
	if update.Enabled != nil {
		sets = append(sets, "enabled = ?")
		if *update.Enabled {
			args = append(args, 1)
		} else {
			args = append(args, 0)
		}
	}
	if len(sets) == 0 {
		return nil, errors.New("sqlite: project update has no fields")
	}
	sets = append(sets, "updated_at = ?")
	args = append(args, formatTime(s.now().UTC()), id)
	res, err := s.db.ExecContext(ctx,
		`UPDATE projects SET `+strings.Join(sets, ", ")+` WHERE id = ?`, args...)
	if err != nil {
		return nil, fmt.Errorf("sqlite: update project: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return nil, fmt.Errorf("sqlite: update project: %w", err)
	}
	if n != 1 {
		return nil, fmt.Errorf("%w: project %d", store.ErrNotFound, id)
	}
	return s.GetProject(ctx, id)
}

// UpdateProjectToken stores a rotated encrypted access token.
func (s *Store) UpdateProjectToken(ctx context.Context, id int64, update store.ProjectTokenUpdate) error {
	if id <= 0 {
		return errors.New("sqlite: project id is required")
	}
	if len(update.TokenEnc) == 0 {
		return errors.New("sqlite: encrypted token is required")
	}
	scopes := update.Scopes
	if scopes == nil {
		scopes = []string{}
	}
	scopesJSON, err := json.Marshal(scopes)
	if err != nil {
		return fmt.Errorf("sqlite: encode token scopes: %w", err)
	}
	res, err := s.db.ExecContext(ctx, `UPDATE projects
		SET token_enc = ?, token_expires_at = ?, token_scopes = ?, updated_at = ?
		WHERE id = ?`,
		update.TokenEnc, nullableTime(update.ExpiresAt), string(scopesJSON),
		formatTime(s.now().UTC()), id)
	if err != nil {
		return fmt.Errorf("sqlite: update project token: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("sqlite: update project token: %w", err)
	}
	if n != 1 {
		return fmt.Errorf("%w: project %d", store.ErrNotFound, id)
	}
	return nil
}

// UpdateProjectHook stores the service-created hook ID and its secret.
func (s *Store) UpdateProjectHook(ctx context.Context, id int64, hookID *int64, secretEnc []byte) error {
	if id <= 0 {
		return errors.New("sqlite: project id is required")
	}
	if len(secretEnc) == 0 {
		return errors.New("sqlite: encrypted webhook secret is required")
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE projects SET hook_id = ?, webhook_secret_enc = ?, updated_at = ? WHERE id = ?`,
		nullableInt(hookID), secretEnc, formatTime(s.now().UTC()), id)
	if err != nil {
		return fmt.Errorf("sqlite: update project hook: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("sqlite: update project hook: %w", err)
	}
	if n != 1 {
		return fmt.Errorf("%w: project %d", store.ErrNotFound, id)
	}
	return nil
}

// DeleteProject removes one project and the rows outside database cascades.
// Foreign keys cover workspaces, context docs, workflows, nodes and findings;
// the statements below cover block bindings, deliveries, review state and
// observability rows that have no foreign key.
func (s *Store) DeleteProject(ctx context.Context, id int64) error {
	if id <= 0 {
		return errors.New("sqlite: project id is required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sqlite: begin delete project: %w", err)
	}
	defer tx.Rollback()

	var found int
	if err := tx.QueryRowContext(ctx, `SELECT 1 FROM projects WHERE id = ?`, id).Scan(&found); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: project %d", store.ErrNotFound, id)
		}
		return fmt.Errorf("sqlite: find project: %w", err)
	}

	const workflowScope = `SELECT id FROM workflows WHERE project_id = ?`
	statements := []struct {
		query string
		args  []any
	}{
		{`DELETE FROM feedback WHERE project_id = ?`, []any{id}},
		{`DELETE FROM publications WHERE project_id = ?`, []any{id}},
		{`DELETE FROM webhook_deliveries WHERE project_id = ?`, []any{id}},
		{`DELETE FROM block_bindings WHERE project_id = ?`, []any{id}},
		{`DELETE FROM review_controls WHERE project_id = ?`, []any{id}},
		{`DELETE FROM llm_calls WHERE workflow_id IN (` + workflowScope + `)`, []any{id}},
		{`DELETE FROM events WHERE workflow_id IN (` + workflowScope + `)`, []any{id}},
		{`DELETE FROM workflow_dossiers WHERE workflow_id IN (` + workflowScope + `)`, []any{id}},
		{`DELETE FROM jobs WHERE "key" IN (
			SELECT 'review.publish:' || id FROM workflows WHERE project_id = ?
		)`, []any{id}},
		{`DELETE FROM jobs
		  WHERE "key" = ? OR "key" LIKE ? OR "key" LIKE ? OR "key" LIKE ?
		     OR "key" = ? OR "key" LIKE ?`,
			[]any{fmt.Sprintf("workspace.onboard:%d", id),
				fmt.Sprintf("review:%d:%%", id), fmt.Sprintf("review.cancel:%d:%%", id),
				fmt.Sprintf("review.command:%d:%%", id),
				fmt.Sprintf("ws:%d", id), fmt.Sprintf("ws:%d:%%", id)}},
	}
	for _, statement := range statements {
		if _, err := tx.ExecContext(ctx, statement.query, statement.args...); err != nil {
			return fmt.Errorf("sqlite: delete project data: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM projects WHERE id = ?`, id); err != nil {
		return fmt.Errorf("sqlite: delete project: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("sqlite: commit delete project: %w", err)
	}
	return nil
}
