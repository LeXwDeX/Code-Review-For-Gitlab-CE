package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/agent"
	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/config"
	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/gitlab"
	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/llm"
	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/mcp"
	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/repo"
	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/review"
	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/skill"
	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/store"
	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/store/sqlite"
	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/pkg/dag"
)

// GitLabClient is the GitLab surface consumed by review orchestration and
// publication. Every method carries the project token so the client stays
// stateless per project.
type GitLabClient interface {
	CurrentUser(ctx context.Context, token string) (*gitlab.User, error)
	GetMergeRequest(ctx context.Context, token string, projectID, iid int64) (*gitlab.MergeRequest, error)
	LatestMRVersion(ctx context.Context, token string, projectID, iid int64) (*gitlab.Version, error)
	ListMRDiffs(ctx context.Context, token string, projectID, iid int64) ([]gitlab.FileDiff, error)
	ListMRPipelines(ctx context.Context, token string, projectID, iid int64) ([]gitlab.Pipeline, error)
	ListPipelineJobs(ctx context.Context, token string, projectID, pipelineID int64) ([]gitlab.Job, error)
	ListClosingIssues(ctx context.Context, token string, projectID, iid int64) ([]gitlab.Issue, error)
	GetRawFile(ctx context.Context, token string, projectID int64, path, ref string) ([]byte, error)
	ListMRNotes(ctx context.Context, token string, projectID, iid int64) ([]gitlab.Note, error)
	CreateMRNote(ctx context.Context, token string, projectID, iid int64, body string) (*gitlab.Note, error)
	UpdateMRNote(ctx context.Context, token string, projectID, iid, noteID int64, body string) error
	ListDiscussions(ctx context.Context, token string, projectID, iid int64) ([]gitlab.Discussion, error)
	CreateDiscussion(ctx context.Context, token string, projectID, iid int64, d gitlab.NewDiscussion) (*gitlab.Discussion, error)
	SetCommitStatus(ctx context.Context, token string, projectID int64, sha string, st gitlab.CommitStatus) error
	ListNoteAwards(ctx context.Context, token string, projectID, iid, noteID int64) ([]gitlab.Award, error)
}

// GitLabService is the complete GitLab surface used by the app layer:
// administration, command checks, review orchestration, and publication.
type GitLabService interface {
	GitLabClient
	gitlab.API
}

var _ GitLabService = (*gitlab.Client)(nil)

// GitOps is the git surface used by workspace and review orchestration. The
// production implementation wraps repo.Git; tests inject a local fake.
type GitOps interface {
	EnsureMirror(ctx context.Context, dir, remoteURL string, a repo.Auth) error
	Fetch(ctx context.Context, mirror string, a repo.Auth, refspecs ...string) error
	Resolve(ctx context.Context, dir, ref string) (string, error)
	AddWorktree(ctx context.Context, mirror, dir, sha string) error
	RemoveWorktree(ctx context.Context, mirror, dir string) error
	Checkout(ctx context.Context, dir, sha string) error
	ChangedFiles(ctx context.Context, dir, from, to string) ([]string, error)
	IsAncestor(ctx context.Context, dir, ancestor, descendant string) (bool, error)
}

// mcpToolProvider exposes only the explicitly bound MCP tools for one block.
// A nil manager means MCP is not configured.
type mcpToolProvider struct {
	manager *mcp.Manager
	blocks  map[string]config.BlockBinding
}

// NewMCPToolProvider adapts an MCP manager to review.ToolProvider.
func NewMCPToolProvider(manager *mcp.Manager, blocks map[string]config.BlockBinding) review.ToolProvider {
	return &mcpToolProvider{manager: manager, blocks: blocks}
}

func (p *mcpToolProvider) Tools(ctx context.Context, block string) ([]agent.Tool, error) {
	if p == nil || p.manager == nil || len(p.blocks) == 0 {
		return nil, nil
	}
	allowed := p.blocks[block].MCPTools
	if len(allowed) == 0 {
		return nil, nil
	}
	set, err := p.manager.Tools(ctx, allowed)
	if err != nil {
		return nil, fmt.Errorf("app: bind MCP tools for block %q: %w", block, err)
	}
	return set.Tools, nil
}

// skillProvider binds the skills named by a block and exposes load_skill.
type skillProvider struct {
	loader *skill.Loader
}

// NewSkillProvider adapts a skill loader to review.SkillProvider.
func NewSkillProvider(loader *skill.Loader) review.SkillProvider {
	return &skillProvider{loader: loader}
}

func (p *skillProvider) BindSkills(ctx context.Context, names []string) (review.SkillBinding, error) {
	if p == nil || p.loader == nil {
		return review.SkillBinding{}, errors.New("app: skill loader is not configured")
	}
	binding, err := p.loader.Bind(names)
	if err != nil {
		return review.SkillBinding{}, fmt.Errorf("app: bind skills: %w", err)
	}
	catalogue := make(map[string]skill.Skill)
	for _, item := range p.loader.Catalogue() {
		catalogue[item.Name] = item
	}
	out := review.SkillBinding{Roots: binding.Roots(), Tool: binding.Tool()}
	for _, name := range binding.Names() {
		if item, ok := catalogue[name]; ok {
			out.Skills = append(out.Skills, review.Skill{Name: item.Name, Description: item.Description})
		}
	}
	return out, nil
}

// engineSnapshot adapts Engine.Snapshot to review.WorkflowSnapshot.
func engineSnapshot(engine *dag.Engine) review.WorkflowSnapshot {
	return func(ctx context.Context, workflowID string) (dag.WorkflowView, error) {
		if engine == nil {
			return dag.WorkflowView{}, errors.New("app: workflow engine is not configured")
		}
		return engine.Snapshot(ctx, workflowID)
	}
}

// locateFunc adapts a DiffIndex to review.LocateFunc.
func locateFunc(index *gitlab.DiffIndex) review.LocateFunc {
	if index == nil {
		return func(string, int, review.Side) bool { return false }
	}
	return func(path string, line int, side review.Side) bool {
		mapped := gitlab.SideNew
		if side == review.SideOld {
			mapped = gitlab.SideOld
		}
		_, ok := index.Locate(path, line, mapped)
		return ok
	}
}

// newCostFunc converts provider usage to micro-dollars using the configured
// per-million-token prices. Unknown providers cost zero.
func newCostFunc(providers map[string]config.ProviderConfig) review.CostFunc {
	return func(provider string, usage llm.Usage) int64 {
		cfg, ok := providers[provider]
		if !ok || cfg.Price == nil {
			return 0
		}
		price := cfg.Price
		micros := float64(usage.InputTokens)*price.InputPerMTok +
			float64(usage.OutputTokens)*price.OutputPerMTok
		if micros <= 0 {
			return 0
		}
		return int64(math.Round(micros))
	}
}

// executorRegistry dispatches engine calls to the per-workflow Executor that
// owns the sanitized dossier and diff index.
type executorRegistry struct {
	mu            sync.RWMutex
	exec          map[string]*review.Executor
	projectLookup func(context.Context, string) (int64, bool)
}

func newExecutorRegistry() *executorRegistry {
	return &executorRegistry{exec: make(map[string]*review.Executor)}
}

// SetProjectLookup installs the workflow-to-project resolver used to attach a
// project ID to node contexts. Without it, providers fall back to platform
// configuration.
func (r *executorRegistry) SetProjectLookup(lookup func(context.Context, string) (int64, bool)) {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.projectLookup = lookup
	r.mu.Unlock()
}

type projectIDContextKey struct{}

func projectIDFromContext(ctx context.Context) (int64, bool) {
	id, ok := ctx.Value(projectIDContextKey{}).(int64)
	return id, ok && id > 0
}

func (r *executorRegistry) withProject(ctx context.Context, workflowID string) context.Context {
	if r == nil || workflowID == "" {
		return ctx
	}
	r.mu.RLock()
	lookup := r.projectLookup
	r.mu.RUnlock()
	if lookup == nil {
		return ctx
	}
	id, ok := lookup(ctx, workflowID)
	if !ok || id <= 0 {
		return ctx
	}
	return context.WithValue(ctx, projectIDContextKey{}, id)
}

// Register attaches an executor to a workflow ID. It replaces a previous
// registration so recovery can rebuild the executor.
func (r *executorRegistry) Register(workflowID string, executor *review.Executor) {
	if r == nil || workflowID == "" || executor == nil {
		return
	}
	r.mu.Lock()
	r.exec[workflowID] = executor
	r.mu.Unlock()
}

// Unregister drops the executor for a terminal workflow.
func (r *executorRegistry) Unregister(workflowID string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	delete(r.exec, workflowID)
	r.mu.Unlock()
}

func (r *executorRegistry) lookup(workflowID string) (*review.Executor, error) {
	if r == nil {
		return nil, errors.New("app: executor registry is not configured")
	}
	r.mu.RLock()
	executor := r.exec[workflowID]
	r.mu.RUnlock()
	if executor == nil {
		return nil, fmt.Errorf("app: no executor registered for workflow %q", workflowID)
	}
	return executor, nil
}

// Execute implements dag.NodeExecutor.
func (r *executorRegistry) Execute(ctx context.Context, in dag.NodeInput) (dag.NodeOutput, error) {
	executor, err := r.lookup(in.WorkflowID)
	if err != nil {
		return dag.NodeOutput{}, err
	}
	return executor.Execute(r.withProject(ctx, in.WorkflowID), in)
}

// Expand implements dag.Expander.
func (r *executorRegistry) Expand(ctx context.Context, wf dag.WorkflowView, from dag.NodeSpec, out json.RawMessage) (dag.Extension, error) {
	executor, err := r.lookup(wf.ID)
	if err != nil {
		return dag.Extension{}, err
	}
	return executor.Expand(r.withProject(ctx, wf.ID), wf, from, out)
}

var _ dag.NodeExecutor = (*executorRegistry)(nil)
var _ dag.Expander = (*executorRegistry)(nil)

// trimProjectWebURL normalizes a GitLab project URL to a clone URL.
func trimProjectWebURL(webURL string) string {
	value := strings.TrimRight(strings.TrimSpace(webURL), "/")
	if value == "" {
		return ""
	}
	if strings.HasSuffix(value, ".git") {
		return value
	}
	return value + ".git"
}

// ProjectExtensionResolver builds and caches project-scoped review inputs:
// merged block bindings, a project-aware MCP manager and a project skill
// loader. A project without stored overrides reuses the platform providers.
type ProjectExtensionResolver struct {
	store      *sqlite.Store
	blocks     map[string]config.BlockBinding
	mcpConfig  mcp.Config
	skillDirs  []string
	workspace  string
	deniedEnv  []string
	now        func() time.Time
	logger     *slog.Logger
	baseTools  review.ToolProvider
	baseSkills review.SkillProvider

	mu   sync.Mutex
	sets map[int64]*projectExtensionSet
}

// ProjectExtensionResolverConfig configures ProjectExtensionResolver. Base
// providers are the platform-only providers and are never closed here.
type ProjectExtensionResolverConfig struct {
	Store      *sqlite.Store
	Blocks     map[string]config.BlockBinding
	MCP        mcp.Config
	SkillDirs  []string
	Workspace  string
	DeniedEnv  []string
	Now        func() time.Time
	Logger     *slog.Logger
	BaseTools  review.ToolProvider
	BaseSkills review.SkillProvider
}

type projectExtensionSet struct {
	blocks    map[string]config.BlockBinding
	tools     review.ToolProvider
	skills    review.SkillProvider
	mcp       *mcp.Manager
	loader    *skill.Loader
	mcpConfig mcp.Config
}

// NewProjectExtensionResolver validates dependencies and copies base state.
func NewProjectExtensionResolver(cfg ProjectExtensionResolverConfig) (*ProjectExtensionResolver, error) {
	if cfg.Store == nil {
		return nil, errors.New("app: project extension store is required")
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &ProjectExtensionResolver{
		store:      cfg.Store,
		blocks:     cloneBlockBindings(cfg.Blocks),
		mcpConfig:  mcp.Merge(cfg.MCP, mcp.Config{}),
		skillDirs:  append([]string(nil), cfg.SkillDirs...),
		workspace:  cfg.Workspace,
		deniedEnv:  append([]string(nil), cfg.DeniedEnv...),
		now:        now,
		logger:     logger,
		baseTools:  cfg.BaseTools,
		baseSkills: cfg.BaseSkills,
		sets:       make(map[int64]*projectExtensionSet),
	}, nil
}

// ForProject returns the project-scoped inputs, building them on first use.
// A read error is returned instead of silently running with platform-only
// configuration.
func (r *ProjectExtensionResolver) ForProject(ctx context.Context, projectID int64) (*projectExtensionSet, error) {
	if projectID <= 0 {
		return nil, errors.New("app: project id is required")
	}
	r.mu.Lock()
	if existing := r.sets[projectID]; existing != nil {
		r.mu.Unlock()
		return existing, nil
	}
	r.mu.Unlock()

	set, err := r.build(ctx, projectID)
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	if existing := r.sets[projectID]; existing != nil {
		r.mu.Unlock()
		if set.mcp != nil {
			_ = set.mcp.Close()
		}
		return existing, nil
	}
	r.sets[projectID] = set
	r.mu.Unlock()
	return set, nil
}

// Close releases project MCP managers. Platform managers are owned elsewhere.
func (r *ProjectExtensionResolver) Close() error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	sets := r.sets
	r.sets = make(map[int64]*projectExtensionSet)
	r.mu.Unlock()
	var errs []error
	for _, set := range sets {
		if set.mcp != nil {
			errs = append(errs, set.mcp.Close())
		}
	}
	return errors.Join(errs...)
}

func (r *ProjectExtensionResolver) build(ctx context.Context, projectID int64) (*projectExtensionSet, error) {
	projectRows, err := r.store.ListBlockBindings(ctx, store.ScopeProject, &projectID)
	if err != nil {
		return nil, fmt.Errorf("app: load project block bindings: %w", err)
	}
	blocks := cloneBlockBindings(r.blocks)
	if blocks == nil {
		blocks = make(map[string]config.BlockBinding)
	}
	for _, row := range projectRows {
		if strings.TrimSpace(row.Block) == "" {
			continue
		}
		blocks[row.Block] = config.BlockBinding{
			MCPTools: append([]string(nil), row.MCPTools...),
			Skills:   append([]string(nil), row.Skills...),
		}
	}

	platformConfig := mcp.Merge(r.mcpConfig, mcp.Config{})
	platformServers, err := r.store.ListMCPServers(ctx, store.ScopePlatform, nil)
	if err != nil {
		return nil, fmt.Errorf("app: load platform MCP servers: %w", err)
	}
	for _, row := range platformServers {
		if !row.Enabled {
			continue
		}
		var server mcp.ServerConfig
		if err := json.Unmarshal(row.Config, &server); err != nil {
			return nil, fmt.Errorf("app: decode platform MCP server %q: %w", row.Name, err)
		}
		platformConfig.MCPServers[row.Name] = server
	}
	set := &projectExtensionSet{
		blocks:    blocks,
		tools:     r.baseTools,
		skills:    r.baseSkills,
		mcpConfig: platformConfig,
	}

	servers, err := r.store.ListMCPServers(ctx, store.ScopeProject, &projectID)
	if err != nil {
		return nil, fmt.Errorf("app: load project MCP servers: %w", err)
	}
	projectMCP := mcp.Config{MCPServers: make(map[string]mcp.ServerConfig, len(servers))}
	for _, row := range servers {
		if !row.Enabled {
			continue
		}
		var server mcp.ServerConfig
		if err := json.Unmarshal(row.Config, &server); err != nil {
			return nil, fmt.Errorf("app: decode project MCP server %q: %w", row.Name, err)
		}
		projectMCP.MCPServers[row.Name] = server
	}
	if len(projectMCP.MCPServers) > 0 {
		set.mcpConfig = mcp.Merge(platformConfig, projectMCP)
		workspace := r.projectWorkspace(projectID)
		manager, err := mcp.New(mcp.Options{
			Platform:  platformConfig,
			Project:   projectMCP,
			Workspace: workspace,
			DeniedEnv: r.deniedEnv,
			Now:       r.now,
		})
		if err != nil {
			return nil, fmt.Errorf("app: build project MCP manager: %w", err)
		}
		set.mcp = manager
		set.tools = NewMCPToolProvider(manager, blocks)
	}

	// Platform skills are re-read on every project build so skills uploaded
	// after startup are visible to the next review. The runtime base loader is
	// a startup snapshot and cannot see them.
	dirs := append([]string(nil), r.skillDirs...)
	seen := make(map[string]bool, len(dirs))
	for _, dir := range dirs {
		seen[dir] = true
	}
	platformSkills, err := r.store.ListSkills(ctx, store.ScopePlatform, nil)
	if err != nil {
		if set.mcp != nil {
			_ = set.mcp.Close()
		}
		return nil, fmt.Errorf("app: load platform skills: %w", err)
	}
	dirs, added := appendSkillContainers(dirs, seen, platformSkills)
	projectSkills, err := r.store.ListSkills(ctx, store.ScopeProject, &projectID)
	if err != nil {
		if set.mcp != nil {
			_ = set.mcp.Close()
		}
		return nil, fmt.Errorf("app: load project skills: %w", err)
	}
	dirs, projectAdded := appendSkillContainers(dirs, seen, projectSkills)
	added = added || projectAdded
	if added {
		loader, err := skill.New(skill.Options{PlatformDirs: dirs})
		if err != nil {
			if set.mcp != nil {
				_ = set.mcp.Close()
			}
			return nil, fmt.Errorf("app: load project skills: %w", err)
		}
		set.loader = loader
		set.skills = NewSkillProvider(loader)
	}
	return set, nil
}

func appendSkillContainers(dirs []string, seen map[string]bool, rows []store.Skill) ([]string, bool) {
	added := false
	for _, row := range rows {
		if !row.Enabled || strings.TrimSpace(row.Dir) == "" {
			continue
		}
		container := filepath.Dir(row.Dir)
		if seen[container] {
			continue
		}
		seen[container] = true
		dirs = append(dirs, container)
		added = true
	}
	return dirs, added
}

func (r *ProjectExtensionResolver) projectWorkspace(projectID int64) string {
	if strings.TrimSpace(r.workspace) == "" {
		return r.workspace
	}
	return filepath.Join(r.workspace, fmt.Sprintf("%d", projectID))
}

// loadStoredPlatformExtensions merges database-backed platform extensions over
// file configuration. Database rows win on a name conflict.
func loadStoredPlatformExtensions(ctx context.Context, st *sqlite.Store, blocks map[string]config.BlockBinding,
	mcpConfig mcp.Config, skillDirs []string) (map[string]config.BlockBinding, mcp.Config, []string, error) {
	if st == nil {
		return cloneBlockBindings(blocks), mcpConfig, append([]string(nil), skillDirs...), nil
	}
	mergedBlocks := cloneBlockBindings(blocks)
	if mergedBlocks == nil {
		mergedBlocks = make(map[string]config.BlockBinding)
	}
	rows, err := st.ListBlockBindings(ctx, store.ScopePlatform, nil)
	if err != nil {
		return nil, mcp.Config{}, nil, fmt.Errorf("app: load platform block bindings: %w", err)
	}
	for _, row := range rows {
		if strings.TrimSpace(row.Block) == "" {
			continue
		}
		mergedBlocks[row.Block] = config.BlockBinding{
			MCPTools: append([]string(nil), row.MCPTools...),
			Skills:   append([]string(nil), row.Skills...),
		}
	}

	servers, err := st.ListMCPServers(ctx, store.ScopePlatform, nil)
	if err != nil {
		return nil, mcp.Config{}, nil, fmt.Errorf("app: load platform MCP servers: %w", err)
	}
	if mcpConfig.MCPServers == nil {
		mcpConfig.MCPServers = make(map[string]mcp.ServerConfig)
	}
	for _, row := range servers {
		if !row.Enabled {
			continue
		}
		var server mcp.ServerConfig
		if err := json.Unmarshal(row.Config, &server); err != nil {
			return nil, mcp.Config{}, nil, fmt.Errorf("app: decode platform MCP server %q: %w", row.Name, err)
		}
		mcpConfig.MCPServers[row.Name] = server
	}

	storedSkills, err := st.ListSkills(ctx, store.ScopePlatform, nil)
	if err != nil {
		return nil, mcp.Config{}, nil, fmt.Errorf("app: load platform skills: %w", err)
	}
	mergedDirs := append([]string(nil), skillDirs...)
	seen := make(map[string]bool, len(storedSkills))
	for _, row := range storedSkills {
		if !row.Enabled || strings.TrimSpace(row.Dir) == "" {
			continue
		}
		container := filepath.Dir(row.Dir)
		if seen[container] {
			continue
		}
		seen[container] = true
		mergedDirs = append(mergedDirs, container)
	}
	return mergedBlocks, mcpConfig, mergedDirs, nil
}

func cloneBlockBindings(in map[string]config.BlockBinding) map[string]config.BlockBinding {
	if in == nil {
		return nil
	}
	out := make(map[string]config.BlockBinding, len(in))
	for name, binding := range in {
		out[name] = config.BlockBinding{
			MCPTools: append([]string(nil), binding.MCPTools...),
			Skills:   append([]string(nil), binding.Skills...),
		}
	}
	return out
}

const (
	projectMCPStub       = "__project_mcp__"
	projectSkillStubBase = "__project_skill__:"
)

// projectBindingStubs makes the static executor bindings admit project-scoped
// overrides. The executor only observes non-empty lists; the project-aware
// providers replace the stub with the merged project binding before use.
func projectBindingStubs(blocks map[string]config.BlockBinding) map[string]config.BlockBinding {
	out := cloneBlockBindings(blocks)
	if out == nil {
		out = make(map[string]config.BlockBinding)
	}
	for _, block := range []string{"code-review", "review", "reasoner", "verify", "synthesize", "onboarding"} {
		out[block] = config.BlockBinding{
			MCPTools: []string{projectMCPStub},
			Skills:   []string{projectSkillStubBase + block},
		}
	}
	for block, binding := range out {
		if len(binding.MCPTools) == 0 {
			binding.MCPTools = []string{projectMCPStub}
		}
		if len(binding.Skills) == 0 {
			binding.Skills = []string{projectSkillStubBase + block}
		}
		out[block] = binding
	}
	return out
}

// projectAwareToolProvider resolves the project from the node context and
// delegates to that project's merged tool provider.
type projectAwareToolProvider struct {
	resolver *ProjectExtensionResolver
	base     review.ToolProvider
}

func (p *projectAwareToolProvider) Tools(ctx context.Context, block string) ([]agent.Tool, error) {
	if p == nil {
		return nil, nil
	}
	if id, ok := projectIDFromContext(ctx); ok && p.resolver != nil {
		set, err := p.resolver.ForProject(ctx, id)
		if err == nil && set.tools != nil {
			return set.tools.Tools(ctx, block)
		}
	}
	if p.base == nil {
		return nil, nil
	}
	return p.base.Tools(ctx, block)
}

// projectAwareSkillProvider resolves the project from the node context and
// replaces the stub names with that project's merged skill list.
type projectAwareSkillProvider struct {
	resolver *ProjectExtensionResolver
	base     review.SkillProvider
	blocks   map[string]config.BlockBinding
}

func (p *projectAwareSkillProvider) BindSkills(ctx context.Context, names []string) (review.SkillBinding, error) {
	if p == nil {
		return review.SkillBinding{}, nil
	}
	block := ""
	if len(names) == 1 && strings.HasPrefix(names[0], projectSkillStubBase) {
		block = strings.TrimPrefix(names[0], projectSkillStubBase)
	}
	if id, ok := projectIDFromContext(ctx); ok && p.resolver != nil {
		if set, err := p.resolver.ForProject(ctx, id); err == nil {
			if block != "" {
				names = set.blocks[block].Skills
				if len(names) == 0 {
					return review.SkillBinding{}, nil
				}
			}
			if set.skills == nil {
				return review.SkillBinding{}, nil
			}
			return set.skills.BindSkills(ctx, names)
		}
	}
	if block != "" {
		names = p.blocks[block].Skills
		if len(names) == 0 {
			return review.SkillBinding{}, nil
		}
	}
	if p.base == nil {
		return review.SkillBinding{}, nil
	}
	return p.base.BindSkills(ctx, names)
}
