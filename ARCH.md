# ARCH — GitLab CE 自动评审服务设计基线

> 状态：**设计基线 v0（讨论稿）**，尚未开始编码。
> 本文汇总了立项讨论中的全部结论。标注「已定」的条目是用户确认过的决策；标注「建议默认」的条目是讨论中提出、尚待确认的方案，见 §15 待决事项。

---

## 1. 定位

一个开源、MIT 协议、基于 Go 的 Web 服务，接入 **GitLab CE（社区版）**，对 Merge Request（下文统称 MR，即 PR）做 AI 自动评审：

- 通过**项目级 Token** 授权，既能被 GitLab webhook 回调触发，也能主动触发评审，并把结果回写到 MR。
- 服务拥有 clone 项目代码的权限，评审时不只看 diff，也能读取完整仓库上下文。
- 评审由一个**主 agent 单入口**统一编排：主 agent 派发多种评审模式，经过跨模型或同模型的**对抗性核验**后，回到主 agent 评估，最终输出结论到 MR。
- 编排层以 **DAG 引擎**为唯一执行架构，从作者本人的 OpenCode-GraphAgent DAG 引擎移植而来（见 §12）。

### 非目标

- 不执行仓库代码，不在服务内跑测试或构建。验证证据来自 GitLab CI 的结果。
- 不修改仓库，不推送代码，不自动合并 MR。
- 不支持订阅制账号登录（ChatGPT、Claude 订阅等 OAuth 方式），只支持 API Key。
- 不做 IDE 或交互式功能（暂停、单步、人工裁决超时等）。

---

## 2. 决策总表

| # | 决策 | 状态 |
|---|---|---|
| D1 | 技术栈：Go Web 服务 | 已定 |
| D2 | 对接 GitLab CE，使用项目级 Token 授权，具备 clone 权限 | 已定 |
| D3 | 许可证 MIT，依赖必须与 MIT 兼容 | 已定 |
| D4 | 模型只用 API Key 计费方式，不支持订阅登录 | 已定 |
| D5 | 支持三种协议：OpenAI Chat Completions、OpenAI Responses、Anthropic Messages | 已定 |
| D6 | 能用官方 SDK 就用官方 SDK（`openai-go/v3`、`anthropic-sdk-go`、GitLab `client-go`） | 已定 |
| D7 | 评审模式：`reasoner`、`review`、`verify`、`code-review`（传统） | 已定 |
| D8 | 支持跨模型和同模型的对抗性审核 | 已定 |
| D9 | 主 agent 单入口 → 多模式评审 → 回到主 agent 评估 → 输出结论到 MR | 已定 |
| D10 | 复用 OpenCode-GraphAgent 的 DAG 引擎；作者为版权人，可将其改为 MIT 后移植 | 已定 |
| D11 | `verify` 在 MR 场景下定位为**挑战者**：对单条 finding 做只读反证，不跑测试 | 建议默认 |
| D12 | 驱动方式：规划 → 执行 → 汇总，允许有限轮补派（extend），不用自由工具循环 | 建议默认 |
| D13 | 部署形态：单个二进制，默认 SQLite、可选 Postgres，提供 Docker 镜像，任务队列在进程内 | 建议默认 |
| D14 | 评审图由**块模板**生成，主 agent 只输出受限的规划参数，不手写整张图 | 建议默认 |

---

## 3. 总体架构

```
GitLab webhook / 评论命令 / 手动 API
        │  校验 X-Gitlab-Token → 入队 → 立即返回 200
        ▼
┌─[采集阶段 · 纯代码]──────────────────────────────────────────────┐
│ MR 元数据、diff、versions(base/start/head sha)、pipeline 状态、  │
│ 关联 issue、worktree → MR 材料包 (Dossier)                      │
└───────────────────────────────────────────────────────────────────┘
        ▼
┌─[DAG 工作流 · 每个 (MR, head_sha) 一个]──────────────────────────┐
│ plan(主 agent) ──fan_out──┬─ code-review      (provider A)       │
│                           ├─ review:standards ┐                  │
│                           ├─ review:intent    ┘─ review 裁决     │
│                           └─ reasoner (可选)                     │
│                                   │                              │
│              adversarial：verify 跨模型逐条反证                  │
│                                   │                              │
│        synthesize(主 agent)：逐条采纳/反驳 → 结论                 │
│           └─ 可申请 extend 补派节点（有上限）                     │
└───────────────────────────────────────────────────────────────────┘
        ▼
┌─[发布阶段 · 纯代码]──────────────────────────────────────────────┐
│ fingerprint 比对 → 行号校验 → 汇总评论就地更新 → 行内讨论 →      │
│ commit status (ai-review)                                        │
└───────────────────────────────────────────────────────────────────┘
```

原则：**LLM 负责判断，代码负责执行和兜底。** 采集、调度、校验、发布都不交给 LLM。

### 3.1 代码布局（建议）

```
cmd/server/            入口
internal/server/       HTTP、webhook handler、管理 API
internal/gitlab/       client-go 封装、webhook payload、diff 与 position 计算、发布器
internal/repo/         bare mirror 缓存、worktree、安全的 git 参数
internal/llm/          LLMClient 接口 + 三个协议 adapter
internal/agent/        工具循环、只读工具集、submit_result 结构化交付
internal/review/       块模板、节点执行器、契约、裁决不变量、渲染
internal/store/        数据库访问与迁移（SQLite / Postgres）
internal/config/       服务配置与仓库内配置解析
pkg/dag/               DAG 引擎核心（场景无关，可被 agencoding 复用）
prompts/               各 agent 的 prompt（embed 进二进制）
```

`pkg/dag` 不依赖任何评审或 GitLab 概念，`internal/review` 在它之上定义评审专用的块和执行器。

---

## 4. GitLab 集成

### 4.1 授权

- 以**项目级 Project Access Token** 为主。每个项目一个 token，服务维护 `project_id → token` 注册表，token 加密存储（AES-GCM，主密钥来自环境变量）。
- 所需 scope 和角色：

| 能力 | 需要 |
|---|---|
| clone / fetch | `read_repository`，至少 Reporter |
| 读 MR、diff、versions、pipeline | `read_api` 或 `api` |
| 发评论、行内讨论 | `api`，私有项目至少 Reporter |
| 写 commit status、resolve 讨论 | `api`，一般需要 Developer |

- 是否同时支持 Group Token 或 bot 账号 PAT，见 §15。

### 4.2 触发

| 来源 | 条件 | 说明 |
|---|---|---|
| Merge Request Hook | `open`、`reopen`；`update` 且带 `oldrev`（表示有新 push） | 主路径 |
| Note Hook | 评论 `/review`、`/review <profile>` | 手动重跑或切换评审档位 |
| 过滤 | Draft 状态、标签、目标分支 | 由项目配置决定 |
| 手动 API | 服务自身的 REST 接口 | CI job 或运维调用 |

- webhook handler 只做三件事：校验 `X-Gitlab-Token`、入队、返回 200。GitLab 有 webhook 超时限制，连续失败还会自动禁用 hook。
- **幂等键**：`(project_id, mr_iid, head_sha, profile)`。同一个 MR 有新 push 时，旧工作流转为 `cancelled`，旧结果作废。
- **增量评审**：记录上次已评审的 `head_sha`，默认只评审新增变更，`/review full` 强制全量。

### 4.3 取 diff 与代码

- diff：`GET /projects/:id/merge_requests/:iid/diffs`（`/changes` 已废弃）。
- 三个 sha：`GET .../versions` 取 `base_sha`、`start_sha`、`head_sha`。行内评论必须带这三个值。
- 代码：每个项目维护一个 bare mirror 缓存，评审时 `git fetch origin refs/merge-requests/<iid>/head`，再 `git worktree add` 检出。
- git 安全参数：`core.hooksPath=/dev/null`，跳过 LFS（`GIT_LFS_SKIP_SMUDGE=1`），禁用 submodule 自动拉取，**绝不执行仓库内任何脚本**。
- 忽略规则：`*.lock`、生成代码、`vendor/`、二进制文件等不发送给模型，可在项目配置中扩展。

### 4.4 回写

- **汇总评论**：每个 MR 一条，内嵌隐藏标记 `<!-- gitlab-ce-review:summary -->`，重跑时就地更新，不重复发布。
- **行内讨论**：Discussions API，`position_type=text`，带 `new_path` / `old_path` 和 `new_line` / `old_line`。行号**必须落在 diff hunk 内**，否则 GitLab 返回 400。校验不通过的条目降级并入汇总评论。
- **commit status（可选）**：评审结论为 BLOCKING 时，给 `head_sha` 写入名为 `ai-review` 的 `failed` 状态。项目开启「Pipelines must succeed」后即可拦截合并，这是 CE 不依赖付费审批规则也能做门禁的方式。是否默认开启，见 §15。
- 发布前再次比对 MR 当前的 `head_sha`。如果已经变化，丢弃本次结果，不发布。

---

## 5. 评审编排

### 5.1 主 agent 单入口

主 agent 在 DAG 中占两个节点：

1. **plan**：读取材料包摘要（文件列表、变更统计、pipeline 状态、MR 描述），判断风险，输出受限的规划参数：
   ```json
   { "packages": [ { "id": "...", "block": "code-review|review|reasoner", "files": ["..."], "provider": "..." } ],
     "challenge": { "only": ["P0", "P1"] } }
   ```
   这些参数由代码严格校验：块类型、provider 必须在项目允许列表内，文件必须属于本次变更，总节点数受预算限制。
2. **synthesize**：接收所有子节点的结构化结果（**只接收结论，不接收子 agent 的完整对话**），逐条采纳或反驳，给出最终 findings、PASS/BLOCKING 和汇总文案。最多可申请 1 次 `extend` 补派节点（例如对争议条目追加反证）。

`brain.md` 的「严肃决策、风险取舍」定位可以作为 synthesize 阶段 prompt 的基础。

### 5.2 四种评审模式（MR 版契约）

现有 agent 定义（`agents_multi-orchestration/agents/*.md`）是为开发流水线写的，在 MR 场景需要改写输入和输出：

| 模式 | 原定位 | MR 场景的改写 |
|---|---|---|
| `code-review` | 无（新写） | diff 加少量上下文，一次调用，输出带行号的 findings。轻量、高覆盖，作为默认 |
| `review` | 独立终审，需要验证证据，输出 PASS/BLOCKING | `spec_goal` 取自 MR 标题、描述和关联 issue；验证证据取自 **GitLab pipeline 和 job 状态**；保留 PASS/BLOCKING 和 P0/P1/P2；去掉 `reflow_target`、`ready_for_patcher` 等开发流水线字段。可展开为 standards 和 intent 两条独立通道，再加裁决 |
| `reasoner` | 审方案和设计文档，不审 diff | 输入改为 MR 描述、关联 issue、本次改动的设计文档（`*.md`、ADR），检查「声称要做的事在逻辑上是否成立、漏了哪些边界」 |
| `verify` | 运行测试并诊断 | 改为**挑战者**：对单条 finding 只读反证，输出 CONFIRMED / REFUTED / UNCERTAIN，并且必须引用代码证据。读取 CI 日志诊断失败原因作为后续可选能力 |

agent 原本依赖的 `codebase-memory-mcp` 等工具在服务中不可用，改为在 worktree 上内置一组**只读工具**：`read_file`、`grep`、`glob`、`list_dir`、`git_log`、`git_blame`。所有工具都限定在 worktree 内，且有输出长度上限。

### 5.3 统一 finding 模型

各模式的严重级别不同（review：P0/P1/P2；reasoner：Critical/Notable/Informational；verify：CONFIRMED/REFUTED/UNCERTAIN），内部统一为：

```go
type Finding struct {
    ID         string
    Origin     NodeRef   // 产生它的节点；主 agent 自提的为 origin=main
    Severity   Severity  // P0 | P1 | P2 | Info
    Category   string    // correctness | security | compat | logic | maintainability ...
    Path       string
    Line       *LineRef  // 可空：无法定位到行的 finding 只进汇总
    Title      string
    Evidence   string    // 引用的代码或依据
    Confidence string    // high | medium | low
    Challenge  *ChallengeResult // verify 的反证结果
    Decision   *Decision        // synthesize 的采纳或反驳记录
}
```

各模式的原生字段（`verdict`、`evidence_status`、`result_type` 等）保留在节点输出里，供主 agent 参考。

### 5.4 对抗性核验

**角色**：Proposer（code-review / review / reasoner）提出问题 → Challenger（verify）逐条反证 → Judge 在双方不一致时裁决。Judge 优先用确定性规则，LLM 裁决作为兜底。

**拓扑**：

| 拓扑 | 流程 | 用途 |
|---|---|---|
| A. 提出加反证 | 1 个 proposer，每条 finding 交给 1 个 challenger | `default` 档 |
| B. 多方独立加交叉 | N 个模型独立评审，合并去重；多方都提出的直接保留，单方提出的交给其他模型反证 | `deep` 档 |
| C. 辩论 | proposer 和 challenger 来回 1 到 2 轮，再由 judge 裁决 | 仅用于 P0/P1 争议条目，预留接口 |

**同模型对抗的独立性要求**（否则对抗只是走形式）：

1. challenger 新开上下文，不继承 proposer 的任何对话。
2. 信息不对称：challenger 只看到结论、位置和相关代码，看不到 proposer 的推理过程、置信度和模型身份。
3. 立场相反：proposer 被要求「找出问题」，challenger 被要求「假设它是误报，去找证据推翻」。
4. 采样差异：temperature 不同，或者给出的上下文顺序和范围不同。
5. 拓扑 B 投票时，同模型的票按权重打折（默认 0.5）。

**跨模型**：challenger 可配置为 `cross`，自动选择与 proposer 不同的 provider。反证比从头发现容易，challenger 可以用较便宜的模型，但 P0/P1 条目建议用强模型反证。

**默认裁决规则**：

| 提出方 | 挑战方 | 处置 |
|---|---|---|
| 提出 | CONFIRMED | 发布 |
| 提出 | REFUTED（带代码证据） | 丢弃，记录在运行日志里 |
| 提出 | REFUTED（无证据） | 视为 UNCERTAIN |
| 提出 | UNCERTAIN | 按项目策略：`drop` / `mark`（标「存疑」后发布）/ `judge` |
| 多方都提出（拓扑 B） | 无需反证 | 发布 |

**去重**：先按「文件、行号区间重叠、类别」做规则合并，可选再用 LLM 做语义合并。

### 5.5 裁决不变量（由代码强制）

| # | 不变量 |
|---|---|
| I1 | 每个节点的输出必须通过 `output_schema` 校验。缺失时补问 1 次，仍失败则节点 `failed` |
| I2 | finding 的行号必须落在 diff 的行集合内，否则去掉行号，降级进汇总 |
| I3 | REFUTED 必须引用代码位置作为证据，否则按 UNCERTAIN 处理 |
| I4 | 子节点已 CONFIRMED 的 P0/P1，synthesize 要丢弃或降级，必须给出 `{node, index, reason}` 形式的逐条反驳，否则结算失败 |
| I5 | synthesize 自提的 finding 标记为 `origin=main`，同样要经过 verify 反证 |
| I6 | 最终结论一致性：存在已确认的 P0/P1 时，不得给出 PASS |
| I7 | 结论绑定 fingerprint（见 §6.5）。发布前 fingerprint 不一致，结果作废 |

I4 的「逐条反驳」规则移植自 GraphAgent 的 accept 结算规则（裁决为 ACCEPT 时不得自带 required_actions；每条 required_actions 必须逐条反驳）。

---

## 6. DAG 引擎（`pkg/dag`）

从 OpenCode-GraphAgent 的 `packages/core/src/dag/` 和 `packages/opencode/src/dag/` 移植（TS → Go，参照原代码逐模块重写）。只保留场景无关的部分，IDE 交互相关的部分裁掉。

### 6.1 数据模型

- **节点**：一次子 agent 调用。字段：`id`、`block`、`depends_on`、`required`、`prompt_template`、`input_mapping`（形如 `a.output.findings`）、`condition`、`output_schema`、`provider`、`timeout`。
- **块（Block）**：可复用的子图模板，由编译器展开成节点。MR 评审定义 5 种块：

| 块 | 展开结果 |
|---|---|
| `code-review` | 1 个节点 |
| `review` | standards 通道 + intent 通道 + 1 个裁决节点 |
| `reasoner` | 1 个节点（默认可选） |
| `adversarial` | 针对上游每条待挑战的 finding 生成 verify 节点（运行时扇出），再加 1 个汇聚节点 |
| `synthesize` | 主 agent 的最终裁决节点 |

- **fan_out**：plan 节点输出的 `packages[]` 在运行时展开成并行节点。评审是只读的，不需要原引擎里「证明 write_set 互不相交」的检查。
- **图校验**：所有图在执行前都要经过唯一的严格校验入口：未知字段报错、检测环、检查 `input_mapping` 引用存在、检查块和 provider 在允许列表内、检查节点总数不超预算。

### 6.2 状态机

两条铁律（移植自原引擎）：**状态只能通过校验过的迁移改变；终态不可逆。**

节点状态（去掉原引擎中用于交互暂停的 `paused`）：

```
pending ─→ queued ─→ running ─→ completed
   │          │         ├────→ failed
   │          │         ├────→ aborted
   │          │         └────→ pending   (restart / 被 replan 替换)
   │          ├─→ pending  (replan 替换后重置)
   │          ├─→ queued   (崩溃恢复后幂等重入)
   └──────────┴─→ skipped / failed  (条件不满足、级联、排队超时)
终态：completed / failed / aborted / skipped
```

工作流状态（去掉 `paused`、`stepping`）：`pending → running → completed | failed | cancelled`。

### 6.3 事件

采用事件溯源，事件投影成 SQLite（或 Postgres）读模型。保留原事件集的子集：

- 工作流：`created`、`started`、`extended`（对应原 `replanned`）、`completed`、`failed`、`cancelled`
- 节点：`registered`、`queued`、`started`、`completed`、`failed`、`aborted`、`skipped`、`cancelled`、`restarted`

裁掉：`paused`、`resumed`、`stepped`、`deadline_extended`、`timeout_escalated`、`config_updated`。

### 6.4 调度与并发

- Kahn 拓扑排序加 ready 集合。
- **两级限流**：全局并发信号量，加上**按 provider 的并发和速率限制**（多个 MR 同时到达时，不会把某个模型 API 打到限流）。
- 节点先持久化为 `queued`，拿到许可后才发起 LLM 调用。
- 排队等待时间计入节点超时。
- 所有写入都带 attempt / `graph_rev` 栅栏，迟到的过期结果一律拒绝。

### 6.5 Fingerprint 与取消

- fingerprint = `head_sha` + `base_sha` + 项目配置哈希 + prompt 版本。
- MR 有新 push：旧工作流 → `cancelled`，运行中的节点 → `aborted`，正在进行的 LLM 请求通过 context 取消。
- 发布前比对 fingerprint，见 I7。

### 6.6 失败语义

- `required` 节点失败 → 下游级联为不可满足 → 工作流 `failed`。此时在 MR 上发布一条简短的「评审未完成」说明（是否发布可配置）。
- 可选节点失败 → 下游照常运行，对应输入用占位文本。
- 依赖全部为 `skipped` 的节点级联 `skipped`。
- 超时没有人工裁决环节：硬超时加预算上限，超时后按 required / optional 语义处理。

### 6.7 恢复

评审是只读的，恢复比原引擎简单得多：

- 服务重启后，`running` 和 `queued` 的节点直接重新执行。LLM 调用无副作用，重跑是安全的。
- 只有发布阶段需要幂等：汇总评论按隐藏标记就地更新；行内讨论按 `(fingerprint, finding_id)` 去重。

### 6.8 预算

| 项 | 默认值（建议） |
|---|---|
| 每个工作流的节点总数 | 30 |
| extend 次数 | 1 |
| 每个工作流的 token 上限 / 金额上限 / 时长上限 | 项目配置 |
| 超出预算时 | 降级为拓扑 A，或者只挑战 P0/P1 |

---

## 7. LLM 接入层（`internal/llm`）

### 7.1 统一接口

```go
type Client interface {
    // 一次请求-响应；工具循环在 internal/agent 中实现
    Generate(ctx context.Context, req Request) (*Response, error)
}
```

`Request` 和 `Response` 统一表示消息、工具定义与调用、推理块、用量和停止原因。DAG 节点和 agent 只依赖这个接口，不感知底层协议。

### 7.2 三个 adapter

| 协议 | SDK | 要点 |
|---|---|---|
| OpenAI Chat Completions | `github.com/openai/openai-go/v3` | 兼容端点最多（vLLM、Ollama、各家国产模型），是内网部署的主力。`base_url` 可配置 |
| OpenAI Responses | `github.com/openai/openai-go/v3`（`responses` 包） | 使用**无状态模式**（不依赖 `previous_response_id`），推理项按协议要求原样回传，便于重试和切换模型 |
| Anthropic Messages | `github.com/anthropics/anthropic-sdk-go` | 开启 thinking 时，thinking 块（含签名）在工具循环中原样回传；对材料包等稳定前缀设置 `cache_control`，降低同一 MR 多次评审的成本 |

### 7.3 结构化交付

所有子 agent 通过调用 **`submit_result` 工具**提交结果，参数结构由节点的 `output_schema` 约束。三种协议都支持工具调用，这样只需一套实现，不依赖各家不同的 structured output 参数。

### 7.4 Provider 配置

每个 provider 配置 `protocol`、`base_url`、`model`、`api_key`（从环境变量或加密存储读取），以及可选的 `max_concurrency`、`rpm`、`tpm`、`temperature`、`thinking` 参数。

---

## 8. 数据与持久化

默认 SQLite，可选 Postgres。主要表：

| 表 | 内容 |
|---|---|
| `projects` | GitLab 项目注册、加密后的 token、webhook secret、项目级配置 |
| `workflows` | 工作流读模型：MR、fingerprint、profile、状态、预算用量 |
| `workflow_nodes` | 节点读模型：状态、attempt、provider、用量、结构化输出 |
| `events` | 事件日志（只追加） |
| `findings` | 统一 finding、挑战结果、裁决记录 |
| `publications` | 已发布到 GitLab 的评论和讨论 ID，用于幂等更新 |
| `llm_calls` | 每次模型调用的 prompt 摘要或完整内容、模型版本、原始输出、用量（保留期可配置） |
| `feedback` | 来自 GitLab 的反馈信号（见 §11） |

---

## 9. 配置

### 9.1 服务配置（示例）

```yaml
server:
  listen: ":8080"
  base_url: "https://review.example.internal"
gitlab:
  url: "https://gitlab.example.internal"
storage:
  driver: sqlite            # sqlite | postgres
  dsn: "file:data/review.db"
secrets:
  master_key_env: REVIEW_MASTER_KEY

providers:
  claude: { protocol: anthropic,        base_url: "...", model: "...", api_key_env: ANTHROPIC_API_KEY }
  gpt:    { protocol: openai-responses, base_url: "...", model: "...", api_key_env: OPENAI_API_KEY }
  local:  { protocol: openai-chat,      base_url: "http://vllm:8000/v1", model: "...", max_concurrency: 4 }

profiles:
  default:
    propose:   [{ block: code-review, provider: local }]
    challenge: { provider: claude, only: [P0, P1] }
    on_uncertain: mark       # drop | mark | judge
  deep:
    main: { provider: claude }
    propose:
      - { block: review,   provider: claude }
      - { block: review,   provider: gpt }
      - { block: reasoner, provider: claude }
    consensus: { min_votes: 2, same_model_weight: 0.5 }
    challenge: { provider: cross }
    judge:     { provider: gpt, max_rounds: 1 }
```

### 9.2 仓库内配置 `.gitlab-review.yml`（可选）

由项目方放在仓库根目录，用于覆盖项目级设置：默认 profile、忽略路径、评审语言、严格度、自定义规则、触发过滤（Draft、标签、目标分支）。只读取**目标分支**上的版本，防止 MR 作者通过修改配置绕过评审。

---

## 10. 安全

| 风险 | 措施 |
|---|---|
| 源码发送给外部模型 | ignore 规则；可选的密钥扫描与脱敏；内网项目可只配置内网模型 |
| Prompt 注入（MR 描述、代码注释、评论） | 外部内容一律作为不可信数据传入，并标注「非授权内容」；agent 只有只读工具，唯一输出是 `submit_result`；发布内容由代码渲染 |
| 恶意仓库内容 | 禁用 git hooks、LFS、submodule；不执行任何仓库代码；只读工具限定在 worktree 内 |
| 伪造 webhook | 校验 `X-Gitlab-Token`，并按 `project_id` 匹配对应的 secret |
| Token 泄露 | 加密存储；日志脱敏；最小 scope |
| 配置绕过 | 仓库内配置只读目标分支版本 |
| SSRF | 只访问配置中的 GitLab 地址和 provider 地址 |

---

## 11. 可观测性与反馈闭环

- 每个工作流可完整回放：事件日志、每次 LLM 调用的输入输出、模型版本、用量。
- 指标：工作流耗时、节点失败率、各 provider 的延迟、错误率和 token 用量、每个 MR 的成本。
- **反馈闭环**：从 GitLab 收集信号，包括评论的 👍/👎（award emoji）、讨论被 resolve 时对应行是否真的被改动。据此统计**每个模型作为提出方的准确率、作为挑战方的误杀率**，作为模型选择和 prompt 迭代的依据。

---

## 12. 许可证与依赖

- 本项目：**MIT**。
- 依赖许可证（已核对）：

| 依赖 | 许可证 | 说明 |
|---|---|---|
| `github.com/openai/openai-go/v3` (v3.85.0) | Apache-2.0 | 分发时附带许可证文本 |
| `github.com/anthropics/anthropic-sdk-go` (v1.84.0) | MIT | — |
| `gitlab.com/gitlab-org/api/client-go` (v1.46.0) | Apache-2.0 | 间接依赖 `hashicorp/go-retryablehttp`、`go-cleanhttp`（MPL-2.0，不修改即可正常使用） |

- 发布物中附带 `THIRD_PARTY_LICENSES`，构建时用 `go-licenses` 自动收集。
- CI 许可证检查：允许 MIT、BSD、Apache-2.0、ISC、MPL-2.0（不修改）；**禁止 GPL、AGPL**。
- **DAG 引擎来源**：`pkg/dag` 移植自 OpenCode-GraphAgent 的 DAG 引擎（原为 AGPL-3.0-or-later，Copyright (c) 2026 LeXwDeX）。版权人同意以 MIT 授权移植到本项目，需在 `NOTICE` 中写明。注意：
  - 原 DAG 代码引用的上游 opencode 模块是 MIT（Copyright (c) 2025 opencode），若一并移植，需保留其版权声明。
  - 原块合约改编自 mattpocock/skills（MIT），若移植相关 prompt，需保留出处。
  - 移植前需确认原仓库中署名为 `Test <test@opencode.test>` 的两个提交（含引擎首个提交 `c8e7b676fd`）出自版权人本人。

### 12.1 移植清单

| 原模块 | 处理 |
|---|---|
| `core/types.ts`、`core/transitions.ts`（状态与迁移表、两条铁律） | 直接移植，裁掉 `paused`、`stepping` |
| `core/graph.ts`、`core/layering.ts`、`core/required-validator.ts` | 直接移植 |
| `core/scheduling.ts` | 移植，增加按 provider 限流 |
| `core/replan.ts`、attempt / graphRev 栅栏 | 移植，用于取消和 extend |
| `schema/dag-event.ts`、`projector.ts`、`sql.ts`、`store.ts` | 移植，事件集裁剪见 §6.3 |
| `blocks.ts`、`review-lifecycle.ts` | 移植思路，块类型按 §6.1 重写 |
| 状态迁移、级联 skip 等测试用例 | 转写为 Go 表驱动测试 |
| `runtime/spawn.ts`、admission、recovery、会话与权限推导 | 不移植，按 MR 服务重写 |
| hooks 体系、TUI、Goal 自治循环、并行 writer 与 `git status` 聚合 | 不移植 |

---

## 13. 部署

- 单个静态二进制，提供 Docker 镜像（内含 git）。
- 依赖：一个可写的数据目录（SQLite 数据库和 git mirror 缓存）。
- 水平扩展（多实例、外部队列）不在首版范围内；`store` 和任务队列通过接口隔离，为将来换成 Postgres 加外部队列预留空间。

---

## 14. 里程碑（建议）

| 里程碑 | 内容 |
|---|---|
| M0 骨架 | 服务配置、项目注册、webhook 接收与校验、任务队列、SQLite |
| M1 单模式闭环 | 采集阶段、`code-review` 单节点、一个 provider、汇总评论与行内讨论发布、行号校验、幂等 |
| M2 DAG 引擎 | 移植 `pkg/dag`（状态机、图校验、调度、事件与投影）、取消与 fingerprint、三个 LLM adapter |
| M3 主 agent 编排 | plan / fan_out / synthesize、`review` 与 `reasoner` 块、只读工具循环、裁决不变量 I1–I7 |
| M4 对抗核验 | `verify` 挑战者、拓扑 A 和 B、跨模型选择、去重、UNCERTAIN 策略、commit status |
| M5 运维与反馈 | 管理界面或 CLI、可观测性、反馈收集、许可证检查与 `THIRD_PARTY_LICENSES` |

---

## 15. 待决事项

| # | 问题 | 当前建议 |
|---|---|---|
| Q1 | 是否同时支持 Group Token 或 bot 账号 PAT？ | 首版只支持 Project Token，数据模型预留 |
| Q2 | 项目接入方式：管理员手工登记，还是项目方自助（服务用项目 token 自动创建 webhook）？ | 首版手工登记加 CLI，自助注册放到 M5 |
| Q3 | 是否需要 Web 管理界面？ | 首版用 CLI 和管理 API，界面放到 M5 |
| Q4 | `verify` 是否就定位为挑战者（D11）？读取 CI 日志诊断是否需要？ | 挑战者为主，CI 诊断作为后续可选 |
| Q5 | 默认拓扑用 A 还是 B？ | `default` 档用 A，`deep` 档用 B |
| Q6 | UNCERTAIN 条目的默认处理？ | `mark`（标「存疑」后发布） |
| Q7 | 评论中是否标注模型来源（如「由 X 提出，经 Y 核验」）？ | 默认不标注，可配置开启 |
| Q8 | 主 agent 能否自提 finding？ | 可以，但须遵守 I5 |
| Q9 | 主 agent 和子 agent 的模型：配置写死，还是主 agent 从允许列表中自选？ | 配置给出允许列表，主 agent 在列表内选择 |
| Q10 | BLOCKING 是否默认写 commit status 卡合并？ | 默认关闭，按项目开启 |
| Q11 | 规模预期（项目数、日均 MR 数）？ | 影响 §13 是否需要多实例，待提供 |
| Q12 | reasoner 的输入是否只用 MR 描述和关联 issue，还是重点审改动的设计文档？ | 两者都给，设计文档优先 |
