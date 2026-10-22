package mcp

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBuildEnvRestrictsInheritance(t *testing.T) {
	workspace := t.TempDir()
	t.Setenv("MCP_TEST_PLAIN", "plain-value")
	t.Setenv("MCP_TEST_PARENT_SECRET", "parent-secret-value")
	t.Setenv("GITLAB_TOKEN", "gitlab-secret-value")
	t.Setenv("OPENAI_API_KEY", "openai-secret-value")
	t.Setenv("LD_PRELOAD", "/tmp/evil.so")
	t.Setenv("MCP_PROJECT_TOKEN", "project-token-value")

	cfg := ServerConfig{
		Command: "helper",
		Env:     map[string]string{"MCP_EXPLICIT": "explicit-value"},
		XReview: XReviewConfig{
			EnvAllowlist: []string{
				"MCP_TEST_PLAIN",
				"MCP_TEST_PARENT_SECRET",
				"GITLAB_TOKEN",
				"OPENAI_API_KEY",
				"LD_PRELOAD",
				"MCP_PROJECT_TOKEN",
				"HOME",
			},
		},
	}
	policy := newEnvPolicy(nil, []string{"MCP_PROJECT_TOKEN"})
	env, home, err := policy.build(cfg, workspace, "local")
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	values := envMap(env)
	if values["MCP_TEST_PLAIN"] != "plain-value" {
		t.Fatalf("plain allowlisted value = %q", values["MCP_TEST_PLAIN"])
	}
	if values["MCP_EXPLICIT"] != "explicit-value" {
		t.Fatalf("explicit value = %q", values["MCP_EXPLICIT"])
	}
	for _, name := range []string{"MCP_TEST_PARENT_SECRET", "GITLAB_TOKEN", "OPENAI_API_KEY", "LD_PRELOAD", "MCP_PROJECT_TOKEN"} {
		if _, ok := values[name]; ok {
			t.Fatalf("restricted variable %s was inherited", name)
		}
	}
	wantHome := filepath.Join(workspace, ".mcp", "local", "home")
	if home != wantHome || values["HOME"] != wantHome {
		t.Fatalf("home = %q env HOME = %q, want %q", home, values["HOME"], wantHome)
	}
	if strings.Contains(strings.Join(env, "\n"), "parent-secret-value") ||
		strings.Contains(strings.Join(env, "\n"), "gitlab-secret-value") ||
		strings.Contains(strings.Join(env, "\n"), "openai-secret-value") ||
		strings.Contains(strings.Join(env, "\n"), "project-token-value") {
		t.Fatal("child environment contains a restricted secret value")
	}
	info, err := os.Stat(home)
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 {
		t.Fatalf("home directory: %v %v", info, err)
	}
}

func TestBuildEnvRejectsUnsafeExplicitValues(t *testing.T) {
	workspace := t.TempDir()
	cases := []struct {
		name string
		env  map[string]string
		deny []string
	}{
		{name: "home override", env: map[string]string{"HOME": "/tmp"}},
		{name: "loader", env: map[string]string{"LD_PRELOAD": "/tmp/evil.so"}},
		{name: "git config", env: map[string]string{"GIT_CONFIG_COUNT": "1"}},
		{name: "project token", env: map[string]string{"GITLAB_TOKEN": "secret"}, deny: []string{"GITLAB_TOKEN"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			policy := newEnvPolicy(nil, tc.deny)
			_, _, err := policy.build(ServerConfig{Command: "helper", Env: tc.env}, workspace, "local")
			if !errors.Is(err, ErrUnsafeEnv) {
				t.Fatalf("error = %v, want ErrUnsafeEnv", err)
			}
		})
	}
}

func TestHomeMustStayInsideWorkspace(t *testing.T) {
	workspace := t.TempDir()
	outside := t.TempDir()
	cfg := ServerConfig{
		Command: "helper",
		XReview: XReviewConfig{Home: filepath.Join(outside, "home")},
	}
	if _, _, err := newEnvPolicy(nil, nil).build(cfg, workspace, "local"); !errors.Is(err, ErrUnsafeWorkspace) {
		t.Fatalf("outside home error = %v", err)
	}

	cfg.XReview.Home = "nested/home"
	if _, home, err := newEnvPolicy(nil, nil).build(cfg, workspace, "local"); err != nil {
		t.Fatalf("relative home: %v", err)
	} else if home != filepath.Join(workspace, "nested", "home") {
		t.Fatalf("relative home = %q", home)
	}

	link := filepath.Join(workspace, "link")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	cfg.XReview.Home = "link/home"
	if _, _, err := newEnvPolicy(nil, nil).build(cfg, workspace, "local"); !errors.Is(err, ErrUnsafeWorkspace) {
		t.Fatalf("symlinked home error = %v", err)
	}

	cfg.XReview.Home = ".git/home"
	if _, _, err := newEnvPolicy(nil, nil).build(cfg, workspace, "local"); !errors.Is(err, ErrUnsafeWorkspace) {
		t.Fatalf("git home error = %v", err)
	}
}

func TestWorkspaceMustBeRealDirectory(t *testing.T) {
	parent := t.TempDir()
	workspace := filepath.Join(parent, "workspace")
	if _, err := prepareWorkspace(workspace); err != nil {
		t.Fatalf("create workspace: %v", err)
	}
	if _, err := prepareWorkspace(filepath.Join(parent, "missing", "nested")); err != nil {
		t.Fatalf("nested workspace: %v", err)
	}
	link := filepath.Join(parent, "link")
	if err := os.Symlink(workspace, link); err != nil {
		t.Fatal(err)
	}
	if _, err := prepareWorkspace(link); !errors.Is(err, ErrUnsafeWorkspace) {
		t.Fatalf("symlink workspace error = %v", err)
	}
}

func envMap(env []string) map[string]string {
	out := make(map[string]string, len(env))
	for _, entry := range env {
		key, value, ok := strings.Cut(entry, "=")
		if ok {
			out[key] = value
		}
	}
	return out
}
