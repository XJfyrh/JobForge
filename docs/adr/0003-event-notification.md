# ADR-0003：事件通知机制（PostgreSQL LISTEN/NOTIFY）

- 状态：Accepted
- 日期：2026-07-29
- 决策者：XJfyrh
- 关联 Issue/PR：W2-W3 Scheduler + Gateway
- 取代：无
- 被取代：无

## 上下文

Scheduler 需要感知新任务入队（scheduled/retry_wait → ready 推进），Worker Gateway 的 Poll long-poll 需要在无可用任务时等待新任务到来。纯短轮询方案（每 1s 或 500ms 查询 DB）在低负载时引入不必要的延迟和数据库压力；而引入外部消息系统（NATS/Redis）则违反"PostgreSQL 是 P0 唯一事实源"原则并增加部署依赖。

PostgreSQL 原生 LISTEN/NOTIFY 提供进程间异步通知能力，无需额外基础设施，且与事务提交天然绑定。

## 决策驱动因素

- 可靠性优先：NOTIFY 丢失不得导致任务卡住或静默丢失。
- 最小依赖：不引入 PostgreSQL 以外的 P0 组件。
- 多实例兼容：通知必须跨进程可见（排除内存 channel）。
- 延迟可接受：P0 目标为亚秒级唤醒，非毫秒级实时。

## 决策

### Channel 定义

| Channel | 触发时机 | 消费者 |
|---|---|---|
| `jobforge_job_ready` | Enqueue 事务提交后；Scheduler promote 后 | Gateway Poll、Scheduler（可选快速路径） |

Payload 为 queue 名称（可选，消费者收到后重新查询 DB 获取具体任务）。

### 语义

- NOTIFY 仅为"提示信号"（hint），**不作为事实源**。
- 接收方收到 NOTIFY 后必须重新查询 DB 确认实际状态。
- NOTIFY 丢失（如进程 crash 瞬间、连接断开）不影响正确性。

### 兜底机制

| 组件 | 兜底策略 |
|---|---|
| Scheduler | 每 scan_interval（1s）定时扫描，无论是否收到 NOTIFY |
| Gateway Poll | 在 RPC deadline 内每 500ms fallback 查询一次 DB |

### 连接管理

- LISTEN 使用独立 pgx 连接（`pgx.Connect`），不走连接池（pool 连接不可长期占用）。
- 断线后自动重连（指数退避，最大 5s），重连后重新 LISTEN。
- 每个需要 LISTEN 的进程（Scheduler、Gateway）维护一条专用连接。

### 发送端

- 在事务**提交后**发送 NOTIFY（`tx.Commit()` 之后），避免事务回滚时发出虚假通知。
- 使用 `pool.Exec(ctx, "SELECT pg_notify($1, $2)", channel, payload)`。
- NOTIFY 发送失败仅记录 warn 日志，不阻塞主流程。

## 公开契约与数据影响

- 无 schema 变更。
- 无公开 API 变更。
- 调度延迟语义：正常路径 < 100ms（NOTIFY 到达）；降级路径 ≤ scan_interval（1s）。
- 恢复时间上界不变：lease_ttl + scan_interval + 2s（NOTIFY 不缩短也不延长此上界）。

## 后果

### 正面

- Poll long-poll 响应延迟从 500ms-1s 降至 < 100ms（正常路径）。
- Scheduler 可在事件驱动模式下更快推进，同时保留定时兜底。
- 无额外部署依赖，Docker Compose 不增加组件。

### 负面与成本

- 每个 LISTEN 进程占用一条额外 PostgreSQL 连接。
- 需要处理连接断开、重连和 NOTIFY 丢失的边界情况。
- PostgreSQL NOTIFY payload 上限 8000 字节（本场景仅传 queue 名，无影响）。

### 风险与缓解

- NOTIFY 在 PostgreSQL crash 时丢失 → 兜底轮询保证最终一致。
- LISTEN 连接长时间空闲可能被防火墙/负载均衡器断开 → 心跳检测 + 自动重连。
- 高并发 Enqueue 产生大量 NOTIFY → 消费者合并处理（收到多个通知只触发一次查询）。

## 考虑过的替代方案

### 纯短轮询

Scheduler 每 1s 扫描，Gateway Poll 每 500ms 查询。实现最简单，无连接管理复杂度。但低负载时任务等待时间平均增加 250-500ms，高并发时 DB 空查询压力增大。作为兜底方案保留。

### 内存 channel（Go channel）

进程内通知，延迟最低。但不支持多实例部署（Scheduler 和 Gateway 可能在不同进程/容器），P0 已规划单二进制多子命令模式，各子命令独立进程运行。不采用。

### 外部消息系统（NATS/Redis Pub-Sub）

延迟低、功能丰富。但引入 P0 额外依赖，违反"PostgreSQL 唯一事实源"原则，增加部署和故障测试复杂度。P1 的 JetStream 适配器是独立的事件发布通道，不用于内部进程通信。不采用。

## 验证与退出条件

- Gateway Poll 在 Enqueue 后 < 500ms 返回任务（集成测试验证）。
- 杀掉 LISTEN 连接后，Scheduler 仍能在 1s 内通过兜底轮询推进任务。
- `go test -race ./...` 通过。

## 后续工作

- [ ] W2：Scheduler 集成 LISTEN + 定时扫描
- [ ] W3：Gateway Poll 集成 LISTEN 唤醒
- [ ] P1：评估是否需要 `jobforge_job_cancelled` channel 用于更快的取消传播
