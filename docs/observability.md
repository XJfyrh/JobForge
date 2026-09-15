# 可观测性体系

本文档描述 JobForge 的可观测性架构，包括分布式 Trace、Prometheus 指标和 pprof 性能剖析。

技术选型见 [ADR-0004](adr/0004-observability-stack.md)；任务计数口径与可选 OTLP 增量见已接受的 [ADR-0012](adr/0012-task-observability-and-otlp.md)。

## 架构概览

```text
┌─────────────┐     ┌──────────────┐     ┌─────────────┐
│  HTTP API   │────▶│   Gateway    │────▶│   Worker    │
│ submit_job  │     │ claim_jobs   │     │  execute    │
└──────┬──────┘     │ complete_job │     └──────┬──────┘
       │            └──────┬───────┘            │
       │                   │                    │
       ▼                   ▼                    ▼
┌──────────────────────────────────────────────────────┐
│              OpenTelemetry TracerProvider             │
│         (stdout exporter / OTLP switchable)          │
└──────────────────────────────────────────────────────┘
       │
       ▼
┌──────────────────────────────────────────────────────┐
│         Prometheus MeterProvider (/metrics)          │
│              pprof (/debug/pprof/*)                  │
│           绑定 127.0.0.1:6060 (PRD 11.4)            │
└──────────────────────────────────────────────────────┘
```

## Trace Span 列表（PRD 12.2）

| Span 名称 | 组件 | 关键属性 | 文件 |
|---|---|---|---|
| `http.submit_job` | API | tenant_id, queue, type | `internal/api/http/job_handler.go` |
| `scheduler.promote_jobs` | Scheduler | jobs.promoted, jobs.recovered | `internal/scheduler/scheduler.go` |
| `gateway.claim_jobs` | Gateway | worker_id, max_jobs, jobs.claimed | `internal/gateway/grpc/worker_service.go` |
| `worker.execute` | Worker | queue, type, attempt, worker_id | `internal/worker/runtime.go` |
| `gateway.complete_job` | Gateway | worker_id | `internal/gateway/grpc/worker_service.go` |
| `gateway.cancel_signal` | Gateway | path、cancel.signal_latency_seconds | `internal/gateway/grpc/worker_service.go` |
| `outbox.publish` | Publisher | event_id, event_type, aggregate_id, transport, transport_durable, ack（delivered/failed；不含 payload） | `internal/outbox/publisher.go` |
| `event.consume` | Reference Consumer | messaging.system、consumer group、redelivered、delivery_count、ACK/pending/poison 结果 | `internal/eventconsumer/consumer.go` |
| `event.process` | Reference Consumer | inbox duplicate、事务结果；从 envelope traceparent 续接 | `internal/eventconsumer/processor.go` |

### Span 属性约束

- 包含：queue、type、attempt、worker_id、tenant_id
- 不包含：完整 payload、API key、Authorization header
- Consumer span 不包含 Redis URL、凭据或完整 payload；consumer group 仅来自已校验的启动配置。

### 传播机制

1. **HTTP 入口**：OTel 自动从 `traceparent` header 提取 parent context
2. **API → DB**：`http.submit_job` span 的 W3C TraceContext 序列化后写入 `jobs.trace_context` 列；`trace_id` 列保留兼容性标识（`X-Trace-ID` 或自动生成）
3. **DB → Worker**：Poll 响应的 `ClaimedJob.trace_context` 携带 traceparent，Worker 提取后以 submit span 为 parent 创建 `worker.execute` child span
4. **Worker → Gateway**：Heartbeat/Complete/Fail RPC 通过 gRPC metadata 携带 `traceparent`；取消 heartbeat 使 `gateway.cancel_signal` span 加入同一 trace，Complete/Fail 使 `gateway.complete_job` 加入同一 trace
5. **向后兼容**：`X-Trace-ID` header 继续支持；job.TraceID 字段保留；无 `trace_context` 的历史任务按原逻辑运行
6. **Outbox → Consumer**：envelope v1 的 `traceparent` 由 Consumer 提取，`event.consume` 与其子 span `event.process` 续接原 trace；空 traceparent 合法并形成新 trace

## Prometheus 指标（PRD 12.1）

| 指标名 | 类型 | 标签 |
|---|---|---|
| `jobforge_jobs_submitted_total` | Counter | tenant, queue, type |
| `jobforge_job_attempts_total` | Counter | queue, type, outcome |
| `jobforge_queue_depth` | Gauge | tenant, queue, state |
| `jobforge_job_latency_seconds` | Histogram | queue, type, outcome |
| `jobforge_claim_duration_seconds` | Histogram | —（单次 Poll 可能跨多个声明队列领取，无单一 queue 标签） |
| `jobforge_retries_total` | Counter | queue, type, error_code（固定分类） |
| `jobforge_dlq_total` | Counter | queue, type |
| `jobforge_lease_expired_total` | Counter | queue, type, resolution（requeued/cancelled） |
| `jobforge_workers_active` | Gauge | version, status |
| `jobforge_tenant_throttled_total` | Counter | tenant, reason |
| `jobforge_contract_rejections_total` | Counter | surface, reason |

### Outbox 发布指标（PRD v0.2 §8）

| 指标名 | 类型 | 标签 |
|---|---|---|
| `jobforge_outbox_pending` | Gauge | — |
| `jobforge_outbox_published_total` | Counter | event_type |
| `jobforge_outbox_publish_failures_total` | Counter | event_type, reason |

`jobforge_outbox_pending` 为采样值：publisher 每轮发布后统计 `published_at IS NULL` 的事件数。事件发布是 at-least-once，发布失败/重复发布不影响任务状态（详见[故障语义](failure-semantics.md)的 outbox 事件发布语义）。

### 事件 transport 指标（PRD v0.3 §8，ADR-0006）

| 指标名 | 类型 | 标签 |
|---|---|---|
| `jobforge_event_publish_lag_seconds` | Histogram | transport |
| `jobforge_event_transport_failures_total` | Counter | transport, reason |
| `jobforge_event_redeliveries_total` | Counter | transport, consumer_group |
| `jobforge_consumer_inbox_duplicates_total` | Counter | consumer_group |

- `jobforge_event_publish_lag_seconds`：outbox `created_at` 到 broker ACK 的时延；成功批量标记时逐事件记录（created_at 为 PostgreSQL 时钟，本地时钟负偏差钳位为 0）。
- `jobforge_event_transport_failures_total`：broker/序列化/ACK/标记/完整性失败计数；`reason` 取值 `channel_error`（transport 投递失败）、`mark_error`（PostgreSQL 标记失败）与 `pending_payload_deleted`（Redis 7 报告 PEL payload 已被裁剪/XDEL；Consumer fail closed）。
- `jobforge_event_redeliveries_total`：`XAUTOCLAIM` 恢复 PEL entry 时增加，消费侧 `transport` 当前固定为 `redis_streams`。
- `jobforge_consumer_inbox_duplicates_total`：inbox 冲突事务成功提交后增加；表示协议安全吸收的重复，不是失败计数。
- `transport` 标签仅有界取值 `notify` / `redis_streams`；`consumer_group` 只来自启动配置并受 1～128 字符安全字符集校验，不能由单条消息动态提供。

### 租户配额指标（PRD v0.3 §8）

| 指标名 | 类型 | 标签 |
|---|---|---|
| `jobforge_quota_reservation_conflicts_total` | Counter | — |
| `jobforge_quota_counter_drift` | Gauge | — |

- `jobforge_quota_reservation_conflicts_total`：Gateway 每次 Poll 统计候选因 counter 快照已达租户硬配额而在定批阶段被跳过的数量；持续上升说明预筛陈旧窗口内配额竞争激烈（只影响性能，不影响硬上限）。
- `jobforge_quota_counter_drift`：Scheduler leader 周期核对（`JOBFORGE_QUOTA_RECONCILE_INTERVAL`，默认 5m）发现的 counter 与 jobs 聚合绝对差异之和；发现差异后自动按 jobs 聚合并归零（详见[故障语义](failure-semantics.md)的租户配额计数节）。

### 取消延迟指标（PRD v0.3 FR-731/732，ADR-0008）

| 指标名 | 类型 | 标签 | 口径 |
|---|---|---|---|
| `jobforge_cancel_signal_latency_seconds` | Histogram | path | PostgreSQL `cancel_requested_at` → Gateway 发出 CANCEL；`path` 当前为 `heartbeat`，预留 `stream` |
| `jobforge_cancel_handler_stop_latency_seconds` | Histogram | type | Worker 收到 CANCEL/取消 context → 预注册 Handler 返回 |

signal 指标只接受 Heartbeat 查询以同一 PostgreSQL `clock_timestamp()` 返回的 elapsed，Gateway 不混用本地时钟；其直方图显式包含 6s bucket，健康 heartbeat 路径 p95 门禁为 ≤6s。Handler stop 是 Worker 单调本地时钟的独立段，受控测试另行报告 Cancel API 成功→context 取消，二者都不混入 signal SLO。指标不包含 job_id、worker_id、trace_id、payload 或凭据。

### Demo 持久业务效果指标（PRD v0.4 FR-807，ADR-0009）

| 指标名 | 类型 | 标签 | 说明 |
|---|---|---|---|
| `jobforge_demo_idempotent_effects_total` | Counter | outcome | `demo.idempotent_effect` 的持久效果结果；outcome 仅为 `applied`、`deduplicated`、`failed` |

Handler 同时输出结构化 `job_id`、`effect_outcome` 与 duration，便于按单任务审计真实崩溃窗口；日志不记录 payload、数据库 URL 或凭据。job ID 只进入日志，不作为 metric label。`applied` 在持久效果提交后立即记录，即使进程随后在 Complete 前退出也保留证据；`deduplicated` 表示重投读到了既有 `result_ref`。

### 执行契约拒绝指标（PRD v0.5 FR-907，ADR-0010）

`jobforge_contract_rejections_total{surface,reason}` 只记录固定枚举，不把实际 type、queue、worker ID 或 job ID 放入标签：

| surface | 可出现的 reason | 触发点 |
|---|---|---|
| `submit` | `unknown_type` | API 在 job/outbox 持久化前拒绝目录外 type |
| `register` | `malformed_capability`、`unknown_type` | Worker 登记字段非法或类型不在目录 |
| `poll` | `malformed_capability`、`unknown_type`、`capability_mismatch`、`unregistered_worker`、`capacity_exceeded` | Poll 本地字段校验或数据库同事务能力/容量核对 |
| `grpc_interceptor` | `missing_deadline` | Worker RPC 未携带 deadline，handler 不执行 |

API 与 Gateway 启动时记录 `catalog_size` 与 `catalog_sha256`。指纹基于排序目录，不受配置输入顺序影响，可在发布 smoke 中比较两进程配置；日志不输出目录原文、payload、DSN 或凭据。

### 标签约束

高基数字段 `event_id`、`job_id`、`trace_id`、`worker_id` 以及用户提供的 type/queue **不得**作为契约拒绝 metrics label（PRD 12.1 + code-standards）。既有任务指标的受控 type/queue 维度不因本增量改变。

### Gauge 发射点

| 指标 | 发射位置 | 触发时机 |
|---|---|---|
| `jobforge_queue_depth` | Scheduler `scanCycle` | 每次扫描周期按 (tenant, queue, state) 采样 pending 任务数 |
| `jobforge_workers_active` | Gateway `Register` + 周期采样器 | Worker 注册后即时采样；Gateway 后台每 `max(LeaseTTL/2, 5s)` 周期采样，仅统计心跳新鲜的 worker（见下节） |
| `jobforge_outbox_pending` | Publisher `Run` | 每轮发布结束后采样未发布事件数 |
| `jobforge_quota_counter_drift` | Scheduler `reconcileQuota` | 每 `JOBFORGE_QUOTA_RECONCILE_INTERVAL`（默认 5m）核对一次；修复成功后归零 |

### Worker 存活判定与 workers_active 语义

`workers.last_heartbeat_at` 是唯一的 worker 存活信号，由 Gateway 在两类 RPC 入口刷新（best-effort，失败不影响 RPC 结果）：

- **Heartbeat**：运行中任务按 job 心跳触达；
- **Poll**：空闲 worker 不发 job 心跳，只通过长轮询触达 Gateway，因此 Poll 同样刷新存活时间。

job lease 默认每 5s 都续租，**不节流**。只有附属的 `workers.last_heartbeat_at` 写由 SQL 条件节流：仅当时间缺失或早于 `LeaseTTL/3`（默认 10s）前才写，与 RPC 频率无关。因此默认一个 30s lease 周期约有 6 次 job lease UPDATE，但 worker liveness 仍约 10s 更新一次；非默认 TTL/interval 组合由真实 PostgreSQL 集成测试覆盖。

`jobforge_workers_active` 只统计 `last_heartbeat_at > now() - 2×LeaseTTL` 的 worker：

| 语义 | 说明 |
|---|---|
| 新鲜度窗口 2×LeaseTTL | 容忍一次触达失败（空闲 worker 触达间隔上界为 PollTimeout = 1×TTL） |
| 下降延迟上界 ~2.5×TTL | worker 崩溃后，最坏经过 2×TTL 心跳老化 + TTL/2 采样周期，gauge 归零 |
| 归零机制 | 周期采样器对上一轮存在、本轮消失的 (version, status) 序列显式记录 0 |

运维查询：`jobforge ctl workers-status`（只读直连数据库）列出全部注册 worker 的心跳时间与 stale 标记，`--stale-after` 默认 `3×LeaseTTL`（可覆盖）；gauge 新鲜度窗口（2×TTL）与该默认值是不同口径，前者服务监控降噪，后者服务运维巡检。

## pprof 诊断

pprof 端点与 /metrics 共享 debug 端口（默认 `127.0.0.1:6060`）：

| 端点 | 用途 |
|---|---|
| `/debug/pprof/profile` | CPU 剖析 |
| `/debug/pprof/heap` | 堆内存剖析 |
| `/debug/pprof/goroutine` | Goroutine 堆栈 |
| `/debug/pprof/trace` | 执行追踪 |
| `/metrics` | Prometheus 指标 |

### 使用示例

```sh
# 采集 5 秒 CPU profile
go tool pprof http://127.0.0.1:6060/debug/pprof/profile?seconds=5

# 查看堆内存
go tool pprof http://127.0.0.1:6060/debug/pprof/heap

# 查看 goroutine
go tool pprof http://127.0.0.1:6060/debug/pprof/goroutine
```

## 配置

| 环境变量 | 默认值 | 说明 |
|---|---|---|
| `JOBFORGE_OTEL_EXPORTER` | `stdout` | Trace exporter（stdout / none / otlp） |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | OTel 标准默认 | OTLP/HTTP 接收端；Compose 为 `http://otel-collector:4318`，主机 SDK 为 `http://localhost:4318` |
| `JOBFORGE_OTEL_SAMPLE_RATIO` | `1.0` | 采样率 [0.0, 1.0] |
| `JOBFORGE_METRICS_ADDR` | `127.0.0.1:6060` | Debug server 地址 |

## 安全约束（PRD 11.4）

- pprof、metrics 和管理端点只绑定内网或 localhost
- 日志不得记录完整敏感 payload、API key 或 Authorization header
- Span 属性不包含完整 payload

## Agent/RAG 本地观测闭环

先按[真实任务指南](real-tasks.md)启动模型并下载固定版本。Windows PowerShell 中可选启用：

```powershell
$env:JOBFORGE_OTEL_EXPORTER = 'otlp'
# 与集成测试数据库隔离，避免测试清空演示库。
$env:JOBFORGE_POSTGRES_PORT = '55433'
docker compose -p jobforge-agent-rag -f deploy/compose.yaml --profile models --profile obs up -d --build
$env:OTEL_EXPORTER_OTLP_ENDPOINT = 'http://localhost:4318'
.venv/Scripts/python.exe -m pip install './sdk/python[demo]'
.venv/Scripts/python.exe examples/agent_rag.py
```

Collector 0.160.0 接收 OTLP/HTTP 并向 Jaeger 2.20.0 转发；Jaeger 使用内存存储，重启后 Trace 丢失。任务与业务产物仍以 PostgreSQL 为事实源。Prometheus 3.14.0 / Grafana 13.2.1 为固定开发镜像，不配置外发告警通知。

- [任务仪表盘](http://localhost:3000/d/jobforge-tasks)：本地演示账号 `admin` / `jobforge`。切换 Task 查看 `rag.index` 或 `agent.extract`；Queue 只过滤具有该维度的指标。积压是队列级数据，没有 task type 维度。
- [Jaeger](http://localhost:16686)：粘贴 SDK 输出的 `trace_id`；也可填入仪表盘的 Trace ID 后点击 Open task trace。Grafana 同时预置 Jaeger 数据源，支持 Explore 查询。
- [Prometheus targets](http://localhost:9091/targets)：七个 JobForge 目标应为 UP；[alerts](http://localhost:9091/alerts) 展示规则状态。

链路为 `sdk.submit → http.submit_job → gateway.claim_jobs → worker.execute → business.<type> → business.model.* / business.artifact.publish → gateway.complete_job / gateway.fail_job`。SDK get/cancel/retry、产物 GET/search 也传播 TraceContext。租约恢复 Span `scheduler.recover_lease` 使用任务持久化的提交父上下文；后续 attempt 留在同一 Trace。人工重试创建新 job_id，使用本次请求 Trace，并以 Span Link 与 `retry_of_job_id` 关联原任务。只传 W3C TraceContext，不透传任意 Baggage。

强制 kill 可能丢失该进程尚未结束或尚未导出的 Span；恢复 Span、attempt 审计与后继 Worker 仍可关联。模型服务只接收 traceparent，本例不声称 Ollama 内部已实现 OTel；适配器的客户端 Span 覆盖模型调用。Trace 不存正文、prompt、输出、业务 key、产物内容或凭据。

### 指标口径与告警

`jobs_submitted_total` 只计新持久任务，幂等提交重放不计数；人工重试的新任务计数。`job_attempts_total` 按已提交转换记录 `succeeded / failed_retry / failed_dead / cancelled / lease_expired`；重复 Complete/Fail、旧 token 和取消竞争中的拒绝不计数。租约恢复计 attempt 的 `lease_expired` 及独立 recovery counter，不混入业务失败或自动重试。`retries_total` 只计 Fail 导致的 retry_wait，人工重试与租约恢复不计；未知 error_code 归类为 `OTHER`。

执行耗时由 Worker 的单调本地时钟测量，排除排队等待；只在结果被接受时记录一次。取消 Fail 记录已执行时长；无有效上报的租约回收不伪造时长。该指标不是提交到完成总延迟。QueueDepth 对消失序列显式归零，采样出错保留前值。

计数在业务事务提交后尽力发送，提交后立刻崩溃可能漏计；进程重启清零。因此统计卡注明 process totals，速率/5 分钟增长用于运行趋势，审计以 jobs/job_attempts 为准。首次出现即为 1 的 DLQ/recovery 序列也触发告警，避免纯 increase() 漏掉首个事件。

规则包括：抓取中断 15s、ready 积压且无活 Worker 30s、ready >100 持续 1m、新 dead 事件、租约恢复。Worker gauge 有 2×TTL 新鲜窗口与 TTL/2 采样延迟，默认约 75s 才归零，再加 30s 告警持续期。阈值供小型开发环境示例，生产应按容量设置。

### 自动化故障复现与排障

以下命令只针对可重建演示项目，实际停止 Collector/Ollama/Worker、暂停模型请求并 SIGKILL Worker，约需数分钟。脚本 finally 恢复被操作服务；若主机也被强制关闭，手动 `unpause ollama` 和 `start` 这些服务。

```powershell
.venv/Scripts/python.exe examples/observability_acceptance.py --project jobforge-agent-rag
```

脚本检查：Collector 故障时两类真实任务成功且结果不变；积压与服务失联告警触发、恢复后消退；两类任务自动重试、TIMEOUT→dead→人工重试、真实 Worker kill→自然租约恢复；按类型查询成功/失败/恢复指标、Jaeger Span 和 Grafana 的预置仪表盘。发布后/Complete 前崩溃由真实模型 Go 进程测试补充：

```powershell
docker compose -f deploy/compose.yaml up -d postgres
$env:JOBFORGE_TEST_DSN = 'postgres://jobforge:jobforge@localhost:5433/jobforge?sslmode=disable'
$env:JOBFORGE_TEST_PYTHON = 'E:\JobForge\.venv\Scripts\python.exe'
$env:JOBFORGE_REAL_MODEL_URL = 'http://localhost:11435'
$env:JOBFORGE_TEST_OTLP_ENDPOINT = 'http://localhost:4318'
$env:JOBFORGE_TEST_JAEGER_URL = 'http://localhost:16686'
go test -race -v ./tests/integration -run '^TestRealTasks' -count=1
```

未设置真实模型地址时明确 skip；未设置 Jaeger 地址时不执行后端查询断言。两项均设置才构成模型与观测联合验收。该命令使用测试库，不清理演示库。

排障顺序：SDK get 的 state/attempts/error_code → result_ref 与租户产物 GET → trace_id → 指标与 Worker 日志。缺 Trace 先查 exporter 配置、Collector 日志和 Jaeger 是否重启；勿重新提交业务去补 Trace。出现 out-of-order samples 或 Jaeger clock-skew 提示时检查主机/WSL/Docker 时钟；本地曾观察到时钟跳变，不能据跨进程时间戳推断精确耗时。执行时长与租约判定各自使用既定时钟口径。

OTLP 使用有界异步队列（2048 Span），网络超时 2s、批量导出超时 3s、关闭最多等待 5s，失败丢弃遥测，不重试业务也不回滚状态。队列满时丢 Span，不能阻塞 Claim/Complete。错误日志限频且不打印导出端响应/凭据。停止可选组件：`docker compose -p jobforge-agent-rag -f deploy/compose.yaml --profile obs stop otel-collector jaeger prometheus grafana`。
