package server

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/app"
	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/config"
	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/secret"
	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/store"
	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/store/sqlite"
)

func testProjectService(t *testing.T, st *sqlite.Store, api *stubGitLab, workspace string) *app.ProjectService {
	t.Helper()
	cipher, err := secret.New([]byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatalf("secret.New: %v", err)
	}
	service, err := app.NewProjectService(app.ProjectServiceConfig{
		Store:     st,
		GitLab:    api,
		Cipher:    cipher,
		Profiles:  map[string]config.ProfileConfig{"default": {}, "deep": {}},
		PublicURL: "https://review.example.com",
		Workspace: workspace,
	})
	if err != nil {
		t.Fatalf("NewProjectService: %v", err)
	}
	return service
}

func registerProject(t *testing.T, client *m6Client) {
	t.Helper()
	response := client.json(http.MethodPost, "/api/v1/projects", map[string]any{
		"project": "42",
		"token":   "glpat-project-token",
	})
	if response.status != http.StatusCreated {
		t.Fatalf("register = %d %s", response.status, response.body)
	}
}

func TestProjectWriteEndpoints(t *testing.T) {
	ctx := context.Background()
	workspace := t.TempDir()
	api := defaultStubGitLab()
	env := newM6Env(t, m6Options{adminFactory: func(st *sqlite.Store) AdminService {
		return testProjectService(t, st, api, workspace)
	}})
	client := env.loginAs("code-admin")
	registerProject(t, client)

	updated := client.json(http.MethodPatch, "/api/v1/projects/42", map[string]any{
		"profile": "deep", "settings": map[string]any{"skip_draft": true}, "enabled": false,
	})
	if updated.status != http.StatusOK {
		t.Fatalf("patch = %d %s", updated.status, updated.body)
	}
	project := env.decode(t, updated)
	if project["profile"] != "deep" || project["enabled"] != false {
		t.Fatalf("patched project = %v", project)
	}

	list := client.request(http.MethodGet, "/api/v1/projects?q=group&enabled=false", nil, nil)
	if list.status != http.StatusOK {
		t.Fatalf("filtered list = %d %s", list.status, list.body)
	}
	if projects := env.decode(t, list)["projects"].([]any); len(projects) != 1 {
		t.Fatalf("filtered projects = %v", projects)
	}
	mismatch := client.request(http.MethodGet, "/api/v1/projects?q=absent&enabled=true", nil, nil)
	if mismatch.status != http.StatusOK || len(env.decode(t, mismatch)["projects"].([]any)) != 0 {
		t.Fatalf("mismatched filter = %d %s", mismatch.status, mismatch.body)
	}
	badFilter := client.request(http.MethodGet, "/api/v1/projects?enabled=maybe", nil, nil)
	if badFilter.status != http.StatusBadRequest {
		t.Fatalf("bad filter = %d %s", badFilter.status, badFilter.body)
	}

	for _, body := range []map[string]any{
		{"profile": "missing"},
		{"bogus": true},
		{},
		{"settings": "nope"},
	} {
		response := client.json(http.MethodPatch, "/api/v1/projects/42", body)
		if response.status != http.StatusBadRequest {
			t.Fatalf("invalid body %v = %d %s", body, response.status, response.body)
		}
	}
	if response := client.json(http.MethodPatch, "/api/v1/projects/999", map[string]any{"enabled": true}); response.status != http.StatusNotFound {
		t.Fatalf("patch missing = %d %s", response.status, response.body)
	}
	if response := client.request(http.MethodGet, "/api/v1/projects/abc", nil, nil); response.status != http.StatusBadRequest {
		t.Fatalf("invalid id = %d %s", response.status, response.body)
	}

	rotated := client.json(http.MethodPut, "/api/v1/projects/42/token", map[string]any{"token": "glpat-rotated"})
	if rotated.status != http.StatusOK {
		t.Fatalf("rotate = %d %s", rotated.status, rotated.body)
	}
	saved, err := env.sqlite.GetProject(ctx, 42)
	if err != nil {
		t.Fatalf("GetProject: %v", err)
	}
	cipher, _ := secret.New([]byte("0123456789abcdef0123456789abcdef"))
	plain, err := cipher.Decrypt(saved.TokenEnc)
	if err != nil || string(plain) != "glpat-rotated" {
		t.Fatalf("rotated token = %q, %v", plain, err)
	}
	if response := client.json(http.MethodPut, "/api/v1/projects/42/token", map[string]any{"token": ""}); response.status != http.StatusBadRequest {
		t.Fatalf("empty token = %d %s", response.status, response.body)
	}
	if response := client.json(http.MethodPut, "/api/v1/projects/999/token", map[string]any{"token": "x"}); response.status != http.StatusNotFound {
		t.Fatalf("rotate missing = %d %s", response.status, response.body)
	}

	hook := client.request(http.MethodPost, "/api/v1/projects/42/webhook", nil, nil)
	if hook.status != http.StatusOK {
		t.Fatalf("webhook = %d %s", hook.status, hook.body)
	}
	if hookID := env.decode(t, hook)["hook_id"]; hookID != float64(99) {
		t.Fatalf("hook id = %v", hookID)
	}
	api.hookID = 100
	rebuilt := client.request(http.MethodPost, "/api/v1/projects/42/webhook", nil, nil)
	if rebuilt.status != http.StatusOK || env.decode(t, rebuilt)["hook_id"] != float64(100) {
		t.Fatalf("rebuilt hook = %d %s", rebuilt.status, rebuilt.body)
	}
	if api.callCount("DeleteProjectHook") != 1 {
		t.Fatalf("previous hook delete calls = %d", api.callCount("DeleteProjectHook"))
	}
	if response := client.request(http.MethodPost, "/api/v1/projects/999/webhook", nil, nil); response.status != http.StatusNotFound {
		t.Fatalf("webhook missing = %d %s", response.status, response.body)
	}

	projectID := int64(42)
	if _, err := env.sqlite.CreateMCPServer(ctx, store.MCPServer{
		Scope: "project", ProjectID: &projectID, Name: "srv",
		Config: json.RawMessage(`{"command":"x"}`),
	}); err != nil {
		t.Fatalf("seed MCP: %v", err)
	}
	if _, err := env.sqlite.UpsertBlockBinding(ctx, store.BlockBinding{
		Scope: "project", ProjectID: &projectID, Block: "code-review",
		MCPTools: []string{"tool"}, Skills: []string{"skill"},
	}); err != nil {
		t.Fatalf("seed binding: %v", err)
	}
	skill, err := env.sqlite.CreateSkill(ctx, store.Skill{
		Scope: "project", ProjectID: &projectID, Name: "dead", Description: "d",
	})
	if err != nil {
		t.Fatalf("seed skill: %v", err)
	}
	managed := filepath.Join(workspace, "skills", "project-42", skill.ID, "skill")
	if err := os.MkdirAll(managed, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := env.sqlite.SetSkillDir(ctx, skill.ID, managed); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(workspace, "42", "base"), 0o700); err != nil {
		t.Fatal(err)
	}

	removed := client.request(http.MethodDelete, "/api/v1/projects/42", nil, nil)
	if removed.status != http.StatusNoContent {
		t.Fatalf("delete = %d %s", removed.status, removed.body)
	}
	if _, err := env.sqlite.GetProject(ctx, 42); err == nil {
		t.Fatal("project row still exists")
	}
	if servers, err := env.sqlite.ListMCPServers(ctx, "project", &projectID); err != nil || len(servers) != 0 {
		t.Fatalf("project servers = %+v, %v", servers, err)
	}
	if skills, err := env.sqlite.ListSkills(ctx, "project", &projectID); err != nil || len(skills) != 0 {
		t.Fatalf("project skills = %+v, %v", skills, err)
	}
	if bindings, err := env.sqlite.ListBlockBindings(ctx, "project", &projectID); err != nil || len(bindings) != 0 {
		t.Fatalf("project bindings = %+v, %v", bindings, err)
	}
	for _, path := range []string{filepath.Join(workspace, "42"), filepath.Join(workspace, "skills", "project-42")} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("%s was not cleaned up: %v", path, err)
		}
	}
	if response := client.request(http.MethodDelete, "/api/v1/projects/42", nil, nil); response.status != http.StatusNotFound {
		t.Fatalf("second delete = %d %s", response.status, response.body)
	}
}

func TestProjectWritePermissions(t *testing.T) {
	api := defaultStubGitLab()
	env := newM6Env(t, m6Options{adminFactory: func(st *sqlite.Store) AdminService {
		return testProjectService(t, st, api, t.TempDir())
	}})
	admin := env.loginAs("code-admin")
	registerProject(t, admin)

	noCSRF := admin.withoutCSRF()
	if response := noCSRF.json(http.MethodPatch, "/api/v1/projects/42", map[string]any{"enabled": false}); response.status != http.StatusForbidden {
		t.Fatalf("session write without CSRF = %d %s", response.status, response.body)
	}

	bearer := env.bearerClient()
	if response := bearer.json(http.MethodPatch, "/api/v1/projects/42", map[string]any{"enabled": true}); response.status != http.StatusOK {
		t.Fatalf("bootstrap bearer write = %d %s", response.status, response.body)
	}

	viewer := env.loginAs("code-viewer")
	if response := viewer.request(http.MethodGet, "/api/v1/projects", nil, nil); response.status != http.StatusOK {
		t.Fatalf("viewer list = %d %s", response.status, response.body)
	}
	for _, response := range []apiResponse{
		viewer.json(http.MethodPatch, "/api/v1/projects/42", map[string]any{"enabled": true}),
		viewer.json(http.MethodPut, "/api/v1/projects/42/token", map[string]any{"token": "x"}),
		viewer.request(http.MethodPost, "/api/v1/projects/42/webhook", nil, nil),
		viewer.request(http.MethodDelete, "/api/v1/projects/42", nil, nil),
	} {
		if response.status != http.StatusForbidden {
			t.Fatalf("viewer write = %d %s", response.status, response.body)
		}
	}
	if _, err := env.sqlite.GetProject(context.Background(), 42); err != nil {
		t.Fatalf("viewer write changed the project: %v", err)
	}
}
