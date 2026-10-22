# 07 Web 控制台

> 平台内部使用的管理控制台。页面原型见设计画布「GitLab CE 评审平台原型」；本文定义页面、每页用到的 API 和前端技术选型。

---

## 1. 技术选型（建议默认）

| 项 | 选择 | 理由 |
|---|---|---|
| 形态 | SPA，构建产物用 `embed.FS` 打进 Go 二进制 | 保持单二进制部署 |
| 框架 | Vite + React + TypeScript | 生态成熟，GPT 生成代码质量稳定 |
| 数据 | TanStack Query（请求缓存与轮询）、原生 `EventSource`（SSE） | 评审详情页需要实时刷新 |
| 路由 | TanStack Router 或 React Router | 二选一即可 |
| 样式 | CSS Modules + CSS 变量（亮色 / 暗色两套 token） | 依赖少，不绑定组件库 |
| DAG 视图 | 自己按拓扑分层布局（列 = 层，行 = 节点），SVG 画连线 | 节点数 ≤ 30，不需要图布局库 |
| 代码与 diff 展示 | `<pre>` + 行号；后续可换 Shiki | 首版够用 |
| API 类型 | 从 Go 结构体生成 OpenAPI，再生成 TS 类型（可选） | 前后端字段一致 |

目录：

```
web/
  index.html
  src/
    main.tsx  app.tsx  api/  pages/  components/  styles/
  vite.config.ts
```

开发时 Vite dev server 代理 `/api` 和 `/auth` 到 Go 服务；生产构建输出到 `web/dist`，由 `internal/server/web.go` embed。

---

## 2. 导航结构

```
总览
评审
项目
  └ 项目详情（概览 / 工作区与上下文 / 设置 / 扩展绑定）
扩展（MCP 服务器 / Skill / 块绑定）
模型与档位
统计与反馈
系统（API Token / 平台配置只读视图）
```

`viewer` 角色看不到写操作按钮；后端同样校验。

---

## 3. 页面

### 3.1 总览

| 区块 | 内容 | API |
|---|---|---|
| 运行中 | 正在运行的评审：项目、MR、当前阶段、已用时 | `GET /reviews?status=running` + SSE |
| 近 7 天 | 评审数、BLOCKING 比例、发布条目数、被挑战推翻比例、成本 | `GET /stats` |
| 需要处理 | 工作区异常、token 30 天内过期、provider 连续错误、发布失败的评审 | `GET /projects`、`GET /providers`、`GET /reviews?status=failed` |
| 最近评审 | 最近 20 条 | `GET /reviews?limit=20` |

### 3.2 评审列表

过滤：项目、状态、结论、档位、触发方式、时间。列：MR、项目、档位、状态、结论、P0/P1/P2 数、用时、成本、时间。

### 3.3 评审详情（核心页面）

| 区块 | 内容 | API |
|---|---|---|
| 头部 | MR 标题与链接、head sha、档位、触发方式、状态、结论；操作：取消、重新发布、在 GitLab 中打开 | `GET /reviews/{id}` |
| DAG 视图 | 按层排列节点，颜色表示状态，点击打开节点抽屉 | `GET /reviews/{id}/graph` + SSE |
| Findings | 表格：级别、标题、文件:行、来源（节点 / provider）、挑战结果、处置、处置理由；可按处置和级别过滤；展开看正文和证据 | `GET /reviews/{id}/findings` |
| 发布 | 汇总评论链接、行内讨论数量、commit status | 详情中的 `publications` |
| 节点抽屉 | 参数、输出 JSON、错误、每轮 LLM 调用（用量、耗时、stop reason，可展开请求和响应） | `GET /reviews/{id}/nodes/{node}`、`.../calls` |

### 3.4 项目列表与注册

列表列：项目路径、档位、工作区状态、最近评审、token 过期时间、启用开关。

注册对话框字段：项目路径或 ID、Project Access Token、档位、是否自动创建 webhook、跳过 Draft、目标分支。提交后显示校验结果（可访问、scope、过期时间、webhook 是否创建）。

### 3.5 项目详情

| 标签 | 内容 | API |
|---|---|---|
| 概览 | 基本信息、最近评审、本项目统计 | `GET /projects/{id}`、`GET /stats?project_id=` |
| 工作区与上下文 | 工作区状态、base sha、索引状态、最近刷新；操作：刷新、重建索引、重新生成上下文；上下文文档查看与编辑、锁定开关、历史版本 | `/workspace`、`/context-docs` |
| 设置 | 项目设置表单（06-config.md §3）；轮换 token；重建 webhook | `PATCH /projects/{id}`、`PUT .../token` |
| 扩展绑定 | 项目级 MCP、Skill，以及按块绑定（覆盖平台默认） | `/mcp-servers`、`/skills`、`/block-bindings` |

### 3.6 扩展

- **MCP 服务器**：列表（作用域、名称、命令、状态、工具数）；新增表单；「粘贴 Claude Code 配置导入」；「测试」按钮列出工具。
- **Skill**：列表（作用域、名称、描述、来源目录）；上传 zip。
- **块绑定**：矩阵视图，行是块（code-review / review / reasoner / verify / synthesize / onboarding），列是可用的 MCP 工具和 Skill，勾选即绑定。

### 3.7 模型与档位

- Provider 卡片：协议、base_url、模型、并发和速率限制、近 24 小时调用数、错误率、P50 延迟；「测试连通性」。
- 档位：只读展示 profile 定义（规划方式、提出方、挑战方、裁决策略、预算），用小型示意图画出拓扑 A / B。

### 3.8 统计与反馈

- 按项目、按 provider、按块的运行数、成本、耗时趋势。
- 提出方准确率（👍 + 改动后 resolve / 已发布）、各 provider 被挑战推翻比例。
- 被丢弃条目抽样标注入口（M6）。

---

## 4. 交互约定

- 所有破坏性操作（注销项目、删除 MCP、取消评审）二次确认。
- 长操作（注册、刷新、触发评审）返回 job ID，页面用 toast 提示并跳转到对应详情。
- 时间显示为相对时间，悬停显示绝对时间（本地时区）。
- 表格超出宽度时在容器内横向滚动；窄屏下侧边导航收起为顶部菜单。
