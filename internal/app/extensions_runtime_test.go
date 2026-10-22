package app

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/config"
	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/mcp"
	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/review"
	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/skill"
)

func TestUploadSkillExtractsAndIsLoadable(t *testing.T) {
	ctx := context.Background()
	service, _ := newExtensionFixture(t)
	projectID := int64(42)

	uploaded, err := service.UploadSkill(ctx, SkillUpload{
		Scope:     "project",
		ProjectID: &projectID,
		Data: testSkillArchiveFiles(t, map[string]string{
			"skill/SKILL.md":     "---\nname: go-review\ndescription: Review Go changes\n---\n",
			"skill/references/a": "reference",
		}),
	})
	if err != nil {
		t.Fatalf("UploadSkill: %v", err)
	}
	if uploaded.Name != "go-review" {
		t.Fatalf("name = %q", uploaded.Name)
	}
	if _, err := os.Stat(filepath.Join(uploaded.Dir, "SKILL.md")); err != nil {
		t.Fatalf("extracted manifest: %v", err)
	}
	if _, err := os.Stat(filepath.Join(uploaded.Dir, "references", "a")); err != nil {
		t.Fatalf("extracted resource: %v", err)
	}
	if !strings.HasPrefix(uploaded.Dir, filepath.Join(service.workspace, "skills")) {
		t.Fatalf("dir = %q is outside the managed root", uploaded.Dir)
	}
	loader, err := skill.New(skill.Options{PlatformDirs: []string{filepath.Dir(uploaded.Dir)}})
	if err != nil {
		t.Fatalf("skill.New: %v", err)
	}
	names := catalogueNames(loader)
	if !containsString(names, "go-review") {
		t.Fatalf("catalogue = %v", names)
	}

	if err := service.DeleteSkill(ctx, uploaded.ID); err != nil {
		t.Fatalf("DeleteSkill: %v", err)
	}
	if _, err := os.Stat(filepath.Dir(uploaded.Dir)); !os.IsNotExist(err) {
		t.Fatalf("managed directory still exists: %v", err)
	}
}

func TestUploadSkillRejectsUnsafeArchives(t *testing.T) {
	ctx := context.Background()
	service, _ := newExtensionFixture(t)

	cases := []struct {
		name string
		data []byte
	}{
		{name: "traversal", data: testSkillArchiveFiles(t, map[string]string{
			"skill/SKILL.md":         testSkillBody,
			"skill/../../../evil.go": "package evil",
		})},
		{name: "missing manifest", data: testSkillArchiveFiles(t, map[string]string{
			"skill/README.md": "# nope",
		})},
		{name: "symlink", data: symlinkArchive(t)},
		{name: "oversize", data: oversizeArchive(t)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := service.UploadSkill(ctx, SkillUpload{Scope: "platform", Data: tc.data}); err == nil {
				t.Fatal("unsafe archive was accepted")
			}
			if _, err := os.Stat(filepath.Join(service.workspace, "evil.go")); err == nil {
				t.Fatal("traversal wrote outside the managed root")
			}
		})
	}
}

func symlinkArchive(t *testing.T) []byte {
	t.Helper()
	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	entry, err := writer.Create("SKILL.md")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := entry.Write([]byte(testSkillBody)); err != nil {
		t.Fatal(err)
	}
	header := &zip.FileHeader{Name: "link.md"}
	header.SetMode(os.ModeSymlink | 0o777)
	link, err := writer.CreateHeader(header)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := link.Write([]byte("SKILL.md")); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

// oversizeArchive stays well below the compressed limit while expanding past
// the extracted total limit: five 4 MiB files of zeros.
func oversizeArchive(t *testing.T) []byte {
	t.Helper()
	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	if entry, err := writer.Create("SKILL.md"); err != nil {
		t.Fatal(err)
	} else if _, err := entry.Write([]byte(testSkillBody)); err != nil {
		t.Fatal(err)
	}
	zeros := make([]byte, 4<<20)
	for i := 0; i < 5; i++ {
		entry, err := writer.Create(fmt.Sprintf("skill/blob-%d.bin", i))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := entry.Write(zeros); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func TestRegisterSkillDirCopiesIntoManagedRoot(t *testing.T) {
	ctx := context.Background()
	service, _ := newExtensionFixture(t)
	source := t.TempDir()
	if err := os.WriteFile(filepath.Join(source, "SKILL.md"),
		[]byte("---\nname: security\ndescription: Security review\n---\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(source, "references"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "references", "check.md"), []byte("check"), 0o600); err != nil {
		t.Fatal(err)
	}
	registered, err := service.RegisterSkillDir(ctx, SkillDirInput{Scope: "platform", Dir: source})
	if err != nil {
		t.Fatalf("RegisterSkillDir: %v", err)
	}
	if registered.Dir == source || !strings.HasPrefix(registered.Dir, filepath.Join(service.workspace, "skills")) {
		t.Fatalf("dir = %q", registered.Dir)
	}
	if _, err := os.Stat(filepath.Join(registered.Dir, "references", "check.md")); err != nil {
		t.Fatalf("copied resource: %v", err)
	}
}

func TestProjectExtensionResolverMergesOverrides(t *testing.T) {
	ctx := context.Background()
	service, _ := newExtensionFixture(t)
	projectID := int64(42)
	project := int64(42)

	if _, err := service.CreateMCPServer(ctx, MCPServerInput{
		Scope: "platform", Name: "alpha", Enabled: true,
		Config: json.RawMessage(`{"command":"platform-alpha"}`),
	}); err != nil {
		t.Fatalf("platform MCP: %v", err)
	}
	// gamma exists only in the database; the file configuration does not
	// mention it, so the project manager must receive the merged platform
	// configuration rather than the file snapshot.
	if _, err := service.CreateMCPServer(ctx, MCPServerInput{
		Scope: "platform", Name: "gamma", Enabled: true,
		Config: json.RawMessage(`{"command":"platform-gamma"}`),
	}); err != nil {
		t.Fatalf("platform MCP gamma: %v", err)
	}
	if _, err := service.CreateMCPServer(ctx, MCPServerInput{
		Scope: "project", ProjectID: &project, Name: "alpha", Enabled: true,
		Config: json.RawMessage(`{"command":"project-alpha"}`),
	}); err != nil {
		t.Fatalf("project MCP: %v", err)
	}
	if _, err := service.CreateMCPServer(ctx, MCPServerInput{
		Scope: "project", ProjectID: &project, Name: "beta", Enabled: true,
		Config: json.RawMessage(`{"command":"project-beta"}`),
	}); err != nil {
		t.Fatalf("project MCP beta: %v", err)
	}
	if _, err := service.UpsertBlockBinding(ctx, BlockBindingInput{
		Scope: "platform", Block: "code-review",
		MCPTools: []string{"platform-tool"}, Skills: []string{"platform-skill"},
	}); err != nil {
		t.Fatalf("platform binding: %v", err)
	}
	if _, err := service.UpsertBlockBinding(ctx, BlockBindingInput{
		Scope: "project", ProjectID: &project, Block: "code-review",
		MCPTools: []string{"project-tool"}, Skills: []string{"project-skill"},
	}); err != nil {
		t.Fatalf("project binding: %v", err)
	}
	source := t.TempDir()
	if err := os.WriteFile(filepath.Join(source, "SKILL.md"),
		[]byte("---\nname: security\ndescription: Security review\n---\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := service.RegisterSkillDir(ctx, SkillDirInput{Scope: "project", ProjectID: &project, Dir: source}); err != nil {
		t.Fatalf("project skill: %v", err)
	}

	platformBlocks := map[string]config.BlockBinding{
		"code-review": {MCPTools: []string{"platform-tool"}, Skills: []string{"platform-skill"}},
	}
	resolver, err := NewProjectExtensionResolver(ProjectExtensionResolverConfig{
		Store:      service.store,
		Blocks:     platformBlocks,
		MCP:        mcpFileConfig(),
		Workspace:  service.workspace,
		BaseSkills: NewSkillProvider(mustLoader(t)),
	})
	if err != nil {
		t.Fatalf("resolver: %v", err)
	}
	t.Cleanup(func() { _ = resolver.Close() })

	set, err := resolver.ForProject(ctx, projectID)
	if err != nil {
		t.Fatalf("ForProject: %v", err)
	}
	binding := set.blocks["code-review"]
	if len(binding.MCPTools) != 1 || binding.MCPTools[0] != "project-tool" {
		t.Fatalf("project tools = %v", binding.MCPTools)
	}
	if len(binding.Skills) != 1 || binding.Skills[0] != "project-skill" {
		t.Fatalf("project skills = %v", binding.Skills)
	}
	if set.mcp == nil || set.loader == nil {
		t.Fatalf("project runtime not built: mcp=%v loader=%v", set.mcp, set.loader)
	}
	if servers, want := strings.Join(set.mcp.Servers(), ","), "alpha,beta,gamma,shared"; servers != want {
		t.Fatalf("servers = %q, want %q", servers, want)
	}
	if got := set.mcpConfig.MCPServers["alpha"].Command; got != "project-alpha" {
		t.Fatalf("alpha command = %q, want project override", got)
	}
	if got := set.mcpConfig.MCPServers["gamma"].Command; got != "platform-gamma" {
		t.Fatalf("gamma command = %q, want platform database server", got)
	}
	if names := catalogueNames(set.loader); !containsString(names, "security") {
		t.Fatalf("project skill not loaded: %v", names)
	}
	provider, ok := set.tools.(*mcpToolProvider)
	if !ok || provider.blocks["code-review"].MCPTools[0] != "project-tool" {
		t.Fatalf("tool provider does not carry project bindings: %#v", set.tools)
	}
	if _, err := review.NewExecutor(review.ExecutorDeps{
		Profile: review.Profile{Name: "default", Config: testReviewProfile()},
		Blocks:  set.blocks,
		Tools:   set.tools,
		Skills:  set.skills,
	}); err != nil {
		t.Fatalf("executor deps rejected project bindings: %v", err)
	}

	platformSet, err := resolver.ForProject(ctx, 43)
	if err != nil {
		t.Fatalf("ForProject platform-only: %v", err)
	}
	if platformSet.mcp != nil || platformSet.loader != nil {
		t.Fatal("platform-only project built a project runtime")
	}
	if got := platformSet.blocks["code-review"]; len(got.MCPTools) != 1 || got.MCPTools[0] != "platform-tool" {
		t.Fatalf("platform-only blocks = %+v", got)
	}
}

func mcpFileConfig() mcp.Config {
	return mcp.Config{MCPServers: map[string]mcp.ServerConfig{
		"shared": {Command: "platform-shared"},
	}}
}

func mustLoader(t *testing.T) *skill.Loader {
	t.Helper()
	loader, err := skill.New(skill.Options{})
	if err != nil {
		t.Fatalf("skill.New: %v", err)
	}
	return loader
}

func catalogueNames(loader *skill.Loader) []string {
	names := make([]string, 0)
	for _, item := range loader.Catalogue() {
		names = append(names, item.Name)
	}
	return names
}

func TestLoadStoredPlatformExtensions(t *testing.T) {
	ctx := context.Background()
	service, st := newExtensionFixture(t)
	source := t.TempDir()
	if err := os.WriteFile(filepath.Join(source, "SKILL.md"),
		[]byte("---\nname: platform-skill\ndescription: Platform skill\n---\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := service.RegisterSkillDir(ctx, SkillDirInput{Scope: "platform", Dir: source}); err != nil {
		t.Fatalf("platform skill: %v", err)
	}
	if _, err := service.CreateMCPServer(ctx, MCPServerInput{
		Scope: "platform", Name: "stored", Enabled: true,
		Config: json.RawMessage(`{"command":"stored-server"}`),
	}); err != nil {
		t.Fatalf("platform MCP: %v", err)
	}
	if _, err := service.UpsertBlockBinding(ctx, BlockBindingInput{
		Scope: "platform", Block: "code-review", MCPTools: []string{"stored-tool"},
	}); err != nil {
		t.Fatalf("platform binding: %v", err)
	}

	blocks, mcpConfig, dirs, err := loadStoredPlatformExtensions(ctx, st, nil, mcp.Config{}, nil)
	if err != nil {
		t.Fatalf("loadStoredPlatformExtensions: %v", err)
	}
	if got := blocks["code-review"].MCPTools; len(got) != 1 || got[0] != "stored-tool" {
		t.Fatalf("blocks = %+v", blocks)
	}
	if got := mcpConfig.MCPServers["stored"].Command; got != "stored-server" {
		t.Fatalf("mcp config = %+v", mcpConfig)
	}
	loader, err := skill.New(skill.Options{PlatformDirs: dirs})
	if err != nil {
		t.Fatalf("skill.New: %v", err)
	}
	if names := catalogueNames(loader); !containsString(names, "platform-skill") {
		t.Fatalf("platform skill not loaded: %v", names)
	}
}
