# ADR-0006：耐久事件 transport 与 envelope（Redis Streams 首选，NOTIFY 保留为内部 hint 与兼容默认）

- 状态：Accepted
- 日期：2026-08-10
- 决策者：JobForge 维护者
- 关联 Issue/PR：PRD v0.3 §5.1/§5.2（FR-701~706、FR-710~714、NFR-302/303、AT-17~20、D1/D2）
- 取代：无（ADR-0003 的 LISTEN/NOTIFY 结论对内部 job-ready hint 与默认 notify transport 继续有效）
- 被取代：无

## 上下文

v0.2 的 outbox publisher 通过 `pg_notify` 投递外部事件（GAP-701）：PostgreSQL 重启或订阅者离线时通知不可重放，没有消费者组、ACK 与 pending 重投，不能宣称耐久交付。消费端幂等也仅停留在文字约定（GAP-702），AT-15 的去重靠测试内 map，无可复用 schema、事务步骤与崩溃恢复口径。

PRD v0.3 已确认范围决策 D1（Redis Streams 首选）与 D2（notify 默认兼容、标记 durable=false）。本 ADR 固定实现方案：transport 分层、发布进度语义、envelope v1、兼容与切换策略，以及 consumer inbox 契约。

## 决策驱动因素

- PostgreSQL outbox 仍是唯一事件事实源；broker 不参与任务状态事务；
- 继续承诺 at-least-once，不升级 exactly-once；
- 当前 Compose 单机形态与约 366 jobs/sec 已测规模（NFR-302 门槛约 1,800 events/sec）；
- v0.2 部署不得被强制迁移：默认行为保持 notify，仅明确其非耐久。

## 决策

### 1. 通道分层（FR-701）

- **内部唤醒**：`jobforge_job_ready` 继续 LISTEN/NOTIFY + Gateway 500ms / Scheduler 1s 轮询兜底；hint 丢失不影响正确性，文档不得称其为耐久队列；
- **外部事件**：outbox transport 为独立可配置适配器，取值 `notify` | `redis_streams`（`JOBFORGE_OUTBOX_TRANSPORT`，默认 `notify`）。同一时刻只启用一个外部 transport；双写与历史回灌不在本阶段。

### 2. Redis Streams transport（FR-702/704）

- publisher 只在 `XADD` 获得成功响应后调用 MarkPublished；Redis 不可用时事件保持未发布并退避重试，**禁止降级到 NOTIFY 后仍标记成功**；
- Stream key 固定可配置（默认 `jobforge:events`）；业务去重键为 `event_id`，不得依赖 Redis entry ID；
- 消费者使用 consumer group：独立游标 + Pending Entries List；crash 后通过 XAUTOCLAIM/XPENDING 重领 pending 重投；
- Redis 数据耐久性依赖启用 AOF（`appendonly yes`；fsync 策略固定为 `everysec`，兼顾恢复窗口与吞吐）与命名 volume。未启用持久化时不得宣称可跨 Redis 重启恢复。

### 3. 发布与顺序语义

- publisher→broker 为 at-least-once：broker ACK 后 PostgreSQL 标记失败允许重复 entry，消费者按 `event_id` 去重（AT-18/19）；
- 不保证全局顺序；同一 aggregate 的消费方用 `aggregate_version` 检测重复/乱序；
- outbox retention 只清理已成功交付到当前启用 transport 且超过保留期的事件。

### 4. envelope v1（FR-703）

固定字段集合（PRD v0.3 附录 B）：`schema_version`（=1）、`event_id`、`aggregate_id`、`aggregate_version`、`event_type`、`occurred_at`、`payload`、`traceparent`。

- payload 白名单：job_id、state 等状态元数据；禁止 API key、Authorization header、明文秘密或完整敏感任务参数（NFR-309）；
- 未来只允许向后兼容新增字段；`schema_version` 变更需新 ADR；
- `aggregate_version` 以 jobs.state_version 提供，需要随 outbox 写入一并持久化（新增 migration 列）。

### 5. broker-neutral 契约（FR-706，P1）

transport 适配器接口不暴露 Redis 私有类型；envelope、partition key（`aggregate_id`）与 ACK/错误分类以编译期接口断言 + 表驱动契约套件 + 内存 fake 验证。文档不得表述为 Kafka 已兼容/已验证。Kafka 启动门槛见 PRD §6.1（NFR-302 不达标、分区扩展、多日重放/跨机房、消费者组与合规体量）。

### 6. 兼容与切换策略（FR-705，D2）

- 通用配置默认 `notify`，启动日志、健康检查与指标必须能辨识 transport 与 `durable=false`，notify 模式以有界频率输出非耐久警告；durable-events 生产示例显式设置 `redis_streams`；
- `notify → redis_streams` 切换是受控运维变更，非零停机：
  1. 暂停 publisher 与 retention 清理；
  2. 记录 outbox `event_id` 高水位；
  3. 书面决定高水位前事件保留回灌还是截断（v0.3 不自动回灌）；
  4. 启用并验证 Redis 后恢复 publisher，最后恢复 retention；
- 不得把切换前仅经 notify 标记成功的行宣称为已耐久交付；单一 `published_at` 无法证明交付目标，per-sink delivery ledger 留待后续评估。

### 7. consumer inbox 契约（FR-710~713，并入本 ADR）

- 参考 schema：`consumer_inbox(event_id primary key, consumer_group, aggregate_id, aggregate_version, processed_at)`；重复插入经唯一约束无害吸收；该 migration/示例不修改 JobForge 核心 jobs 状态；
- 固定事务步骤：读取事件 → 开启业务事务 → 插入 inbox → 执行业务变更 → commit → broker ACK（XACK）；业务 commit 后、ACK 前崩溃允许重投，副作用只发生一次（AT-19）；
- reference consumer 支持 pending 恢复、重试退避与 poison-event 隔离（重试上限后转入隔离流/死信游标，不阻塞 group）；
- 跨系统副作用边界：若副作用不与 inbox 同事务，必须透传 event_id/idempotency key；平台不保证效果只发生一次。文档统一表述为“at-least-once + 幂等消费”。

## 公开契约与数据影响

- **HTTP API / gRPC worker 协议 / Python SDK**：无变化（consumer 协议先以文档 + Go reference 交付）；
- **数据库 schema**：outbox_events 新增 `aggregate_version` 支撑列（只增 migration）；consumer inbox 为示例/参考 schema，不属于核心状态；
- **配置**：新增 `JOBFORGE_OUTBOX_TRANSPORT`（默认 notify）、Redis URL/stream key/consumer 参数；Redis URL 与认证信息不得进入日志、trace 或 metrics（NFR-309）；
- **指标**：新增 `jobforge_event_publish_lag_seconds`（transport）、`jobforge_event_transport_failures_total`（transport, reason）、`jobforge_event_redeliveries_total`（transport, consumer_group）、`jobforge_consumer_inbox_duplicates_total`（consumer_group）；consumer_group 必须来自有界配置；
- **部署**：新增可选 `durable-events` Compose profile（AOF + 命名 volume 的 Redis）；核心服务仍只强依赖 PostgreSQL。

## 后果

### 正面

- broker/consumer 暂停或重启后事件可恢复、可重放（AT-17/20）；
- 消费端幂等从约定升级为可执行协议，崩溃窗有固化测试（AT-18/19）；
- envelope v1 冻结公开事件契约，为后续 Kafka 演进保留 partition/幂等语义。

### 负面与成本

- 新增 Redis 作为事件交付依赖（非任务状态依赖）与 AOF 运维要求；
- 切换流程是书面运维变更，存在执行纪律成本；
- publisher 到 broker 的重复消息成为常态路径，消费方必须实现 inbox 去重。

### 风险与缓解

- Redis 被误解为第二任务事实源：只从 outbox 发布；broker 故障不回写 job；健康检查分离 task/event readiness；
- Redis 持久化配置不足导致重启丢消息：durable profile 强制 AOF 并验证；未启用禁止耐久声明；
- 切换沿用单一 published_at 造成误清理/误报：按第 6 节受控流程执行并书面决策。

## 考虑过的替代方案

### 方案 A：Kafka 作为首个耐久通道

未采用：运维体量与当前约 366 jobs/sec 规模不匹配；以容量门槛（PRD §6.1）触发后续 Kafka ADR/Spike，届时复用 envelope v1 与 event_id 幂等语义。

### 方案 B：继续仅用 LISTEN/NOTIFY + outbox 表扫描消费

未采用：无消费者组/ACK/pending 语义，订阅断线与 PG 重启窗口的丢通知无法收敛，不满足 G-701。

## 验证与退出条件

- AT-17：Redis stop/start（保留命名 volume，AOF）后停机期事件全部补入 Stream，积压归零；
- AT-18：XADD 成功后、MarkPublished 前 kill publisher，重启补发；允许同 event_id 多 entry，outbox 最终标记成功，任务状态不变；
- AT-19：同 event_id 多 entry + commit 后 ACK 前 kill consumer，pending 重领、inbox 去重、业务副作用恰一次（可独立运行）；
- AT-20：两个 consumer group 独立完整消费，组内分摊，无跨组游标干扰；
- NFR-302 smoke：≥5× W11 Process 吞吐（约 1,800 events/sec）事件写入，publish lag p95 ≤2s；NFR-303：Redis 暂停 60s 恢复后积压归零且不阻塞任务状态事务；
- notify 默认下 AT-15/16 不回退，且启动日志/指标明确 durable=false。

## 后续工作

- [ ] M2：Redis Streams adapter、durable-events Compose profile、事件指标与健康检查；
- [ ] M3：inbox schema、Go reference consumer、pending 恢复与幂等 demo；
- [ ] transport 切换运行手册随事件文档发布；
- [ ] 触发 Kafka 门槛时的后续 ADR/Spike。
