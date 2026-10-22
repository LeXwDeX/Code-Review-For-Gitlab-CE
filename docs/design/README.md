# 详细设计

[ARCH.md](../../ARCH.md) 是设计基线，讲清楚「做什么、为什么」；本目录是实现层面的细化，讲清楚「怎么做」：接口签名、表结构、API、流程步骤和配置格式。

| 文档 | 内容 | 主要对应里程碑 |
|---|---|---|
| [01-modules.md](01-modules.md) | 目录到文件的划分、依赖方向、各包 Go 接口、通用约定、推荐依赖 | 全部 |
| [02-data-model.md](02-data-model.md) | 数据库 DDL、事件 payload、Postgres 差异、数据保留 | M0、M2 |
| [03-http-api.md](03-http-api.md) | webhook 处理规则、管理 API、MR 评论命令 | M0、M1、M6 |
| [04-dag-and-contracts.md](04-dag-and-contracts.md) | 节点种类、图模板、状态迁移表、调度算法、节点输出契约、裁决算法、评论渲染 | M2、M3、M4 |
| [05-flows.md](05-flows.md) | 评审、取消、命令、刷新、onboarding、崩溃恢复、发布幂等、反馈收集 | M1–M5 |
| [06-config.md](06-config.md) | 平台配置、MCP 配置、项目设置、仓库配置、环境变量 | M0 起 |
| [07-web-console.md](07-web-console.md) | 控制台技术选型、页面与所用 API | M6 |

## 建议的实现顺序

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
