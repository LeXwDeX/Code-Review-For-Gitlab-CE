package skill

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadSkillToolBinding(t *testing.T) {
	platform := t.TempDir()
	writeSkillFile(t, platform, "security", "security", "security rules", "security body\n")
	writeSkillFile(t, platform, "coding", "coding", "coding rules", "coding body\n")
	l, err := New(Options{PlatformDirs: []string{platform}})
	if err != nil {
		t.Fatal(err)
	}
	binding, err := l.Bind([]string{"security"})
	if err != nil {
		t.Fatal(err)
	}
	tool := binding.Tool()
	if tool == nil {
		t.Fatal("Tool returned nil")
	}
	def := tool.Def()
	if def.Name != "load_skill" {
		t.Fatalf("tool name = %q", def.Name)
	}
	if !strings.Contains(string(def.InputSchema), "security") || strings.Contains(string(def.InputSchema), "coding") {
		t.Fatalf("schema = %s", def.InputSchema)
	}

	res, err := tool.Call(context.Background(), json.RawMessage(`{"name":"security"}`))
	if err != nil {
		t.Fatalf("load security: %v", err)
	}
	if !strings.Contains(res.Content, "security body") {
		t.Fatalf("content = %q", res.Content)
	}
	if _, err := tool.Call(context.Background(), json.RawMessage(`{"name":"coding"}`)); !errors.Is(err, ErrUnknownSkill) {
		t.Fatalf("unbound skill error = %v", err)
	}
	if _, err := tool.Call(context.Background(), json.RawMessage(`{"name":"security","extra":1}`)); err == nil {
		t.Fatal("unknown input field accepted")
	}
	if _, err := tool.Call(context.Background(), json.RawMessage(`[]`)); err == nil {
		t.Fatal("array input accepted")
	}
	if _, err := tool.Call(context.Background(), json.RawMessage(`null`)); err == nil {
		t.Fatal("null input accepted")
	}
	if _, err := tool.Call(context.Background(), json.RawMessage(`{"name":""}`)); err == nil {
		t.Fatal("empty name accepted")
	}
	oversized := `{"name":"security","x":"` + strings.Repeat("x", maxSkillToolInput) + `"}`
	if _, err := tool.Call(context.Background(), json.RawMessage(oversized)); err == nil {
		t.Fatal("oversized input accepted")
	}
}

func TestLoadSkillResourcesAndNoExecution(t *testing.T) {
	platform := t.TempDir()
	skillDir := writeSkillFile(t, platform, "security", "security", "security rules", "body\n")
	writeAttachment(t, skillDir, "references/guide.md", "guide\n")
	marker := filepath.Join(t.TempDir(), "executed-marker")
	writeAttachment(t, skillDir, "scripts/evil.sh", "#!/bin/sh\ntouch "+marker+"\n")
	writeAttachment(t, skillDir, ".git/config", "secret\n")
	outside := t.TempDir()
	outsideFile := filepath.Join(outside, "outside.txt")
	if err := os.WriteFile(outsideFile, []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outsideFile, filepath.Join(skillDir, "escape")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(skillDir, "escape-dir")); err != nil {
		t.Fatal(err)
	}

	l, err := New(Options{PlatformDirs: []string{platform}})
	if err != nil {
		t.Fatal(err)
	}
	binding, err := l.Bind([]string{"security"})
	if err != nil {
		t.Fatal(err)
	}
	res, err := binding.Tool().Call(context.Background(), json.RawMessage(`{"name":"security"}`))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !strings.Contains(res.Content, "references/guide.md") || !strings.Contains(res.Content, "scripts/evil.sh") {
		t.Fatalf("resources missing: %q", res.Content)
	}
	for _, forbidden := range []string{"escape", "escape-dir", ".git/config"} {
		if strings.Contains(res.Content, forbidden) {
			t.Fatalf("unsafe resource %q leaked into output", forbidden)
		}
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("bundled script executed, marker error = %v", err)
	}
	roots := binding.Roots()
	if len(roots) != 1 || roots[0] != skillDir {
		t.Fatalf("roots = %v, want %q", roots, skillDir)
	}
	for _, root := range roots {
		info, err := os.Lstat(root)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			t.Fatalf("root is not a real directory: %v %v", info, err)
		}
	}
}

func TestLoadSkillTruncatesOutput(t *testing.T) {
	project := t.TempDir()
	writeSkillFile(t, project, "long", "long", "long skill", strings.Repeat("y", 5000))
	l, err := New(Options{ProjectDir: project, MaxOutputChars: 200})
	if err != nil {
		t.Fatal(err)
	}
	binding, err := l.Bind([]string{"long"})
	if err != nil {
		t.Fatal(err)
	}
	res, err := binding.Tool().Call(context.Background(), json.RawMessage(`{"name":"long"}`))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !res.Truncated {
		t.Fatal("expected truncated output")
	}
	if len(res.Content) > 200 {
		t.Fatalf("content length = %d", len(res.Content))
	}
	if !strings.HasSuffix(res.Content, skillTruncationMessage) {
		t.Fatalf("missing truncation marker: %q", res.Content)
	}
}

func TestLoadSkillResourceLimit(t *testing.T) {
	platform := t.TempDir()
	skillDir := writeSkillFile(t, platform, "many", "many", "many resources", "body\n")
	for i := 0; i < maxResources+5; i++ {
		writeAttachment(t, skillDir, fmt.Sprintf("files/resource-%03d.txt", i), "x")
	}
	l, err := New(Options{ProjectDir: platform})
	if err != nil {
		t.Fatal(err)
	}
	binding, err := l.Bind([]string{"many"})
	if err != nil {
		t.Fatal(err)
	}
	res, err := binding.Tool().Call(context.Background(), json.RawMessage(`{"name":"many"}`))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !res.Truncated {
		t.Fatal("resource limit did not mark the result truncated")
	}
	if strings.Contains(res.Content, "resource-204.txt") {
		t.Fatal("resource output exceeded the limit")
	}
}

func TestLoadSkillUnavailableAfterBinding(t *testing.T) {
	platform := t.TempDir()
	skillDir := writeSkillFile(t, platform, "security", "security", "security rules", "body\n")
	l, err := New(Options{PlatformDirs: []string{platform}})
	if err != nil {
		t.Fatal(err)
	}
	binding, err := l.Bind([]string{"security"})
	if err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside.md")
	if err := os.WriteFile(outside, []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(skillDir, "SKILL.md")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(skillDir, "SKILL.md")); err != nil {
		t.Fatal(err)
	}
	_, err = binding.Tool().Call(context.Background(), json.RawMessage(`{"name":"security"}`))
	if err == nil {
		t.Fatal("load followed a symlinked SKILL.md")
	}
	if strings.Contains(err.Error(), outside) {
		t.Fatalf("error leaked a filesystem path: %v", err)
	}
}
