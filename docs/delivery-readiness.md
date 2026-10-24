# 交付就绪、升级/回退与真实验收计划

> 状态：计划与记录模板，不是执行结果。本文不表示已连接测试机、已升级、已回退或已通过外部集成验收。实际执行需按阶段另行授权。

本轮完成源码核对、证据归档、文档整理及隔离数据库测试；未连接远程环境，未读取 Docker/Compose 项目、卷或磁盘状态，未对真实数据运行升级、迁移、GitLab/OAuth/provider 调用。未知项在下方明确标为“未知”。

## 证据与边界

- 迁移实现：`internal/store/sqlite/sqlite.go:108-184`。
- 迁移文件：`internal/store/migrations/0001_init.sql`、`0003_review_state.sql`、`0004_context_revisions.sql`、`0005_retention_indexes.sql`、`0006_finding_annotations.sql`、`0007_project_access.sql`、`0008_publication_state.sql`。
- CLI：`cmd/reviewctl/main.go`；分页上限：`internal/server/params.go`。示例默认从 `REVIEW_ADMIN_TOKEN` 读取 bootstrap 凭据；若部署改了变量名，须在子命令前加 `--api-token-env <NAME>`。
- 操作步骤复用 [operations.md](operations.md)，本文不重复停止、整目录备份、空卷恢复和 masterkey 保持等全文。
- 交付边界、完整修复清单和归档由主 agent 维护，见 [delivery-boundary.md](delivery-boundary.md)。既有验证记录见 [Issue #20 评论 6369328377](https://github.com/LeXwDeX/Code-Review-For-Gitlab-CE/issues/20#issuecomment-6369328377)，本文不复制整套 Issue ledger。
- 本地证据目录：`artifacts/delivery/2026-10-24-review-repairs/`，已生成并由 `.gitignore` 排除，不提交；归档中不得放入真实密钥、token、数据库副本或含真实凭据的日志。

### 已核对事实

- 迁移按文件名排序，每个未应用文件单独开事务；`0001` 与 `0003` 是最早的两个版本，序列为 `1,3,4,5,6,7,8`，共 **7** 个文件。不能用 `max(version)=8` 当作已应用数量。
- 启动时读取 `schema_migrations` 中已应用版本，只跳过这些版本；数据库中已记录、但不在当前嵌入文件列表的高版本会被忽略而不报错。没有 down migration。
- `migration_rollback_test` 只覆盖 `0004` 单文件在“建表完成、记录版本前”失败时的事务回滚，以及之后重试；不覆盖整个升级批次失败、跨版本回滚或降级。
- `0007` 给 `users` 增加 `disabled INTEGER NOT NULL DEFAULT 0`，并新建空的 `project_grants` 表；升级不会给任何用户创建 grant。
- `0008` 增加 `publication_error TEXT NOT NULL DEFAULT ''`，用于保存有界、已脱敏的发布失败原因；`publication_state` 的终态 `failed` 由代码写入，不是该迁移新增的列值。
- 旧二进制可能打开已被新版本迁移的 schema，但它没有新权限逻辑，不能据此把“能启动”当作可安全回退。

## 当前准备状态与待真实执行

### 已完成（文档层）

- [x] 明确交付边界：不提交、不推送、不合并、不部署。
- [x] 核对迁移、回退测试、`0007`/`0008` 和现有 CLI 参数。
- [x] 设计两阶段授权、验收矩阵、回退触发和记录模板。
- [x] 复用现有运维文档的停服备份/空卷恢复/主密钥规则，避免重复流程。
- [x] 固定本地证据路径，并确定只链接边界文档和 Issue #20 评论。
- [x] 生成证据归档及 `delivery-boundary.md`，校验归档哈希和 Git 忽略规则。

### 待真实执行（未做）

- [ ] 连接目标测试机，读取实际 Compose 项目、卷映射、镜像和磁盘状态。
- [ ] 记录旧/新镜像的 tag 与 digest；确认旧镜像仍可取得或本地保留。
- [ ] 停机备份整个 `data`（含 WAL/SHM、workspaces）及配置/secret，并验证备份可恢复。
- [x] 本机隔离副本用旧 schema 升级到新 schema，验证迁移和授权默认值；未使用目标环境数据库。
- [ ] 在目标环境的断网副本上启动新镜像，验证 worker、recover 和 cleanup 不接触真实服务。
- [ ] 经另行授权后，在目标测试数据卷执行升级并等待 `/readyz`。
- [x] 本机模拟完成 2 用户/2 项目、grant/撤销/禁用/凭据撤销和发布状态接口验证（合成数据）。
- [ ] 在目标环境完成两阶段授权和 2 用户/2 项目真实验收。
- [ ] 执行真实 GitLab OAuth、webhook、SSE、publication 和外部 provider 测试；这些会访问真实环境并可能产生费用，需单独授权。

## 首次升级前记录（模板）

不假设旧测试机仍保持文档中的“99% 磁盘占用”；以下字段必须在目标机现场读取，未知时保持空白。

| 字段 | 实际值 |
|---|---|
| 执行时间 / 操作人 / 变更单 |  |
| 旧镜像 tag@digest |  |
| 新镜像 tag@digest |  |
| 实际 Compose 项目名 |  |
| 数据卷名称 / 容器内挂载点 |  |
| 配置文件与 secret 注入方式 |  |
| `data`、数据库、workspaces 大小 |  |
| Docker 主机可用磁盘 |  |
| 备份目录 / 校验和 |  |
| SQLite `PRAGMA integrity_check` |  |
| 旧镜像可用性（本地/可拉取） |  |
| masterkey 是否仍在原安全位置（只记存在性） |  |

升级前必须同时满足：旧镜像可用、备份完整且已校验、主密钥可用、入口已隔离、目标机资源足够。任一不满足即停止，不进入迁移。

## 入口隔离与备份

1. 暂停 GitLab webhook 或按项目禁用 hook，阻止人工触发、定时触发和 API 触发；如不能暂停，先在反向代理阻断 webhook 入口并记录 GitLab 侧可能的重试。
2. 记录旧实例停止时的 queued/running jobs、running/pending workflows 和 pending publications。新实例启动会由 worker/recover 接管并可能重放这些任务，不能假设队列为空；先人工核对 GitLab 已有评论/状态，避免重复副作用。
3. 停止旧服务后备份整个 `/app/data`，同时备份实际配置和 secret；不要把真实密钥写入仓库、本文或证据目录。
4. 保留旧镜像、旧卷和升级前快照，直到新实例验收完成；不要让两个实例共享同一 SQLite 卷。
5. 启动新实例会执行迁移、启动 worker、运行 recover/publish pending 和 cleanup，因此断网副本验证必须阻断对 GitLab、OAuth 和模型 provider 的出站访问，并使用占位配置；不得把真实库副本直接接到真实外部服务。
6. 恢复或离线验证使用新空卷，不把备份覆盖到旧数据目录，不混入旧 WAL。

## 升级操作（计划）

1. 完成前置记录和备份校验，确认旧镜像与备份均可恢复。
2. 隔离入口，停止旧服务，保存升级前全量快照。
3. 复制快照到隔离环境，启动新镜像；阻断外联，观察迁移、worker、recover 和 cleanup。确认迁移记录为 7 个已知版本，不把最大版本号当数量。
4. 隔离验证通过且已获部署授权后，在目标测试数据卷启动新镜像；等待 `/readyz` 200，检查日志和迁移错误。生产部署不在本计划当前执行范围内。
5. 先完成阶段 A 管理面检查，再执行阶段 B 授权；授权完成前不向普通用户开放业务入口。
6. 按验收矩阵逐项记录；任何跨项目访问、误 PASS、数据损坏或迁移失败都触发回退评估。
7. 本轮只形成计划和记录模板；实际升级、推送、部署和外部调用另行授权。

## 两阶段授权

### 阶段 A：只开管理面

升级后 `project_grants` 为空，已有用户默认没有任何项目访问权。平台 `admin` 标签本身只表示管理面角色，`bootstrap` Bearer 的用户 ID 为 0，没有 grant，不能读取项目业务数据；`projects list` 对 bootstrap 通常为空，不能据此判断项目不存在。

先用管理员凭据列出本地用户和全部已登记项目，结合人工批准矩阵确定“本地用户 ID × 项目 ID × read/manage”：

```bash
docker compose exec -T review /app/reviewctl users list
docker compose exec -T review /app/reviewctl projects all
```

- `users list` 与 `projects all` 会自动跟随 `next_cursor` 汇总全部页。
- 未登录过 OAuth 的 GitLab 用户不在本地 `users` 表，无法预先授权；先让其完成一次登录。

### 阶段 B：按数字 ID 授权并验证

本地用户 ID 即 GitLab user id；项目 ID 使用 `projects all` 返回的数字 ID。只通过管理面授权，不直接写 SQL。以下是四个独立用法示例，不要顺序执行；按批准矩阵选择一次操作，示例 ID 不代表真实授权对象。

```bash
docker compose exec -T review /app/reviewctl projects grants list 42
docker compose exec -T review /app/reviewctl projects grants set 42 17 read
docker compose exec -T review /app/reviewctl projects grants set 42 17 manage
docker compose exec -T review /app/reviewctl projects grants delete 42 17
```

- `projects grants list <项目 ID>` 只请求单页，默认 50 条、上限 200 条，CLI 不会自动翻页；超过 50 条时用管理 API 的 `GET /api/v1/admin/projects/{id}/grants?cursor=...` 继续读取。
- 每次 `set` 后重新 `list` 核对，再让用户用自己的 session/API token 验证业务读取。
- 授权矩阵先由人工批准；禁止用直接 SQL 插入 `project_grants` 代替审批和审计。

## 真实验收矩阵

使用 2 个用户、2 个项目，建议记为 U1/U2、P1/P2；结果栏留空，真实执行后再填。外部 GitLab/provider 调用需另行授权并确认费用。

| 场景 | 操作 | 预期 | 结果 |
|---|---|---|---|
| 管理面 | bootstrap 调 `users list`、`projects all` | 成功；项目业务接口仍不可读 |  |
| 仅管理面 | bootstrap 读 P1 项目详情、指定评审、项目统计 | 404，不泄露存在性 |  |
| 跨项目 | U1 仅授权 P1，读取 P2 | 404；列表不出现 P2 |  |
| 只读边界 | U1 对 P1 执行 manage 写操作 | 403 |  |
| 管理授权 | U2 对 P2 有 manage，执行允许的项目写操作 | 成功 |  |
| 宿主配置 | U2 用 manage 创建项目级 MCP/Skill/块绑定 | 403；宿主执行配置仍需平台管理员 + manage |  |
| 会话/Token | 同一用户分别用当前 session 和 API token 读取 | 授权结果一致 |  |
| 撤销/禁用 | `users revoke` 后旧 token 失效；`users disable` 后 session/token 失效且 OAuth 被拒 | 旧凭据不可用 |  |
| 重新登录 | `enable` 后重新 OAuth；不恢复旧 token | 登录成功、旧凭据仍无效、grant 保留 |  |
| SSE | 打开已授权评审流，随后撤 grant 或禁用用户 | 下一轮询关闭，不继续泄露 |  |
| 统计 | 有 read 的用户读取授权项目统计；bootstrap 读项目统计 | 仅授权范围可见；bootstrap 404 |  |
| Payload | read 用户请求 `include_payload=true` | 403；manage 才可读 |  |
| 扩展 | read 用户列项目级扩展；访问平台级扩展 | 项目级按 grant；平台级 403 |  |
| 正常发布 | 完成评审并发布 | `published`，汇总评论/状态与 MR 一致 |  |
| 部分失败 | 模拟一项发布失败、其他项成功 | 可重试失败保持 `pending`；永久失败为 `failed`；不重复副作用 |  |
| 重启恢复 | 发布 pending 或运行中断后重启 | recover 继续，状态最终一致 |  |
| stale | 发布前改变 MR head/base | `skipped` 并记录原因，不发布旧结果 |  |
| INCOMPLETE | 评审结果为 INCOMPLETE | 明确显示 INCOMPLETE，不得写 PASS 或绿灯状态 |  |

## 回退触发与操作计划

以下为 0 容忍触发，出现任一项即停止放行并评估回退：

- 任何跨项目读取或写入。
- 任何误 PASS，或 INCOMPLETE 被显示为通过。
- 数据损坏、SQLite integrity 失败、workspaces 与数据库不一致。
- 迁移失败、迁移记录不完整，或启动后 schema 与预期不符。

回退操作计划（本轮不执行）：

1. 停止新实例并隔离入口；保存升级后新数据的只读事故快照，保留新卷、旧卷、旧镜像和日志。
2. 新建空卷，恢复升级前全量数据，准备匹配的旧镜像、旧配置和原 masterkey；不覆盖旧卷，不混入新 WAL。**启动旧实例之前必须阻断 GitLab/OAuth/provider 出站访问**，并确认入站维护隔离生效。不能先启动再补防火墙规则，也不能把停用 webhook 当作阻止后台发布。
3. 不使用 down SQL，不删除 `schema_migrations` 行来绕过旧二进制检查，不只回退镜像而保留已迁移数据库。
4. 先在出站阻断下启动旧实例，按同样的隔离要求处理未知的旧版 worker/recover/publish 行为；不假设旧版提供“仅启动管理面”开关。保留不可变的原始备份，因为副本启动也可能改写任务状态。核对旧数据、镜像 digest、queued/running jobs 和 pending publications，并逐项比对 GitLab 已有外部副作用；核对完成且获得授权前，不解除出站限制。
5. 数据库能打开只允许继续受控管理面/只读核对，不能据此开放普通业务；恢复访问必须满足下述安全边界。旧镜像不能满足项目隔离要求时维持维护状态，转向安全版本修复，不以恢复可用性覆盖授权要求。

### 回退后的安全边界

- 升级后新增的 grant、禁用、撤销、反馈等写入不在旧快照中，回退会丢失；需要重新审批和补录，不能盲目合并。
- 旧快照中的 session/token 可能随数据库恢复而复活。旧代码没有项目隔离，可能让用户看到超出范围的数据。
- 因此回退后保持维护状态：阻断 OAuth 登录、所有用户 token 及既有浏览器 session 的业务请求，仅保留受控运维/管理面通道，不直接对普通用户重新开放。只有在恢复撤销语义或部署安全修复版本后，才可重新开放业务访问。
- 维护入口默认全拒：对普通网络阻断 `/auth/*`、业务 `/api/v1/*`、SSE 和控制台页面；只通过运维独占的回环/受控隧道开放核对所需接口，使用 bootstrap Bearer。该通道拒绝浏览器 Cookie 和个人 API token，包括旧库中的 admin 会话；具体代理/网络规则必须在目标环境验证，不能依赖旧应用实现新 grant 策略。
- GitLab 外部 note、commit status 等副作用不会随数据库恢复自动回滚。恢复发布前必须人工核对已发布内容和状态，避免重复评论、重复状态或错误覆盖。

## 放行与归档

### 放行条件

- [ ] 前置记录完整，旧镜像和新镜像 digest 可追溯。
- [ ] 全量备份、配置/secret 备份和 masterkey 均可用，恢复演练成功。
- [ ] 入口隔离完成，GitLab 重试和 pending 发布已有人工核对方案。
- [ ] 断网副本完成迁移、worker、recover 和 cleanup 验证。
- [ ] 两阶段授权完成，2 用户/2 项目矩阵全部通过。
- [ ] publication 正常、部分失败、重启恢复、stale、INCOMPLETE 均有记录。
- [ ] 没有未处置的跨项目访问、误 PASS、数据损坏或迁移失败。

前两项及入口隔离是进入授权测试升级的前置条件；迁移、授权和发布验收在隔离的测试实例中完成，全部勾选后才可开放该实例的普通业务入口。任一未满足或出现 0 容忍触发，保持维护状态。生产部署还需另行授权、当前提交的 CI 和发布审批，不能由此表自动放行。

### 归档

- 固定证据目录：`artifacts/delivery/2026-10-24-review-repairs/`（已生成，作为 local only/gitignored 证据保留，不提交）。
- 归档内容建议按证据类别保存：前置记录、镜像 digest、备份校验、迁移/就绪日志、授权矩阵、验收结果、回退决策；不复制整套 Issue ledger。
- 已用 `git check-ignore` 核对归档目录被排除；完整性清单与有限敏感信息检查见边界文档。部署前记录尚未产生，不应与本地修复验证日志混为一谈。
- 关联文档：[delivery-boundary.md](delivery-boundary.md)；既有验证评论：[Issue #20 / 6369328377](https://github.com/LeXwDeX/Code-Review-For-Gitlab-CE/issues/20#issuecomment-6369328377)。

## 本轮文档检查

- 已执行：源码与现有运维文档核对、代码围栏与相对链接检查；`operations.md` 已加入导航和回退启动前的出站隔离要求。
- 相对链接：`operations.md`、`delivery-boundary.md` 均存在。
- 外部链接：Issue #20 与评论 `6369328377` 已用 `gh` 只读核对（Issue 为 OPEN，评论存在）。
- 独立验证：SQLite 迁移、授权、撤销和发布状态 12 个针对性测试以 `-race -count=1` 通过；使用临时或内存数据库。归档初始 790 个文件的大小/哈希、350 文件基线、权限及忽略规则均通过；日志归档在 `checks/`。
- 本机隔离模拟：真实 HTTP 认证/授权路径 11/11 通过；旧库升级与回滚风险演示 6/6 通过。详见 [模拟报告](simulation-report.md)。该结果不代表真实 OAuth/GitLab/provider 或部署验收。
- 未执行：未连接远程环境、未运行 Docker/Compose、未对真实数据库执行迁移或回退、未调用真实 GitLab/OAuth/provider。隔离单元测试不等于真实升级演练。
