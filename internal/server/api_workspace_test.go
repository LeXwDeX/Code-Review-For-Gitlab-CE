package server

import (
	"context"
	"net/http"
	"testing"

	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/store"
)

func prepareProject(t *testing.T, env *m6Env, projectID int64, baseSHA string) {
	t.Helper()
	ctx := context.Background()
	if _, err := env.sqlite.RegisterProject(ctx, store.Project{
		ID:                projectID,
		PathWithNamespace: "group/repo",
		WebURL:            "https://gitlab.example.com/group/repo",
		DefaultBranch:     "main",
		TokenEnc:          []byte("token"),
		TokenScopes:       []string{"api"},
	}); err != nil {
		t.Fatalf("RegisterProject: %v", err)
	}
	if _, err := env.sqlite.SaveWorkspace(ctx, store.Workspace{
		ProjectID: projectID,
		State:     "ready",
		BaseSHA:   baseSHA,
	}); err != nil {
		t.Fatalf("SaveWorkspace: %v", err)
	}
}

func TestWorkspaceStatusAndRefresh(t *testing.T) {
	env := newM6Env(t, m6Options{})
	prepareProject(t, env, 42, "base-1")
	client := env.loginAs("code-admin")

	status := client.request(http.MethodGet, "/api/v1/projects/42/workspace", nil, nil)
	assertStatus(t, status, http.StatusOK)
	body := env.decode(t, status)
	if body["state"] != "ready" || body["base_sha"] != "base-1" {
		t.Fatalf("workspace = %v", body)
	}

	missing := client.request(http.MethodGet, "/api/v1/projects/99/workspace", nil, nil)
	assertStatus(t, missing, http.StatusNotFound)

	refresh := client.json(http.MethodPost, "/api/v1/projects/42/workspace/refresh",
		map[string]any{"regenerate_context": true, "reindex": true})
	assertStatus(t, refresh, http.StatusAccepted)
	if env.decode(t, refresh)["job_id"] != "job-refresh" {
		t.Fatalf("refresh body = %s", refresh.body)
	}
	if len(env.workspace.calls) != 1 {
		t.Fatalf("refresh calls = %v", env.workspace.calls)
	}
	call := env.workspace.calls[0]
	if call.projectID != 42 || !call.regenerateContext || !call.reindex {
		t.Fatalf("refresh call = %+v", call)
	}

	viewer := env.loginAs("code-viewer")
	denied := viewer.json(http.MethodPost, "/api/v1/projects/42/workspace/refresh", map[string]any{})
	assertStatus(t, denied, http.StatusForbidden)
}

func TestContextDocsKindValidationAndManualSave(t *testing.T) {
	env := newM6Env(t, m6Options{})
	prepareProject(t, env, 42, "base-1")
	client := env.loginAs("code-admin")

	list := client.request(http.MethodGet, "/api/v1/projects/42/context-docs", nil, nil)
	assertStatus(t, list, http.StatusOK)
	if docs := env.decode(t, list)["documents"].([]any); len(docs) != 0 {
		t.Fatalf("documents = %v", docs)
	}

	badKind := client.request(http.MethodGet, "/api/v1/projects/42/context-docs?kind=bogus", nil, nil)
	assertStatus(t, badKind, http.StatusBadRequest)

	noDoc := client.request(http.MethodGet, "/api/v1/projects/42/context-docs/current?kind=agents", nil, nil)
	assertStatus(t, noDoc, http.StatusNotFound)

	mismatch := client.json(http.MethodPut, "/api/v1/projects/42/context-docs/current?kind=agents",
		map[string]any{"kind": "claude", "content": "body"})
	assertStatus(t, mismatch, http.StatusBadRequest)

	badQueryKind := client.json(http.MethodPut, "/api/v1/projects/42/context-docs/current?kind=other",
		map[string]any{"content": "body"})
	assertStatus(t, badQueryKind, http.StatusBadRequest)

	empty := client.json(http.MethodPut, "/api/v1/projects/42/context-docs/current?kind=agents",
		map[string]any{"content": "  "})
	assertStatus(t, empty, http.StatusBadRequest)

	saved := client.json(http.MethodPut, "/api/v1/projects/42/context-docs/current?kind=agents",
		map[string]any{"kind": "agents", "content": "first body", "locked": false})
	assertStatus(t, saved, http.StatusOK)
	savedBody := env.decode(t, saved)
	firstID, _ := savedBody["id"].(string)
	if firstID == "" || savedBody["source"] != "manual" || savedBody["base_sha"] != "base-1" {
		t.Fatalf("saved doc = %v", savedBody)
	}
	if savedBody["locked"] != false {
		t.Fatalf("locked = %v", savedBody["locked"])
	}
	if savedBody["updated_by"] != float64(7) {
		t.Fatalf("updated_by = %v", savedBody["updated_by"])
	}

	// The schema keeps one row per (project, base_sha, kind), so the second
	// manual edit updates the same version in place.
	again := client.json(http.MethodPut, "/api/v1/projects/42/context-docs/current?kind=agents",
		map[string]any{"content": "second body"})
	assertStatus(t, again, http.StatusOK)
	againBody := env.decode(t, again)
	if againBody["id"] != firstID || againBody["content"] != "second body" {
		t.Fatalf("second save = %v", againBody)
	}
	listAgain := client.request(http.MethodGet, "/api/v1/projects/42/context-docs?kind=agents", nil, nil)
	assertStatus(t, listAgain, http.StatusOK)
	docs := env.decode(t, listAgain)["documents"].([]any)
	if len(docs) != 1 {
		t.Fatalf("manual save created %d versions", len(docs))
	}

	// A locked document may still be replaced through the manual path.
	lock := client.json(http.MethodPost, "/api/v1/projects/42/context-docs/current/lock?kind=agents",
		map[string]any{"locked": true})
	assertStatus(t, lock, http.StatusNoContent)
	current := client.request(http.MethodGet, "/api/v1/projects/42/context-docs/current?kind=agents", nil, nil)
	assertStatus(t, current, http.StatusOK)
	if env.decode(t, current)["locked"] != true {
		t.Fatalf("locked document = %s", current.body)
	}

	// A different kind is a separate document.
	claude := client.json(http.MethodPut, "/api/v1/projects/42/context-docs/current?kind=claude",
		map[string]any{"content": "claude body"})
	assertStatus(t, claude, http.StatusOK)
	if env.decode(t, claude)["kind"] != "claude" {
		t.Fatalf("claude doc = %s", claude.body)
	}
}

func TestContextDocLockRequiresBody(t *testing.T) {
	env := newM6Env(t, m6Options{})
	prepareProject(t, env, 42, "base-1")
	client := env.loginAs("code-admin")
	response := client.json(http.MethodPost, "/api/v1/projects/42/context-docs/current/lock?kind=agents",
		map[string]any{})
	assertStatus(t, response, http.StatusBadRequest)
}
