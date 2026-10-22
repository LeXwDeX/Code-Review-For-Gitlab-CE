package agent

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func newTestSandbox(t *testing.T) (*Sandbox, string, string) {
	t.Helper()
	parent := t.TempDir()
	head := filepath.Join(parent, "head")
	base := filepath.Join(parent, "base")
	outside := filepath.Join(parent, "outside")
	skills := filepath.Join(parent, "skills")

	writeFile(t, filepath.Join(head, "main.go"), "package head\n\n// TODO: root\n")
	writeFile(t, filepath.Join(head, "pkg", "a.go"), "package pkg\n\n// TODO: nested a\n")
	writeFile(t, filepath.Join(head, "pkg", "sub", "b.go"), "package sub\n\nfunc B() {}\n")
	writeFile(t, filepath.Join(head, "notes.txt"), "plain notes\nno markers\n")
	writeFile(t, filepath.Join(head, "binary.bin"), "abc\x00def\n")
	writeFile(t, filepath.Join(head, ".git", "config"), "TOPSECRET git config\n")
	writeFile(t, filepath.Join(base, "main.go"), "package base\n")
	writeFile(t, filepath.Join(outside, "secret.txt"), "TOPSECRET outside\n")
	writeFile(t, filepath.Join(skills, "security", "SKILL.md"), "security skill\n")

	if err := os.Symlink(filepath.Join(outside, "secret.txt"), filepath.Join(head, "link")); err != nil {
		t.Fatalf("symlink file: %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(head, "linkdir")); err != nil {
		t.Fatalf("symlink dir: %v", err)
	}
	if err := os.Symlink(".git", filepath.Join(head, "gitlink")); err != nil {
		t.Fatalf("symlink .git: %v", err)
	}
	if err := os.Symlink("notes.txt", filepath.Join(head, "notes-link")); err != nil {
		t.Fatalf("symlink inside: %v", err)
	}
	big := strings.Repeat("0123456789abcdef\n", (maxReadBytes/17)+100)
	writeFile(t, filepath.Join(head, "big.txt"), big)

	sb, err := NewSandbox(head, base, skills)
	if err != nil {
		t.Fatalf("NewSandbox: %v", err)
	}
	t.Cleanup(func() { sb.Close() })
	return sb, head, outside
}

func toolByName(t *testing.T, tools []Tool, name string) Tool {
	t.Helper()
	for _, tool := range tools {
		if tool.Def().Name == name {
			return tool
		}
	}
	t.Fatalf("tool %q not found", name)
	return nil
}

func invoke(t *testing.T, tool Tool, input string) (Result, error) {
	t.Helper()
	return tool.Call(context.Background(), json.RawMessage(input))
}

func TestBuiltinToolsOrder(t *testing.T) {
	sb, _, _ := newTestSandbox(t)
	tools := BuiltinTools(sb)
	names := make([]string, 0, len(tools))
	for _, tool := range tools {
		names = append(names, tool.Def().Name)
		if len(tool.Def().InputSchema) == 0 || !json.Valid(tool.Def().InputSchema) {
			t.Fatalf("%s has an invalid schema", tool.Def().Name)
		}
	}
	want := "read_file,grep,glob,list_dir,git_log,git_blame"
	if strings.Join(names, ",") != want {
		t.Fatalf("tools = %v, want %s", names, want)
	}
}

func TestReadFileLinesRootsAndRange(t *testing.T) {
	sb, _, _ := newTestSandbox(t)
	tools := BuiltinTools(sb)
	read := toolByName(t, tools, "read_file")

	res, err := invoke(t, read, `{"path":"main.go"}`)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(res.Content, "     1\tpackage head") || !strings.Contains(res.Content, "     3\t// TODO: root") {
		t.Fatalf("content = %q", res.Content)
	}

	res, err = invoke(t, read, `{"path":"main.go","root":"base"}`)
	if err != nil {
		t.Fatalf("read base: %v", err)
	}
	if strings.TrimSpace(res.Content) != "1\tpackage base" {
		t.Fatalf("base content = %q", res.Content)
	}

	res, err = invoke(t, read, `{"path":"main.go","start_line":2,"end_line":2}`)
	if err != nil {
		t.Fatalf("read range: %v", err)
	}
	if strings.TrimSpace(res.Content) != "2" {
		t.Fatalf("range content = %q", res.Content)
	}

	res, err = invoke(t, read, `{"path":"notes-link"}`)
	if err != nil {
		t.Fatalf("read inner symlink: %v", err)
	}
	if !strings.Contains(res.Content, "plain notes") {
		t.Fatalf("inner symlink content = %q", res.Content)
	}
}

func TestReadFileRejectsEscapes(t *testing.T) {
	sb, _, _ := newTestSandbox(t)
	read := toolByName(t, BuiltinTools(sb), "read_file")
	cases := []string{
		`{"path":"../outside/secret.txt"}`,
		`{"path":"/etc/passwd"}`,
		`{"path":".git/config"}`,
		`{"path":"pkg/../.git/config"}`,
		`{"path":"link"}`,
		`{"path":"linkdir/secret.txt"}`,
		`{"path":"gitlink/config"}`,
		`{"path":"gitlink"}`,
		`{"path":"main.go","root":"nope"}`,
	}
	for _, input := range cases {
		if _, err := invoke(t, read, input); err == nil {
			t.Fatalf("input %s succeeded, want error", input)
		}
	}
}

func TestBuiltinListingsHideGitAndSymlinkEscapes(t *testing.T) {
	sb, _, _ := newTestSandbox(t)
	tools := BuiltinTools(sb)

	res, err := invoke(t, toolByName(t, tools, "list_dir"), `{}`)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if strings.Contains(res.Content, ".git") {
		t.Fatalf("list contains .git: %q", res.Content)
	}
	if !strings.Contains(res.Content, "pkg/") || !strings.Contains(res.Content, "notes-link@") {
		t.Fatalf("list content = %q", res.Content)
	}
	lines := strings.Split(res.Content, "\n")
	for i := 1; i < len(lines); i++ {
		if lines[i-1] > lines[i] {
			t.Fatalf("list is not sorted: %v", lines)
		}
	}

	res, err = invoke(t, toolByName(t, tools, "glob"), `{"pattern":"**/*.txt"}`)
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if strings.Contains(res.Content, ".git") || strings.Contains(res.Content, "secret") {
		t.Fatalf("glob leaked: %q", res.Content)
	}
	if !strings.Contains(res.Content, "notes.txt") || !strings.Contains(res.Content, "big.txt") {
		t.Fatalf("glob content = %q", res.Content)
	}

	res, err = invoke(t, toolByName(t, tools, "grep"), `{"pattern":"TOPSECRET"}`)
	if err != nil {
		t.Fatalf("grep: %v", err)
	}
	if strings.Contains(res.Content, "TOPSECRET") {
		t.Fatalf("grep leaked outside content: %q", res.Content)
	}
	if res.Content != "(no matches)" {
		t.Fatalf("grep content = %q", res.Content)
	}
}

func TestGlobSupportsDoubleStarAndRootOnly(t *testing.T) {
	sb, _, _ := newTestSandbox(t)
	glob := toolByName(t, BuiltinTools(sb), "glob")

	res, err := invoke(t, glob, `{"pattern":"**/*.go"}`)
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	for _, want := range []string{"main.go", "pkg/a.go", "pkg/sub/b.go"} {
		if !strings.Contains(res.Content, want) {
			t.Fatalf("glob missing %s: %q", want, res.Content)
		}
	}

	res, err = invoke(t, glob, `{"pattern":"*.go"}`)
	if err != nil {
		t.Fatalf("glob root: %v", err)
	}
	if strings.TrimSpace(res.Content) != "main.go" {
		t.Fatalf("root-only glob = %q", res.Content)
	}

	for _, pattern := range []string{"../*.go", "/tmp/*.go", ".git/*"} {
		if _, err := invoke(t, glob, `{"pattern":"`+pattern+`"}`); err == nil {
			t.Fatalf("pattern %q succeeded, want error", pattern)
		}
	}
}

func TestGlobCharacterClasses(t *testing.T) {
	cases := []struct {
		pattern string
		name    string
		want    bool
	}{
		{"[abc].go", "b.go", true},
		{"[abc].go", "d.go", false},
		{"[abc].go", "ab.go", false},
		{"[!abc].go", "d.go", true},
		{"[!abc].go", "a.go", false},
		{"[!abc].go", "!.go", true},
		{"pkg/[ab].go", "pkg/a.go", true},
		{"pkg/[ab].go", "pkg/c.go", false},
		{"pkg/[ab].go", "x/pkg/a.go", false},
		{"[^abc].go", "^.go", true},
		{"[^abc].go", "a.go", true},
		{"[^abc].go", "z.go", false},
	}
	for _, tc := range cases {
		t.Run(tc.pattern+"_"+tc.name, func(t *testing.T) {
			expr, err := globRegexp(tc.pattern)
			if err != nil {
				t.Fatalf("globRegexp(%q): %v", tc.pattern, err)
			}
			re, err := regexp.Compile(expr)
			if err != nil {
				t.Fatalf("compile %q: %v", expr, err)
			}
			if got := re.MatchString(tc.name); got != tc.want {
				t.Fatalf("match(%q, %q) = %v, want %v (expr %q)", tc.pattern, tc.name, got, tc.want, expr)
			}
		})
	}
	for _, pattern := range []string{"[]", "[!]", "[abc"} {
		if _, err := globRegexp(pattern); err == nil {
			t.Fatalf("globRegexp(%q) succeeded, want error", pattern)
		}
	}
}

func TestGrepGlobFilterLimitAndBinary(t *testing.T) {
	sb, _, _ := newTestSandbox(t)
	grep := toolByName(t, BuiltinTools(sb), "grep")

	res, err := invoke(t, grep, `{"pattern":"TODO","glob":"**/*.go"}`)
	if err != nil {
		t.Fatalf("grep: %v", err)
	}
	if !strings.Contains(res.Content, "main.go:3: // TODO: root") ||
		!strings.Contains(res.Content, "pkg/a.go:3: // TODO: nested a") {
		t.Fatalf("grep content = %q", res.Content)
	}
	if strings.Contains(res.Content, "notes.txt") {
		t.Fatalf("grep ignored glob filter: %q", res.Content)
	}

	res, err = invoke(t, grep, `{"pattern":"TODO","max_results":1}`)
	if err != nil {
		t.Fatalf("grep limit: %v", err)
	}
	if !res.Truncated || !strings.Contains(res.Content, "[truncated: search limit reached]") {
		t.Fatalf("grep limit result = %#v", res)
	}

	res, err = invoke(t, grep, `{"pattern":"abc"}`)
	if err != nil {
		t.Fatalf("grep binary: %v", err)
	}
	if strings.Contains(res.Content, "binary.bin") {
		t.Fatalf("grep read a binary file: %q", res.Content)
	}

	if _, err := invoke(t, grep, `{"pattern":"("}`); err == nil {
		t.Fatal("invalid regexp succeeded")
	}
	if _, err := invoke(t, grep, `{"pattern":""}`); err == nil {
		t.Fatal("empty pattern succeeded")
	}
}

func TestReadFileBounds(t *testing.T) {
	sb, _, _ := newTestSandbox(t)
	read := toolByName(t, BuiltinTools(sb), "read_file")
	res, err := invoke(t, read, `{"path":"big.txt"}`)
	if err != nil {
		t.Fatalf("read big: %v", err)
	}
	if !res.Truncated || !strings.Contains(res.Content, "[truncated: read limit reached]") {
		t.Fatalf("big file result = %#v", res)
	}
	if len(res.Content) > maxReadBytes+1024 {
		t.Fatalf("big file content too large: %d", len(res.Content))
	}
	if _, err := invoke(t, read, `{"path":"main.go","start_line":-1}`); err == nil {
		t.Fatal("negative start_line succeeded")
	}
	if _, err := invoke(t, read, `{"path":"main.go","start_line":3,"end_line":2}`); err == nil {
		t.Fatal("reversed range succeeded")
	}
	if _, err := invoke(t, read, `{"path":"main.go","end_line":-1}`); err == nil {
		t.Fatal("negative end_line succeeded")
	}
	if _, err := invoke(t, read, `{"path":"main.go","unknown":1}`); err == nil {
		t.Fatal("unknown field succeeded")
	}
}

func TestSandboxRejectsBadRoots(t *testing.T) {
	parent := t.TempDir()
	file := filepath.Join(parent, "file")
	writeFile(t, file, "x")
	if _, err := NewSandbox("", ""); err == nil {
		t.Fatal("empty sandbox succeeded")
	}
	if _, err := NewSandbox(file, ""); !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("file root error = %v", err)
	}
	if _, err := NewSandbox("relative", ""); !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("relative root error = %v", err)
	}
	if _, err := NewSandbox(string(filepath.Separator), ""); !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("filesystem root error = %v", err)
	}
	link := filepath.Join(parent, "rootlink")
	if err := os.Symlink(parent, link); err != nil {
		t.Fatal(err)
	}
	if _, err := NewSandbox(link, ""); !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("symlink root error = %v", err)
	}
}

func TestSandboxCloseAndRootNames(t *testing.T) {
	sb, _, _ := newTestSandbox(t)
	names := sb.RootNames()
	if strings.Join(names, ",") != "base,head,skills" {
		t.Fatalf("root names = %v", names)
	}
	if err := sb.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if _, err := invoke(t, toolByName(t, BuiltinTools(sb), "list_dir"), `{}`); err == nil {
		t.Fatal("closed sandbox succeeded")
	}
}

// Ensure fs.SkipAll exists on the tested Go version used for directory walks.
var _ = fs.SkipAll

func TestBuiltinRejectsAbsoluteAndTrailingJSON(t *testing.T) {
	sb, _, _ := newTestSandbox(t)
	tools := BuiltinTools(sb)
	if _, err := invoke(t, toolByName(t, tools, "grep"), `{"pattern":"x","path":"/etc"}`); err == nil {
		t.Fatal("grep absolute path succeeded")
	}
	if _, err := invoke(t, toolByName(t, tools, "list_dir"), `{"path":"../outside"}`); err == nil {
		t.Fatal("list_dir escape succeeded")
	}
	if _, err := invoke(t, toolByName(t, tools, "read_file"), `{"path":"main.go"} {"path":"notes.txt"}`); err == nil {
		t.Fatal("trailing JSON accepted")
	}
	res, err := invoke(t, toolByName(t, tools, "read_file"), `{"path":"security/SKILL.md","root":"skills"}`)
	if err != nil {
		t.Fatalf("read skill root: %v", err)
	}
	if strings.TrimSpace(res.Content) != "1\tsecurity skill" {
		t.Fatalf("skill content = %q", res.Content)
	}
}

func TestGitSymlinkIsRejected(t *testing.T) {
	sb, _, _ := newTestSandbox(t)
	tools := BuiltinTools(sb)
	if _, err := invoke(t, toolByName(t, tools, "list_dir"), `{"path":"gitlink"}`); err == nil {
		t.Fatal("list_dir followed a .git symlink")
	}
	if _, err := invoke(t, toolByName(t, tools, "grep"), `{"pattern":"x","path":"gitlink/config"}`); err == nil {
		t.Fatal("grep followed a .git symlink")
	}
	if _, err := invoke(t, toolByName(t, tools, "git_log"), `{"path":"gitlink/config"}`); err == nil {
		t.Fatal("git_log followed a .git symlink")
	}
	if _, err := invoke(t, toolByName(t, tools, "git_blame"),
		`{"path":"gitlink/config","start_line":1,"end_line":1}`); err == nil {
		t.Fatal("git_blame followed a .git symlink")
	}
	res, err := invoke(t, toolByName(t, tools, "glob"), `{"pattern":"**/*"}`)
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if strings.Contains(res.Content, "gitlink/config") {
		t.Fatalf("glob listed .git internals: %q", res.Content)
	}
}
