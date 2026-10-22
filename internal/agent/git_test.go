package agent

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func newGitRepo(t *testing.T) (*Sandbox, string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.com",
			"GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.com",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	run("init", "-q")
	run("config", "user.name", "Test")
	run("config", "user.email", "test@example.com")
	writeFile(t, filepath.Join(dir, "a.txt"), "first line\nsecond line\n")
	run("add", "a.txt")
	run("commit", "-qm", "initial commit")
	sb, err := NewSandbox(dir, "")
	if err != nil {
		t.Fatalf("NewSandbox: %v", err)
	}
	t.Cleanup(func() { sb.Close() })
	return sb, dir
}

func TestGitLogAndBlame(t *testing.T) {
	sb, _ := newGitRepo(t)
	tools := BuiltinTools(sb)

	res, err := invoke(t, toolByName(t, tools, "git_log"), `{"path":"a.txt"}`)
	if err != nil {
		t.Fatalf("git_log: %v", err)
	}
	if !strings.Contains(res.Content, "initial commit") {
		t.Fatalf("git_log content = %q", res.Content)
	}

	res, err = invoke(t, toolByName(t, tools, "git_blame"), `{"path":"a.txt","start_line":1,"end_line":2}`)
	if err != nil {
		t.Fatalf("git_blame: %v", err)
	}
	if !strings.Contains(res.Content, "first line") || !strings.Contains(res.Content, "second line") {
		t.Fatalf("git_blame content = %q", res.Content)
	}
}

func TestGitToolsRejectOptionAndPathInjection(t *testing.T) {
	sb, _ := newGitRepo(t)
	tools := BuiltinTools(sb)
	for _, path := range []string{"-p", "--output=/tmp/evil", "../../etc/passwd", ".git/config"} {
		input := `{"path":` + quoteJSON(path) + `}`
		if _, err := invoke(t, toolByName(t, tools, "git_log"), input); err == nil {
			t.Fatalf("git_log path %q succeeded", path)
		}
		if _, err := invoke(t, toolByName(t, tools, "git_blame"),
			`{"path":`+quoteJSON(path)+`,"start_line":1,"end_line":1}`); err == nil {
			t.Fatalf("git_blame path %q succeeded", path)
		}
	}
	if _, err := invoke(t, toolByName(t, tools, "git_blame"),
		`{"path":"a.txt","start_line":1,"end_line":500}`); err == nil {
		t.Fatal("oversized blame range succeeded")
	}
	if _, err := invoke(t, toolByName(t, tools, "git_blame"),
		`{"path":"a.txt","start_line":2,"end_line":1}`); err == nil {
		t.Fatal("reversed blame range succeeded")
	}
	if _, err := invoke(t, toolByName(t, tools, "git_log"), `{"max":101}`); err == nil {
		t.Fatal("git_log max=101 succeeded")
	}
}

func TestGitEnvAllowlist(t *testing.T) {
	t.Setenv("GIT_DIR", "/tmp/evil")
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("MY_TOKEN", "secret")
	env := gitEnv(t.TempDir())
	keys := make(map[string]string, len(env))
	for _, item := range env {
		key, value, ok := strings.Cut(item, "=")
		if !ok {
			t.Fatalf("malformed env entry %q", item)
		}
		keys[key] = value
	}
	for _, key := range []string{"GIT_DIR", "GIT_CONFIG_COUNT", "MY_TOKEN", "HOME"} {
		if value, ok := keys[key]; ok {
			t.Fatalf("env leaks %s=%q", key, value)
		}
	}
	if keys["PATH"] == "" {
		t.Fatal("PATH missing")
	}
	if keys["GIT_CONFIG_NOSYSTEM"] != "1" || keys["GIT_TERMINAL_PROMPT"] != "0" {
		t.Fatalf("hardening env missing: %#v", keys)
	}
	if keys["GIT_ASKPASS"] != "/bin/false" || keys["GIT_PAGER"] != "cat" {
		t.Fatalf("askpass/pager missing: %#v", keys)
	}
	if keys["GIT_CEILING_DIRECTORIES"] == "" {
		t.Fatalf("ceiling directories missing: %#v", keys)
	}
}

func TestGitToolsOutsideRepository(t *testing.T) {
	dir := t.TempDir()
	sb, err := NewSandbox(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	defer sb.Close()
	if _, err := invoke(t, toolByName(t, BuiltinTools(sb), "git_log"), `{}`); err == nil {
		t.Fatal("git_log outside repository succeeded")
	}
}

func quoteJSON(value string) string {
	encoded, _ := json.Marshal(value)
	return string(encoded)
}
