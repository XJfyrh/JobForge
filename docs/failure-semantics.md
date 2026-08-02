# 故障语义与恢复

本文档描述 JobForge 的故障模型、恢复机制和验收测试证据。产品需求见 [PRD 第 13 节](product/JobForge_PRD_v0.1.md)，架构概览见 [architecture.md](architecture.md)。

## 交付保证

JobForge 保证 **at-least-once** 投递：任务在未进入终态前可再次被投递。

**不承诺 exactly-once**：Worker 在完成外部副作用后、Complete RPC 前崩溃时，任务可能被重复执行。业务侧必须通过幂等键保护外部副作用。

## 故障模型分类

| 故障类型 | 描述 | 检测机制 |
|---|---|---|
| Worker 崩溃 | Worker 进程异常终止（kill -9、OOM） | 心跳停止 → lease 过期 |
| 网络分区 | Worker 与 Gateway 之间网络中断 | 心跳超时 → lease 过期 |
| Scheduler 宕机 | Scheduler 进程终止 | Advisory lock 释放 → follower 接管 |
| PostgreSQL 短暂不可用 | 数据库连接中断 | 连接池重试；事务回滚 |
| 重复投递 | 相同任务被多次执行 | 幂等键 / fencing token |
| 取消竞争 | Complete 与 Cancel 同时到达 | 事务先提交者生效 |

## 故障矩阵

| AT ID | 场景 | 故障注入 | 检测机制 | 恢复路径 | 一致性保证 | 测试 |
|---|---|---|---|---|---|---|
| AT-01 | 并发领取 | 两个 Worker 同时 claim 同一批任务 | FOR UPDATE SKIP LOCKED | 每个 fencing token 只有一个 owner | 无重复领取 | `TestGatewayRegisterWorker` |
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

## 恢复时间保证

| NFR | 约束 | 公式 | 默认值 |
|---|---|---|---|
| NFR-003 | 任务恢复上界 | lease_ttl + scan_interval + 2s | 30 + 1 + 2 = 33s |
| NFR-004 | Scheduler 接管时间 | 2 × lock_retry_interval + 2s | 2 × 5 + 2 = 12s |

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

## 取消竞争规则

Complete 与 Cancel 同时到达时，采用"事务先提交者生效"：

| 先提交 | 结果 | 后到者收到 |
|---|---|---|
| Complete | state = succeeded | cancel → ALREADY_TERMINAL |
| Cancel | state = cancelling | complete → CANCEL_REQUESTED |

cancelling 状态下：
- Worker 通过 Heartbeat 控制消息收到 cancel 信号 → 停止执行 → Fail(cancelled)
- 若 Worker 未响应 → lease 过期 → Scheduler 将 cancelling → cancelled

## DLQ 进入与恢复

**进入条件**：
- `attempt >= max_attempts` 且错误为 retryable → state = dead
- 错误分类为 fatal（不可重试）→ state = dead

**人工恢复**：
- `POST /v1/jobs/{job_id}:retry` → 克隆新 job_id，记录 `retry_of_job_id`
- 原任务终态（dead/cancelled）不可变
- 新任务继承原 type/queue/payload，attempt 重置为 0

## 测试证据索引

| 文件 | 覆盖场景 |
|---|---|
| `tests/integration/fault_test.go` | AT-02, AT-03, AT-04 |
| `tests/integration/gateway_test.go` | AT-01, AT-05, AT-06, AT-07, AT-08 |
| `tests/integration/scheduler_test.go` | AT-09, lease 回收, advisory lock |
| `tests/integration/tenant_test.go` | AT-10 |
| `tests/integration/worker_test.go` | AT-11, goroutine 稳态 |
| `tests/integration/observability_test.go` | AT-12 |
| `tests/integration/job_store_test.go` | Claim 并发, 幂等键, 状态转换 |

## 相关文档

- [系统架构](architecture.md) — 组件职责与数据流
- [性能基线](benchmark.md) — 恢复时间实测数据
- [演示脚本](demo-script.md) — 故障演示步骤
