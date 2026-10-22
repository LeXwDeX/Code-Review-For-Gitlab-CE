# 06 配置

> 配置分三层，后者覆盖前者：**平台配置文件** → **项目设置**（数据库 `projects.settings`，控制台可改）→ **仓库内 `.gitlab-review.yml`**（只读目标分支版本，且只能覆盖白名单内的字段）。

---

## 1. 平台配置文件

默认路径 `config/server.yaml`，可用 `--config` 指定，也可以传多个文件按顺序合并。字符串中的 `${ENV_NAME}` 在加载时展开；密钥类字段**只接受** `*_env` 形式，不允许明文写在文件里。

当前最小启动示例见 [`config/server.example.yaml`](../../config/server.example.yaml)，但它没有 `providers` 和 `profiles.*.propose`，只能启动服务、注册项目和跑路由，**不能执行真实评审**；要跑评审必须先补齐 provider、profile 的 `propose` / `challenge` 等配置。OAuth 可选：不配置时 `/auth/login` 返回 503，Bearer 仍可用。当前存储只实现 SQLite，配置 `postgres` 会在启动时明确报错。管理 API 使用 `auth.bootstrap_token_env`（默认 `REVIEW_ADMIN_TOKEN`，至少 32 字节）；浏览器登录和用户级 API token 已实现。多文件加载递归合并映射、替换数组，拒绝未知字段、重复键和多文档 YAML；环境变量在解析后的字符串上展开，避免改变 YAML 结构。

```yaml
server:
  listen: ":8080"
  public_url: "https://review.example.internal"   # 用于 commit status 的 target_url、OAuth 回调
  read_timeout: 15s
  write_timeout: 60s
  trusted_proxies: ["10.0.0.0/8"]

gitlab:
  url: "https://gitlab.example.internal"
  request_timeout: 30s
  max_concurrency: 16               # 对 GitLab API 的总并发
  bot_usernames: []                 # 额外需要忽略的 bot 用户名（项目 bot 会自动识别）

auth:
  oauth:
    client_id_env: GITLAB_OAUTH_CLIENT_ID
    client_secret_env: GITLAB_OAUTH_CLIENT_SECRET
  bootstrap_admins: ["alice"]       # GitLab 用户名
  session_ttl: 168h

storage:
  driver: sqlite                    # sqlite | postgres
  dsn: "file:data/review.db"
  llm_call_retention: 720h

secrets:
  master_key_env: REVIEW_MASTER_KEY # 32 字节，base64 编码

workspace:
  root: "data/workspaces"
  git_bin: "git"
  context_refresh:
    changed_files: 200
    max_age: 168h
  run_cleanup_after: 1h

jobs:
  workers: 4
  lease: 5m

engine:
  max_concurrent_nodes: 8
  default_node_timeout: 10m

providers:
  claude:
    protocol: anthropic             # anthropic | openai-chat | openai-responses
    base_url: "https://api.anthropic.com"
    model: "claude-opus-5-5"
    api_key_env: ANTHROPIC_API_KEY
    max_concurrency: 4
    rpm: 50
    max_tokens: 16000
    thinking: { budget_tokens: 8000 }
    price: { input_per_mtok: 0, output_per_mtok: 0 }   # USD / 百万 token；用于估算，按实际填写
  gpt:
    protocol: openai-responses
    base_url: "https://api.openai.com/v1"
    model: "<model>"
    api_key_env: OPENAI_API_KEY
    reasoning: { effort: medium }
  local:
    protocol: openai-chat
    base_url: "http://vllm.internal:8000/v1"
    model: "<model>"
    api_key_env: LOCAL_LLM_KEY
    max_concurrency: 8
    temperature: 0.2

profiles:
  default:
    plan:       { mode: code, package_tokens: 60000 }
    propose:    [{ block: code-review, provider: local }]
    challenge:  { provider: claude, only: [P0, P1] }
    on_uncertain: mark              # drop | mark | judge
    synthesize: { mode: code }
    budget:     { max_nodes: 30, max_extends: 0, deadline: 20m, max_tokens: 1500000 }
  deep:
    plan:       { mode: agent, provider: claude, max_packages: 6 }
    propose:
      - { block: review,      provider: claude }
      - { block: review,      provider: gpt }
      - { block: code-review, provider: local }
    reasoner:   { provider: claude, when: auto }   # auto | always | never
    consensus:  { min_votes: 2, same_model_weight: 0.5 }
    challenge:  { provider: cross, fallback: gpt, only: [P0, P1, P2] }
    on_uncertain: judge
    synthesize: { mode: agent, provider: claude }
    budget:     { max_nodes: 30, max_extends: 1, deadline: 40m, max_tokens: 4000000 }
  onboarding:
    agent:      { provider: claude }

mcp:
  config: "config/mcp.json"         # 平台级，Claude Code mcpServers 格式
skills:
  dirs: ["config/skills"]           # 平台级

blocks:                             # 平台级默认绑定；项目可覆盖
  code-review: { mcp_tools: [], skills: [] }
  review:
    mcp_tools: [mcp__codebase-memory__search_graph, mcp__codebase-memory__trace_path, mcp__codebase-memory__get_code_snippet]
    skills: [coding-standards]
  verify:
    mcp_tools: [mcp__codebase-memory__get_code_snippet]
  onboarding:
    mcp_tools: [mcp__codebase-memory__get_architecture, mcp__codebase-memory__search_graph]

review_defaults:                    # 项目设置的默认值（见 §3）
  language: zh-CN
  skip_draft: true
  ignore: ["**/*.lock", "**/vendor/**", "**/node_modules/**", "**/*.pb.go", "**/*_generated.*", "**/*.min.js"]
  max_files: 200
  max_diff_lines: 8000
  commit_status: false
  post_failure_note: true
  show_model_attribution: false
```

### Go 结构体（节选）

```go
type Config struct {
	Server         ServerConfig              `yaml:"server"`
	GitLab         GitLabConfig              `yaml:"gitlab"`
	Auth           AuthConfig                `yaml:"auth"`
	Storage        StorageConfig             `yaml:"storage"`
	Secrets        SecretsConfig             `yaml:"secrets"`
	Workspace      WorkspaceConfig           `yaml:"workspace"`
	Jobs           JobsConfig                `yaml:"jobs"`
	Engine         EngineConfig              `yaml:"engine"`
	Providers      map[string]ProviderConfig `yaml:"providers"`
	Profiles       map[string]ProfileConfig  `yaml:"profiles"`
	MCP            MCPConfig                 `yaml:"mcp"`
	Skills         SkillsConfig              `yaml:"skills"`
	Blocks         map[string]BlockBinding   `yaml:"blocks"`
	ReviewDefaults ProjectSettings           `yaml:"review_defaults"`
}

type ProviderConfig struct {
	Protocol       string          `yaml:"protocol"`
	BaseURL        string          `yaml:"base_url"`
	Model          string          `yaml:"model"`
	APIKeyEnv      string          `yaml:"api_key_env"`
	MaxConcurrency int             `yaml:"max_concurrency"`
	RPM            int             `yaml:"rpm"`
	MaxTokens      int             `yaml:"max_tokens"`
	Temperature    *float64        `yaml:"temperature"`
	Thinking       *ThinkingConfig `yaml:"thinking"`
	Reasoning      *ReasoningConfig `yaml:"reasoning"`
	Price          *PriceConfig    `yaml:"price"`
}

type ProfileConfig struct {
	Plan        PlanConfig        `yaml:"plan"`
	Propose     []ProposeConfig   `yaml:"propose"`
	Reasoner    *ReasonerConfig   `yaml:"reasoner"`
	Consensus   *ConsensusConfig  `yaml:"consensus"`
	Challenge   ChallengeConfig   `yaml:"challenge"`
	OnUncertain string            `yaml:"on_uncertain"`
	Synthesize  SynthesizeConfig  `yaml:"synthesize"`
	Budget      BudgetConfig      `yaml:"budget"`
	Agent       *AgentConfig      `yaml:"agent"` // 仅 onboarding
}
```

### 启动校验（`Config.Validate()`）

- 每个 provider 的 `protocol` 合法，`api_key_env` 对应的环境变量存在且非空。
- profile 中引用的 provider 都存在；`challenge.provider` 为 `cross` 时必须提供 `fallback`。
- `on_uncertain` 为 `judge` 时，`synthesize.mode` 必须是 `agent`。
- `budget.max_nodes` ≤ 100；`max_extends` ≤ 2。
- `blocks` 中的 `mcp_tools` 名称格式为 `mcp__<server>__<tool>`，且 server 在 MCP 配置中存在（工具是否存在在启动后探测，不存在只告警）。
- `secrets.master_key_env` 解码后必须为 32 字节。

校验失败直接退出，并逐条打印错误。

---

## 2. MCP 配置文件

格式与 Claude Code 的 `.mcp.json` 一致，额外允许一个平台扩展字段 `x-review`（Claude Code 会忽略未知字段，所以同一份文件可以两边共用）：

```json
{
  "mcpServers": {
    "codebase-memory": {
      "command": "codebase-memory-mcp",
      "args": ["--tool-profile=analysis"],
      "env": {},
      "x-review": {
        "index_command": ["codebase-memory-mcp", "cli", "--quiet", "index_repository", "{\"repo_path\":\"{base_dir}\"}"],
        "env_allowlist": ["PATH", "HOME"],
        "home": "{workspace}/index/codebase-memory",
        "call_timeout": "60s",
        "max_output_chars": 20000,
        "idle_timeout": "10m"
      }
    }
  }
}
```

| `x-review` 字段 | 含义 |
|---|---|
| `index_command` | onboarding 和刷新时执行的建索引命令；`{base_dir}`、`{workspace}` 为占位符 |
| `env_allowlist` | 子进程可继承的环境变量；默认只有 `PATH` |
| `home` | 为子进程设置的 `HOME`，用于按工作区隔离索引（是否有效需按 ARCH.md §8.4 验证） |
| `call_timeout`、`max_output_chars`、`idle_timeout` | 调用超时、输出上限、空闲回收时间 |

`index_command` 中的工具参数以实际版本的 `codebase-memory-mcp cli --help` 为准。

---

## 3. 项目设置（`projects.settings`）

```json
{
  "language": "zh-CN",
  "skip_draft": true,
  "target_branches": ["main", "release/*"],
  "labels_include": [],
  "labels_exclude": ["no-ai-review"],
  "ignore": ["docs/generated/**"],
  "max_files": 200,
  "max_diff_lines": 8000,
  "commit_status": false,
  "post_failure_note": true,
  "show_model_attribution": false,
  "allow_repo_config": true,
  "allow_repo_skills": false,
  "profile_by_label": { "deep-review": "deep" },
  "custom_rules": [
    "金额计算必须使用 decimal 类型",
    "对外 API 的错误码不得变更"
  ]
}
```

- 未设置的字段取平台 `review_defaults`。
- `profile_by_label`：MR 带指定标签时使用对应档位。
- `custom_rules` 作为项目规则注入 prompt 的项目背景段。
- `ignore` 与平台默认值**合并**，不是替换。

---

## 4. 仓库内 `.gitlab-review.yml`

> 实现偏差：平台 → 项目设置 → 仓库配置的三层优先级只在评审执行阶段由 `resolveReviewConfig` 部分应用。webhook 过滤（Draft、标签、目标分支）在入队前使用项目设置，早于仓库配置的合并；因此仓库级 `skip_draft` / `labels_exclude` 不能阻止已入队的评审。仓库级 `profile` 会在评审时覆盖项目档位。

只读取**目标分支**上的版本；`allow_repo_config=false` 时完全忽略。

```yaml
version: 1
language: zh-CN
profile: default
ignore:
  - "migrations/**"
rules:
  - "所有 SQL 必须参数化"
review:
  skip_draft: true
  labels_exclude: ["wip"]
```

允许覆盖的字段（白名单）：`language`、`profile`（必须是平台已有的档位）、`ignore`（只能追加）、`rules`（追加到 `custom_rules`）、`review.skip_draft`、`review.labels_exclude`。

**不允许**覆盖：provider、预算、commit_status、MCP 和 Skill 绑定。非白名单字段忽略并在汇总中提示一次。

---

## 5. 环境变量

| 变量 | 用途 |
|---|---|
| `REVIEW_MASTER_KEY` | 主密钥（base64 32 字节），加密项目 token 和 webhook secret |
| `REVIEW_ADMIN_TOKEN` | 管理 API 的 bootstrap Bearer token，至少 32 字节；可由 `auth.bootstrap_token_env` 改名 |
| `GITLAB_OAUTH_CLIENT_ID` / `GITLAB_OAUTH_CLIENT_SECRET` | 控制台登录 |
| `ANTHROPIC_API_KEY`、`OPENAI_API_KEY`、… | 按 `providers.*.api_key_env` 自定义 |
| `REVIEW_LOG_LEVEL` | `debug` / `info` / `warn` / `error` |
| `REVIEW_LOG_FORMAT` | `json`（默认）/ `text` |

---

## 6. prompt 覆盖

`prompts/` 目录 embed 进二进制。部署时可设置 `prompts_dir` 指向外部目录，同名文件优先使用外部版本；每个 prompt 文件首行写版本号注释（如 `<!-- v3 -->`），版本号参与 fingerprint 计算。
