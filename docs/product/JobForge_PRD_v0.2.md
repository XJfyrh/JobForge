# JobForge：下一阶段 PRD（v0.2 定稿）

> 文档版本：v0.2  
> 文档状态：定稿（Q1–Q6 已确认，2026-08-04）  
> 创建日期：2026-08-04  
> 基线版本：PRD v0.1（S0–W7 已全部交付）  
> 计划周期：约 4 周；可裁剪为 3 周  
> 前置文档：[PRD v0.1](JobForge_PRD_v0.1.md)、[架构](../architecture.md)、[故障语义](../failure-semantics.md)、[性能基线](../benchmark.md)、[ADR 索引](../adr/README.md)

本文档不重写 v0.1 已固定的产品边界。v0.1 中的状态机、并发规则、交付语义、错误分类和性能门禁在本版本中**全部沿用**，本文仅在其上叠加下一阶段的增量需求。凡与 v0.1 冲突的语义变更，必须先通过 ADR，不在本文档中擅自引入。

---

## 0. 决策摘要

v0.1 定义的 P0 范围已在 S0–W7 阶段全部交付：任务全生命周期、租约与 fencing、重试与 DLQ、取消竞争、Scheduler advisory-lock 高可用、租户隔离与背压、Python SDK、OTel/Prometheus/pprof 可观测性，以及 AT-01~AT-12 全部验收测试。W4 性能基线已冻结，W7 复测确认无回归且全部改善。

v0.2 的主题是 **“可靠性验证规模化 + 事件与运维 P1 裁剪落地”**（Q1 决策：混合方案，硬化先行，2026-08-04 确认）：

1. **闭环 P0 遗留证据缺口**：NFR-001（100 轮 Worker kill 无静默丢失）与 NFR-002（万级幂等任务重复副作用为 0）目前没有对应规模的自动化测试，v0.2 将其补齐为独立的 scale 测试套件。
2. **让 outbox 成为可演进的事件底座**：outbox_events 目前只在任务事务中写入、无发布方。v0.2 引入轻量 outbox publisher，维持 [ADR-0003](../adr/0003-event-notification.md)（PostgreSQL LISTEN/NOTIFY，不引入外部 MQ）的决策，不引入 JetStream 硬依赖。
3. **文档与代码对齐**：修订 v0.1 中与实际交付不一致的表述（§3.2 观测组件、§17 交付清单、failure-semantics.md 的 AT-01 映射），并为 Compose 增加可选观测 profile。
4. **运维 CLI**：提供 `jobforge ctl` 只读与 DLQ 恢复操作入口，替代完整 Web UI。

原 §12.3 的六项范围决策 Q1–Q6 已全部定稿（确认日期 2026-08-04）：**Q1 = C**（混合方案，硬化先行）、**Q2 = A**（轻量 outbox publisher，维持 ADR-0003 LISTEN/NOTIFY）、**Q3 = A**（Compose 可选观测 profile，非默认强依赖）、**Q4 = A**（`-tags scale` 测试补齐 NFR-001/002 规模证据）、**Q5 = B**（运维界面优先 CLI，不做完整 Web UI）、**Q6 = B**（Claim 热点优化冻结不动，启动需先出 ADR）。本文档各章节均按上述决策编写；后续如需变更任一决策，按 ADR 流程执行。

以下 v0.1 边界在 v0.2 中**不可变**：

- 交付语义为 at-least-once，不承诺 exactly-once；外部副作用由业务幂等保护；
- PostgreSQL 是 P0 唯一事实源；任何事件系统只能通过 outbox 作为下游适配器，不进入任务状态事务；
- Claim 事务原子更新 owner、lease、attempt、fencing token 与 state；陈旧写入返回 `STALE_LEASE`；
- 只运行预注册 Handler，不引入任意代码执行；
- 性能相对 W4 冻结基线不可回退超过 v0.1 §11.2 阈值。

---

## 1. 背景与当前基线

### 1.1 已交付能力（截至 W7，2026-08-03）

| 领域 | 状态 | 证据 |
|---|---|---|
| 任务控制面 FR-001~005 | 完成 | HTTP API：submit/get/list/cancel/retry；`tests/integration/gateway_test.go` |
| Worker 可靠执行 FR-101~108 | 完成 | gRPC Gateway + Worker Runtime；AT-01~04、AT-11 测试 |
| 调度与 HA FR-201~204 | 完成 | `TestSchedulerAdvisoryLock`、`TestSchedulerFailover`（AT-09） |
| 多租户 FR-301~303 | 完成 | `TestTenantAT10Isolation`；背压软/硬阈值 + `QUEUE_OVERLOADED` + `jobforge_tenant_throttled_total` |
| SDK 与 Demo FR-401~403 | 完成 | `sdk/python`（submit/get/cancel/retry）；demo.echo/sleep/fail/http/idempotent_effect、pagewise.reindex |
| 可观测 FR-501~504 | 完成 | `TestObservabilityAT12FullSpans`；10 个核心指标；pprof 绑定 localhost:6060 |
| 故障语义文档 | 完成 | `docs/failure-semantics.md` 故障矩阵与恢复路径 |
| 性能基线 | 冻结 | `docs/benchmark.md` W4 基线 + W7 复测 |

### 1.2 冻结性能基线（不可回退基准）

| 指标 | W4 基线 | W7 最终 | 门禁阈值（v0.1 §11.2） |
|---|---|---|---|
| Enqueue | 2,715,866 ns/op | 2,563,809 ns/op | 吞吐下降 < 15% |
| Claim | 4,171,091 ns/op | 3,553,890 ns/op | 吞吐下降 < 15% |
| e2e Submit 吞吐 | 278.01 jobs/sec | 363.92 jobs/sec | 同上 |
| e2e p50 / p95 / p99 | 9.59 / 14.23 / 69.12 ms | 8.41 / 12.85 / 25.48 ms | 控制面 p95 恶化 < 20% |
| Goroutine 稳态 | 差异 0 | 差异 0 | ±5% |

恢复时间上界：任务恢复 ≤ lease_ttl + scan_interval + 2s（默认 33s）；Scheduler 接管 ≤ 2 × lock_retry_interval + 2s（默认 12s）。

### 1.3 已识别缺口（v0.2 的直接动因）

| # | 缺口 | 性质 |
|---|---|---|
| GAP-1 | NFR-001（100 轮 Worker kill）与 NFR-002（万级幂等）无对应规模测试；现有故障测试为单场景 | 证据缺口 |
| GAP-2 | PRD v0.1 §3.2 要求 Compose 启动观测组件，但 ADR-0004 选择零外部依赖，compose.yaml 只有核心服务 | 文档未对齐 |
| GAP-3 | PRD v0.1 §17 交付清单写 `cmd/api`/`cmd/scheduler`/`cmd/worker`/`tests/fault`，实际为单二进制 `cmd/jobforge` 子命令与 `tests/integration/fault_test.go` | 文档未对齐 |
| GAP-4 | failure-semantics.md 将 AT-01 映射到 `TestGatewayRegisterWorker`，并发领取证据实际在 `job_store_test.go` | 文档未对齐 |
| GAP-5 | outbox_events 只写不发：无 publisher、无消费方、无 retention 策略 | 功能缺口（P1 前置） |
| GAP-6 | v0.1 的 P1 项（FR-304/305、FR-404、FR-505、cron、mTLS）均未实现 | 范围待定 |

---

## 2. 下一阶段目标

| ID | 目标 | 对应缺口 |
|---|---|---|
| G-01 | 以 scale 测试套件闭环 NFR-001/NFR-002 的字面验收口径，形成可复现的可靠性报告 | GAP-1 |
| G-02 | outbox publisher 可用：状态事件 at-least-once 发布、有进度追踪与重复投递指标 | GAP-5 |
| G-03 | `jobforge ctl` CLI 覆盖查询与 DLQ 人工恢复路径 | GAP-6（FR-505 裁剪） |
| G-04 | 文档与代码 100% 对齐；Compose 提供可选观测 profile | GAP-2/3/4 |
| G-05 | 全部变更通过性能门禁：相对 W4 基线不回退 | — |

## 3. 非目标与明确排除项

以下内容**不属于 v0.2**（沿用 v0.1 §3.3 并叠加）：

- 宣称 exactly-once delivery；
- JetStream/NATS 硬依赖与外部 MQ 进入任务状态事务（Q2 已确认：v0.2 采用轻量 outbox publisher，维持 ADR-0003；未来引入 JetStream 需新 ADR）；
- 完整 Web 管理后台与低代码工作流画布（Q5 已确认：运维界面以 CLI 交付）；
- cron/周期任务、队列暂停与恢复（推迟到 v0.3 评估）；
- Worker mTLS、密钥轮换、payload 加密（推迟）；
- Kubernetes Operator、跨地域容灾、多集群调度；
- Claim 协议变更（如 payload 分离优化）（Q6 已确认：冻结不动；未来启动必须先出 ADR）；
- 复刻 Temporal 的 Workflow History 与确定性重放；
- 允许用户上传并执行任意代码。

---

## 4. 用户场景与核心问题

| 场景 | 角色 | 核心问题 | v0.2 应对 |
|---|---|---|---|
| 可靠性评审 | 项目评审者/面试官 | “100 轮 kill 不丢任务、万级重复投递零副作用”有证据吗？ | FR-601/602 scale 套件 + 可靠性报告 |
| 事件消费 | 应用开发者 | 任务进入终态后如何被下游感知，而不用轮询 HTTP？ | FR-603 outbox publisher |
| DLQ 运维 | 系统维护者 | 不开数据库客户端如何查看与恢复 dead 任务？ | FR-604 `jobforge ctl` |
| 观测演示 | 系统维护者 | 如何在 Compose 环境看到指标曲线而不是裸 /metrics？ | FR-605 obs profile |
| 文档审查 | 项目评审者 | PRD 交付清单和实际目录为什么不一致？ | G-04 文档对齐包 |

---

## 5. 功能需求

编号延续 v0.1 的 FR 体系，使用 6xx 段。

### 5.1 规模化可靠性验证

| ID | 优先级 | 需求 | 验收标准 |
|---|---|---|---|
| FR-601 | P0 | 循环故障注入套件：N 轮（默认 100，可配置）Worker kill / lease 过期场景下，非终态任务不得静默丢失 | 以 `go test -tags scale` 独立运行；每轮记录丢失计数，最终为 0；输出每轮恢复耗时分布 |
| FR-602 | P0 | 万级幂等断言套件：10,000 个包含重复投递的任务，基于 `demo.idempotent_effect` 的副作用表断言 | 重复业务副作用计数为 0；套件可分批执行；结果写入报告 |
| FR-603 | P0 | scale 测试不进入默认 CI（控制时长），由独立命令/工作流触发；报告归档于 `docs/benchmark.md` 或独立的可靠性报告节 | CI 默认套件时长不因 scale 套件增加 |

规模参数（100 轮、10,000 任务）直接采用 v0.1 NFR-001/NFR-002 字面值（Q4 已确认：以 `-tags scale` 套件补齐证据）；如因环境限制需降采样，必须同步修订 PRD 措辞并记录理由。

### 5.2 Outbox 事件发布

| ID | 优先级 | 需求 | 验收标准 |
|---|---|---|---|
| FR-610 | P0 | outbox publisher 以轮询方式发布 `published_at IS NULL` 的事件；发布语义为 at-least-once | 发布失败不影响任务状态事务；重试有退避；事件按 event_id 可幂等消费 |
| FR-611 | P0 | 发布通道默认为 PostgreSQL LISTEN/NOTIFY（维持 ADR-0003）；通道可插拔，但不引入外部 MQ 依赖 | 默认部署零新增外部服务 |
| FR-612 | P0 | 发布进度追踪：更新 `published_at` 与 `publish_attempts`（字段已存在于 0004_create_outbox_events migration） | 可通过 SQL 与指标查询积压 |
| FR-613 | P0 | retention 策略：已发布事件按保留期清理（新增 versioned migration，只追加不改写既有 migration） | 清理只删除 `published_at IS NOT NULL` 且超过保留期的行；保留期可配置 |
| FR-614 | P1 | 重复投递与发布失败计数指标 | 见 §8 指标清单 |

**约束**：publisher 崩溃后重启必须从 outbox 表恢复进度，不依赖内存状态；publisher 不得改写任务核心状态。

### 5.3 运维 CLI

| ID | 优先级 | 需求 | 验收标准 |
|---|---|---|---|
| FR-620 | P0 | `jobforge ctl` 子命令：`list`（按 state/queue/type 分页）、`get`（详情 + attempt 时间线）、`cancel`、`retry`（dead/cancelled 人工重试） | 复用现有 HTTP API 与 API key 鉴权；输出支持 table/json；不新增服务端特权路径 |
| FR-621 | P1 | `ctl outbox status`：显示 outbox 积压与最旧未发布时间 | 只读查询 |

### 5.4 文档对齐与 Compose 观测 profile

| ID | 优先级 | 需求 | 验收标准 |
|---|---|---|---|
| FR-630 | P0 | 修订 v0.1 §3.2/§17 与实际交付的偏差，或在 v0.2 中以取代条款明确新事实（v0.1 原文不改写历史结论） | 交付清单与 `cmd/jobforge` 单二进制结构一致 |
| FR-631 | P0 | failure-semantics.md 的 AT-01 测试映射修正为并发领取的真实证据测试 | 故障矩阵每一行可追溯到真实测试函数 |
| FR-632 | P0 | `deploy/compose.yaml` 增加可选观测 profile（如 `--profile obs` 拉起 Prometheus + Grafana，抓取各服务 :6060/metrics）；默认 `up` 行为不变（Q3 已确认） | 默认启动无新增依赖；obs profile 可展示 jobforge_* 指标曲线 |

---

## 6. 非功能需求

| ID | 要求 |
|---|---|
| NFR-201 | 性能门禁沿用 v0.1 §11.2：吞吐相对 W4 下降 < 15%、控制面 p95 恶化 < 20%、goroutine 稳态 ±5%；v0.2 结束时复测并更新 `docs/benchmark.md` |
| NFR-202 | `go test -race ./...`（默认套件）无数据竞争；scale 套件同样在 race 下至少抽样通过一轮 |
| NFR-203 | publisher 引入后，任务状态事务路径（enqueue/claim/complete/fail）性能不得因 outbox 发布逻辑产生可测量回退（发布在事务外异步进行） |
| NFR-204 | scale 套件单轮故障注入的恢复时间仍遵守 NFR-003 上界（lease_ttl + scan_interval + 2s） |
| NFR-205 | CLI 不得记录 API key、完整 payload 或 Authorization header（沿用 v0.1 §11.4 日志纪律） |

## 7. 可靠性与故障语义要求

**全部沿用 v0.1 §7/§8 与 failure-semantics.md**，包括：

- at-least-once 投递；业务幂等保护外部副作用；
- 状态机与八个终态/并发规则（FOR UPDATE SKIP LOCKED、fencing 校验、`STALE_LEASE`、事务先提交者生效、cancelling 语义、lease 回收规则）；
- 重试退避公式与 fatal/retryable 错误分类（ADR-0002）；
- 人工重试克隆新 job_id 并记录 `retry_of_job_id`（ADR-0001）。

v0.2 新增的事件发布语义：

1. 事件发布是 at-least-once：消费方必须按 `event_id` 幂等去重；
2. 发布失败、重复发布或 publisher 崩溃均不得改变任务状态；
3. 事件内容只包含 aggregate_id、event_type 与既有 outbox payload，不新增敏感字段；不记录完整业务 payload（沿用 v0.1 §11.4）。

---

## 8. 可观测性要求

沿用 v0.1 §12 的 10 个核心指标与标签纪律（job_id/trace_id 不作 metrics label）。新增：

| 指标 | 类型 | 关键标签 | 说明 |
|---|---|---|---|
| `jobforge_outbox_pending` | Gauge | — | `published_at IS NULL` 的事件数（采样） |
| `jobforge_outbox_published_total` | Counter | event_type | 成功发布计数 |
| `jobforge_outbox_publish_failures_total` | Counter | event_type, reason | 发布失败计数 |

Trace 要求：publisher 发布动作产生独立 span（如 `outbox.publish`），不与任务执行 span 混用；span 属性不含完整 payload。

---

## 9. 兼容性影响

| 面 | 影响 | 说明 |
|---|---|---|
| HTTP API | 零破坏 | 不修改既有端点与错误码；CLI 是纯客户端 |
| gRPC Proto | 零破坏 | `jobforge.worker.v1` 不改动；如未来启动 Q6 优化仅允许向后兼容新增字段，且需 ADR |
| Python SDK | 不变 | submit/get/cancel/retry 语义不变 |
| Migration | 只追加 | 仅新增 versioned migration（outbox retention 相关索引/清理支持）；不改写已应用 migration |
| 状态机/错误码 | 不变 | 无新增状态；无新增对外错误码（publisher 内部错误仅进入指标与日志） |
| 部署 | 可选扩展 | obs profile 为 opt-in；默认 Compose 行为不变 |

---

## 10. 测试与验收标准

### 10.1 新增验收矩阵

| ID | 场景 | 操作 | 预期结果 |
|---|---|---|---|
| AT-13 | 循环故障注入 | scale 套件执行 N 轮（默认 100）Worker kill / lease 过期 | 非终态任务零静默丢失；每轮恢复时间符合 NFR-003 |
| AT-14 | 万级幂等 | 10,000 任务含重复投递 | `demo.idempotent_effect` 副作用表重复计数为 0 |
| AT-15 | Outbox 重复投递 | 模拟发布失败重试与 publisher 崩溃重启 | 事件可重复发布；消费方按 event_id 去重后效果一致；任务状态不受影响 |
| AT-16 | CLI DLQ 恢复 | `jobforge ctl retry` 对 dead 任务 | 克隆新 job_id 执行；原任务终态不变；有审计记录 |

### 10.2 不回退要求

- AT-01~AT-12 全部既有测试保持通过；
- 默认 CI 门禁（golangci-lint、`go test -race ./...`、ruff、mypy、buf lint）不变；
- scale 套件与默认套件物理隔离（build tag 或独立目录），默认 `go test ./...` 不执行 scale 用例。

### 10.3 测试策略补充

- scale 测试必须使用真实 PostgreSQL（沿用 v0.1 §15）；
- Windows 本地运行需遵循 AGENTS.md 的 `JOBFORGE_TEST_DSN` 约定；
- publisher 测试覆盖：正常发布、发布失败重试、崩溃恢复、retention 清理边界（未发布事件不得被清理）。

---

## 11. 性能基线与不可回退要求

1. W4 冻结基线（见 §1.2）是 v0.2 的唯一对比基准；
2. v0.2 结束（M3）必须在相同环境复测微基准与 e2e 基准，结果追加到 `docs/benchmark.md`；
3. 门禁：吞吐下降 < 15%、控制面 p95 恶化 < 20%、goroutine 稳态 ±5%；
4. publisher 轮询不得对 PostgreSQL 产生可观测的持续性压力：空闲时轮询间隔可配置且默认 ≥ 1s，积压为 0 时退避；
5. W6 已定位的 claim pgx 解码热点保持“记录不实施”状态（Q6 已确认：冻结不动）；未来如需启动优化，必须先通过 ADR。

---

## 12. 风险、依赖与开放问题

### 12.1 风险

| 风险 | 可能影响 | 应对 |
|---|---|---|
| scale 套件在 Windows Docker Desktop 下耗时过长 | 开发反馈循环变慢 | 支持轮数/任务数参数化；CI 中在 Linux runner 运行 |
| publisher 与 LISTEN/NOTIFY 交互出现重复风暴 | 指标噪声、消费方压力 | at-least-once 明确写入文档；发布侧限流与退避 |
| retention 清理误删未发布事件 | 事件丢失 | 清理条件严格限定 `published_at IS NOT NULL` + 保留期；专项测试 |
| scale 测试暴露既有并发缺陷 | 返工 | 视为阶段成果而非风险；发现即修复并补 ADR/文档 |
| obs profile 引入镜像依赖 | 拉取成本 | profile 默认不启动；文档说明可选性 |

### 12.2 依赖

- 现有 fault/e2e 测试框架与 `benchmarks/` 结构；
- ADR-0001/0002/0003/0004 全部继续有效，v0.2 不预期推翻任何已接受 ADR；
- Q2 已确认采用轻量 outbox publisher（维持 ADR-0003）；未来如引入 JetStream 路线，必须先新增 ADR 并修订本 PRD 的事件章节。

### 12.3 范围决策记录（Q1–Q6，已确认）

以下六项范围决策已于 **2026-08-04** 全部确认（Accepted），不再是开放问题；本文档各章节按确认结果编写。

| 编号 | 问题 | 决策 | 决策要点 | 确认日期 |
|---|---|---|---|---|
| Q1 | 下一阶段主轴 | **Accepted：C 混合方案，硬化先行** | 先闭环 scale 验证与文档对齐，再落地 publisher 与 CLI；备选 A（硬化优先）/B（功能优先）不采纳 | 2026-08-04 |
| Q2 | 事件路线 | **Accepted：A 轻量 outbox publisher** | 维持 ADR-0003 LISTEN/NOTIFY，不引入外部 MQ；备选 B（JetStream 适配器）不采纳，未来引入需新 ADR | 2026-08-04 |
| Q3 | Compose 观测组件 | **Accepted：A 可选观测 profile** | `--profile obs` opt-in，默认 `up` 行为与基准环境不变；备选 B（仅修订措辞）不采纳 | 2026-08-04 |
| Q4 | NFR 规模口径 | **Accepted：A 补齐 `-tags scale` 测试** | 按 v0.1 NFR-001/002 字面规模（100 轮、10,000 任务）补齐证据，独立于默认 CI；备选 B（修订措辞降规模）不采纳 | 2026-08-04 |
| Q5 | 运维界面 | **Accepted：B CLI** | `jobforge ctl` 覆盖查询与 DLQ 恢复，不做完整 Web UI；备选 A（最小只读页）保留为未来升级项，C（不做）不采纳 | 2026-08-04 |
| Q6 | Claim 热点优化 | **Accepted：B 冻结不动** | 维持 W6 “记录不实施”结论，性能承诺为不回退；未来启动必须先通过 ADR | 2026-08-04 |

**需求追溯**：FR-611 通道选择依据 Q2 决策；FR-632 依据 Q3 决策；FR-601/602 规模依据 Q4 决策；FR-620 依据 Q5 决策；里程碑结构依据 Q1 决策；§11 第 5 条依据 Q6 决策。上述决策如需变更，按 ADR 流程执行并修订对应 FR 条目，不影响 v0.2 其余部分。

---

## 13. 里程碑计划

| 阶段 | 时间 | 交付物 | 退出条件 |
|---|---:|---|---|
| M1：对齐与框架 | 第 1 周 | 文档对齐包（FR-630/631）；Compose obs profile（FR-632）；scale 测试框架与参数化骨架 | 文档审查无自相矛盾；`--profile obs` 可拉起 Prometheus/Grafana 并抓取指标 |
| M2：规模验证与事件发布 | 第 2–3 周 | FR-601/602/603 scale 套件达标；FR-610~614 outbox publisher + retention migration | AT-13/14/15 通过；race 抽样通过；可靠性报告归档 |
| M3：运维与发布评审 | 第 4 周 | `jobforge ctl`（FR-620/621）；`docs/benchmark.md` 复测追加；v0.2 发布评审 | AT-16 通过；性能门禁全部 PASS；AT-01~12 不回退 |

裁剪规则（压缩为 3 周）：删除 FR-621（outbox status）、FR-614（发布指标降级为日志）、obs profile 的 Grafana 面板（只保留 Prometheus）；FR-601/602/610/613 与文档对齐不可裁剪。

## 14. Definition of Done

沿用 v0.1 §19，并追加：

- scale 套件的运行命令、环境参数与结果已归档；
- 新增指标与事件语义已同步写入 `docs/observability.md` 与 `docs/failure-semantics.md`；
- Q1–Q6 已全部确认（见 §12.3），无遗留待确认假设；后续任何改变核心语义或契约的变更仍须先通过 ADR。

---

## 附录 A：与 PRD v0.1 的条款映射

| v0.1 条款 | v0.2 处理 |
|---|---|
| §3.2 Compose 观测组件 | 由 FR-632 落地（obs profile）；Q3 已确认（2026-08-04） |
| §11.1 NFR-001/002 | 由 FR-601/602/603 按字面规模闭环；Q4 已确认（2026-08-04） |
| §11.2 性能门禁 | 原文沿用，见本 PRD §6/§11 |
| §17 交付清单 | 由 FR-630 以 v0.2 取代条款修订 |
| §4.2 P1 JetStream | 不实施；由 FR-610/611 的轻量 publisher 替代；Q2 已确认（2026-08-04） |
| §4.2 P1 运维界面 | 由 FR-620 CLI 裁剪替代；Q5 已确认（2026-08-04） |
| §4.2 P1 cron、mTLS、加权公平、令牌桶 | 推迟到 v0.3 评估，不在本版 |

---

## 附录 B：对 PRD v0.1 的取代条款（FR-630）

以下条款以本附录为准；v0.1 原文不改写历史结论（FR-630 验收口径，2026-08-05 生效）。

### B.1 取代 v0.1 §17 交付清单

v0.1 §17 列举的 `cmd/api`、`cmd/scheduler`、`cmd/worker`、`tests/fault` 与实际交付结构不一致。实际交付采用**单二进制多子命令**模式（与 v0.1 §5.2 “可先部署为一个 Go 二进制的不同子命令”的允许一致），取代后的交付清单为：

- `cmd/jobforge`：单二进制入口，子命令 `migrate` / `api` / `scheduler` / `gateway` / `worker` / `publisher` / `ctl`；
- `internal/domain`、`internal/store/postgres`、`internal/gateway/grpc`、`internal/api/http`、`internal/scheduler`、`internal/worker`、`internal/outbox`、`internal/ctl`、`internal/observability`；
- `proto/jobforge/worker/v1`；`sdk/python`；`migrations/0001~0007`；
- 故障注入测试位于 `tests/integration/fault_test.go`（AT-02~04），不单独设 `tests/fault` 目录；
- `deploy/compose.yaml`；`docs/architecture.md`、`docs/failure-semantics.md`、`docs/benchmark.md`；README、架构图、状态机、故障矩阵与演示脚本。

### B.2 取代 v0.1 §3.2 发布成功标准中的观测组件表述

v0.1 §3.2 要求“Docker Compose 一条命令启动……观测组件”。按 Q3 决策（2026-08-04）与 ADR-0004（零外部依赖默认），取代为：

- 默认 `docker compose up` 仅启动核心服务（PostgreSQL、API、Scheduler、Gateway、Publisher、2 Workers），观测以 pprof + `/metrics`（localhost:6060）与 OTel stdout exporter 提供，无外部观测依赖；
- Prometheus + Grafana 通过**可选 obs profile** 提供：`docker compose --profile obs up -d` 拉起并抓取各服务 `:6060/metrics`（FR-632 落地，配置见 `deploy/prometheus/` 与 `deploy/grafana/`）。

其余 v0.1 §3.2 成功标准（P0 功能验收、race、故障测试、benchmark 数据、README/演示脚本等）维持原文有效。
