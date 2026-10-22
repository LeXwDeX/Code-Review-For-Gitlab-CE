# 04 DAG 引擎与节点契约

> 细化 [ARCH.md](../../ARCH.md) §5、§6。本文给出：节点种类、profile 对应的图模板、状态迁移表、调度算法、每个节点的输入输出契约、裁决算法和评论渲染格式。

---

## 1. 节点种类

节点执行器分两类：**agent 节点**（跑一次 agent 工具循环）和**代码节点**（纯 Go 逻辑，不调用 LLM）。两类对 `pkg/dag` 没有区别，都实现 `NodeExecutor`。

| kind | 类别 | 来源块 | 默认 required | 工具 | 输出契约 |
|---|---|---|---|---|---|
| `plan` | agent 或代码 | 主 agent | 是 | 只读工具 | `PlanOut` |
| `code-review` | agent | `code-review` | 是 | 只读工具 + 绑定的 MCP / Skill | `CodeReviewOut` |
| `review.standards` | agent | `review` | 是 | 同上 | `ReviewOut` |
| `review.intent` | agent | `review` | 是 | 同上 | `ReviewOut` |
| `review.gate` | agent | `review` | 是 | 无工具 | `ReviewGateOut` |
| `reasoner` | agent | `reasoner` | 否 | 只读工具 | `ReasonerOut` |
| `gather` | 代码 | `adversarial` | 是 | — | `GatherOut`（并展开 verify 节点） |
| `verify` | agent | `adversarial` | 否 | 只读工具 + 绑定的 MCP | `VerifyOut` |
| `adjudicate` | 代码 | `adversarial` | 是 | — | `AdjudicateOut` |
| `synthesize` | agent | 主 agent | 是 | 只读工具 | `SynthesizeOut` |
| `onboarding` | agent | `onboarding` | 是 | 只读工具 + 绑定的 MCP / Skill | `OnboardingOut` |

`plan` 有两种实现，由 profile 的 `plan.mode` 决定：

- `code`：不调用 LLM。按文件把变更切成若干包（每包估算 token 不超过 `plan.package_tokens`，默认 60k；同目录文件尽量放一起），每包一个 code-review 节点。适合 `default` 档。
- `agent`：主 agent 读材料包摘要，输出 `PlanOut`。适合 `deep` 档。

---

## 2. 节点 ID 规则

| 节点 | ID |
|---|---|
| 规划 | `plan` |
| 每个包的评审 | `cr.<pkg>`、`rv.std.<pkg>`、`rv.intent.<pkg>`、`rv.gate.<pkg>` |
| reasoner | `reasoner` |
| 汇聚 | `gather` |
| 挑战 | `verify.<finding_seq>` |
| 规则裁决 | `adjudicate` |
| 主 agent 汇总 | `synthesize`；extend 后为 `synthesize.2` |
| extend 追加的挑战 | `verify.x<n>.<finding_seq>` |

`<pkg>` 由 plan 输出，只允许 `[a-z0-9_-]{1,32}`。

---

## 3. 图模板

### 3.1 `default` 档（拓扑 A）

```
plan(code) ──expands──▶ cr.p1 ─┐
                       cr.p2 ─┼──▶ gather ──expands──▶ verify.1 ─┐
                       cr.pN ─┘                       verify.k ─┼──▶ adjudicate ──▶ synthesize
                                                               ─┘
```

- 初始图只有 `plan`、`gather`、`adjudicate`、`synthesize` 四个节点；`gather` 依赖 `plan`，展开时把新增的 `cr.*` 加进 `gather` 的依赖（见 §5.3）。
- `gather` 只把 P0/P1（由 `challenge.only` 配置）交给 `verify`；其余不挑战，直接进入 `adjudicate`。
- `default` 档的 `synthesize` 可以配置为代码实现（`synthesize.mode: code`），直接把 `adjudicate` 结果渲染，不再调用 LLM。

### 3.2 `deep` 档（拓扑 B）

```
plan(agent) ──expands──▶ rv.std.p1 ─┐
                        rv.intent.p1 ┴─▶ rv.gate.p1 ─┐
                        rv.std.p2 ─┐                  │
                        rv.intent.p2 ┴─▶ rv.gate.p2 ─┼──▶ gather ─▶ verify.* ─▶ adjudicate ─▶ synthesize ─(extend?)─▶ …
                        cr.p1（另一个 provider）─────┤
                        reasoner（可选）──────────────┘
```

- 同一个包可以由多个 provider 各跑一遍（profile 中 `propose` 列了几个，就展开几份），`gather` 合并时统计票数。
- `synthesize` 可以发起一次 extend（§7.3）。

### 3.3 onboarding 工作流

```
onboarding ─▶ (代码) 渲染并写入 context_docs
```

只有一个 agent 节点；建索引在工作流开始前由 `workspace_service` 用 CLI 完成，不进 DAG。

---

## 4. 状态迁移表

```go
var nodeTransitions = map[NodeStatus][]NodeStatus{
	NodePending: {NodeQueued, NodeRunning, NodeSkipped, NodeFailed},
	NodeQueued:  {NodeQueued, NodeRunning, NodePending, NodeSkipped, NodeFailed},
	NodeRunning: {NodeCompleted, NodeFailed, NodeAborted, NodePending, NodeSkipped},
	// 终态：不可再迁移
	NodeCompleted: {}, NodeFailed: {}, NodeAborted: {}, NodeSkipped: {},
}

var workflowTransitions = map[WorkflowStatus][]WorkflowStatus{
	WorkflowPending: {WorkflowRunning, WorkflowCancelled},
	WorkflowRunning: {WorkflowCompleted, WorkflowFailed, WorkflowCancelled},
	WorkflowCompleted: {}, WorkflowFailed: {}, WorkflowCancelled: {},
}
```

说明（相对原引擎的裁剪）：

- 去掉 `paused`、`stepping`，以及与之相关的迁移。
- `queued → queued`：崩溃恢复后的幂等重入。
- `queued → failed`：排队等待超过节点超时。
- `running → pending`：崩溃恢复时，运行中的节点重置为 pending 并 `attempt+1`。
- `pending → failed`：上游 required 节点失败导致不可满足。

非法迁移返回 `TerminalViolationError` 或 `InvalidTransitionError`，并且**不写事件**。

---

## 5. 调度

### 5.1 主循环

```
Run(wf):
  emit workflow.started（如果是 pending）
  loop:
    snapshot := project(events)
    if snapshot 已终态: return
    if deadline 到期: Cancel(wf, "deadline"); return
    for n in readyNodes(snapshot):          # 依赖全部终态、自身 pending
        if 有依赖为 skipped 且全部依赖都 skipped: emit node.skipped(deps_skipped); continue
        if 有 required 依赖 failed/aborted:   emit node.failed(unsatisfied); continue
        if n.Condition 为假:                  emit node.skipped(condition_false); continue
        emit node.queued
        go runNode(n)                         # 先拿全局许可再执行
    if 所有节点终态:
        if 存在 required 节点 failed: emit workflow.failed
        else: emit workflow.completed
        return
    wait(节点完成通知 | ctx 取消 | 定时器)
```

### 5.2 执行单个节点

```
runNode(n):
  release := globalSem.Acquire(ctx)          # 默认 8
  defer release()
  attempt := n.attempt
  emit node.started(attempt)
  ctx, cancel := context.WithTimeout(wfCtx, n.Timeout - 已排队时长)
  out, err := executor.Execute(ctx, input(n))
  if 当前 attempt != attempt 或工作流已终态: 丢弃结果; return
  if err:  emit node.failed  (ctx 被工作流取消时 emit node.aborted)
  else:    emit node.completed(out)
           if n.Expands:
               nodes := expander.Expand(...)
               validate(nodes, budget)       # 失败则本节点视为 failed
               emit workflow.extended(nodes)
```

所有 `emit` 都走 `EventStore.Append(expectRev)`；遇到 `ErrStaleRevision` 时重新读取快照后再判断是否还需要写。

### 5.3 展开时的依赖改写

`gather` 必须等所有动态生成的评审节点完成后才能运行。展开 `plan` 时，Expander 同时返回一条依赖改写：把新节点 ID 追加到 `gather.DependsOn`。引擎在 `workflow.extended` 事件中记录 `{nodes, dep_patches: [{node: "gather", add: [...]}]}`，投影时一起应用。

约束：只能给**仍为 pending** 的节点追加依赖；目标节点已开始则展开失败。

### 5.4 失败语义

| 情况 | 处理 |
|---|---|
| required 节点失败 | 下游全部 `failed(unsatisfied)`，工作流 `failed` |
| 可选节点失败 | 下游照常运行；该输入填占位 `{"_unavailable": true, "reason": "..."}` |
| 依赖全部 skipped | 节点 `skipped(deps_skipped)` |
| verify 节点失败 | 对应 finding 的挑战结果记为 `uncertain` |
| 工作流取消 | 运行中节点 `aborted`，未开始节点 `skipped(workflow_cancelled)` |

### 5.5 预算校验

每次 `Create` / `Extend` 前检查：

- 节点总数 ≤ `budget.max_nodes`（默认 30）。
- extend 次数 ≤ `budget.max_extends`（默认 1）。
- 累计 token 超过 `budget.max_tokens` 时：不再展开 verify（`gather` 只输出不挑战的结果），并在汇总里注明「预算不足，部分条目未经核验」。

---

## 6. 节点输入输出契约

所有 agent 节点通过 `submit_result` 提交；下面的 Go 结构体用 `invopop/jsonschema` 生成 schema 交给模型，解码时 `DisallowUnknownFields` 并执行 `Validate()`。

### 6.1 公共类型

```go
type Severity string   // P0 | P1 | P2 | info
type Confidence string // high | medium | low

type FindingOut struct {
	Path       string     `json:"path"`
	Line       int        `json:"line,omitempty"`    // 0 = 无法定位到行
	Side       string     `json:"side,omitempty"`    // new（默认）| old
	Severity   Severity   `json:"severity"`
	Category   string     `json:"category"`          // correctness | security | concurrency | compat | performance | logic | maintainability
	Title      string     `json:"title"`             // ≤ 120 字符
	Body       string     `json:"body"`              // 问题说明 + 触发条件 + 影响
	Suggestion string     `json:"suggestion,omitempty"`
	Evidence   string     `json:"evidence"`          // 引用的代码或依据
	Confidence Confidence `json:"confidence"`
}

type CodeRef struct {
	Path      string `json:"path"`
	StartLine int    `json:"start_line"`
	EndLine   int    `json:"end_line"`
	Root      string `json:"root,omitempty"` // head | base
	Note      string `json:"note"`
}
```

### 6.2 各节点

```go
// plan（agent 模式）
type PlanOut struct {
	Risk        string    `json:"risk"`          // low | medium | high
	RiskReasons []string  `json:"risk_reasons"`
	Packages    []Package `json:"packages"`      // 1..max_packages
	RunReasoner bool      `json:"run_reasoner"`
	ChallengeOnly []Severity `json:"challenge_only"` // 默认 [P0, P1]
}
type Package struct {
	ID       string   `json:"id"`
	Blocks   []string `json:"blocks"`    // 允许列表内：code-review | review
	Files    []string `json:"files"`     // 必须是本次变更的文件
	Focus    string   `json:"focus"`     // 本包需要重点关注什么
	Provider string   `json:"provider,omitempty"` // 允许列表内；空则用 profile 默认
}

// code-review
type CodeReviewOut struct {
	Summary  string       `json:"summary"`
	Findings []FindingOut `json:"findings"`
}

// review.standards / review.intent
type ReviewOut struct {
	Verdict        string       `json:"verdict"`         // PASS | BLOCKING
	EvidenceStatus string       `json:"evidence_status"` // sufficient | insufficient
	Findings       []FindingOut `json:"findings"`
	EvidenceGaps   []string     `json:"evidence_gaps"`
	Summary        string       `json:"summary"`
}

// review.gate：合并两条通道
type ReviewGateOut struct {
	Verdict   string   `json:"verdict"`
	Accepted  []Ref    `json:"accepted"`  // 采纳的 finding：{node, index}
	Rebutted  []Rebut  `json:"rebutted"`  // 不采纳的必须逐条反驳
	Summary   string   `json:"summary"`
}
type Ref struct {
	Node  string `json:"node"`
	Index int    `json:"index"`
}
type Rebut struct {
	Ref
	Reason string `json:"reason"`
}

// reasoner
type ReasonerOut struct {
	ResultType string            `json:"result_type"` // CLEAN | FINDINGS | INSUFFICIENT_SUBJECT
	Findings   []ReasonerFinding `json:"findings"`
}
type ReasonerFinding struct {
	Severity    string     `json:"severity"`    // critical | notable | informational
	Dimension   string     `json:"dimension"`
	Finding     string     `json:"finding"`
	Where       string     `json:"where"`       // MR 描述 / issue / 文档中的位置
	Why         string     `json:"why"`
	Assumptions []string   `json:"assumptions"`
	Confidence  Confidence `json:"confidence"`
	Path        string     `json:"path,omitempty"` // 能落到代码时填写
	Line        int        `json:"line,omitempty"`
}

// verify：输入只有单条 finding（去掉来源节点、置信度和模型身份）
type VerifyOut struct {
	Status            string    `json:"status"`   // CONFIRMED | REFUTED | UNCERTAIN
	Reason            string    `json:"reason"`
	Evidence          []CodeRef `json:"evidence"` // REFUTED 时至少 1 条，否则按 UNCERTAIN 处理（I3）
	SuggestedSeverity Severity  `json:"suggested_severity,omitempty"`
}

// synthesize
type SynthesizeOut struct {
	Verdict        string          `json:"verdict"`  // PASS | BLOCKING
	Summary        string          `json:"summary"`  // 汇总评论正文（Markdown，≤ 3000 字符）
	Decisions      []Decision      `json:"decisions"`
	NewFindings    []FindingOut    `json:"new_findings"`    // origin=main，必须经过挑战（I5）
	ExtendRequests []ExtendRequest `json:"extend_requests"` // 最多 max_extends 次
}
type Decision struct {
	FindingID string   `json:"finding_id"`
	Action    string   `json:"action"` // publish | drop | mark | downgrade
	Severity  Severity `json:"severity,omitempty"`
	Reason    string   `json:"reason"`
}
type ExtendRequest struct {
	Kind      string   `json:"kind"`       // verify | review
	FindingID string   `json:"finding_id,omitempty"`
	Files     []string `json:"files,omitempty"`
	Focus     string   `json:"focus"`
}

// onboarding
type OnboardingOut struct {
	Overview      string   `json:"overview"`
	Architecture  string   `json:"architecture"`
	Modules       []Module `json:"modules"`
	Conventions   []string `json:"conventions"`
	BuildAndTest  []string `json:"build_and_test"`
	RiskHotspots  []string `json:"risk_hotspots"`
	ReviewFocus   []string `json:"review_focus"`
}
type Module struct {
	Path    string `json:"path"`
	Purpose string `json:"purpose"`
}
```

### 6.3 严重级别归一化

| 来源 | 原始值 | 归一化 |
|---|---|---|
| review / code-review | P0 / P1 / P2 | 原样 |
| reasoner | critical / notable / informational | P1 / P2 / info |
| verify | `suggested_severity` | 仅供 adjudicate 参考，不直接覆盖 |

---

## 7. 汇聚与裁决

### 7.1 gather（代码）

1. 收集所有 proposer 节点（`cr.*`、`rv.std.*`、`rv.intent.*`、`reasoner`）的 findings；`rv.gate.*` 中被反驳的条目标记为 `drop`（有理由），不再挑战。
2. 去重：同一 `path`，行号区间重叠（±3 行）且 `category` 相同 → 同组。组内取最高严重级别，正文保留证据最充分的一条，记录所有来源。
3. 计票（拓扑 B）：每个来源 1 票，同一 provider 的多票按 `same_model_weight` 打折。
4. 选择待挑战：严重级别在 `challenge_only` 内，且票数 < `consensus.min_votes` 的组。
5. 写入 `findings` 表，输出 `GatherOut{groups, to_challenge}`，并展开 `verify.<seq>` 节点。

挑战者 provider 选择（`challenge.provider: cross`）：从允许列表中选一个**不在该组来源 provider 集合里**的；没有可选的就用配置的 fallback，并把该 finding 的同模型标记设为真。

### 7.2 adjudicate（代码）

按 [ARCH.md](../../ARCH.md) §5.4 的裁决表计算每组的 `decision`：

```
for g in groups:
  if g 已在 gather 阶段 drop:        keep drop
  elif g.votes >= min_votes:         publish
  elif g 未挑战（不在 challenge_only）: publish
  elif challenge == CONFIRMED:       publish
  elif challenge == REFUTED && len(evidence) > 0: drop
  else (UNCERTAIN / REFUTED 无证据 / verify 失败):
       按 on_uncertain：drop | mark | judge（judge 交给 synthesize 决定）
  if !diffIndex.Locate(g.path, g.line): g.position_ok = false（只进汇总，I2）
```

### 7.3 synthesize（agent）与 extend

输入：`adjudicate` 的结果（每组：ID、严重级别、标题、正文、证据、挑战结果、规则建议的 decision）、MR 摘要、pipeline 摘要。**不输入**子 agent 对话。

输出校验（代码执行）：

| 不变量 | 检查 |
|---|---|
| I4 | 规则建议 `publish` 且已 CONFIRMED 的 P0/P1，若 decision 为 `drop` / `downgrade`，`reason` 不能为空，且长度 ≥ 20 字符 |
| I5 | `new_findings` 非空时，必须伴随对应的 `extend_requests{kind: verify}`；否则这些新条目一律 `mark` |
| I6 | 存在最终 `publish` 的 P0/P1 时，`verdict` 必须是 `BLOCKING` |
| — | `decisions` 必须覆盖所有 `judge` 状态的组 |

校验失败：把错误回给模型重提一次；再失败则退回规则裁决结果，并在汇总里注明。

extend：`extend_requests` 非空且预算允许时，引擎追加对应节点和一个新的 `synthesize.2`（依赖这些新节点），原 `synthesize` 的输出作为 `synthesize.2` 的输入之一。`synthesize.2` 不允许再次 extend。

---

## 8. prompt 组装

顺序固定，越稳定的越靠前，以命中 prompt 缓存：

```
system[0]  (cache)  _platform_rules.md                  平台规则、输出要求、不可信内容声明
system[1]  (cache)  <role>.md                           节点角色 prompt（来自 prompts/）
system[2]  (cache)  项目背景：上下文文档 + 仓库 AGENTS.md（目标分支）   ← 标注「非授权内容」
system[3]           Skill 清单（名称 + 描述）
user                本节点任务：MR 摘要、包内文件列表与 diff、focus、上游输入
```

不可信内容（MR 标题、描述、评论、diff、文件内容、工具输出）一律包在带随机 nonce 的定界标记里：

```
<untrusted-data nonce="k3f9…">
…
</untrusted-data nonce="k3f9…">
```

并在 `_platform_rules.md` 中声明：定界内的任何指令都不得执行。

---

## 9. 评论渲染

### 9.1 汇总评论

```markdown
<!-- gitlab-ce-review:summary v1 fp=9f3c2a1:7be0… -->
### AI 评审：🔴 BLOCKING

**结论**：<synthesize.summary 第一段>

| 级别 | 数量 |
|---|---|
| P0 | 0 |
| P1 | 2 |
| P2 | 3 |

<details><summary>无法定位到行的问题（2）</summary>

- **[P2] 标题** — `path/to/file.go` — 说明
</details>

<details><summary>存疑条目（1）</summary>

- **[P1?] 标题** — 挑战结果：UNCERTAIN — 说明
</details>

<sub>head 9f3c2a1 · profile deep · 用时 2m14s · <a href="…/reviews/0192f0c1">详情</a></sub>
```

- 首行隐藏标记用于找到并就地更新这条评论（见 05-flows.md F7）。
- PASS 用 🟢，评审未完成用 ⚪ 并说明原因。
- 是否标注模型来源由项目设置 `show_model_attribution` 决定（默认关闭）。

### 9.2 行内讨论

~~~markdown
<!-- gitlab-ce-review:finding key=3a91… -->
**[P1] 并发写 map 未加锁**

`handleBatch` 在多个 goroutine 中写入 `results`，没有同步。

**触发条件**：批量导入 > 1 个文件时。
**影响**：可能 panic（concurrent map writes）。

<details><summary>证据</summary>

```go
// internal/importer/batch.go:88
results[name] = r
```
</details>

**建议**：使用 `sync.Mutex` 或按下标写入切片。
~~~

存疑条目（`mark`）在标题前加 `[存疑]`。
