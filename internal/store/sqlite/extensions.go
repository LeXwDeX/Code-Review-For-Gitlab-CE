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

// ---------------------------------------------------------------------------
// MCP servers

const mcpServerColumns = `id, scope, project_id, name, config, enabled, created_at, updated_at`

func scanMCPServer(row rowScanner) (*store.MCPServer, error) {
	var (
		server    store.MCPServer
		projectID sql.NullInt64
		createdAt string
		updatedAt string
	)
	if err := row.Scan(&server.ID, &server.Scope, &projectID, &server.Name,
		&server.Config, &server.Enabled, &createdAt, &updatedAt); err != nil {
		return nil, err
	}
	if projectID.Valid {
		value := projectID.Int64
		server.ProjectID = &value
	}
	var err error
	if server.CreatedAt, err = parseTime(createdAt); err != nil {
		return nil, err
	}
	if server.UpdatedAt, err = parseTime(updatedAt); err != nil {
		return nil, err
	}
	return &server, nil
}

// ListMCPServers returns servers matching the scope selector. An empty scope
// lists every scope; a nil project ID with project scope is rejected.
func (s *Store) ListMCPServers(ctx context.Context, scope string, projectID *int64) ([]store.MCPServer, error) {
	scope = strings.TrimSpace(scope)
	if err := store.ValidateExtensionScope(scope, projectID, false); err != nil {
		return nil, err
	}
	where := []string{"1 = 1"}
	args := make([]any, 0, 2)
	switch scope {
	case store.ScopePlatform:
		where = append(where, "scope = ?", "project_id IS NULL")
		args = append(args, store.ScopePlatform)
	case store.ScopeProject:
		where = append(where, "scope = ?", "project_id = ?")
		args = append(args, store.ScopeProject, *projectID)
	default:
		if projectID != nil {
			where = append(where, "project_id = ?")
			args = append(args, *projectID)
		}
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+mcpServerColumns+` FROM mcp_servers
		WHERE `+strings.Join(where, " AND ")+` ORDER BY created_at DESC, id DESC`, args...)
	if err != nil {
		return nil, fmt.Errorf("sqlite: list mcp servers: %w", err)
	}
	defer rows.Close()
	servers := make([]store.MCPServer, 0)
	for rows.Next() {
		server, err := scanMCPServer(rows)
		if err != nil {
			return nil, fmt.Errorf("sqlite: scan mcp server: %w", err)
		}
		servers = append(servers, *server)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite: iterate mcp servers: %w", err)
	}
	return servers, nil
}

// GetMCPServer returns one server by ID.
func (s *Store) GetMCPServer(ctx context.Context, id string) (*store.MCPServer, error) {
	if strings.TrimSpace(id) == "" {
		return nil, errors.New("sqlite: mcp server id is required")
	}
	server, err := scanMCPServer(s.db.QueryRowContext(ctx,
		`SELECT `+mcpServerColumns+` FROM mcp_servers WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: mcp server %s", store.ErrNotFound, id)
	}
	if err != nil {
		return nil, fmt.Errorf("sqlite: get mcp server: %w", err)
	}
	return server, nil
}

// CreateMCPServer stores one server. The unique name is enforced per
// (scope, project); platform rows have no project row so uniqueness is
// checked inside the transaction instead of relying on the SQL unique index.
func (s *Store) CreateMCPServer(ctx context.Context, server store.MCPServer) (*store.MCPServer, error) {
	server.Scope = strings.TrimSpace(server.Scope)
	server.Name = strings.TrimSpace(server.Name)
	if err := store.ValidateExtensionScope(server.Scope, server.ProjectID, true); err != nil {
		return nil, err
	}
	if server.Name == "" {
		return nil, errors.New("sqlite: mcp server name is required")
	}
	if !validJSONObject(server.Config) {
		return nil, errors.New("sqlite: mcp server config must be a JSON object")
	}
	id, err := newID()
	if err != nil {
		return nil, err
	}
	server.ID = id
	now := s.now().UTC()
	server.CreatedAt = now
	server.UpdatedAt = now

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("sqlite: begin create mcp server: %w", err)
	}
	defer tx.Rollback()
	if err := requireMCPNameFree(ctx, tx, server.Scope, server.ProjectID, server.Name, ""); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO mcp_servers
		(id, scope, project_id, name, config, enabled, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		server.ID, server.Scope, nullableProjectID(server.ProjectID), server.Name,
		[]byte(server.Config), server.Enabled, formatTime(server.CreatedAt), formatTime(server.UpdatedAt)); err != nil {
		if isConstraintErr(err) {
			return nil, fmt.Errorf("%w: mcp server %s", store.ErrConflict, server.Name)
		}
		return nil, fmt.Errorf("sqlite: insert mcp server: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("sqlite: commit mcp server: %w", err)
	}
	return &server, nil
}

// UpdateMCPServer replaces only the provided fields and returns the stored
// row.
func (s *Store) UpdateMCPServer(ctx context.Context, id string, update store.MCPServerUpdate) (*store.MCPServer, error) {
	if strings.TrimSpace(id) == "" {
		return nil, errors.New("sqlite: mcp server id is required")
	}
	if update.Config != nil && !validJSONObject(update.Config) {
		return nil, errors.New("sqlite: mcp server config must be a JSON object")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("sqlite: begin update mcp server: %w", err)
	}
	defer tx.Rollback()
	current, err := scanMCPServer(tx.QueryRowContext(ctx,
		`SELECT `+mcpServerColumns+` FROM mcp_servers WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: mcp server %s", store.ErrNotFound, id)
	}
	if err != nil {
		return nil, fmt.Errorf("sqlite: read mcp server: %w", err)
	}
	if update.Name != nil {
		name := strings.TrimSpace(*update.Name)
		if name == "" {
			return nil, errors.New("sqlite: mcp server name is required")
		}
		if name != current.Name {
			if err := requireMCPNameFree(ctx, tx, current.Scope, current.ProjectID, name, id); err != nil {
				return nil, err
			}
		}
		current.Name = name
	}
	if update.Config != nil {
		current.Config = append(json.RawMessage(nil), update.Config...)
	}
	if update.Enabled != nil {
		current.Enabled = *update.Enabled
	}
	current.UpdatedAt = s.now().UTC()
	if _, err := tx.ExecContext(ctx, `UPDATE mcp_servers
		SET name = ?, config = ?, enabled = ?, updated_at = ? WHERE id = ?`,
		current.Name, []byte(current.Config), current.Enabled,
		formatTime(current.UpdatedAt), id); err != nil {
		if isConstraintErr(err) {
			return nil, fmt.Errorf("%w: mcp server %s", store.ErrConflict, current.Name)
		}
		return nil, fmt.Errorf("sqlite: update mcp server: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("sqlite: commit mcp server update: %w", err)
	}
	return current, nil
}

// DeleteMCPServer removes one server.
func (s *Store) DeleteMCPServer(ctx context.Context, id string) error {
	if strings.TrimSpace(id) == "" {
		return errors.New("sqlite: mcp server id is required")
	}
	res, err := s.db.ExecContext(ctx, `DELETE FROM mcp_servers WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("sqlite: delete mcp server: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("sqlite: delete mcp server: %w", err)
	}
	if n != 1 {
		return fmt.Errorf("%w: mcp server %s", store.ErrNotFound, id)
	}
	return nil
}

func requireMCPNameFree(ctx context.Context, tx *sql.Tx, scope string, projectID *int64, name, exceptID string) error {
	var found int
	err := tx.QueryRowContext(ctx, `SELECT 1 FROM mcp_servers
		WHERE scope = ? AND project_id IS ? AND name = ? AND id <> ?`,
		scope, nullableProjectID(projectID), name, exceptID).Scan(&found)
	if err == nil {
		return fmt.Errorf("%w: mcp server %s", store.ErrConflict, name)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("sqlite: check mcp server name: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Skills

const skillColumns = `id, scope, project_id, name, description, dir, enabled, created_at`

func scanSkill(row rowScanner) (*store.Skill, error) {
	var (
		skill     store.Skill
		projectID sql.NullInt64
		createdAt string
	)
	if err := row.Scan(&skill.ID, &skill.Scope, &projectID, &skill.Name,
		&skill.Description, &skill.Dir, &skill.Enabled, &createdAt); err != nil {
		return nil, err
	}
	if projectID.Valid {
		value := projectID.Int64
		skill.ProjectID = &value
	}
	var err error
	if skill.CreatedAt, err = parseTime(createdAt); err != nil {
		return nil, err
	}
	return &skill, nil
}

// ListSkills returns skills matching the scope selector.
func (s *Store) ListSkills(ctx context.Context, scope string, projectID *int64) ([]store.Skill, error) {
	scope = strings.TrimSpace(scope)
	if err := store.ValidateExtensionScope(scope, projectID, false); err != nil {
		return nil, err
	}
	where := []string{"1 = 1"}
	args := make([]any, 0, 2)
	switch scope {
	case store.ScopePlatform:
		where = append(where, "scope = ?", "project_id IS NULL")
		args = append(args, store.ScopePlatform)
	case store.ScopeProject:
		where = append(where, "scope = ?", "project_id = ?")
		args = append(args, store.ScopeProject, *projectID)
	default:
		if projectID != nil {
			where = append(where, "project_id = ?")
			args = append(args, *projectID)
		}
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+skillColumns+` FROM skills
		WHERE `+strings.Join(where, " AND ")+` ORDER BY created_at DESC, id DESC`, args...)
	if err != nil {
		return nil, fmt.Errorf("sqlite: list skills: %w", err)
	}
	defer rows.Close()
	skills := make([]store.Skill, 0)
	for rows.Next() {
		skill, err := scanSkill(rows)
		if err != nil {
			return nil, fmt.Errorf("sqlite: scan skill: %w", err)
		}
		skills = append(skills, *skill)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite: iterate skills: %w", err)
	}
	return skills, nil
}

// CreateSkill stores one skill row. Uploaded archives keep Dir empty.
func (s *Store) CreateSkill(ctx context.Context, skill store.Skill) (*store.Skill, error) {
	skill.Scope = strings.TrimSpace(skill.Scope)
	skill.Name = strings.TrimSpace(skill.Name)
	skill.Description = strings.TrimSpace(skill.Description)
	if err := store.ValidateExtensionScope(skill.Scope, skill.ProjectID, true); err != nil {
		return nil, err
	}
	if skill.Name == "" {
		return nil, errors.New("sqlite: skill name is required")
	}
	if skill.Description == "" {
		return nil, errors.New("sqlite: skill description is required")
	}
	id, err := newID()
	if err != nil {
		return nil, err
	}
	skill.ID = id
	skill.CreatedAt = s.now().UTC()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("sqlite: begin create skill: %w", err)
	}
	defer tx.Rollback()
	var found int
	err = tx.QueryRowContext(ctx, `SELECT 1 FROM skills
		WHERE scope = ? AND project_id IS ? AND name = ?`,
		skill.Scope, nullableProjectID(skill.ProjectID), skill.Name).Scan(&found)
	if err == nil {
		return nil, fmt.Errorf("%w: skill %s", store.ErrConflict, skill.Name)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("sqlite: check skill name: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO skills
		(id, scope, project_id, name, description, dir, enabled, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		skill.ID, skill.Scope, nullableProjectID(skill.ProjectID), skill.Name,
		skill.Description, skill.Dir, skill.Enabled, formatTime(skill.CreatedAt)); err != nil {
		if isConstraintErr(err) {
			return nil, fmt.Errorf("%w: skill %s", store.ErrConflict, skill.Name)
		}
		return nil, fmt.Errorf("sqlite: insert skill: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("sqlite: commit skill: %w", err)
	}
	return &skill, nil
}

// DeleteSkill removes one skill row.
func (s *Store) DeleteSkill(ctx context.Context, id string) error {
	if strings.TrimSpace(id) == "" {
		return errors.New("sqlite: skill id is required")
	}
	res, err := s.db.ExecContext(ctx, `DELETE FROM skills WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("sqlite: delete skill: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("sqlite: delete skill: %w", err)
	}
	if n != 1 {
		return fmt.Errorf("%w: skill %s", store.ErrNotFound, id)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Block bindings

const blockBindingColumns = `scope, project_id, block, mcp_tools, skills`

func scanBlockBinding(row rowScanner) (*store.BlockBinding, error) {
	var (
		binding   store.BlockBinding
		projectID int64
		tools     string
		skills    string
	)
	if err := row.Scan(&binding.Scope, &projectID, &binding.Block, &tools, &skills); err != nil {
		return nil, err
	}
	if projectID != 0 {
		value := projectID
		binding.ProjectID = &value
	}
	if err := json.Unmarshal([]byte(tools), &binding.MCPTools); err != nil {
		return nil, fmt.Errorf("sqlite: decode mcp tools: %w", err)
	}
	if err := json.Unmarshal([]byte(skills), &binding.Skills); err != nil {
		return nil, fmt.Errorf("sqlite: decode skills: %w", err)
	}
	if binding.MCPTools == nil {
		binding.MCPTools = []string{}
	}
	if binding.Skills == nil {
		binding.Skills = []string{}
	}
	return &binding, nil
}

// ListBlockBindings returns bindings matching the scope selector.
func (s *Store) ListBlockBindings(ctx context.Context, scope string, projectID *int64) ([]store.BlockBinding, error) {
	scope = strings.TrimSpace(scope)
	if err := store.ValidateExtensionScope(scope, projectID, false); err != nil {
		return nil, err
	}
	where := []string{"1 = 1"}
	args := make([]any, 0, 2)
	switch scope {
	case store.ScopePlatform:
		where = append(where, "scope = ?", "project_id = 0")
		args = append(args, store.ScopePlatform)
	case store.ScopeProject:
		where = append(where, "scope = ?", "project_id = ?")
		args = append(args, store.ScopeProject, *projectID)
	default:
		if projectID != nil {
			where = append(where, "project_id = ?")
			args = append(args, *projectID)
		}
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+blockBindingColumns+` FROM block_bindings
		WHERE `+strings.Join(where, " AND ")+` ORDER BY scope, project_id, block`, args...)
	if err != nil {
		return nil, fmt.Errorf("sqlite: list block bindings: %w", err)
	}
	defer rows.Close()
	bindings := make([]store.BlockBinding, 0)
	for rows.Next() {
		binding, err := scanBlockBinding(rows)
		if err != nil {
			return nil, fmt.Errorf("sqlite: scan block binding: %w", err)
		}
		bindings = append(bindings, *binding)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite: iterate block bindings: %w", err)
	}
	return bindings, nil
}

// UpsertBlockBinding creates or replaces one block binding.
func (s *Store) UpsertBlockBinding(ctx context.Context, binding store.BlockBinding) (*store.BlockBinding, error) {
	binding.Scope = strings.TrimSpace(binding.Scope)
	binding.Block = strings.TrimSpace(binding.Block)
	if err := store.ValidateExtensionScope(binding.Scope, binding.ProjectID, true); err != nil {
		return nil, err
	}
	if binding.Block == "" {
		return nil, errors.New("sqlite: block is required")
	}
	if binding.MCPTools == nil {
		binding.MCPTools = []string{}
	}
	if binding.Skills == nil {
		binding.Skills = []string{}
	}
	tools, err := json.Marshal(binding.MCPTools)
	if err != nil {
		return nil, fmt.Errorf("sqlite: encode mcp tools: %w", err)
	}
	skills, err := json.Marshal(binding.Skills)
	if err != nil {
		return nil, fmt.Errorf("sqlite: encode skills: %w", err)
	}
	projectValue := int64(0)
	if binding.ProjectID != nil {
		projectValue = *binding.ProjectID
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO block_bindings
		(scope, project_id, block, mcp_tools, skills) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (scope, project_id, block) DO UPDATE SET
			mcp_tools = excluded.mcp_tools,
			skills = excluded.skills`,
		binding.Scope, projectValue, binding.Block, string(tools), string(skills)); err != nil {
		if isConstraintErr(err) {
			return nil, fmt.Errorf("%w: block binding %s", store.ErrConflict, binding.Block)
		}
		return nil, fmt.Errorf("sqlite: upsert block binding: %w", err)
	}
	return &binding, nil
}

func nullableProjectID(projectID *int64) any {
	if projectID == nil {
		return nil
	}
	return *projectID
}

func validJSONObject(raw json.RawMessage) bool {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || !json.Valid([]byte(trimmed)) {
		return false
	}
	var object map[string]json.RawMessage
	return json.Unmarshal([]byte(trimmed), &object) == nil && object != nil
}
