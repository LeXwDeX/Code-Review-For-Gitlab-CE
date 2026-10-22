# 02 数据模型

> 以 SQLite 方言给出 DDL，Postgres 差异见 §4。迁移文件放在 `internal/store/migrations/NNNN_name.sql`，按序号执行。
> 时间字段存 RFC 3339 UTC 字符串；JSON 字段存 TEXT（Postgres 用 JSONB）。

---

## 1. 表一览

| 表 | 聚合 | 说明 |
|---|---|---|
| `projects` | 项目 | 注册信息、加密 token、项目级设置 |
| `workspaces` | 项目 | 工作区状态 |
| `context_docs` | 项目 | 生成或人工维护的上下文文档 |
| `mcp_servers`、`skills`、`block_bindings` | 扩展 | MCP、Skill 注册与按块授权 |
| `webhook_deliveries` | 入口 | webhook 去重 |
| `jobs` | 入口 | 持久化任务队列 |
| `workflows`、`workflow_nodes`、`events` | 工作流 | DAG 读模型与事件日志 |
| `findings` | 评审结果 | 统一 finding、挑战结果、裁决 |
| `publications` | 发布 | 已发布到 GitLab 的对象，用于幂等 |
| `llm_calls` | 审计 | 每次模型调用 |
| `feedback` | 反馈 | 👍/👎、resolve 信号 |
| `users`、`sessions`、`api_tokens` | 访问控制 | Web 控制台登录与 CLI token；`sessions` 表当前未使用 |

---

## 2. DDL（SQLite）

```sql
-- 0001_init.sql

CREATE TABLE projects (
  id                  INTEGER PRIMARY KEY,          -- GitLab project_id
  path_with_namespace TEXT    NOT NULL,
  web_url             TEXT    NOT NULL,
  default_branch      TEXT    NOT NULL,
  token_enc           BLOB    NOT NULL,             -- AES-GCM: nonce || ciphertext
  token_expires_at    TEXT,
  token_scopes        TEXT    NOT NULL DEFAULT '[]',-- JSON 数组，注册时从 GitLab 读取
  webhook_secret_enc  BLOB    NOT NULL,
  hook_id             INTEGER,                      -- 自动创建 webhook 时记录
  profile             TEXT    NOT NULL DEFAULT 'default',
  settings            TEXT    NOT NULL DEFAULT '{}',-- JSON，结构见 06-config.md §3
  enabled             INTEGER NOT NULL DEFAULT 1,
  created_at          TEXT    NOT NULL,
  updated_at          TEXT    NOT NULL
);

CREATE TABLE workspaces (
  project_id      INTEGER PRIMARY KEY REFERENCES projects(id) ON DELETE CASCADE,
  state           TEXT    NOT NULL,   -- new | onboarding | ready | refreshing | error
  base_sha        TEXT,
  index_sha       TEXT,               -- 外部索引对应的 sha
  context_sha     TEXT,               -- 当前生效上下文文档对应的 sha
  last_refresh_at TEXT,
  last_error      TEXT,
  updated_at      TEXT    NOT NULL
);

CREATE TABLE context_docs (
  id          TEXT    PRIMARY KEY,
  project_id  INTEGER NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
  base_sha    TEXT    NOT NULL,
  kind        TEXT    NOT NULL,       -- agents | claude
  source      TEXT    NOT NULL,       -- generated | manual
  content     TEXT    NOT NULL,
  locked      INTEGER NOT NULL DEFAULT 0,
  model       TEXT,
  workflow_id TEXT,
  created_at  TEXT    NOT NULL,
  updated_by  INTEGER,                -- users.id，人工编辑时
  UNIQUE (project_id, base_sha, kind)
);

CREATE TABLE mcp_servers (
  id         TEXT    PRIMARY KEY,
  scope      TEXT    NOT NULL,        -- platform | project
  project_id INTEGER REFERENCES projects(id) ON DELETE CASCADE,
  name       TEXT    NOT NULL,
  config     TEXT    NOT NULL,        -- 单个 mcpServers 条目的 JSON（Claude Code 格式）
  enabled    INTEGER NOT NULL DEFAULT 1,
  created_at TEXT    NOT NULL,
  updated_at TEXT    NOT NULL,
  UNIQUE (scope, project_id, name)
);

CREATE TABLE skills (
  id          TEXT    PRIMARY KEY,
  scope       TEXT    NOT NULL,       -- platform | project
  project_id  INTEGER REFERENCES projects(id) ON DELETE CASCADE,
  name        TEXT    NOT NULL,
  description TEXT    NOT NULL,
  dir         TEXT    NOT NULL,       -- 服务器上的目录
  enabled     INTEGER NOT NULL DEFAULT 1,
  created_at  TEXT    NOT NULL,
  UNIQUE (scope, project_id, name)
);

CREATE TABLE block_bindings (
  scope      TEXT    NOT NULL,        -- platform | project
  project_id INTEGER NOT NULL DEFAULT 0,
  block      TEXT    NOT NULL,        -- code-review | review | reasoner | verify | synthesize | onboarding
  mcp_tools  TEXT    NOT NULL DEFAULT '[]',
  skills     TEXT    NOT NULL DEFAULT '[]',
  PRIMARY KEY (scope, project_id, block)
);

CREATE TABLE webhook_deliveries (
  uuid        TEXT    NOT NULL,       -- X-Gitlab-Event-UUID，按项目去重
  project_id  INTEGER NOT NULL,
  event       TEXT    NOT NULL,       -- Merge Request Hook | Note Hook | Push Hook
  outcome     TEXT    NOT NULL,       -- enqueued | ignored | rejected
  reason      TEXT,
  job_id      TEXT,
  received_at TEXT    NOT NULL,
  PRIMARY KEY (project_id, uuid)
);

CREATE TABLE jobs (
  id          TEXT    PRIMARY KEY,
  kind        TEXT    NOT NULL,
  key         TEXT,
  payload     TEXT    NOT NULL,
  state       TEXT    NOT NULL,       -- queued | running | done | failed | cancelled
  attempts    INTEGER NOT NULL DEFAULT 0,
  max_attempts INTEGER NOT NULL DEFAULT 3,
  run_after   TEXT    NOT NULL,
  lease_owner TEXT,
  lease_until TEXT,
  last_error  TEXT,
  created_at  TEXT    NOT NULL,
  updated_at  TEXT    NOT NULL
);
CREATE INDEX jobs_claim ON jobs (state, run_after);
-- 运行期间的新事件保留为一个排队后继，不被旧任务去重丢弃。
CREATE UNIQUE INDEX jobs_active_key ON jobs (key, state)
  WHERE key IS NOT NULL AND state IN ('queued', 'running');

CREATE TABLE workflows (
  id           TEXT    PRIMARY KEY,
  kind         TEXT    NOT NULL,      -- review | onboarding
  project_id   INTEGER NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
  mr_iid       INTEGER,
  head_sha     TEXT,
  base_sha     TEXT,
  fingerprint  TEXT,
  profile      TEXT,
  trigger      TEXT    NOT NULL,      -- mr_open | mr_update | command | api | push | register
  triggered_by INTEGER,               -- GitLab user id（命令或 API 触发时）
  status       TEXT    NOT NULL,      -- pending | running | completed | failed | cancelled
  graph_rev    INTEGER NOT NULL DEFAULT 0,
  extends_used INTEGER NOT NULL DEFAULT 0,
  verdict      TEXT,                  -- PASS | BLOCKING
  cancel_reason TEXT,
  error        TEXT,
  tokens_in    INTEGER NOT NULL DEFAULT 0,
  tokens_out   INTEGER NOT NULL DEFAULT 0,
  cost_micros  INTEGER NOT NULL DEFAULT 0,
  created_at   TEXT    NOT NULL,
  started_at   TEXT,
  finished_at  TEXT
);
CREATE INDEX workflows_mr ON workflows (project_id, mr_iid, created_at DESC);
CREATE INDEX workflows_status ON workflows (status, created_at DESC);
CREATE UNIQUE INDEX workflows_active_review
  ON workflows (project_id, mr_iid)
  WHERE kind = 'review' AND status IN ('pending', 'running');

CREATE TABLE workflow_nodes (
  workflow_id TEXT    NOT NULL REFERENCES workflows(id) ON DELETE CASCADE,
  id          TEXT    NOT NULL,
  kind        TEXT    NOT NULL,
  status      TEXT    NOT NULL,
  required    INTEGER NOT NULL,
  depends_on  TEXT    NOT NULL DEFAULT '[]',
  params      TEXT    NOT NULL DEFAULT '{}',
  attempt     INTEGER NOT NULL DEFAULT 0,
  provider    TEXT,
  model       TEXT,
  output      TEXT,                   -- 通过 schema 校验后的 JSON
  error       TEXT,
  tokens_in   INTEGER NOT NULL DEFAULT 0,
  tokens_out  INTEGER NOT NULL DEFAULT 0,
  cost_micros INTEGER NOT NULL DEFAULT 0,
  added_rev   INTEGER NOT NULL DEFAULT 0, -- 由哪次 graph_rev 加入（fan_out / extend）
  queued_at   TEXT,
  started_at  TEXT,
  finished_at TEXT,
  PRIMARY KEY (workflow_id, id)
);

CREATE TABLE events (
  workflow_id TEXT    NOT NULL,
  seq         INTEGER NOT NULL,       -- 每个工作流内自增，等于写入后的 graph_rev
  type        TEXT    NOT NULL,
  payload     TEXT    NOT NULL,
  at          TEXT    NOT NULL,
  PRIMARY KEY (workflow_id, seq)
);

CREATE TABLE findings (
  id                 TEXT    PRIMARY KEY,
  workflow_id        TEXT    NOT NULL REFERENCES workflows(id) ON DELETE CASCADE,
  origin_node        TEXT    NOT NULL, -- 产生它的节点；主 agent 自提为 synthesize 节点
  origin_index       INTEGER NOT NULL, -- 在该节点输出 findings 数组中的下标
  origin_provider    TEXT,
  severity           TEXT    NOT NULL, -- P0 | P1 | P2 | info（归一化后）
  category           TEXT    NOT NULL,
  path               TEXT,
  line               INTEGER,
  side               TEXT,             -- new | old
  title              TEXT    NOT NULL,
  body               TEXT    NOT NULL,
  evidence           TEXT,
  confidence         TEXT,             -- high | medium | low
  dedupe_group       TEXT,             -- 合并后的组 ID
  votes              REAL    NOT NULL DEFAULT 1, -- 拓扑 B 的加权票数
  challenge_status   TEXT,             -- confirmed | refuted | uncertain | skipped
  challenge_node     TEXT,
  challenge_evidence TEXT,
  decision           TEXT,             -- publish | drop | mark | downgrade
  decision_reason    TEXT,
  final_severity     TEXT,
  position_ok        INTEGER,          -- I2：行号是否落在 diff 内
  publish_key        TEXT              -- 跨次运行去重：hash(path, 规范化标题, 行附近内容)
);
CREATE INDEX findings_wf ON findings (workflow_id);

CREATE TABLE publications (
  id          TEXT    PRIMARY KEY,
  project_id  INTEGER NOT NULL,
  mr_iid      INTEGER NOT NULL,
  kind        TEXT    NOT NULL,       -- summary | discussion | commit_status | failure_note
  publish_key TEXT    NOT NULL,       -- summary 固定为 'summary'
  gitlab_id   TEXT,                   -- note_id / discussion_id
  workflow_id TEXT    NOT NULL,
  fingerprint TEXT    NOT NULL,
  body_hash   TEXT,
  created_at  TEXT    NOT NULL,
  updated_at  TEXT    NOT NULL,
  UNIQUE (project_id, mr_iid, kind, publish_key)
);

CREATE TABLE llm_calls (
  id            TEXT    PRIMARY KEY,
  workflow_id   TEXT    NOT NULL,
  node_id       TEXT    NOT NULL,
  turn          INTEGER NOT NULL,
  provider      TEXT    NOT NULL,
  model         TEXT    NOT NULL,
  request       BLOB,                 -- gzip(JSON)，保留期可配置
  response      BLOB,                 -- gzip(JSON)
  input_tokens  INTEGER NOT NULL DEFAULT 0,
  output_tokens INTEGER NOT NULL DEFAULT 0,
  cache_read    INTEGER NOT NULL DEFAULT 0,
  cache_write   INTEGER NOT NULL DEFAULT 0,
  latency_ms    INTEGER NOT NULL,
  stop_reason   TEXT,
  error_kind    TEXT,
  request_id    TEXT,
  created_at    TEXT    NOT NULL
);
CREATE INDEX llm_calls_node ON llm_calls (workflow_id, node_id, turn);

CREATE TABLE feedback (
  id             TEXT    PRIMARY KEY,
  project_id     INTEGER NOT NULL,
  mr_iid         INTEGER NOT NULL,
  publication_id TEXT    NOT NULL,
  finding_id     TEXT,
  signal         TEXT    NOT NULL,    -- thumbs_up | thumbs_down | resolved_changed | resolved_unchanged
  gitlab_user_id INTEGER,
  observed_at    TEXT    NOT NULL,
  UNIQUE (publication_id, signal, gitlab_user_id)
);

CREATE TABLE users (
  id            INTEGER PRIMARY KEY,  -- GitLab user id
  username      TEXT    NOT NULL,
  name          TEXT    NOT NULL,
  role          TEXT    NOT NULL,     -- admin | viewer
  last_login_at TEXT
);

CREATE TABLE sessions (
  id         TEXT    PRIMARY KEY,     -- 随机 256 bit，cookie 中只存它
  user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  expires_at TEXT    NOT NULL,
  created_at TEXT    NOT NULL
);
```

> 实现偏差：当前浏览器会话保存在服务进程内存中（`internal/server/auth.go`），不读写 `sessions` 表。重启后所有浏览器会话失效，用户需要重新登录。

```sql
CREATE TABLE api_tokens (
  id           TEXT    PRIMARY KEY,
  user_id      INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  name         TEXT    NOT NULL,
  token_hash   BLOB    NOT NULL,      -- SHA-256；明文只在创建时显示一次
  last_used_at TEXT,
  expires_at   TEXT,
  created_at   TEXT    NOT NULL
);
```

---

## 3. 事件 payload

事件类型前缀统一为 `dag.`，payload 为 JSON。

| 事件 | payload |
|---|---|
| `dag.workflow.created` | `{spec: GraphSpec, meta}` |
| `dag.workflow.started` | `{}` |
| `dag.workflow.extended` | `{nodes: [NodeSpec], source: "fan_out" \| "extend", from_node}` |
| `dag.workflow.completed` | `{}` |
| `dag.workflow.failed` | `{reason, node_id?}` |
| `dag.workflow.cancelled` | `{reason}`（如 `superseded_by:<wf_id>`、`user`、`deadline`） |
| `dag.node.registered` | `{node_id}`（随 created / extended 批量写入） |
| `dag.node.queued` | `{node_id, attempt}` |
| `dag.node.started` | `{node_id, attempt, provider?, model?}` |
| `dag.node.completed` | `{node_id, attempt, output, usage}` |
| `dag.node.failed` | `{node_id, attempt, error, error_kind}` |
| `dag.node.aborted` | `{node_id, attempt, reason}` |
| `dag.node.skipped` | `{node_id, reason}`（`condition_false` \| `deps_skipped` \| `workflow_cancelled`） |
| `dag.node.cancelled` | `{node_id, reason}`（被 extend 替换） |
| `dag.node.restarted` | `{node_id, next_attempt}`（崩溃恢复） |

规则：

- 写事件和更新读模型（`workflows`、`workflow_nodes`）在**同一个数据库事务**里完成。
- `events.seq` 即写入后的 `graph_rev`；`Append` 时带 `expectRev`，不一致返回 `ErrStaleRevision`。
- 迟到的节点结果（attempt 不等于当前 attempt）直接丢弃，并记 debug 日志。

---

## 4. Postgres 差异

| SQLite | Postgres |
|---|---|
| `INTEGER PRIMARY KEY`（project_id） | `BIGINT PRIMARY KEY` |
| `BLOB` | `BYTEA` |
| 时间 `TEXT`（RFC 3339） | `TIMESTAMPTZ` |
| JSON `TEXT` | `JSONB` |
| `REAL` | `DOUBLE PRECISION` |
| 部分索引 `WHERE ...` | 相同语法 |

SQLite 必要的 PRAGMA：`journal_mode=WAL`、`busy_timeout=5000`、`foreign_keys=ON`、`synchronous=NORMAL`。写操作走单连接或串行化，读用连接池。

---

## 5. 保留与清理

> 实现状态：定时清理已实现（`internal/app/retention_service.go`，默认每 6 小时）。`llm_calls` 超过保留期时删除整行，而不只是清空 payload；项目注销时的关联删除属于注销路径，不是保留策略。

| 数据 | 默认保留 | 配置 | 清理方式 |
|---|---|---|---|
| `llm_calls` | 30 天 | `storage.llm_call_retention` | 定时删除整行 |
| `webhook_deliveries` | 14 天 | `storage.delivery_retention` | 定时删除 |
| `jobs`（done/failed/cancelled） | 7 天 | `storage.job_retention` | 定时删除；queued/running 永不删除 |
| `events`、`workflows`、`findings` | 长期 | — | 不清理；按需归档 |
| `context_docs` | 每项目每种保留最近 5 个未锁定版本 + 所有 locked | `storage.context_versions` | 定时按 `(project_id, kind)` 窗口清理 |
