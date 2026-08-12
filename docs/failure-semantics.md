# 故障语义与恢复

本文档描述 JobForge 的故障模型、恢复机制和验收测试证据。产品需求见 [PRD 第 13 节](product/JobForge_PRD_v0.1.md)，架构概览见 [architecture.md](architecture.md)。

## 交付保证

JobForge 保证 **at-least-once** 投递：任务在未进入终态前可再次被投递。

**不承诺 exactly-once**：Worker 在完成外部副作用后、Complete RPC 前崩溃时，任务可能被重复执行。业务侧必须通过幂等键保护外部副作用。

## 故障模型分类

| 故障类型 | 描述 | 检测机制 |
|---|---|---|
| Worker 崩溃 | Worker 进程异常终止（kill -9、OOM） | 心跳停止 → lease 过期；`workers.last_heartbeat_at` 老化 → `jobforge_workers_active` 在 ~2×TTL 后不再计入该 worker（见[可观测性](observability.md)的 Worker 存活判定） |
| 网络分区 | Worker 与 Gateway 之间网络中断 | 心跳指数退避重试；超过 lease 仍未续租成功 → lease 过期 |
| Scheduler 宕机 | Scheduler 进程终止 | Advisory lock 随连接断开释放 + 优雅让位置空领导权行 → follower 立即接管 |
| Scheduler 卡死 | 进程存活但扫描循环停摆（死循环、阻塞），advisory lock 不释放 | 领导权租约心跳停止（心跳在扫描循环内）→ `last_seen` 过期（默认 10s）→ standby 条件接管；旧实例复活后 epoch 自检让位（ADR-0005） |
| PostgreSQL 短暂不可用 | 数据库连接中断 | 连接池重试；事务回滚 |
| 重复投递 | 相同任务被多次执行 | 幂等键 / fencing token |
| 取消竞争 | Complete 与 Cancel 同时到达 | 事务先提交者生效 |

## 故障矩阵

| AT ID | 场景 | 故障注入 | 检测机制 | 恢复路径 | 一致性保证 | 测试 |
|---|---|---|---|---|---|---|
| AT-01 | 并发领取 | 两个 Worker 同时 claim 同一批任务 | FOR UPDATE SKIP LOCKED | 每个 fencing token 只有一个 owner | 无重复领取 | `TestConcurrentClaim` |
| AT-02 | ACK 前崩溃 | Handler 执行后 kill Worker | lease 过期 | 任务重投；幂等 Handler 跳过重复 | 业务副作用仅一次 | `TestFaultAT02CrashBeforeACK` |
| AT-03 | 旧 Worker 晚到 | lease 过期后旧 Worker 发 Complete | fencing token 不匹配 | 返回 STALE_LEASE；新状态不被覆盖 | 新 owner 状态不变 | `TestFaultAT03StaleWorkerLateComplete` |
| AT-04 | Heartbeat 丢失 | 阻断 Worker 网络直到 lease 过期 | lease_until < now() | Scheduler 回收 → 新 Worker 领取 | 旧 Worker 后续写入被拒 | `TestFaultAT04HeartbeatLoss` |
| AT-05 | Cancel 先提交 | running 时 cancel，再 Complete | cancel_requested_at 已设置 | 状态进入 cancelling → cancelled | Complete 被拒绝 | `TestEndToEndSubmitExecuteComplete` |
| AT-06 | Complete 先提交 | Complete 后再 cancel | 状态已为 succeeded | 状态保持 succeeded | cancel 返回 ALREADY_TERMINAL | `TestEndToEndSubmitExecuteComplete` |
| AT-07 | 重试退避 | 连续返回 retryable error | attempt < max_attempts | retry_wait → 退避到期 → ready | 退避时间符合公式 | `TestEndToEndFailRetryRecover` |
| AT-08 | DLQ 人工恢复 | 任务进入 dead 后执行 retry | 人工触发 POST :retry | 克隆新 job_id 重新执行 | 原任务终态不变 | `TestEndToEndManualRetry` |
| AT-09 | Scheduler 切换 | 启动双 Scheduler，kill leader | advisory lock 释放 | follower 接管；任务只推进一次 | 无重复推进 | `TestSchedulerFailover` |
| AT-10 | 租户隔离 | 租户 A 填满并发配额 | Claim 时检查 tenant running count | 租户 B 仍可在自身配额内执行 | 配额互不影响 | `TestTenantAT10Isolation` |
| AT-11 | 优雅退出 | Worker 收到 SIGTERM | context 取消 | 停止领取；进行中任务完成或释放 | 无 goroutine 泄漏 | `TestWorkerAT11GracefulShutdown` |
| AT-12 | Trace 串联 | 提交一个真实任务 | OTel span recorder | API/Gateway/Worker span 同一 trace | trace_id 一致 | `TestObservabilityAT12FullSpans` |
| AT-13 | 循环故障注入（scale） | 100 轮 Worker kill / lease 过期 | lease 过期 → Scheduler 回收 | 每轮全部任务重领并完成 | 非终态任务零静默丢失；恢复时间符合 NFR-003 | `TestScaleAT13WorkerKillRounds`（`-tags scale`） |
| AT-14 | 万级幂等（scale） | 10,000 任务 ACK 前崩溃后重复投递 | lease 过期 → 重投 → Handler 去重 | `demo.idempotent_effect` 按 job_id 幂等 | 重复业务副作用计数为 0 | `TestScaleAT14IdempotentTenThousand`（`-tags scale`） |

## 恢复时间保证

| NFR | 约束 | 公式 | 默认值 |
|---|---|---|---|
| NFR-003 | 任务恢复上界 | lease_ttl + scan_interval + 2s | 30 + 1 + 2 = 33s |
| NFR-004 | Scheduler 接管时间（终止路径） | 2 × lock_retry_interval + 2s | 2 × 5 + 2 = 12s |

NFR-004 公式适用于主实例终止路径；卡死路径（进程存活但扫描停摆）的接管上界为 `leadership_timeout + lock_retry_interval`（默认 10 + 2 = 12s，见 [ADR-0005](adr/0005-scheduler-leadership-lease.md)）。

## Fencing Token 机制

每次 Claim 事务递增 `fencing_token`（BIGINT 序列）：

```text
Claim 事务:
  UPDATE jobs SET
    state = 'running',
    lease_owner = $worker_id,
    lease_until = now() + $lease_ttl,
    attempt = attempt + 1,
    fencing_token = nextval('jobs_fencing_token_seq')
  WHERE id = $job_id AND state = 'ready'
  RETURNING fencing_token
```

**验证点**（Heartbeat / Complete / Fail）：

```text
WHERE id = $job_id
  AND lease_owner = $worker_id
  AND fencing_token = $token
  AND state IN ('running', 'cancelling')
```

若 WHERE 不匹配（旧 token、错误 owner、状态已变更），返回 `STALE_LEASE` 错误，不修改任何数据。

## Worker 侧续租与结果上报韧性

fencing token 只拦截陈旧的**结果提交**，无法阻止陈旧 Worker 继续**执行**。为缩小双执行窗口，Worker Runtime 在客户端实施以下策略：

**心跳续租**：

- 默认 cadence 为 5s、Lease TTL 为 30s。Gateway 注册响应返回自身配置；Worker 未显式配置时采用建议值，显式本地值优先。
- PostgreSQL 在一次 `UPDATE ... RETURNING` 中以同一个 `clock_timestamp()` 设置 `lease_until`、检测 cancelling 并返回 `cancel_requested_at`→signal elapsed；Gateway 不以本机 `time.Now()`估算该跨进程时延。
- 心跳瞬时失败（`Unavailable` / `DeadlineExceeded` / `Unknown` 等）**不**停止续租：按指数退避（1s/2s/4s…上限 10s）持续重试。
- 租约截止时刻以 Gateway 返回的 `lease_until`（服务端时钟）为准，避免本地 TTL 估算的时钟漂移。
- 仅当重试持续到 `lease_until` 仍无法续租时才放弃：取消执行上下文（让 Handler 收尾）并标记租约丢失，最小化双执行窗口。
- 心跳被拒 `STALE_LEASE`（租约已被新 Worker 持有）时立即放弃，不做重试。

**租约丢失后的结果处理**：租约丢失后 Worker **丢弃**执行结果，不上报 Complete/Fail——陈旧 Worker 不得覆盖新租约状态，任务由 Scheduler 正常重投。

**结果上报**：Complete/Fail RPC 对瞬时故障最多重试 3 次（退避 1s/2s）；Gateway 对同一租约的重复上报幂等吸收（`isIdempotentComplete` / `isIdempotentFail`）。`STALE_LEASE` 等永久性错误不重试。

**语义边界**：上述机制缩小但不能消除双执行窗口（lease 过期即可能重投），at-least-once 交付保证与业务幂等要求不变。

## 取消竞争规则

Complete 与 Cancel 同时到达时，采用"事务先提交者生效"：

| 先提交 | 结果 | 后到者收到 |
|---|---|---|
| Complete | state = succeeded | cancel → ALREADY_TERMINAL |
| Cancel | state = cancelling | complete → CANCEL_REQUESTED |

cancelling 状态下：
- Worker 通过 Heartbeat 控制消息收到 cancel 信号 → 先记录本地接收时刻并取消执行 context → Handler 返回后单独记录 stop latency → Fail(cancelled)
- 若 Worker 未响应 → lease 过期 → Scheduler 将 cancelling → cancelled

健康 Heartbeat 路径的 SLO 只覆盖 PostgreSQL `cancel_requested_at` 到 Gateway 发出 CANCEL signal，p95 ≤6s。Cancel API 成功返回到 Worker context 取消、以及 context 取消到 Handler return 是独立报告段，不与 signal SLO 混算。瞬时 Gateway 故障恢复后继续查询 cancelling；若持续故障越过 lease deadline，则由原有 lease expiry 路径收敛。M4 不实现 P1 ControlStream。

## 租户配额计数（PRD v0.3）

租户 inflight 硬配额由派生计数表 `tenant_quota_counters` 执行（ADR-0007），故障语义与 PRD v0.3 §7.2 一致：

1. quota slot 的预留/释放必须与对应 job 状态变化同事务：Claim 预留与 lease 更新同事务提交；Complete、Fail（retry/dead/cancelling→cancelled）与 Scheduler lease 回收均在状态转换事务内扣减；running→cancelling 不释放（cancelling 继续占用）。
2. 事务回滚后 slot 与 job 均不变化：预留是事务内的条件 upsert，回滚时与领取一起消失，不存在未用 slot。
3. 计数漂移修复以 `jobs WHERE state IN ('running','cancelling')` 聚合为准：Scheduler leader 周期核对（默认 5m，`JOBFORGE_QUOTA_RECONCILE_INTERVAL`）与 `jobforge ctl quota-reconcile [--repair]` 都从 jobs 聚合覆盖写入 counter（含无 inflight 任务的租户归零）。
4. 计数表不可替代 jobs 进行任务查询、恢复或审计：它是 PostgreSQL 内的派生一致性数据，任何时候可重建。
5. 同租户 Claim 可在计数行上短暂串行化（counter 行按 tenant_id 升序加锁，全局锁序 jobs 行 → counter 行），禁止通过放松硬上限换取吞吐。

**故障与恢复路径**：

| 故障 | 表现 | 恢复 |
|---|---|---|
| 预筛陈旧（并发事务刚填满/释放配额） | 候选预留被拒（`jobforge_quota_reservation_conflicts_total` 递增）或满额租户多等一轮 Claim | 事务内条件预留保证不超配；下一轮 Claim 自然收敛 |
| 计数漂移（人为改动/历史路径遗漏） | `jobforge_quota_counter_drift` > 0；错误限流或超配风险 | 周期核对自动修复（日志含 tenant/counter/actual 审计）；或手工 `ctl quota-reconcile --repair` |
| migration 0013 backfill 后存量运行中任务 | counter 在迁移时从 jobs 聚合初始化 | backfill 幂等（ON CONFLICT 覆盖），无需人工介入 |

硬上限正确性（并发 Claim 不超配、cancelling 占用、满额租户不阻塞他人）由 AT-21/22/23 验收（见下节测试证据）。

## DLQ 进入与恢复

**进入条件**：
- `attempt >= max_attempts` 且错误为 retryable → state = dead
- 错误分类为 fatal（不可重试）→ state = dead

**人工恢复**：
- `POST /v1/jobs/{job_id}:retry` → 克隆新 job_id，记录 `retry_of_job_id`
- 原任务终态（dead/cancelled）不可变
- 新任务继承原 type/queue/payload，attempt 重置为 0

## Outbox 事件发布语义（PRD v0.2 / v0.3 §7.1，ADR-0006）

任务终态转换（succeeded/dead/cancelled/cancelling→cancelled）与 lease 回收均在任务事务内写入 `outbox_events`（同时捕获 envelope v1 的 `aggregate_version`/`traceparent`，migration 0016）；`publisher` 子命令异步发布这些事件。外部 transport 可配置为 `notify`（ADR-0003 的 PostgreSQL LISTEN/NOTIFY，v0.2 兼容默认，非耐久）或 `redis_streams`（ADR-0006，耐久）。

**发布保证（PRD v0.3 §7.1 逐条落地）**：

1. 任务状态事务与 outbox 写入原子提交；broker 调用永远在事务外。
2. broker 调用失败时 `published_at` 保持 NULL，publisher 退避重试；redis_streams 下禁止降级 NOTIFY 后标记成功（FR-702）。
3. broker ACK 成功、PostgreSQL 标记前崩溃时允许重复消息（批量标记把该崩溃窗扩大到整轮，语义不变）；消费者按 `event_id` 去重（AT-18 固化）。
4. consumer 业务事务成功、ACK 前崩溃时允许 redelivery；inbox 唯一约束吸收重复（M3 交付，见 ADR-0006 §7）。
5. consumer ACK 后不再依赖 JobForge outbox 作为其个人消费游标。
6. Redis 数据耐久性依赖启用 AOF（durable-events profile 固定 `appendonly yes`、`appendfsync everysec`）；未启用持久化时不得宣称可跨 Redis 重启恢复（AT-17 验证）。

**其他不变量**：

- **at-least-once**：事件可能重复发布；消费方必须按 `event_id` 幂等去重，不得宣称 exactly-once。
- 领取为单语句原子操作：`UPDATE outbox_events SET claimed_at = now() WHERE ... FOR UPDATE SKIP LOCKED RETURNING`，并发 publisher 不会领到同一事件；发布失败与优雅关停即时释放领取，仅硬崩溃的领取标记留存至过期（5 分钟）后重领。
- notify 仅作 hint：payload 只携带 `event_id`（通道 `jobforge_outbox`），消费方回查 `outbox_events` 获取完整事件；redis_streams 直接携带 envelope v1。
- 发布在任务状态事务之外异步进行：发布失败、重复发布、publisher 崩溃或 Redis 停机均**不得**改变任务状态（NFR-303 固化：broker 暂停期间任务状态事务不被阻塞）。
- publisher 不持有内存进度：崩溃重启后仅从 `published_at IS NULL` 恢复。
- Redis 故障不回写 jobs；健康检查分离 task/event readiness。

### Consumer inbox、pending 与 poison

参考 Consumer 的原子边界仅覆盖同一个 PostgreSQL 事务：`consumer_inbox` 首次插入与预注册 Handler 的业务效果一起提交，提交成功后才执行 Redis `XACK`。因此：

| 故障窗口 | 可观察结果 | 恢复 |
|---|---|---|
| BEGIN/Handler/COMMIT 失败 | inbox 与业务效果一起回滚，entry 保持 pending | 基础设施恢复后重投；不计入 poison 上限 |
| COMMIT 成功、XACK 前进程退出 | inbox 与效果已存在，entry 保持 pending | 其他实例 `XAUTOCLAIM`；inbox 冲突跳过 Handler 后 ACK |
| XACK 瞬时失败 | 与上一行相同，可能重复读取 | 退避；重投由 inbox 吸收 |
| envelope/schema 或 Handler 明确永久错误 | entry 保持 pending，并按 PEL delivery count 重试 | 达到上限后先 XADD poison 元数据，再 ACK 原 entry |
| poison XADD 成功、原 entry ACK 前退出 | poison 记录可能重复，原 entry 仍 pending | 再次隔离；按 `source_stream + source_entry_id` 人工去重 |
| Redis/数据库/context 瞬时故障 | 不 ACK、不 poison，不触碰 jobs | 有界指数退避，依赖恢复后收敛 |
| inbox 已绑定其他 group 或同 event_id 元数据冲突 | 配置/数据完整性错误，当前 entry 不 ACK、不 poison | Consumer fail closed；修正 schema/group 或调查事件来源后重启 |
| pending payload 被 MAXLEN/XDEL 删除 | Redis 7 从 PEL 清除缺失 ID，但业务事务从未执行 | `pending_payload_deleted` 指标增加且 Consumer 退出；依据 outbox 高水位人工恢复，不得误报 drain |

poison 记录只包含 source stream/entry、固定 consumer group、可解析的 event_id、delivery count、有界 reason 与 failed_at；不复制 payload，且 poison stream 必须与源 stream 不同。新 consumer group 从 `0-0` 创建以读取已有 backlog，已有 group 的 cursor/PEL 不被重置。单 entry 处理且默认 `process_timeout=10s < pending_min_idle=30s`，避免正常事务被过早抢占；持续新流量与 pending recovery 交替执行，空页返回的非零 XAUTOCLAIM cursor 也会继续扫描，因此 PEL 后部不会因新消息或前部 fresh entry 永久饥饿。

`consumer_inbox_binding` 在数据库中强制一个 inbox schema 只对应一个逻辑 consumer group；不同 group 不能靠运维约定静默复用。PostgreSQL 事务外的 HTTP、邮件、支付等副作用仍可能在调用成功后、数据库提交前重复；必须透传 `event_id`/idempotency key，由目标系统幂等，不得称为 exactly-once。Poison 自动重放不在本阶段范围内。

**故障恢复路径**：

| 故障 | 表现 | 恢复 |
|---|---|---|
| 发布通道失败 | `publish_attempts` 递增、`jobforge_event_transport_failures_total` 上升，`published_at` 保持 NULL | 退避后下一轮重试 |
| publisher 崩溃（broker ACK 已得、未标记） | 重启后重复发布同一事件（Stream 中同 event_id 多 entry 被允许） | 消费方按 event_id 去重；任务状态不受影响（AT-18） |
| publisher 领取后崩溃（`claimed_at` 已置、未发布） | 事件暂停参与领取 | 领取过期（5 分钟）后下一轮自动重领；at-least-once 下可能重复发布 |
| Redis 停机/重启 | 停机期事件保持未发布；AOF + 命名 volume 保留停机前 Stream 记录 | 恢复后全部补入 Stream、积压归零（AT-17/NFR-303） |
| 事件积压 | `jobforge_outbox_pending` 上升、`jobforge_event_publish_lag_seconds` 变大 | 增加 publisher 实例（单语句原子领取，并发不重复） |
| Consumer commit 后 ACK 前退出 | Stream entry 留在 PEL；业务效果已提交 | `XAUTOCLAIM` + inbox 冲突吸收重复（AT-19） |
| 永久坏事件 | PEL delivery count 增长；合法后续事件继续处理 | 达上限写 poison 元数据后 ACK；人工检查/重放 |

**Retention**：仅清理 `published_at IS NOT NULL` 且超过保留期（默认 7 天，`JOBFORGE_OUTBOX_RETENTION`）的事件，即只清理已成功交付到当前启用 transport 的行；未发布事件永不被清理。Stream 自身的裁剪（`JOBFORGE_REDIS_STREAM_MAXLEN`，默认 0 不裁剪）属运维责任，不改变 PostgreSQL outbox 事实源，但可能删除尚未 ACK 的 Redis payload；reference Consumer 会检测 Redis 7 deleted PEL IDs 并故障退出，而不是把该事件视为已消费。

## 测试证据索引

| 文件 | 覆盖场景 |
|---|---|
| `tests/integration/fault_test.go` | AT-02, AT-03, AT-04；Gateway 抖动（TTL 内恢复）零重投、租约丢失取消执行并丢弃结果（`TestFaultGatewayBlipWithinTTLNoRedelivery`、`TestFaultLeaseLostCancelsExecutionAndDiscardsResult`） |
| `tests/integration/gateway_test.go` | AT-05, AT-06, AT-07, AT-08 |
| `tests/integration/scheduler_test.go` | AT-09, lease 回收, advisory lock；卡死 leader 租约接管与 epoch fencing、优雅让位即时接管（`TestSchedulerStuckLeaderTakeover`、`TestSchedulerGracefulReleaseImmediateTakeover`） |
| `tests/integration/tenant_test.go` | AT-10 |
| `tests/integration/quota_test.go` | AT-21（64 并发硬上限，事务内/采样/终态三层断言，预筛开关双变体）、AT-22（满额租户不阻塞：单任务 ≤1s；30s 持续流量 p95≤1s/max≤2s）、AT-23（取消风暴占用 slot）、FR-724 漂移核对与修复（Scheduler 与 ctl 双路径）；AT-25 保留为可裁剪 M5 骨架 |
| `tests/integration/cancel_slo_test.go` | AT-24：真实 PostgreSQL + HTTP Cancel + gRPC Gateway + Worker Runtime，默认 5s 随机相位；DB-clock signal p95≤6s，API→context 与 Handler stop 分段报告；默认/非默认 interval/TTL、liveness 节流与 trace/metric 标签同时验证 |
| `tests/integration/worker_test.go` | AT-11, goroutine 稳态 |
| `tests/integration/observability_test.go` | AT-12 |
| `tests/integration/job_store_test.go` | AT-01, Claim 并发, 幂等键, 状态转换 |
| `tests/integration/outbox_test.go` | AT-15：发布失败重试、publisher 崩溃恢复（批量标记崩溃窗语义）、重复投递幂等、retention 边界；双 publisher 并发零重复 NOTIFY、僵尸领取回收（`TestOutboxConcurrentPublishersNoDuplicateNotify`、`TestOutboxStaleClaimReclaimed`） |
| `tests/integration/durable_events_test.go` | AT-17（Redis stop/start AOF 重启恢复）、AT-18（XADD 后标记前崩溃窗重复 entry）、AT-20（双 consumer group 隔离与组内分摊）、NFR-303（60s broker 暂停恢复与任务事务不阻塞）；`JOBFORGE_TEST_REDIS_URL` 缺省时 skip |
| `tests/integration/event_consumer_test.go` | AT-19（commit 后 ACK 前确定性退出、XAUTOCLAIM、inbox 单效果）、晚建 group backlog、poison 隔离/后续合法事件、消费指标与 traceparent 传播；Redis 缺省时 skip |
| `tests/scale/nfr302_event_publish_test.go`（`-tags scale`） | NFR-302 事件发布 smoke floor（约 1,800 events/sec）与健康态 publish lag p95 ≤2s |
| `tests/scale/heartbeat_write_test.go`（`-tags scale`） | NFR-306：逻辑 30s 窗口下 10s→5s cadence 的真实 lease UPDATE 为 300→600，精确 2.00×；只报告 p50/p95，不设机器相关延迟阈值 |
| `tests/integration/ctl_test.go` | AT-16：ctl retry 克隆新 job_id、原任务终态不可变、retry_of_job_id 审计；鉴权失败、DLQ 列表、outbox 只读状态 |
| `tests/scale/kill_test.go` | AT-13：100 轮 Worker kill 零静默丢失、恢复耗时分布（`-tags scale`） |
| `tests/scale/idempotent_test.go` | AT-14：10,000 任务重复投递零重复副作用（`-tags scale`） |

scale 套件的规模参数、运行命令与结果归档见 [可靠性报告](reliability-report.md)。

## 相关文档

- [系统架构](architecture.md) — 组件职责与数据流
- [性能基线](benchmark.md) — 恢复时间实测数据
- [可靠性报告](reliability-report.md) — scale 套件（AT-13/AT-14）运行结果与复现命令
- [演示脚本](demo-script.md) — 故障演示步骤
