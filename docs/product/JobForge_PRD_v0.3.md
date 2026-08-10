# JobForge：耐久事件与并发治理 PRD（v0.3 定稿）

> 文档版本：v0.3<br>
> 文档状态：定稿（D1–D4 已确认，2026-08-10）<br>
> 创建日期：2026-08-10<br>
> 确认日期：2026-08-10<br>
> 基线版本：[PRD v0.2](JobForge_PRD_v0.2.md)（已全部交付；后续加固项见 §1.2）<br>
> 代码基线：`e6b2ef3`（migrations 0001~0012）<br>
> 计划周期：5 周核心版；第 6 周为可裁剪的低延迟控制流扩展<br>
> 前置文档：[PRD v0.2](JobForge_PRD_v0.2.md)、[PRD v0.1](JobForge_PRD_v0.1.md)、[架构](../architecture.md)、[故障语义](../failure-semantics.md)、[可靠性报告](../reliability-report.md)、[性能基线](../benchmark.md)、[ADR 索引](../adr/README.md)

本文档是 v0.2 完成后下一阶段的产品范围定稿。v0.1/v0.2 已固定的状态机、lease、fencing、at-least-once、终态不可变、PostgreSQL 唯一事实源和安全边界继续有效。D1–D4 只确认产品范围与验收口径；本文中的关键依赖、并发语义与 Worker 通道调整在编码前仍必须先通过 §11 的新 ADR。本 PRD 不替代 ADR，也不授权改写已接受 ADR 的历史结论。

---

## 0. 决策摘要

v0.2 的 scale 套件、outbox publisher、运维 CLI 与可选观测 profile 已交付，后续修复又补齐了 Worker 续租韧性、幂等键冲突检测、Worker 存活、Multi-Queue Poll、双 publisher 原子领取、Scheduler 卡死接管和热路径索引。当前平台已经证明“任务能可靠执行”，下一阶段应证明“事件能耐久消费、配额在并发下精确、取消反馈有明确 SLO”。

v0.3 采用以下路线：

1. **区分内部唤醒与外部事件。** `jobforge_job_ready` 的 LISTEN/NOTIFY 继续作为内部 hint，并由 Gateway 500ms、Scheduler 1s 的数据库轮询兜底；其丢失不影响正确性。`jobforge_outbox` 面向外部消费，目前在 PostgreSQL 重启或订阅者离线时可能丢通知，不能宣称耐久交付，必须演进。
2. **首个耐久通道选择 Redis Streams。** PostgreSQL outbox 仍是唯一事件事实源；publisher 在 Redis `XADD` 成功后才记录发布进度。Redis Streams 提供持久记录、消费者组、Pending Entries List 与重投，适合当前 Compose 形态和约 366 jobs/sec 的已测规模。Kafka 不在 v0.3 核心版实现，仅保留适配契约与容量决策门槛。
3. **继续承诺 at-least-once，不升级平台承诺。** 提供事务性 consumer inbox 参考协议：`event_id` 去重记录与业务变更在同一数据库事务提交，提交后再 ACK。该模式只在同一事务边界内获得“业务效果不重复”，不得表述为端到端 exactly-once。
4. **租户配额先修正确性，再优化锁。** 当前 Claim 先锁候选任务，再按候选逐条执行 `count(state='running')`；部分索引降低了扫描成本，但没有消除锁内重复计数，也没有证明同租户并发 Claim 不超配额。v0.3 使用派生计数行的原子条件更新实现精确预留，并在锁候选前过滤已满租户。
5. **取消先交付 5 秒级信号 SLO，再评估推送通道。** 心跳默认值从 10s 调整为可配置 5s，并统一 Gateway 建议值与 Worker 实际值。P0 的硬 SLO 使用 PostgreSQL `cancel_requested_at` 到 Gateway 发出信号的单时钟口径；Worker context 取消与 Handler return 作为后续分段另行报告。仅向活跃 Poll 注入取消不可靠：满载 Worker 不会发起 Poll。可裁剪 P1 采用新增 server-streaming 控制 RPC，Heartbeat 永远保留为兜底。

上述关键取舍已固化为 §13.2 的 D1–D4，并于 2026-08-10 全部确认；它们不再是开放问题。后续 ADR 负责确定实现方案，不得在未先修订本 PRD 的情况下否决对应 FR/NFR/AT。

以下边界在 v0.3 中不可变：

- Job 与事件均为 at-least-once，不承诺端到端 exactly-once；
- PostgreSQL 的 jobs/outbox 是任务与事件的唯一事实源，Redis/Kafka 不参与任务状态事务；
- Claim 仍在一个事务中原子更新 owner、lease、attempt、fencing token 与 state；
- Heartbeat、Complete、Fail 继续校验 job、owner、fencing token 与允许状态，陈旧写入返回 `STALE_LEASE`；
- 只执行预注册 Handler，不引入任意代码执行；
- 所有 migration 只新增版本，不改写 0001~0012。

---

## 1. 当前实现基线与完成范围

### 1.1 已完成能力

| 领域 | 当前状态 | 代码与测试证据 |
|---|---|---|
| HTTP 控制面 | 已完成 | submit/get/list/cancel/retry；租户鉴权、背压、幂等冲突；`internal/api/http`、`sdk/python` |
| Worker 可靠执行 | 已完成 | unary long-poll、Heartbeat/Complete/Fail、lease/fencing、续租与结果重试；`internal/gateway/grpc`、`internal/worker` |
| 调度与 HA | 已完成并加固 | advisory lock + 领导权租约 + epoch fencing；`ADR-0005`、`TestSchedulerStuckLeaderTakeover` |
| 多租户基础 | 已完成基础语义 | tenant_id 隔离、租户 inflight 上限、队列软/硬背压；AT-10 与 Gateway wiring 测试 |
| Outbox | 已完成非耐久通知版 | 原子领取、重试、崩溃恢复、retention、LISTEN/NOTIFY；AT-15 |
| 运维 | 已完成 CLI | list/get/cancel/retry、outbox-status、workers-status；AT-16 |
| 可观测性 | 已完成 | OTel、13 个核心/Outbox 指标、pprof、Compose obs profile |
| 规模可靠性 | 已完成 | 100 轮 Worker kill 零丢失；10,000 重投零重复副作用；AT-13/14 |
| 性能 | 门禁通过 | W11 Submit 288.10 jobs/sec、Process 366.53 jobs/sec；p95 13.20ms；Claim 热路径索引已验证 |

### 1.2 v0.2 后加固项

| 加固项 | 当前事实 |
|---|---|
| Worker 续租 | 瞬时 Heartbeat 失败持续退避重试至 lease 截止；丢 lease 后取消 Handler 并丢弃结果 |
| 结果上报 | Complete/Fail 对瞬时 RPC 错误最多重试 3 次；Gateway 吸收幂等重报 |
| 提交幂等 | `request_hash` 区分同键同参去重与同键异参 `CONFLICT` |
| Worker 存活 | Poll/Heartbeat 刷新存活；stale worker 可观测与 CLI 查询 |
| Multi-Queue | Poll 覆盖全部声明队列；空队列 fail loud |
| Outbox 并发 | `claimed_at` 原子领取；双 publisher 不重复领取；僵尸领取超时回收 |
| Scheduler 卡死 | 领导权租约超时接管；epoch fencing 拒绝旧 leader |
| SQL 热路径 | promote 与 tenant running count 使用 migration 0011/0012 部分索引 |

### 1.3 已确认缺口

| ID | 缺口 | 代码事实 | 影响 |
|---|---|---|---|
| GAP-701 | 外部事件通知不耐久 | `NotifyChannel.Publish` 只执行 `pg_notify`，随后标记 `published_at` | PG 重启、订阅断线时通知不可重放；无消费者组/ACK |
| GAP-702 | 消费端幂等未协议化 | AT-15 使用测试内 map 按 event_id 去重 | 无可复用 schema、事务步骤、崩溃恢复与审计口径 |
| GAP-703 | 配额检查位于行锁事务热区 | Claim 锁定候选后逐条 `count(running)` | 批量 Claim 增加 round-trip 与锁持有时间；满额租户可能挡住后续租户候选 |
| GAP-704 | 配额并发精确性证据不足 | AT-10 为串行填满/释放；无“同租户并发 Claim 不超限”测试 | 并发事务可能同时观察旧 count，硬配额存在超配风险 |
| GAP-705 | cancelling 未计入 running count | migration 0012 与查询只统计 `state='running'` | 已请求取消但仍执行的 Handler 可能提前释放配额 |
| GAP-706 | 取消信号最坏等待约一个心跳周期 | Worker 每 10s Heartbeat；Gateway 仅在 Heartbeat 返回 CANCEL | 健康链路下取消感知最坏约 10s，再叠加 Handler 响应时间 |
| GAP-707 | 心跳配置不完全收敛 | Worker 默认可配置，但 RegisterResponse 建议值硬编码 10s，Runtime 未采用响应值 | 运维配置与协议建议值可能不一致 |

---

## 2. 下一阶段目标

| ID | 目标 | 成功判定 |
|---|---|---|
| G-701 | 建立可重放、可分组消费的耐久事件通道 | broker/consumer 暂停或重启后事件可恢复；无静默丢失 |
| G-702 | 将消费端幂等从约定升级为可执行协议 | 崩溃点测试中业务副作用重复数为 0，且仍明确标注 at-least-once |
| G-703 | 让租户 inflight 硬配额在高并发下精确 | 并发 Claim 峰值永不超过 limit；cancelling 继续占用配额 |
| G-704 | 缩短 Claim 锁内工作并避免满额租户阻塞其他租户 | 新建多租户、配额实际生效的性能基线；等价非阻塞 Claim 负载 p95 恶化 < 15%；AT-22 持续流量下 B 租户 ready→claim p95 ≤ 1s、max ≤ 2s |
| G-705 | 将健康链路取消信号收敛到 5 秒级 | Heartbeat 路径 `cancel_requested_at`（DB）→ Gateway 发出信号 p95 ≤ 6s；端到端 context 取消另行报告 |
| G-706 | 保持 v0.1/v0.2 的可靠性、性能与兼容性门禁 | AT-01~16 不回退；W4 性能门禁继续通过 |

## 3. 非目标与明确排除项

- 不宣称任务投递、事件投递或任意外部副作用为 exactly-once；
- 不让 Redis Streams、Kafka 或内存 session 成为任务状态事实源；
- 不在 v0.3 核心版同时维护 Redis 与 Kafka 双写；
- 不在本阶段实现 Kafka 集群、跨地域复制或多集群调度；
- 不实现完整 Web 管理后台、工作流 DSL、cron、队列暂停/恢复；
- 不实现 Worker mTLS、动态 API key 数据库和 payload 加密；
- 不改变 jobs 的八状态状态机，不新增终态或错误码；
- 不实施 Claim payload 分离或 COPY 协议等与本阶段目标无关的热点优化；
- 不保证 Handler 在收到 context cancel 后立即停止；平台只度量信号送达与 Handler 实际退出两个独立阶段。

---

## 4. 用户场景

| 场景 | 角色 | 核心问题 | v0.3 应对 |
|---|---|---|---|
| 耐久事件订阅 | 应用开发者 | 消费者离线或 PostgreSQL 重启后，任务终态事件能否补收？ | Redis Streams + consumer group + pending 重投 |
| 幂等业务消费 | 应用开发者 | 消费成功后、ACK 前崩溃会不会重复扣费/写入？ | 事务性 inbox + event_id 去重 + commit 后 ACK |
| 高并发租户治理 | 平台维护者 | 64 个 Worker 同时 Claim 是否突破租户上限？ | 原子配额预留 + 专项并发测试 |
| 跨租户公平 | 平台维护者 | 租户 A 满额且积压很深时，租户 B 是否还能被领取？ | Claim 前过滤满额租户 + 候选回填 |
| 交互式取消 | 应用开发者 | 取消长任务为何需要等 10 秒才有反应？ | 5s Heartbeat；可选低延迟 ControlStream |
| 容量演进 | 架构维护者 | 何时需要从 Redis Streams 切换 Kafka？ | 同一事件 envelope/adapter 契约 + 容量决策门槛 |

---

## 5. 功能需求

编号延续 v0.2，使用 7xx 段。

### 5.1 事件通道分层与耐久化

| ID | 优先级 | 需求 | 验收标准 |
|---|---|---|---|
| FR-701 | P0 | 明确分离内部 hint 与外部事件：内部 `jobforge_job_ready` 继续 LISTEN/NOTIFY + 轮询兜底；外部 outbox transport 使用独立适配器配置 | 内部通知丢失测试仍在兜底上界内恢复；文档不得把内部 hint 描述为耐久队列 |
| FR-702 | P0 | 新增 Redis Streams transport；publisher 只有在 `XADD` 获得成功响应后才记录发布成功 | Redis 不可用时事件保持未发布并退避；恢复后全部补发；禁止自动降级到 NOTIFY 后仍标记成功 |
| FR-703 | P0 | 定义版本化事件 envelope v1 | 至少包含 `schema_version`、`event_id`、`aggregate_id`、`aggregate_version`、`event_type`、`occurred_at`、`payload`、`traceparent`；不含秘密或完整敏感业务 payload |
| FR-704 | P0 | Redis Stream 使用固定可配置 stream key；`event_id` 作为业务去重键；consumer group 独立维护游标与 Pending Entries | 两个 consumer group 可独立消费同一事件；单 group 内 crash 后可 claim pending 并重投 |
| FR-705 | P0 | 事件 transport 可配置为 `notify` 或 `redis_streams`；为兼容 v0.2，通用配置默认仍为 `notify`，但必须明确标记为本地开发/兼容且非耐久；durable-events 生产示例显式选择 `redis_streams` | 配置、启动日志、健康检查和指标均能辨识 transport/durable 状态；使用 notify 时发出有界频率警告；生产示例不依赖隐式默认值 |
| FR-706 | P1 | 保留 broker-neutral adapter 契约与 Kafka readiness 设计校验，不实现 Kafka 客户端或 Kafka 集成测试 | envelope、partition key（`aggregate_id`）和 ACK/错误分类不依赖 Redis 私有类型；以编译期接口断言、表驱动契约套件和内存 fake 验证，文档不得将其表述为 Kafka 已兼容/已验证 |

**发布与顺序语义**：

- publisher 到 broker 仍为 at-least-once；broker ACK 成功但 PostgreSQL 标记失败时允许重复写入；
- 消费者按 `event_id` 去重；不得依赖 Redis entry ID 作为业务唯一键；
- 不保证全局顺序；同一 aggregate 的消费者使用 `aggregate_version` 检测重复或乱序；
- PostgreSQL outbox retention 只清理已成功交付到当前启用 transport 且超过保留期的事件；
- v0.3 同一时刻只支持一个外部 transport。跨 transport 双写、历史回灌与零停机切换留到后续 per-sink delivery ledger 评估。

**transport 切换约束**：单一 `published_at` 无法证明事件已交付到哪个 transport，因此 v0.3 不支持静默热切换。`notify` → `redis_streams` 切换必须执行事件 ADR 与运行手册中定义的受控流程：暂停 publisher/retention、记录 outbox `event_id` 高水位、明确高水位前事件是保留回灌还是按书面决策截断、启用并验证 Redis 后再恢复 retention。v0.3 不自动回灌历史事件，也不得把切换前仅经 notify 标记成功的行宣称为已耐久交付。

### 5.2 消费端幂等协议

| ID | 优先级 | 需求 | 验收标准 |
|---|---|---|---|
| FR-710 | P0 | 发布 consumer inbox 参考 schema，以 `event_id` 为主键，并记录 consumer_group、processed_at、aggregate_id/version | 重复插入通过唯一约束无害吸收；migration/示例不修改 JobForge 核心 jobs 状态 |
| FR-711 | P0 | 固定事务步骤：读取事件 → 开启业务事务 → 插入 inbox → 执行业务变更 → commit → broker ACK | 在“业务 commit 后、ACK 前”崩溃时重投，业务副作用仍只发生一次 |
| FR-712 | P0 | reference consumer 支持 Pending Entries 恢复、重试退避与 poison-event 隔离 | consumer 重启后能恢复未 ACK 事件；单个坏事件不永久阻塞 group |
| FR-713 | P0 | 明确跨系统副作用边界 | 若副作用不与 inbox 处于同一数据库事务，必须向外部系统透传 event_id/idempotency key；平台不保证效果只发生一次 |
| FR-714 | P1 | 提供最小 Go reference consumer 与可复现 demo；Python 仅提供协议文档，不在本阶段扩展 SDK | demo 能展示重复投递、inbox 命中和零重复业务效果 |

### 5.3 租户配额正确性与 Claim 热路径

| ID | 优先级 | 需求 | 验收标准 |
|---|---|---|---|
| FR-720 | P0 | 新增 PostgreSQL 派生配额计数表，按 tenant 保存 inflight 数；jobs 仍是事实源 | 通过新增 versioned migration 创建；可由 jobs 重建；不得成为跨数据库事实源 |
| FR-721 | P0 | Claim 在候选行锁前用计数表预筛满额租户；事务内以带上限条件的原子更新预留一个或多个 slot | 单 slot 使用 `inflight + 1 <= limit`；批量 N 个使用 `inflight + N <= limit`，整批不满足时不得递增，可降级为更小批次/逐 slot；64 并发 Claim 下最大 inflight 不超过 limit；未使用 slot 必须在提交前回补 |
| FR-722 | P0 | 配额预留、job Claim 与 attempt 写入同一事务；所有离开 inflight 的状态转换在同一事务释放计数 | Complete、Fail→retry/dead、lease recover、cancelling→cancelled 都不漂移；事务回滚或部分批次 Claim 不泄漏 slot |
| FR-723 | P0 | `running` 与 `cancelling` 均计入 inflight；running→cancelling 不释放 slot | 取消风暴期间实际执行数仍不超过租户上限；最终 cancelled 后释放 |
| FR-724 | P0 | 提供计数核对与修复命令/周期任务，差异以指标暴露 | 注入计数漂移后可检测；修复以 jobs 聚合结果为准；过程可审计 |
| FR-725 | P0 | 满额租户不得占满候选窗口；Claim 必须继续寻找有配额租户 | 租户 A 10,000 ready 且满额时，B 单个 ready job 在健康 Gateway 下 ≤ 1s 被领取；持续流量变体满足 AT-22 的 p95/max 上界 |
| FR-726 | P1 | Claim 前允许读取近似/缓存计数减少无效事务，但事务内原子预留不可省略 | 关闭预筛优化仍保持正确；预筛误差只影响性能，不影响配额上限 |

**事务约束**：配额表只是 PostgreSQL 内的派生一致性数据。任何预筛都只能减少无效候选，最终硬限制必须由同一 Claim 事务内的原子预留保证。不得用进程内计数、Redis counter 或仅靠周期聚合作为硬配额依据。

配额 ADR 必须在编码前固定以下实现决策，不得由各状态转换路径自行选择：

1. Claim、Complete、Fail、Cancel 与 Scheduler recover 同时触碰 job 行和 tenant counter 时的全局锁顺序，以及多租户批次的稳定排序；
2. 采用“逐候选原子预留”还是“按租户批量预留 + 同事务回补未用 slot”；批量 N 个 slot 的条件更新必须等价于 `inflight + N <= limit`，条件不满足时整批不得递增，可降级为更小批次/逐 slot；后一方案必须定义回补与错误回滚路径；
3. `FOR UPDATE SKIP LOCKED` 的队列优先级、同租户聚合与跨租户候选回填顺序，避免满额租户占满窗口；
4. 预筛读取的允许陈旧窗口、误判指标和关闭预筛时的正确性退化方式；
5. migration 0012 的 `idx_jobs_tenant_running` 是删除、保留作其他查询，还是由覆盖 `running + cancelling` 的新索引替代；只能新增 migration，不得改写 0012，并需用 EXPLAIN/规模测试证明处置理由。

### 5.4 取消信号加速

| ID | 优先级 | 需求 | 验收标准 |
|---|---|---|---|
| FR-730 | P0 | 默认 Heartbeat 从 10s 调整为 5s，继续支持 `JOBFORGE_HEARTBEAT_INTERVAL`；Lease TTL 默认仍为 30s | Gateway RegisterResponse、Worker Runtime 和文档使用同一配置来源；默认一个 lease 周期有约 6 次 heartbeat |
| FR-731 | P0 | 分段度量 `cancel_requested_at` → Gateway 发出控制信号，以及 Worker 收到信号/取消 context → Handler return；受控测试另行测量 cancel API 成功 → execution context cancelled 的端到端时延 | signal 指标可区分 `heartbeat`/`stream` 路径，Handler 指标可区分类型；不使用 job_id 作为 label；端到端数据不与 signal SLO 混用 |
| FR-732 | P0 | Heartbeat 仍是取消信号的可靠兜底；瞬时 Gateway 故障恢复后继续检查 cancelling | 健康链路 `cancel_requested_at`（DB）→ Gateway 发出信号 p95 ≤ 6s；控制流不可用时不丢取消，最迟仍由 Heartbeat/lease expiry 收敛 |
| FR-733 | P1 | 新增向后兼容的 server-streaming Control RPC；Worker 注册后维持控制流，Gateway 按 worker/session 推送 cancel | 控制流健康时 `cancel_requested_at`（DB）→ Gateway 发出信号 p95 ≤ 1s；断流自动重连并退回 Heartbeat；端到端 context 取消另行报告 |
| FR-734 | P1 | Gateway 多实例通过 PostgreSQL cancel hint 唤醒本地 session registry；通知只作 hint，收到后回查 job owner/token/state | PG NOTIFY 丢失或错误 Gateway 收到通知均不影响最终取消；陈旧 session 不得取消新 lease |

**不采用“仅向活跃 Poll 注入”作为完整方案**：当前 Runtime 在 available capacity 为 0 时不发 Poll，满载 Worker 恰恰是最需要取消正在执行任务的场景。独立 ControlStream 覆盖满载与空闲状态，并允许保留 unary Poll 的兼容性。

---

## 6. 非功能需求

| ID | 要求 |
|---|---|
| NFR-301 | v0.1/v0.2 性能门禁继续有效：相对 W4 吞吐下降 < 15%、控制面 p95 恶化 < 20%、goroutine 稳态 ±5% |
| NFR-302 | Redis Streams 在相同基准环境下应支撑至少 5× W11 Process 吞吐的事件写入目标（约 1,800 events/sec），健康无积压时 publish lag p95 ≤ 2s；该目标用于选型门槛，不替代正式容量报告 |
| NFR-303 | Redis 暂停 60s 后恢复，outbox 无静默丢失且积压最终归零；publisher 不阻塞任务状态事务 |
| NFR-304 | Claim 性能使用双口径并遵守下述基线迁移协议：①当前 Phase B 在改写前由 M0 运行至少 5 轮归档；改造后由 M1 以相同负载参数重写为“预留激活但配额不阻塞”并再运行至少 5 轮，以两组“各轮 p95 中位数”比较，恶化 < 15%；②新增至少 4 租户、包含满额与可用租户、配额实际生效的混合场景，满足 AT-22 公平性上界；304.2ms 仅为历史参考，不作跨硬件硬门禁 |
| NFR-305 | 任意时刻 `running + cancelling` 不超过 tenant limit；该正确性门禁不允许用性能裁剪换取 |
| NFR-306 | Heartbeat 默认 5s 后，job lease 续租写入放大必须基准量化；允许对 `workers.last_heartbeat_at` 等附属存活写使用条件刷新，但不得跳过、合并到超过安全间隔或延迟 job lease 续租；不得使 W4/W11 门禁回退 |
| NFR-307 | P0 Heartbeat 路径 `cancel_requested_at`（DB）→ Gateway 发出信号 p95 ≤ 6s；P1 ControlStream 同口径 p95 ≤ 1s；Worker context 取消与 Handler 停止耗时单独报告，不混入 signal SLO |
| NFR-308 | 新增 goroutine（consumer/control stream）必须有 owner、重连退避、取消与 Wait 路径，并通过 `go test -race ./...` |
| NFR-309 | Redis URL、认证信息、API key、Authorization header 与完整敏感 payload 不进入日志、trace 或 metrics |

**NFR-304 基线迁移协议**：

1. M0 必须在任何 quota 代码、migration 及 `perf_index_test.go` Phase B 改写前，对当前实现运行至少 5 轮并归档：代码 commit、硬件/容器参数、`-race` 条件、p50/p95，以及 `explainQuotaCount` 命中 `idx_jobs_tenant_running` 的计划证据。该计划断言只描述改造前的 `count(*) WHERE state='running'` 路径；不得保留为改造后验收条件。
2. M1 将 Phase B 重写为“预留激活但配额不阻塞”：保留 20,000 jobs / 8 workers / batch 50 / 单租户等负载参数，并继续设置足够高的 limit，使新的 counter 预筛与条件预留路径实际执行但不拒绝 Claim。计划断言必须同步改为 quota counter 主键/新索引及预筛查询的结构性证据，不再引用 `explainQuotaCount` 或 `idx_jobs_tenant_running`。
3. 改造后在与 M0 相同环境运行至少 5 轮，以归档的修改前 p95 中位数比较。这里比较的是等价业务负载，不要求 SQL、索引断言或测试实现保持不变；若 M0 证据未先归档，不得用历史 304.2ms 代替本次性能门禁。
4. 多租户混合场景至少包含一个满额租户 A 与三个持续有 slot 的租户 B/C/D，单独归档 p50/p95、counter lock wait、reservation conflict、各租户 ready→claim 分位数与最大值；B/C/D 各自均须满足 AT-22 的 p95/max 上界。正确性使用 AT-21，公平性使用 AT-22，不把单租户非阻塞结果外推为配额压力容量证明。

### 6.1 Redis 与 Kafka 的容量决策门槛

v0.3 为“耐久事件”能力选择 Redis Streams；这不改变 FR-705 为兼容升级保留的通用配置默认值。满足任一条件时，在后续版本启动 Kafka ADR/Spike，而不是在 v0.3 预先实现双栈：

- Redis 在同环境无法达到 NFR-302，或持续积压导致 publish lag 超过 SLO；
- 需要按 tenant/aggregate 分区并水平扩展到 Redis 单 stream/单集群的容量边界；
- 需要多日大规模重放、跨机房复制或更强的 broker 运维生态；
- 消费者组数量、保留体量或合规审计要求超过 Redis 的可控成本。

Kafka Spike 必须复用 envelope v1 与 `event_id` 幂等语义，并以 `aggregate_id` 作为默认 partition key。切换前需单独设计 per-sink 进度、历史回灌和切流方案。

NFR-302 的约 1,800 events/sec 只是当前规模下排除明显不合格方案的 smoke floor，不是 Redis 单机容量、长期 retention 或生产峰值证明。正式容量结论还必须覆盖持续写入、消费者滞后、PEL 回收、AOF rewrite 与故障恢复期间的尾延迟。

---

## 7. 可靠性与故障语义

### 7.1 事件发布

1. 任务状态事务与 outbox 写入原子提交；broker 调用永远在事务外；
2. broker 调用失败时 `published_at` 保持 NULL，publisher 退避重试；
3. broker ACK 成功、PostgreSQL 标记前崩溃时允许重复消息；消费者按 `event_id` 去重；
4. consumer 业务事务成功、ACK 前崩溃时允许 redelivery；inbox 唯一约束吸收重复；
5. consumer ACK 后不再依赖 JobForge outbox 作为其个人消费游标；
6. Redis 数据耐久性依赖启用 AOF/受支持持久化配置。未启用时不得宣称可跨 Redis 重启恢复。

### 7.2 配额计数

1. quota slot 的预留/释放必须与对应 job 状态变化同事务；
2. transaction rollback 后 slot 与 job 均不变化；
3. 计数漂移修复以 `jobs WHERE state IN ('running','cancelling')` 聚合为准；
4. 计数表不可替代 jobs 进行任务查询、恢复或审计；
5. 同租户 Claim 可在计数行上短暂串行化，禁止通过放松硬上限换取吞吐。

### 7.3 取消

1. Cancel 事务先提交后 state= cancelling；Complete 继续返回 `CANCEL_REQUESTED`；
2. ControlStream/NOTIFY 只是加速路径，不改变状态机或先提交者生效规则；
3. 推送必须包含或回查 fencing token，旧 session 的 cancel 不得作用于新 lease；
4. Worker 收到 cancel 后取消 Handler context；Handler 对外部副作用仍需业务幂等；
5. 推送丢失时由 Heartbeat 检查 cancelling，最终由 lease expiry 收敛为 cancelled。

---

## 8. 可观测性要求

沿用现有指标并新增：

| 指标 | 类型 | 标签 | 说明 |
|---|---|---|---|
| `jobforge_event_publish_lag_seconds` | Histogram | transport | outbox created_at 到 broker ACK |
| `jobforge_event_transport_failures_total` | Counter | transport, reason | broker/序列化/ACK 失败 |
| `jobforge_event_redeliveries_total` | Counter | transport, consumer_group | pending claim 或重复消费 |
| `jobforge_consumer_inbox_duplicates_total` | Counter | consumer_group | inbox 唯一键命中 |
| `jobforge_quota_reservation_conflicts_total` | Counter | — | 原子预留因配额满失败 |
| `jobforge_quota_counter_drift` | Gauge | — | 核对时发现的绝对差异总数 |
| `jobforge_cancel_signal_latency_seconds` | Histogram | path | cancel_requested_at 到 Gateway 决定并发出 Heartbeat/ControlStream 信号；使用 PostgreSQL 返回的 DB-clock elapsed |
| `jobforge_cancel_handler_stop_latency_seconds` | Histogram | type | Worker 收到信号并取消 context 到 Handler return |

标签约束：`event_id`、`job_id`、`trace_id`、worker_id 不作为 metrics label；consumer_group 必须来自有界配置，不接受请求级动态值。

Trace 新增或扩展：`outbox.publish` 标注 transport 与 broker ACK 结果；reference consumer 使用 `event.consume` 与 `event.process`；取消加速使用 `gateway.cancel_signal`。所有 span 不含完整 payload 与凭据。

取消时延不得直接用 Gateway 本地 `time.Now()` 减 PostgreSQL 的 `cancel_requested_at`。Heartbeat/ControlStream 回查必须由 PostgreSQL 在同一查询中返回基于 `clock_timestamp()` 的 elapsed，Gateway 以该值记录 signal histogram，从而避免容器/WSL2 时钟漂移。`cancel_requested_at` 与 cancelling 状态在同一事务写入，早于或等于 commit，因而该口径是对 commit→signal 的保守上界而非精确 commit timestamp。端到端受控测试由同一测试协调器记录“Cancel API 成功返回”和“Worker context cancelled”两个观测点，只作独立报告；跨进程生产指标不据此承诺 ≤ 6s。

---

## 9. 兼容性与迁移影响

| 面 | 影响 | 处理 |
|---|---|---|
| HTTP API | 无破坏 | submit/get/list/cancel/retry 不变；取消只改善延迟 |
| gRPC v1 | P0 无变化；P1 向后兼容新增 | unary Poll/Heartbeat 保留；Control RPC 仅新增，不修改既有字段语义 |
| Python SDK | P0 不变 | 仍是控制面客户端；consumer 协议先以文档/Go reference 交付 |
| Event contract | 新增公开契约 | envelope 带 `schema_version=1`；未来只允许向后兼容新增字段 |
| PostgreSQL | 只追加 migration | 新增 quota 计数/consumer demo inbox/aggregate_version 支撑；显式处置 migration 0012 的 `idx_jobs_tenant_running`，必要时以新 migration 替换为 inflight 口径；不改写 0001~0012 |
| 部署 | 新增可选 durable-events profile | 默认核心服务仍只强依赖 PostgreSQL；通用默认 `notify` 仅用于兼容/本地开发，durable-events 生产示例显式设置 `redis_streams`；Redis 是事件交付依赖，不是任务状态依赖 |
| 配置 | 新增并收敛 | transport/Redis/consumer 参数新增；Heartbeat 默认由 10s 改为 5s，需发布说明；notify 模式显式暴露 `durable=false` 与警告 |
| transport 切换 | 非零停机运维变更 | 切换前暂停 publisher/retention、记录 event_id 高水位并选择旧行回灌或截断策略；验收后恢复 retention；不得把旧 `published_at` 等同于已进 Redis |
| 状态机/错误码 | 不变 | 无新状态、无新 HTTP 领域错误码 |

---

## 10. 测试与验收矩阵

| ID | 场景 | 操作 | 预期结果 |
|---|---|---|---|
| AT-17 | Durable transport 中断/重启恢复 | Redis 停止 60s，期间产生终态事件，再以受支持的 AOF 配置重启 | 重启前 Stream 记录保留；停机期 outbox 未发布行保留；恢复后全部补入 Stream，积压归零 |
| AT-18 | Publisher ACK/mark 崩溃窗 | `XADD` 成功后、MarkPublished 前 kill publisher，重启后再次发布 | Stream 中允许出现同 event_id 的多个 entry；outbox 最终标记成功；任务状态不变；本项不依赖 consumer inbox |
| AT-19 | Consumer commit/ACK 与重复输入崩溃窗 | 构造与 AT-18 相同的“同 event_id、多个 Stream entry”输入，并在业务事务 commit 后、XACK 前 kill consumer | pending 被重领；inbox 去重；跨多个 Stream entry 与 redelivery，业务副作用仍只发生一次；测试可独立运行，不依赖 AT-18 先执行 |
| AT-20 | 消费者组隔离 | 两个 group 各两实例消费同一 Stream | 每组都完整消费；组内实例分摊；无跨组游标干扰 |
| AT-21 | 并发配额硬上限 | 同租户 64 goroutine 并发 Claim，limit=10；事务内检查每次条件预留结果；独立连接使用配置值 ≤10ms 的采样 ticker，从首次 Claim 前持续到 Complete/Fail 与 cancelling→cancelled 混合释放结束并完成终态核对 | 每次事务内断言 counter ≤10；所有已提交采样点 running+cancelling ≤10；记录实际采样次数/最大调度间隔且覆盖取消释放（调度抖动不替代事务内硬断言）；终态 jobs 聚合与 counter 一致且无 slot 泄漏 |
| AT-22 | 满额租户不阻塞与持续流量公平性 | A 满额且持续保有 10,000 ready；变体一向 B 提交 1 个 ready；变体二在 B slot/Worker capacity 可用且任务领取后立即完成的条件下，持续 30s 以 20 jobs/s 提交 B 任务 | 健康 Gateway（500ms fallback）下，变体一从 ready commit 到 Claim ≤1s；变体二 B 的 ready→claim p95 ≤1s、max ≤2s，且 drain 后无 B 任务因 A 占据候选窗口而遗留；验收使用绝对时间，不再使用未定义的“一个 Poll 周期” |
| AT-23 | 取消风暴配额 | 10 个 running 全部转 cancelling，同时继续 Poll | cancelling 仍占 slot；未终止前不再 Claim 该租户 |
| AT-24 | Heartbeat 取消 SLO | 默认 5s，健康 Gateway 下随机相位提交 cancel；由数据库返回 signal elapsed，受控测试另测 context 取消 | `cancel_requested_at`（DB）→ Gateway signal p95 ≤6s；该保守上界达标；端到端 context 取消只报告、不套用同一阈值；AT-05/06 竞争语义不变 |
| AT-25 | ControlStream 降级（P1） | 正常推送、断流、错 Gateway、NOTIFY 丢失 | 同一 signal 口径正常 p95 ≤1s；失败时 Heartbeat 兜底；旧 token 不误取消；端到端时延另报 |

### 10.1 不回退要求

- AT-01~AT-16 全部保持通过；
- scale 套件继续使用真实 PostgreSQL，并新增 Redis 耐久 profile 的故障编排；
- 并发配额、ControlStream 和 consumer goroutine 通过 `go test -race ./...`；
- publisher/consumer 覆盖 broker down、超时、重复、poison event、graceful shutdown 与 hard crash；
- 所有新 migration 有前滚/回滚说明、锁风险与数据核对步骤；
- benchmark 使用相同硬件/参数对比 W4、W11 和本版修改前基线。

### 10.2 Windows 真实 Redis 约定

Windows/Docker Desktop 下 Redis 集成与故障测试不依赖 testcontainers。v0.3 的 `durable-events` Compose profile 必须提供启用 AOF（至少 `appendonly yes`，具体 fsync 策略由事件 ADR 固定）且挂载命名 volume 的 Redis。标准启动与测试环境约定为：

```powershell
docker compose -f deploy/compose.yaml --profile durable-events up -d postgres redis
$env:JOBFORGE_TEST_DSN = "postgres://jobforge:jobforge@localhost:5433/jobforge?sslmode=disable"
$env:JOBFORGE_TEST_REDIS_URL = "redis://localhost:6379/0"
```

AT-17 必须使用保留命名 volume 的 `stop`/`start` 或等价容器重启验证 AOF 恢复，禁止用 `down -v` 后把全新实例误报为重启恢复。Linux CI 可继续使用 testcontainers 或同一 Compose profile，但两种路径必须执行同一组 Redis adapter/consumer 契约与故障测试。

---

## 11. ADR 前置条件

编码前至少需要三项 Accepted ADR，按提交时下一可用编号分配：

1. **耐久事件 transport 与 envelope**：说明内部 NOTIFY 保留、Redis Streams 首选、默认 transport、Kafka 门槛、发布进度、顺序和兼容策略；必须固定 notify→redis 切换的高水位、retention 暂停、旧行回灌/截断与回滚策略；
2. **租户配额原子计数**：说明 inflight 定义（running+cancelling）、计数表、所有转换的全局锁顺序、逐候选或批量预留及未用 slot 回补、候选回填、预筛误差窗口、migration 0012 索引处置、漂移修复与回滚；
3. **取消控制通道与 Heartbeat 参数**：说明 5s 默认、配置单一来源、ControlStream/Heartbeat 降级与 session fencing。

consumer inbox 可并入事件 ADR，或在其对外契约/事务边界需要独立评审时单列 ADR。任何 ADR 若否决本 PRD 的推荐方案，必须先修订相应 FR 和验收矩阵，再进入编码。

---

## 12. 里程碑路线

| 阶段 | 时间 | 交付物 | 退出条件 |
|---|---:|---|---|
| M0：契约与基线 | 第 1 周 | 三项 ADR；event envelope v1；改写前 Phase B 五轮基线与旧 EXPLAIN 证据；多租户配额场景的修改前观测结果；取消/故障测试骨架 | ADR Accepted；NFR-304 第 1 步证据在测试改写前归档；AT-17~25 可执行骨架就绪；无未决核心语义 |
| M1：配额正确性 | 第 2 周 | quota counter migration；预筛 + 原子预留/释放；reconcile；旧索引处置；Phase B 按“预留激活但不阻塞”重写；AT-21~23 | 硬上限零突破；计数可修复；新结构计划断言有效；Claim 双口径达 NFR-304，AT-22 分位数/最大值达标 |
| M2：耐久事件 | 第 3 周 | Redis Streams adapter；durable-events Compose profile；健康检查与事件指标 | AT-17/18/20 通过；broker 故障不影响 jobs 状态事务 |
| M3：消费协议 | 第 4 周 | inbox schema；reference consumer；pending 恢复；幂等 demo | AT-19 独立构造的同 event_id 多 entry 输入通过；文档无 exactly-once 误导；poison event 可隔离 |
| M4：取消 SLO 与收官 | 第 5 周 | Heartbeat 5s 配置收敛；取消延迟指标；全量回归与性能/可靠性报告 | AT-24 通过；AT-01~24 不回退；W4/W11 门禁 PASS |
| M5：低延迟控制流（可裁剪） | 第 6 周 | server-streaming Control RPC；多 Gateway session 路由；降级测试 | AT-25 通过；p95 ≤1s；旧 Worker/旧 token 不误取消 |

### 12.1 裁剪顺序

若周期压缩到 4 周，按顺序裁剪：

1. 删除 M5 ControlStream，只保留 5s Heartbeat SLO；
2. FR-714 Go reference consumer 降为可运行测试 fixture + 协议文档；
3. FR-706 broker-neutral 契约套件降为 envelope/partition 设计记录；
4. 不得裁剪 FR-702、FR-710/711、FR-721~725、FR-730~732 及其故障测试。

---

## 13. 风险与范围决策

### 13.1 风险与应对

| 风险 | 影响 | 应对 |
|---|---|---|
| Redis 被误解为第二任务事实源 | 架构边界混乱 | 只从 outbox 发布；broker 故障不回写 job；文档和健康检查分离 task/event readiness |
| Redis ACK 后 PG 标记失败产生重复 | 重复消费 | event_id inbox 去重；AT-18 固化崩溃窗 |
| Redis 持久化配置不足 | broker 重启后消息丢失 | durable profile 启用并验证 AOF；未启用时禁止耐久声明 |
| notify→redis 切换沿用单一 published_at | 旧事件被误清理或误报耐久 | 切换时暂停 publisher/retention、记录高水位并书面选择回灌/截断；后续再评估 per-sink ledger |
| quota counter 漂移 | 错误限流或超配 | 所有转换同事务；reconcile 指标/命令；jobs 聚合作为修复事实 |
| 配额计数行成为热点或锁序不一致 | 同租户 Claim 串行、死锁、吞吐下降 | ADR 固定全局锁序与逐候选/批量预留策略；锁前预筛、短事务；以正确性优先并量化双口径 p95 |
| 5s Heartbeat 写放大 | job lease 续租写约 2×，Gateway/PG 压力增加 | lease 续租不节流；量化 W4/W11 回归；`workers.last_heartbeat_at` 继续按 `LeaseTTL/3` 条件刷新 |
| Heartbeat 与 worker liveness 参数交互回退 | active worker 指标抖动或附属写入意外翻倍 | 默认 LeaseTTL=30s 下回归验证 5s job heartbeat 与 10s（LeaseTTL/3）存活刷新阈值；覆盖非默认 TTL/interval 组合 |
| ControlStream 增加 session 状态 | 重连、泄漏与多实例复杂度 | P1 可裁剪；session fencing；Heartbeat 永久兜底；race/泄漏测试 |
| 事务性 inbox 被误称 exactly-once | 对外承诺失真 | 文档统一使用“at-least-once + 幂等消费”；跨系统副作用明确不保证 |

### 13.2 范围决策记录（D1–D4，已确认）

以下范围决策已于 **2026-08-10** 全部确认（Accepted），不再是开放问题。它们固定产品方向、兼容边界和验收口径；SQL/Proto、锁序、故障恢复与部署细节仍由 §11 的三项 ADR 承担。

| 编号 | 主题 | 决策 | 决策边界 | 确认日期 |
|---|---|---|---|---|
| D1 | 耐久事件 transport | **Accepted：Redis Streams 首选** | PostgreSQL outbox 仍是唯一事件事实源；Redis 只作事务外耐久发布与消费适配器；Kafka 仅保留 broker-neutral 契约和容量升级门槛 | 2026-08-10 |
| D2 | 升级兼容默认值 | **Accepted：notify 默认兼容** | 为兼容 v0.2，通用配置默认 `notify`，但必须标记 `durable=false`；durable-events 生产示例显式选择 `redis_streams`，不得静默把 notify 宣称为耐久 | 2026-08-10 |
| D3 | 取消加速 | **Accepted：P0 采用 5s Heartbeat** | P0 signal SLO 为 `cancel_requested_at`（DB）→ Gateway signal p95 ≤6s；端到端 context 取消另行报告；P1 ControlStream 为可裁剪扩展 | 2026-08-10 |
| D4 | Claim 性能门禁 | **Accepted：双口径基线迁移** | M0 在改写前归档旧 Phase B；M1 重写为预留激活但不阻塞并比较等价负载，同时以 AT-21/22 验收多租户正确性与公平性；304.2ms 不作跨硬件硬门禁 | 2026-08-10 |

**需求追溯**：FR-702/704/706 与 NFR-302/303 依据 D1；FR-705 与 transport 切换约束依据 D2；FR-730~734、NFR-307 与 AT-24/25 依据 D3；FR-720~726、NFR-304/305 与 AT-21~23 依据 D4。任何 ADR 若否决或实质改变上述 Accepted 决策，必须先修订对应 FR/NFR/AT 与本决策记录，再进入编码。

---

## 14. Definition of Done

v0.3 核心版只有同时满足以下条件才算完成：

- M0 要求的 ADR 已 Accepted，PRD/ADR/代码/测试无语义冲突；
- 本文档已定稿，D1–D4 已全部确认且无遗留未决语义假设；任何否决或实质改变 FR/NFR/AT、核心语义或公开契约的 ADR，必须先修订本文档；
- Redis Streams transport、consumer inbox 协议、quota 原子计数和 5s 取消 SLO 均有正常与故障路径测试；
- AT-01~AT-24 全部通过，AT-25 仅在交付 M5 时必需；
- `go test -race ./...`、golangci-lint、ruff、mypy、buf lint 与相关 migration 检查通过；
- Windows 集成/scale 测试按 AGENTS.md 启动真实 PostgreSQL 并设置 `JOBFORGE_TEST_DSN`；Redis 相关测试按 §10.2 启动 `durable-events` profile、设置 `JOBFORGE_TEST_REDIS_URL`，并归档 AOF/volume 配置与 AT-17 重启证据；
- 性能、事件恢复、配额并发和取消延迟数据归档到 benchmark/reliability 文档；
- README、architecture、failure-semantics、observability、development、Compose 示例与本 PRD 同步；
- 所有公开材料继续明确：平台是 at-least-once，业务效果不重复依赖幂等协议；
- staged diff 已检查敏感信息、migration 安全、生成代码来源和文档链接。

---

## 附录 A：需求追溯

| 本版需求 | 来源 | 处理结论 |
|---|---|---|
| FR-701~706 | v0.2 FR-610/611 的 pluggable channel；当前 NotifyChannel 非持久 | 内部 hint 保留；外部通道升级 Redis Streams；Kafka 设容量门槛 |
| FR-710~714 | v0.1/v0.2 “消费方按 event_id 幂等”仅为文字要求 | 协议化为 inbox 事务、commit 后 ACK 与 crash 测试 |
| FR-720~726 | FR-302 配额；migration 0012 只优化 count 索引 | 采用预筛 + PostgreSQL 原子 slot；补并发硬上限和公平性证据 |
| FR-730~734 | ADR-0001 heartbeat=10s；ADR-0003 后续 cancel channel；当前仅 Heartbeat 返回 cancel | P0 调整 5s；P1 独立 ControlStream；不依赖活跃 Poll |

## 附录 B：事件 envelope v1 示例

```json
{
  "schema_version": 1,
  "event_id": 12345,
  "aggregate_id": "job-uuid",
  "aggregate_version": 7,
  "event_type": "job.succeeded",
  "occurred_at": "2026-08-10T12:00:00Z",
  "payload": {
    "job_id": "job-uuid",
    "state": "succeeded"
  },
  "traceparent": "00-...-...-01"
}
```

说明：本示例固定 envelope v1 的必需字段集合与安全边界，最终类型、可空性和 payload 白名单由事件 ADR 固定。不得在 payload 中加入 API key、Authorization header、明文秘密或完整敏感任务参数。
