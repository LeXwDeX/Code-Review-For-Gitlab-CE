# 03 HTTP API

> 服务对外提供三类接口：GitLab webhook、管理 API（Web 控制台和 `reviewctl` 共用）、健康检查。
> 管理 API 前缀 `/api/v1`，请求和响应都是 JSON（SSE 除外）。

> M0–M6 的本地实现已覆盖 webhook、健康检查、OAuth 会话、管理 API 和 API token。`auth.bootstrap_token_env` 指向的管理员 Bearer token 仍可用于脚本；浏览器登录需要配置 `auth.oauth`。真实 GitLab / OAuth 未联调。实现与本文的偏差在 §4.1–§4.4 内逐条标注。

---

## 1. 通用约定

### 认证

| 调用方 | 方式 |
|---|---|
| GitLab webhook | `X-Gitlab-Token` 与项目的 webhook secret 比对（常量时间比较） |
| Web 控制台 | GitLab OAuth 登录后下发会话 cookie（`HttpOnly`、`Secure`、`SameSite=Lax`）；写操作要求 `X-CSRF-Token` |
| `reviewctl` / 脚本 | `Authorization: Bearer <api_token>` |

角色：`admin` 可读写全部；`viewer` 只读。首个管理员由配置 `auth.bootstrap_admins`（GitLab 用户名列表）指定。

### 错误格式

```json
{ "error": { "code": "not_found", "message": "project 42 not registered", "request_id": "01J…" } }
```

| HTTP | code |
|---|---|
| 400 | `bad_request`、`validation_failed`（附 `details` 字段数组） |
| 401 | `unauthenticated` |
| 403 | `forbidden` |
| 404 | `not_found` |
| 409 | `conflict`（如工作流已在运行、项目已注册） |
| 422 | `gitlab_rejected`（GitLab 返回错误，附原始状态码） |
| 500 | `internal` |

### 分页

列表接口用游标：`?limit=50&cursor=<opaque>`，响应带 `next_cursor`（没有更多时为空）。

---

## 2. Webhook

### `POST /webhooks/gitlab`

请求头：`X-Gitlab-Event`、`X-Gitlab-Token`、`X-Gitlab-Event-UUID`、`X-Gitlab-Instance`。

处理顺序：

1. 读取 body（上限 5 MB），解析出 `project.id`。
2. 查项目；未注册或已禁用 → `200 {"ignored":"project_not_registered"}`。
3. 校验 `X-Gitlab-Token`；不通过 → `401`。
4. `X-Gitlab-Event-UUID` 缺失或非法时忽略；同一项目内已存在于 `webhook_deliveries` → `200 {"ignored":"duplicate"}`。
5. 按事件类型过滤（见下表），命中则入队，返回 `200 {"accepted":true,"job_id":"…"}`；否则 `200 {"ignored":"<reason>"}`。

**除了认证失败，一律返回 200**，避免 GitLab 因连续失败自动禁用 hook。整个处理要求在 1 秒内完成，不做任何 GitLab 或 LLM 调用。

| 事件 | 条件 | 入队 |
|---|---|---|
| `Merge Request Hook` | `action` 为 `open` / `reopen` | `review.run`，trigger=`mr_open` |
| `Merge Request Hook` | `action=update` 且带 `oldrev` | `review.run`，trigger=`mr_update` |
| `Merge Request Hook` | `action` 为 `close` / `merge` | `review.cancel` |
| `Merge Request Hook` | `draft=true` 且项目设置跳过 Draft | 忽略：`draft` |
| `Note Hook` | `noteable_type=MergeRequest` 且正文以 `/review` 开头 | 按命令入队（见 §6） |
| `Push Hook` | `ref` 为目标分支（默认分支或设置中的分支） | `workspace.refresh` |

`review.run` 的 job key：`review:<project_id>:<mr_iid>`。去重记录与入队在同一事务中提交，新 webhook 就地替换同 key、同 kind 的 queued 内容；running 内容不变，允许保留一个 queued 后继。取消任务使用独立 key `review.cancel:<project_id>:<mr_iid>`。工作流取消由评审处理器负责（见 05-flows.md F2）；webhook handler 本身不执行评审。

Note Hook 先入队 `review.command`，由 worker 请求 GitLab 校验身份、MR 状态和权限后入队 run/cancel；这样 webhook handler 不等待网络调用。通过校验的 run 命令使用最新参数替换已有 queued run，保留 `full`、`profile` 和最新 `head_sha`；不覆盖 running 任务。`/review help` 仍返回 `ignored: help_not_implemented`；命令处理器不回复 GitLab 评论，也不添加表情回执。数据库失败返回 `ignored: storage_error`，不会写入去重记录或假报 accepted；由于 HTTP 仍是 200，运维需监控并重放失败事件。

---

## 3. 认证与当前用户

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/auth/login` | 跳转 GitLab OAuth（scope：`read_user`） |
| GET | `/auth/callback` | 回调；创建或更新 `users`，写会话 |
| POST | `/auth/logout` | 删除会话 |
| GET | `/api/v1/me` | `{id, username, name, role, csrf_token}`；CSRF token 仅在内存使用，不保存到 localStorage |
| GET | `/api/v1/api-tokens` | 当前用户的 token 列表（不含明文） |
| POST | `/api/v1/api-tokens` | `{name, expires_in_days}` → `{id, token}`，明文只返回这一次 |
| DELETE | `/api/v1/api-tokens/{id}` | 吊销 |

---

## 4. 管理 API

### 4.1 项目

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/api/v1/projects` | 列表：`?q=&enabled=` |
| POST | `/api/v1/projects` | 注册项目 |
| GET | `/api/v1/projects/{id}` | 详情（含工作区状态、最近评审） |
| PATCH | `/api/v1/projects/{id}` | 修改 `profile`、`settings`、`enabled` |
| PUT | `/api/v1/projects/{id}/token` | 轮换 token：`{token}` |
| POST | `/api/v1/projects/{id}/webhook` | （重新）创建 GitLab webhook |
| DELETE | `/api/v1/projects/{id}` | 注销：删除 webhook（若由本服务创建）、清理工作区 |

注册请求：

```json
POST /api/v1/projects
{
  "project": "group/backend-api",
  "token": "glpat-…",
  "profile": "default",
  "auto_webhook": true,
  "settings": { "skip_draft": true, "target_branches": ["main", "release/*"] }
}
```

服务端校验步骤：

1. 用 token 调 `GET /projects/:id` 确认可访问；读出 `default_branch`、`web_url`。
2. 调 `GET /personal_access_tokens/self` 读取 scope 和过期时间；缺少 `api` 或 `read_repository` → `400 validation_failed`。
3. `auto_webhook=true` 时调 `POST /projects/:id/hooks` 创建 hook（需要 Maintainer），勾选 Merge request、Comments、Push 事件，写入生成的 secret。
4. 入库后入队 `workspace.onboard`。

注册响应：`201 {project, job_id, webhook_secret?}`。`auto_webhook` 默认 false，此时只在注册响应中返回一次 `webhook_secret`，供手工配置 webhook；自动创建时不返回。项目列表支持 `q` / `enabled` 筛选，但仍不分页。

实现偏差：`GET /api/v1/projects/{id}` 当前只返回项目行，不包含工作区状态和最近评审。

### 4.2 评审（工作流）

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/api/v1/reviews` | 列表：`?project_id=&mr_iid=&status=&verdict=&profile=&trigger=&since=`；所有筛选在服务端分页前应用 |
| POST | `/api/v1/projects/{id}/merge_requests/{iid}/reviews` | 手动触发：`{profile?, full?}` → `202 {job_id}` |
| GET | `/api/v1/reviews/{wf_id}` | 工作流详情：基本信息 + 节点 + finding 统计 |
| GET | `/api/v1/reviews/{wf_id}/graph` | 节点和边，供 DAG 视图渲染 |
| GET | `/api/v1/reviews/{wf_id}/findings` | finding 列表：`?decision=&severity=` |
| GET | `/api/v1/reviews/{wf_id}/nodes/{node_id}` | 节点详情：参数、输出、错误 |
| GET | `/api/v1/reviews/{wf_id}/nodes/{node_id}/calls` | 该节点的 LLM 调用（请求和响应按需展开） |
| GET | `/api/v1/reviews/{wf_id}/stream` | SSE：节点状态变化、finding 新增、发布结果 |
| POST | `/api/v1/reviews/{wf_id}/cancel` | 取消 |
| POST | `/api/v1/reviews/{wf_id}/republish` | fingerprint 仍有效时重新渲染并发布 |

工作流详情示例：

```json
{
  "id": "0192f0c1-…",
  "project_id": 42,
  "mr_iid": 318,
  "mr_title": "feat: 支持批量导入",
  "head_sha": "9f3c2a1…",
  "profile": "deep",
  "trigger": "mr_update",
  "status": "completed",
  "verdict": "BLOCKING",
  "nodes": [
    { "id": "plan", "kind": "plan", "status": "completed", "provider": "claude", "duration_ms": 8120 },
    { "id": "review.std.p1", "kind": "review.standards", "status": "completed", "provider": "claude" },
    { "id": "verify.f7", "kind": "verify", "status": "completed", "provider": "gpt" }
  ],
  "findings_summary": { "published": 4, "marked": 1, "dropped": 3, "by_severity": { "P0": 0, "P1": 2, "P2": 3 } },
  "usage": { "tokens_in": 182340, "tokens_out": 12011, "cost_micros": 0 },
  "publications": { "summary_note_id": 99102, "discussions": 4, "commit_status": "failed" },
  "created_at": "…", "finished_at": "…"
}
```

SSE 事件（`event:` 字段）：`node`（`{id, status, attempt}`）、`finding`（`{id, decision}`）、`workflow`（`{status, verdict}`）、`published`（`{kind, gitlab_id}`）。

实现偏差：工作流详情不返回 `mr_title`；`publications` 只返回 `summary_note_id` 和 `discussions`，不返回 `commit_status`。控制台对缺失字段显示占位。SSE 由服务端每 2 秒轮询数据库生成，不是事件推送。

图响应为 `{nodes, edges}`。节点包含 `id`、`kind`、`status`、`depends_on`，以及可选的 `provider`、`attempt`；边为 `{from, to}`。节点 ID 允许点号，必须作为完整路径参数编码，不能按点号拆分。节点详情直接返回对象，不增加 `node` 包装层。

### 4.3 工作区与上下文文档

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/api/v1/projects/{id}/workspace` | 状态、base_sha、索引状态、最近刷新 |
| POST | `/api/v1/projects/{id}/workspace/refresh` | `{regenerate_context?: bool, reindex?: bool}` → `202 {job_id}` |
| GET | `/api/v1/projects/{id}/context-docs` | 列表：`?kind=` |
| GET | `/api/v1/projects/{id}/context-docs/current` | 当前生效版本（`?kind=agents`） |
| PUT | `/api/v1/projects/{id}/context-docs/current` | 人工编辑：`{kind, content, locked}`，生成一条 `source=manual` 的新版本 |
| POST | `/api/v1/projects/{id}/context-docs/current/lock` | `{locked: bool}` |

`current` 的读取、保存与锁定均用 `?kind=agents|claude` 选择文档，默认 `agents`。保存请求同时给出 `kind` 时，必须与查询参数一致。工作区和当前文档直接返回对象；文档列表用 `{documents, next_cursor?}`。

实现偏差：`PUT .../context-docs/current` 当前就地更新 `(project_id, base_sha, kind)` 的当前行，不新增 `source=manual` 版本。工作区响应包含 `state`、`base_sha`、`index_sha`、`context_sha`、`last_refresh_at`、`last_error`，不包含 `index_status`。

### 4.4 扩展：MCP、Skill、块绑定

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/api/v1/mcp-servers` | `?scope=&project_id=` |
| POST | `/api/v1/mcp-servers` | `{scope, project_id?, name, config}`；`config` 为单个 Claude Code `mcpServers` 条目 |
| PUT / DELETE | `/api/v1/mcp-servers/{id}` | 修改 / 删除 |
| POST | `/api/v1/mcp-servers/{id}/test` | 启动并列出工具：`{tools:[{name, description}]}` |
| POST | `/api/v1/mcp-servers/import` | 粘贴整份 `{"mcpServers": {...}}` 批量导入 |
| GET | `/api/v1/skills` | `?scope=&project_id=` |
| POST | `/api/v1/skills` | multipart 上传 zip（含 `SKILL.md`），或 `{scope, dir}` 登记服务器上的目录 |
| DELETE | `/api/v1/skills/{id}` | 删除 |
| GET | `/api/v1/block-bindings` | `?scope=&project_id=` |
| PUT | `/api/v1/block-bindings/{block}` | `{scope, project_id?, mcp_tools: [], skills: []}` |

导入 MCP 时，查询参数指定 `scope`、`project_id`，body 直接使用 `{"mcpServers": {...}}`，不增加包装层。项目作用域必须有有效的项目 ID。MCP 更新允许只传 `name` 或 `enabled`；传入 `config` 则完整替换配置。读取时敏感 env/header 值脱敏，前端不得把脱敏占位符当作密钥保存。

Skill zip 上传字段为 `file`，另带 `scope`、可选 `project_id`；名称和描述由服务端从 `SKILL.md` 读取。目录登记请求为 `{scope, project_id?, dir}`。创建成功直接返回 Skill 对象。

实现偏差：MCP、Skill、块绑定列表未分页。Skill zip 上传已实现，请求体上限 8 MiB，归档内单文件上限 4 MiB、总解压上限 16 MiB、最多 512 个条目，`SKILL.md` 读取上限 64 KiB；目录登记同样受单文件和总量限制。

### 4.5 系统

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/api/v1/providers` | provider 列表（**不含** API key）及近 24 小时调用统计 |
| POST | `/api/v1/providers/{name}/test` | 发一条最小请求验证连通性和模型名 |
| GET | `/api/v1/profiles` | 评审档位定义（来自配置文件，只读） |
| GET | `/api/v1/stats` | `?from=&to=&project_id=`：运行数、耗时、成本、finding 处置分布、各模型准确率 |
| GET | `/api/v1/settings` | 平台配置的只读视图（脱敏） |

首版 provider 和 profile 以配置文件为准，API 只读；改动需修改配置并重启或热加载（`SIGHUP`）。

控制台集合响应按资源命名：`{projects}`、`{reviews}`、`{findings}`、`{servers}`、`{skills}`、`{bindings}`、`{providers}`、`{profiles}`、`{calls}`、`{documents}`、`{tokens}`；可分页的集合另带 `next_cursor`。不提供未发布的旧字段别名。

`stats` 使用字段 `runs`、`blocking`、`blocking_ratio`、`published`、`overturned_ratio`、`avg_duration_ms`、`p95_duration_ms`、`cost_micros`、`by_project`、`by_model`、`decisions`。无法计算的比例和耗时返回 `null`，界面显示「不可用」，不能伪装成零错误或零耗时。

比例字段（包括 provider 的 `error_rate`）统一为 `[0, 1]` 的小数，不混用百分数。`cost_micros` 是按配置价格估算的微美元（1 USD = 1,000,000 micros），不是 provider 账单核对结果。未配置价格时不可把估算零值解释为免费。

Provider 返回 `name`、`protocol`、`base_url`、`model`、`max_concurrency`、`rpm` 及 `stats_24h`（`calls`、`errors`、`error_rate`、`p50_latency_ms`），不返回 API key。连通性测试必须返回 `ok: boolean`，可附 `message`、`model`、`latency_ms`；缺少 `ok` 不能当作成功。

---

## 5. 健康检查与指标

| 路径 | 说明 |
|---|---|
| `GET /healthz` | 进程存活 |
| `GET /readyz` | 数据库可用、job worker 已启动 |
| `GET /metrics` | Prometheus 指标（可选，默认关闭）：工作流数与耗时、节点失败率、LLM 延迟与 token、队列长度 |

---

## 6. MR 评论命令

在 MR 下发表以 `/review` 开头的评论：

| 命令 | 作用 |
|---|---|
| `/review` | 用项目默认 profile 重新评审（增量） |
| `/review full` | 全量评审 |
| `/review <profile>` | 指定档位，如 `/review deep` |
| `/review cancel` | 取消正在运行的评审 |
| `/review help` | 回复命令说明 |

权限：发起人在该项目的访问级别必须 **≥ Developer（30）**，否则回复一条说明并忽略。机器人自己的评论（作者是项目 bot 用户）一律忽略，防止循环触发。
