# 可观测性体系

本文档描述 JobForge 的可观测性架构，包括分布式 Trace、Prometheus 指标和 pprof 性能剖析。

技术选型见 [ADR-0004](adr/0004-observability-stack.md)。

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

### Span 属性约束

- 包含：queue、type、attempt、worker_id、tenant_id
- 不包含：完整 payload、API key、Authorization header

### 传播机制

1. **HTTP 入口**：OTel 自动从 `traceparent` header 提取 parent context
2. **API → DB**：trace_id 写入 jobs 表（W5 兼容）；trace_context 列存储 W3C TraceContext
3. **DB → Worker**：Poll 响应携带 trace_id，Worker 创建 child span
4. **向后兼容**：`X-Trace-ID` header 继续支持；job.TraceID 字段保留

## Prometheus 指标（PRD 12.1）

| 指标名 | 类型 | 标签 |
|---|---|---|
| `jobforge_jobs_submitted_total` | Counter | tenant, queue, type |
| `jobforge_job_attempts_total` | Counter | queue, type, outcome |
| `jobforge_queue_depth` | Gauge | tenant, queue, state |
| `jobforge_job_latency_seconds` | Histogram | queue, type, outcome |
| `jobforge_claim_duration_seconds` | Histogram | queue |
| `jobforge_retries_total` | Counter | queue, error_code |
| `jobforge_dlq_total` | Counter | queue, type |
| `jobforge_lease_expired_total` | Counter | queue |
| `jobforge_workers_active` | Gauge | version, status |
| `jobforge_tenant_throttled_total` | Counter | tenant, reason |

### 标签约束

高基数字段 `job_id`、`trace_id` **不得**作为 metrics label（PRD 12.1 + code-standards）。

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
| `JOBFORGE_OTEL_EXPORTER` | `stdout` | Trace exporter（stdout / none） |
| `JOBFORGE_OTEL_SAMPLE_RATIO` | `1.0` | 采样率 [0.0, 1.0] |
| `JOBFORGE_METRICS_ADDR` | `127.0.0.1:6060` | Debug server 地址 |

## 安全约束（PRD 11.4）

- pprof、metrics 和管理端点只绑定内网或 localhost
- 日志不得记录完整敏感 payload、API key 或 Authorization header
- Span 属性不包含完整 payload
