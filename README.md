# Code Review for GitLab CE

基于 Go 的 GitLab CE MR 自动评审服务，使用 MIT 协议。

当前已完成 **M0–M6 的本地实现**：项目注册与 webhook（M0）、三种 LLM 协议、agent 工具循环与 GitLab 采集（M1/M2）、DAG 引擎与持久化（M2）、主 agent 编排（plan / fan-out / synthesize）与对抗核验、裁决、发布（M3/M4）、工作区 onboarding/刷新、MCP/Skill 与项目级覆盖（M5）、OAuth 会话、角色与 CSRF、管理 API、React 控制台、API token、反馈收集与许可证清单（M6）。

**未与真实 GitLab CE、真实模型 provider、真实 OAuth 登录、真实 MCP 服务器或真实 `index_command` 联调**。现有验证使用本地假 GitLab、录制回放、临时 SQLite 和静态控制台冒烟，不能替代部署联调。

总体目标见 [ARCH.md](ARCH.md)，实现层面的设计与已知偏差见 [详细设计](docs/design/README.md)。

## 本地启动

需要 Go 1.26 或更新版本。SQLite 使用纯 Go 驱动，不需要 C 编译器。控制台产物通过 `//go:embed all:dist`（`web/embed.go`）打进二进制，因此**编译或运行前要先构建前端**：

```bash
cd web && npm ci && npm run build && cd ..
export REVIEW_MASTER_KEY="$(openssl rand -base64 32)"
export REVIEW_ADMIN_TOKEN="$(openssl rand -hex 32)"
go run ./cmd/server --config config/server.example.yaml --check-config
go run ./cmd/server --config config/server.example.yaml
```

全新检出没有 `web/dist` 时，`go build ./...` / `go run ./cmd/server` 会因 embed 找不到目录而失败。先执行 `cd web && npm ci && npm run build`。

示例配置可以启动服务，并提供 Bearer 认证。浏览器控制台登录另外需要 GitLab OAuth application，并在配置中设置 `auth.oauth.client_id_env` / `client_secret_env`（示例配置未包含 `auth.oauth`，`/auth/login` 会返回 503）：

```bash
export GITLAB_OAUTH_CLIENT_ID="..."
export GITLAB_OAUTH_CLIENT_SECRET="..."
```

`REVIEW_MASTER_KEY` 必须持久保存，重启时使用同一个值。丢失或更换它将无法解密已注册项目的凭据。不要在每次启动时重新生成。`REVIEW_ADMIN_TOKEN` 是管理员 bootstrap Bearer token，应由部署环境保管，不写入 YAML 或命令行参数。未配置 OAuth 时，浏览器端无法登录；`REVIEW_ADMIN_TOKEN` 仍可作为 Bearer 使用。已通过控制台创建的用户 API token（明文只在创建时显示一次）也继续可用于 Bearer，但创建 token 本身需要浏览器会话。

注册真实项目之前，需要把 `gitlab.url` 改为实际 GitLab 实例地址。可以用第二个 `--config` 指定覆盖文件；后面的映射递归合并，数组替换。

默认数据库为 `data/review.db`。服务启动时创建目录并执行迁移。

```bash
curl --fail http://localhost:8080/healthz
curl --fail http://localhost:8080/readyz
go run ./cmd/reviewctl --url http://localhost:8080 projects list
```

`/healthz` 检查进程，`/readyz` 检查数据库和 worker 生命周期。非本机部署应使用 HTTPS 反向代理和访问控制，不能把明文 HTTP 管理接口暴露到公网。

## 注册项目

先通过部署环境设置 `GITLAB_PROJECT_TOKEN`。项目 token 需要 `api` 和 `read_repository` scope。自动创建 webhook 还需要 Maintainer 权限。

```bash
go run ./cmd/reviewctl --url http://localhost:8080 projects register \
  --project group/backend-api \
  --token-env GITLAB_PROJECT_TOKEN \
  --profile default
go run ./cmd/reviewctl --url http://localhost:8080 projects get 42
```

`--project` 也可以是 GitLab 项目 ID。默认手工创建 webhook：注册响应中的 `webhook_secret` 只显示这一次，用它配置 GitLab 的 Secret token。URL 为服务地址加 `/webhooks/gitlab`，启用 Merge request、Comments 和 Push 事件。

自动创建时添加 `--auto-webhook`，并把 `server.public_url` 配为 GitLab 能访问的服务地址。重复注册返回冲突，不覆盖已有凭据。自动创建 hook 成功但入库失败时会尝试删除刚创建的 hook；网络中断等异常仍可能需要管理员核对 GitLab 上的 hook。

CLI 默认读取 `REVIEW_ADMIN_TOKEN`，可以用全局参数 `--api-token-env NAME` 改变环境变量名。注册响应和普通查询都不会返回项目 access token 或数据库密文。

## 入口与队列

| 入口 | 当前行为 |
|---|---|
| MR open / reopen / update（带 oldrev） | 按 Draft、标签和目标分支过滤，入队 `review.run` |
| MR close / merge | 入队 `review.cancel` |
| MR 评论 `/review`、`/review full`、`/review <profile>`、`/review cancel` | 先入队 `review.command`，worker 检查机器人身份、MR 状态和 Developer 以上权限，再入队目标任务 |
| 目标分支 push | 入队 `workspace.refresh`；分支删除不入队 |
| 项目注册 | 原子写入项目、初始工作区和 `workspace.onboard` 任务 |
| API 重新发布 | 入队 `review.publish` |

Webhook 不调用 GitLab 或模型。认证失败返回 401；其他情况返回 200，并通过 `accepted` 或 `ignored` 区分结果。缺少或非法的 `X-Gitlab-Event-UUID` 会被忽略。数据库失败返回 `ignored: storage_error`，不会假报已接收；按当前设计，HTTP 200 也意味着 GitLab 不会自动重试这次失败，需监控日志并重放事件。

UUID 在项目内去重，去重记录和入队在同一事务中提交。同一 key 最多保留一个 running 和一个 queued 任务；新 webhook 或通过权限检查的评审命令替换待执行任务的内容，不覆盖正在执行的内容。同一 key 的任务不会并行领取。完成、失败及续租均校验 worker、attempt 和租约，过期 worker 无法覆盖新执行结果。替换 payload 不重置 attempt，以免旧执行编号重新生效。

服务关闭时取消正在执行的 handler；尚未结算的任务保留 running，重启后待租约到期恢复，默认最多等待约 5 分钟。

`review.run`、`review.cancel`、`review.command`、`review.publish`、`workspace.onboard` 和 `workspace.refresh` 均已注册处理器。`/review help` 仍返回 `ignored: help_not_implemented`；命令回复和表情回执未实现。目标分支模式采用 Go `path.Match`，不提供递归 `**` 语义。

## 验证

```bash
cd web && npm test && npm run build && cd ..
go build ./...
go vet ./...
go test ./...
go test -race ./...
go mod verify
```

`go build` 依赖 `web/dist`，因此在全新检出上必须先执行前端构建。测试使用临时 SQLite 数据库和本地 HTTP 假 GitLab，覆盖注册、加密、认证、事件过滤、并发去重、租约恢复、旧 attempt 拒绝、CLI、工作流、发布、工作区、扩展和控制台静态资源。真实 GitLab CE、模型 provider、OAuth、MCP 和索引命令仍需部署时联调。

## 已实现 / 未验证 / 已知限制

**已实现（本地）**

- 项目注册、token 校验/加密、webhook 去重与持久化任务队列。
- OpenAI Chat Completions、OpenAI Responses、Anthropic Messages 三种协议；agent 工具循环。
- DAG 引擎、评审块、主 agent 编排、verify/裁决、结果发布与 republish。
- 工作区 onboarding/刷新、上下文文档、MCP/Skill 与项目级绑定。
- OAuth 会话、CSRF、admin/viewer 角色、API token、管理 API、React 控制台、反馈收集。

**未验证**

- 真实 GitLab CE（版本、token scope、webhook 配置、评论/commit status 发布）。
- 真实模型 provider 的协议兼容性、限流和成本。
- 真实 OAuth 登录、真实 MCP stdio/HTTP 服务器、真实 `index_command`。
- 浏览器端到端流程；目前只有静态控制台冒烟和组件测试。

**已知限制**

- SSE 通过每 2 秒轮询数据库产生事件，不是推送。
- 数据库中的平台/项目 MCP、Skill 和绑定变更按进程内缓存生效，通常需要重启服务。
- 数据保留策略未实现：`storage.llm_call_retention` 只出现在配置和设置视图中，没有定时清理任务。
- 手动保存上下文文档会就地更新当前 `(project, base_sha, kind)` 行，不新增历史版本。
- 项目列表支持 `q` / `enabled` 筛选，但不分页；MCP、Skill、块绑定列表也不分页。
- Postgres 未实现，配置 `storage.driver: postgres` 会在启动时报错。
- `/review help`、命令回复和表情回执未实现。
