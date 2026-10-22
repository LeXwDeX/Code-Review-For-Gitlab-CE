package prompts

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestLoadSelectsAllRolesAndIsDeterministic(t *testing.T) {
	first, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	second, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if first.Hash != second.Hash || len(first.Hash) != 64 {
		t.Fatalf("hash = %q, %q", first.Hash, second.Hash)
	}
	if len(RoleFiles) != 10 {
		t.Fatalf("role count = %d", len(RoleFiles))
	}
	for _, name := range RoleFiles {
		content, ok := first.Content(name)
		if !ok || strings.TrimSpace(content) == "" {
			t.Fatalf("missing role %s", name)
		}
		version := first.RoleVersion(name)
		if version == "" || !regexp.MustCompile(`^[0-9A-Za-z._-]+$`).MatchString(version) {
			t.Fatalf("bad version %s=%q", name, version)
		}
	}
	if len(first.Names()) != len(RoleFiles) || first.Names()[0] != PlatformRules {
		t.Fatalf("names = %v", first.Names())
	}
}

func TestOverrideOnlyUsesRegularVersionedFiles(t *testing.T) {
	dir := t.TempDir()
	override := "<!-- v7 -->\n# Custom code review\nCustom body.\n"
	if err := os.WriteFile(filepath.Join(dir, CodeReview), []byte(override), 0o600); err != nil {
		t.Fatal(err)
	}
	embedded, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	selected, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if selected.Hash == embedded.Hash {
		t.Fatal("override must change the bundle hash")
	}
	if selected.RoleVersion(CodeReview) != "7" {
		t.Fatalf("version = %q", selected.RoleVersion(CodeReview))
	}
	if content, _ := selected.Content(CodeReview); content != override {
		t.Fatalf("override content = %q", content)
	}
	if content, _ := selected.Content(Plan); content != embedded.Files[Plan] {
		t.Fatal("unrelated role was not preserved")
	}

	badDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(badDir, Verify), []byte("# no version\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(badDir); err == nil {
		t.Fatal("missing version must fail")
	}

	dirPath := t.TempDir()
	if err := os.Mkdir(filepath.Join(dirPath, Reasoner), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(dirPath); err == nil {
		t.Fatal("override directory entry must fail")
	}

	largeDir := t.TempDir()
	large := "<!-- v1 -->\n" + strings.Repeat("x", MaxFileBytes)
	if err := os.WriteFile(filepath.Join(largeDir, Onboarding), []byte(large), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(largeDir); err == nil {
		t.Fatal("oversized override must fail")
	}
}

func TestCheckFilesRejectsZeroBundle(t *testing.T) {
	if err := CheckFiles(Bundle{}); err == nil {
		t.Fatal("zero bundle must fail")
	}
}

func TestPromptFilesContainNoSecrets(t *testing.T) {
	bundle, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	patterns := []string{"sk-", "glpat-", "AKIA", "ANTHROPIC_API_KEY=", "OPENAI_API_KEY=", "-----BEGIN", "xoxb-"}
	for name, content := range bundle.Files {
		for _, pattern := range patterns {
			if strings.Contains(content, pattern) {
				t.Fatalf("prompt %s contains secret-like pattern %q", name, pattern)
			}
		}
	}
}
