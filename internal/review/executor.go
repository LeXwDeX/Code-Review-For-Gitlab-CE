package review

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/agent"
	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/config"
	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/llm"
	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/pkg/dag"
	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/prompts"
)

const (
	defaultMaxAgentAttempts = 3
	defaultAgentBackoff     = 250 * time.Millisecond
)

// ErrUnknownNodeKind reports a kind this executor does not implement.
var ErrUnknownNodeKind = errors.New("review: unknown node kind")

// ClientProvider resolves a configured llm.Client by provider name.
type ClientProvider interface {
	Client(name string) (llm.Client, error)
}

// AgentRunner executes one agent tool loop. agent.Run is the default.
type AgentRunner func(ctx context.Context, spec agent.RunSpec) (*agent.RunResult, error)

// ToolProvider returns the explicitly bound MCP tools for one block.
type ToolProvider interface {
	Tools(ctx context.Context, block string) ([]agent.Tool, error)
}

// SkillBinding is the prompt catalogue plus the load_skill tool.
type SkillBinding struct {
	Skills []Skill
	Roots  []string
	Tool   agent.Tool
}

// SkillProvider binds the named skills for one block.
type SkillProvider interface {
	BindSkills(ctx context.Context, names []string) (SkillBinding, error)
}

// WorkflowSnapshot reads the projected workflow with completed node outputs.
type WorkflowSnapshot func(ctx context.Context, workflowID string) (dag.WorkflowView, error)

// CostFunc converts provider usage into micro-dollars.
type CostFunc func(provider string, usage llm.Usage) int64

// ExecutorDeps contains every external capability of the review executor.
// A nil Dossier only supports onboarding graphs.
type ExecutorDeps struct {
	Dossier        *Dossier
	Profile        Profile
	Graph          GraphConfig
	Blocks         map[string]config.BlockBinding
	Clients        ClientProvider
	Prompts        prompts.Bundle
	AgentRun       AgentRunner
	Snapshot       WorkflowSnapshot
	Tools          ToolProvider
	Skills         SkillProvider
	Locate         LocateFunc
	DefaultTimeout time.Duration
	MaxAttempts    int
	Backoff        time.Duration
	Sleep          func(context.Context, time.Duration) error
	Cost           CostFunc
}

var (
	_ dag.NodeExecutor = (*Executor)(nil)
	_ dag.Expander     = (*Executor)(nil)
	_ dag.Expander     = ExpanderSet(nil)
	_ ClientProvider   = (*llm.Registry)(nil)
)

// Executor implements dag.NodeExecutor and dag.Expander for review workflows.
type Executor struct {
	deps           ExecutorDeps
	profile        Profile
	prompts        prompts.Bundle
	runner         AgentRunner
	maxAttempts    int
	backoff        time.Duration
	sleep          func(context.Context, time.Duration) error
	defaultTimeout time.Duration
	expanders      ExpanderSet
}

// NewExecutor validates dependencies and builds the node/expander dispatcher.
func NewExecutor(deps ExecutorDeps) (*Executor, error) {
	profile, err := ValidateProfile(deps.Profile)
	if err != nil {
		return nil, err
	}
	if deps.Prompts.Files == nil {
		loaded, err := prompts.Load("")
		if err != nil {
			return nil, err
		}
		deps.Prompts = loaded
	}
	runner := deps.AgentRun
	if runner == nil {
		runner = agent.Run
	}
	timeout := deps.DefaultTimeout
	if timeout <= 0 {
		timeout = 10 * time.Minute
	}
	attempts := deps.MaxAttempts
	if attempts <= 0 {
		attempts = defaultMaxAgentAttempts
	}
	backoff := deps.Backoff
	if backoff <= 0 {
		backoff = defaultAgentBackoff
	}
	sleep := deps.Sleep
	if sleep == nil {
		sleep = timerSleep
	}
	executor := &Executor{
		deps:           deps,
		profile:        profile,
		prompts:        deps.Prompts,
		runner:         runner,
		maxAttempts:    attempts,
		backoff:        backoff,
		sleep:          sleep,
		defaultTimeout: timeout,
	}
	if deps.Dossier != nil {
		expanders, err := BuildExpanders(profile, deps.Dossier, deps.Graph)
		if err != nil {
			return nil, err
		}
		executor.expanders = ExpanderSet(expanders)
	}
	return executor, nil
}

func timerSleep(ctx context.Context, delay time.Duration) error {
	if delay <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// Execute dispatches one node by kind and enforces the node timeout.
func (e *Executor) Execute(ctx context.Context, in dag.NodeInput) (dag.NodeOutput, error) {
	if ctx == nil {
		return dag.NodeOutput{}, errors.New("review: nil context")
	}
	if e == nil {
		return dag.NodeOutput{}, errors.New("review: nil executor")
	}
	if in.Node.ID == "" {
		return dag.NodeOutput{}, errors.New("review: node id is required")
	}
	timeout := in.Node.Timeout
	if timeout <= 0 {
		timeout = e.defaultTimeout
	}
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	var (
		out dag.NodeOutput
		err error
	)
	switch in.Node.Kind {
	case KindPlan:
		params, decodeErr := decodeParams[planParams](in.Node.Params)
		if decodeErr != nil {
			err = decodeErr
			break
		}
		if params.Mode == "agent" {
			out, err = e.executePlanAgent(ctx, in, params)
		} else {
			out, err = e.executePlanCode(ctx, in)
		}
	case KindCodeReview:
		out, err = e.executeCodeReview(ctx, in)
	case KindReviewStd, KindReviewInt:
		out, err = e.executeReviewChannel(ctx, in)
	case KindReviewGate:
		out, err = e.executeReviewGate(ctx, in)
	case KindReasoner:
		out, err = e.executeReasoner(ctx, in)
	case KindVerify:
		out, err = e.executeVerify(ctx, in)
	case KindSynthesize:
		out, err = e.executeSynthesize(ctx, in)
	case KindOnboarding:
		out, err = e.executeOnboarding(ctx, in)
	case KindGather:
		out, err = e.executeGather(ctx, in)
	case KindAdjudicate:
		out, err = e.executeAdjudicate(ctx, in)
	default:
		err = fmt.Errorf("%w: %q", ErrUnknownNodeKind, in.Node.Kind)
	}
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil && !errors.Is(err, ctxErr) {
			return dag.NodeOutput{}, fmt.Errorf("review: node %s: %w", in.Node.ID, ctxErr)
		}
		return dag.NodeOutput{}, fmt.Errorf("review: node %s (%s): %w", in.Node.ID, in.Node.Kind, err)
	}
	if len(out.Data) == 0 || !json.Valid(out.Data) {
		return dag.NodeOutput{}, fmt.Errorf("review: node %s produced invalid JSON", in.Node.ID)
	}
	return out, nil
}

// Expand dispatches to the plan, gather, or synthesize expander.
func (e *Executor) Expand(ctx context.Context, wf dag.WorkflowView, from dag.NodeSpec, out json.RawMessage) (dag.Extension, error) {
	if e == nil {
		return dag.Extension{}, errors.New("review: nil executor")
	}
	if len(e.expanders) == 0 {
		return dag.Extension{}, &ExtensionError{Field: "expander", Problem: "executor was built without a dossier"}
	}
	ext, err := e.expanders.Expand(ctx, wf, from, out)
	if err != nil {
		return dag.Extension{}, err
	}
	if ext.FromNode != "" && ext.FromNode != from.ID {
		return dag.Extension{}, &ExtensionError{Field: "from_node", Problem: fmt.Sprintf("%q does not match %q", ext.FromNode, from.ID)}
	}
	ext.FromNode = from.ID
	return ext, nil
}

func (e *Executor) runAgent(ctx context.Context, provider string, spec agent.RunSpec) (*agent.RunResult, error) {
	if provider == "" {
		return nil, errors.New("review: provider is required for an agent node")
	}
	if e.deps.Clients != nil {
		client, err := e.deps.Clients.Client(provider)
		if err != nil {
			return nil, err
		}
		spec.Client = client
	} else if e.deps.AgentRun == nil {
		return nil, errors.New("review: no LLM client provider is configured")
	}
	if spec.Submit.Decode == nil {
		return nil, errors.New("review: submit decoder is required")
	}
	if len(spec.Submit.Schema) == 0 {
		return nil, errors.New("review: submit schema is required")
	}
	var lastErr error
	for attempt := 1; attempt <= e.maxAttempts; attempt++ {
		result, err := e.runner(ctx, spec)
		if err == nil {
			return result, nil
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		lastErr = err
		if !retryableTransport(err) || attempt == e.maxAttempts {
			return nil, err
		}
		if sleepErr := e.sleep(ctx, e.backoff*time.Duration(attempt)); sleepErr != nil {
			return nil, sleepErr
		}
	}
	return nil, lastErr
}

func retryableTransport(err error) bool {
	var llmErr *llm.Error
	if errors.As(err, &llmErr) {
		return llmErr.Retryable()
	}
	return false
}

func (e *Executor) dagUsage(provider string, usage llm.Usage) dag.Usage {
	out := dag.Usage{InputTokens: usage.InputTokens, OutputTokens: usage.OutputTokens}
	if e.deps.Cost != nil {
		out.CostMicros = e.deps.Cost(provider, usage)
	}
	return out
}

func (e *Executor) projectBackground() string {
	if e.deps.Dossier == nil {
		return ""
	}
	d := e.deps.Dossier
	var b strings.Builder
	if d.ContextDocs != "" {
		b.WriteString("### 工作区上下文文档（base 版本）\n\n")
		b.WriteString(d.ContextDocs)
		b.WriteString("\n\n")
	}
	if d.RepoDocs.GitLabReviewConfig != "" {
		b.WriteString("### .gitlab-review.yml\n\n")
		b.WriteString(d.RepoDocs.GitLabReviewConfig)
		b.WriteString("\n\n")
	}
	if d.RepoDocs.AGENTS != "" {
		b.WriteString("### AGENTS.md\n\n")
		b.WriteString(d.RepoDocs.AGENTS)
		b.WriteString("\n\n")
	}
	if d.RepoDocs.CLAUDE != "" {
		b.WriteString("### CLAUDE.md\n\n")
		b.WriteString(d.RepoDocs.CLAUDE)
		b.WriteString("\n\n")
	}
	if len(d.CustomRules) > 0 {
		b.WriteString("### 项目规则\n\n")
		for _, rule := range d.CustomRules {
			b.WriteString("- ")
			b.WriteString(rule)
			b.WriteByte('\n')
		}
	}
	return b.String()
}

func (e *Executor) blockBinding(block string) config.BlockBinding {
	if block == "" || e.deps.Blocks == nil {
		return config.BlockBinding{}
	}
	return e.deps.Blocks[block]
}

// agentOptions configures one agent node invocation.
type agentOptions struct {
	Role       string
	Provider   string
	Block      string
	Submit     agent.SubmitSpec
	Task       string
	ExtraTools []agent.Tool
	Builtins   bool
	BlockTools bool
}

// callAgent builds the prompt, tool set, and submit schema, then runs the
// bounded agent loop with transport-only retries.
func (e *Executor) callAgent(ctx context.Context, in dag.NodeInput, opts agentOptions) (*agent.RunResult, error) {
	binding := e.blockBinding(opts.Block)
	var skills []Skill
	tools := append([]agent.Tool(nil), opts.ExtraTools...)
	var skillRoots []string
	if len(binding.Skills) > 0 && e.deps.Skills != nil {
		skillBinding, err := e.deps.Skills.BindSkills(ctx, binding.Skills)
		if err != nil {
			return nil, fmt.Errorf("review: bind skills: %w", err)
		}
		skills = append(skills, skillBinding.Skills...)
		skillRoots = append(skillRoots, skillBinding.Roots...)
		if skillBinding.Tool != nil {
			tools = append(tools, skillBinding.Tool)
		}
	}
	if opts.BlockTools && len(binding.MCPTools) > 0 && e.deps.Tools != nil {
		mcpTools, err := e.deps.Tools.Tools(ctx, opts.Block)
		if err != nil {
			return nil, fmt.Errorf("review: bind MCP tools: %w", err)
		}
		tools = append(tools, mcpTools...)
	}
	built, err := BuildPrompt(PromptInput{
		Bundle:            e.prompts,
		Role:              opts.Role,
		ProjectBackground: e.projectBackground(),
		Skills:            skills,
		Task:              opts.Task,
	})
	if err != nil {
		return nil, err
	}
	system := make([]llm.Block, 0, len(built.System))
	for _, block := range built.System {
		system = append(system, llm.Block{Text: block.Text, Cache: block.Cache})
	}
	sandbox, err := e.sandboxFor(skillRoots)
	if err != nil {
		return nil, err
	}
	if sandbox != nil {
		defer sandbox.Close()
		if opts.Builtins {
			tools = append(tools, agent.BuiltinTools(sandbox)...)
		}
	}
	spec := agent.RunSpec{
		System: system,
		Prompt: built.User,
		Tools:  tools,
		Submit: opts.Submit,
	}
	ctx = llm.WithCallMeta(ctx, llm.CallMeta{WorkflowID: in.WorkflowID, NodeID: in.Node.ID, Turn: 0})
	return e.runAgent(ctx, opts.Provider, spec)
}

func (e *Executor) sandboxFor(skillRoots []string) (*agent.Sandbox, error) {
	var head, base string
	var roots []string
	if e.deps.Dossier != nil {
		head = existingDir(e.deps.Dossier.HeadDir)
		base = existingDir(e.deps.Dossier.BaseDir)
	}
	for _, root := range skillRoots {
		if dir := existingDir(root); dir != "" {
			roots = append(roots, dir)
		}
	}
	if head == "" && base == "" && len(roots) == 0 {
		return nil, nil
	}
	return agent.NewSandbox(head, base, roots...)
}

func existingDir(value string) string {
	if value == "" || !filepath.IsAbs(value) {
		return ""
	}
	info, err := os.Stat(value)
	if err != nil || !info.IsDir() {
		return ""
	}
	return value
}

// providerForNode returns the provider selected by node params or the profile.
func (e *Executor) providerForNode(in dag.NodeInput) string {
	var params struct {
		Provider string `json:"provider"`
	}
	if len(in.Node.Params) > 0 {
		_ = json.Unmarshal(in.Node.Params, &params)
	}
	return params.Provider
}

// requestTimeout is exposed for tests verifying node timeout computation.
func (e *Executor) requestTimeout(spec dag.NodeSpec) time.Duration {
	timeout := spec.Timeout
	if timeout <= 0 {
		timeout = e.defaultTimeout
	}
	return timeout
}
