# 详细设计

[ARCH.md](../../ARCH.md) 是设计基线，讲清楚「做什么、为什么」；本目录是实现层面的细化，讲清楚「怎么做」：接口签名、表结构、API、流程步骤和配置格式。

当前 M0–M6 的本地实现均已完成，实际可用接口、启动方式和限制见 [项目 README](../../README.md)。下文是目标设计；少数实现与文档不同，已在下方和各文档的「实现偏差」中标注。未与真实 GitLab CE、模型 provider 或 OAuth 联调。

| 文档 | 内容 | 主要对应里程碑 |
|---|---|---|
| [01-modules.md](01-modules.md) | 目录到文件的划分、依赖方向、各包 Go 接口、通用约定、推荐依赖 | 全部 |
| [02-data-model.md](02-data-model.md) | 数据库 DDL、事件 payload、Postgres 差异、数据保留 | M0、M2 |
| [03-http-api.md](03-http-api.md) | webhook 处理规则、管理 API、MR 评论命令 | M0、M1、M6 |
| [04-dag-and-contracts.md](04-dag-and-contracts.md) | 节点种类、图模板、状态迁移表、调度算法、节点输出契约、裁决算法、评论渲染 | M2、M3、M4 |
| [05-flows.md](05-flows.md) | 评审、取消、命令、刷新、onboarding、崩溃恢复、发布幂等、反馈收集 | M1–M5 |
| [06-config.md](06-config.md) | 平台配置、MCP 配置、项目设置、仓库配置、环境变量 | M0 起 |
| [07-web-console.md](07-web-console.md) | 控制台技术选型、页面与所用 API | M6 |

## 实现偏差（M0–M6 本地实现）

以下是当前代码与目标设计已知不一致的地方。文档正文保留目标契约，不因实现偏差删除。

- [01-modules.md](01-modules.md)：文件清单是设计目标。发布器实际在 `internal/app/publisher.go`；GitLab 发布与评论命令处理在 `internal/app` 层；`pkg/dag/validate.go` 的校验合并进 `pkg/dag/graph.go`。
- [02-data-model.md](02-data-model.md)：保留/清理策略未实现定时任务；`sessions` 表存在但运行时未使用，会话在内存中，重启后用户需重新登录。
- [03-http-api.md](03-http-api.md)：项目详情暂不返回工作区状态和最近评审；评审详情暂不返回 `mr_title` 和 `publications.commit_status`；`GET /projects` 已支持 `q` / `enabled` 但不分页；上下文文档保存就地更新当前版本；Skill zip 上传已实现并受限；MCP/Skill/绑定列表不分页；SSE 每 2 秒轮询。
- [04-dag-and-contracts.md](04-dag-and-contracts.md)：I4 使用 `decision.finding_id` + `reason`；token 预算耗尽会使未完成节点失败/跳过，而不是降级为「不再核验」；extend 契约增加 `new_finding_index`。
- [06-config.md](06-config.md)：仓库级 `profile` / `skip_draft` / `labels_exclude` 只在评审执行阶段部分合并，webhook 过滤早于仓库配置合并；示例配置缺 `providers` 和 `propose`，不能直接跑真实评审。
- [07-web-console.md](07-web-console.md)：`mr_title`、`publications.commit_status`、`workspace.index_status` 等字段当前降级显示；项目设置的写接口已实现。

## 建议的实现顺序

以下是历史建议顺序；M0–M6 已完成，实际文件位置以「实现偏差」为准。

1. `internal/config`、`internal/store`（迁移 0001）、`internal/secret` —— 能启动、能连库。
2. `internal/server/webhook.go` + `internal/jobs` —— webhook 能入队、能去重。
3. `internal/gitlab`（含 `DiffIndex` 与单元测试）、`internal/repo` —— 能拿到 diff 和代码。
4. `internal/llm` 的一个 adapter + `internal/agent` 的工具循环 —— 能跑通一次 `code-review`。
5. `internal/review/render.go` + `gitlab/publish.go` —— 完成 M1 闭环。
6. `pkg/dag`（先写迁移表和测试，再写引擎）—— M2。
7. 其余块、裁决、对抗核验、工作区、扩展、控制台。

## 给 AI 编码助手的说明

- 以本目录为准生成代码；与 ARCH.md 冲突时以 ARCH.md 为准，并回头修正本目录。
- 每个包先写接口和测试，再写实现；`pkg/dag` 的迁移表和级联规则必须有表驱动测试。
- 不要引入本目录「推荐依赖」以外的库；需要时先补充到 01-modules.md §12 并检查许可证。
