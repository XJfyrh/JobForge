# ADR-0004：可观测性技术选型（OpenTelemetry + Prometheus + pprof）

- 状态：Accepted
- 日期：2026-07-29
- 决策者：XJfyrh
- 关联 Issue/PR：W6 观测与事件扩展
- 取代：无
- 被取代：无

## 上下文

W6 阶段需要实现完整的可观测性体系（PRD 第 12 节），包括：
- 分布式 Trace（PRD 12.2：5 个最少 span）
- Prometheus 指标（PRD 12.1：10 个核心指标）
- 性能剖析（pprof）

W5 已实现 trace_id 日志传播（X-Trace-ID header → job.TraceID → Worker 日志），W6 需在此基础上扩展为完整 OTel span 链路，同时保持向后兼容。

## 决策驱动因素

- 零外部依赖开发体验：P0 不要求部署 Jaeger/Tempo/Prometheus Server。
- 标准化传播：使用 W3C TraceContext 作为跨进程 trace 传播格式。
- 向后兼容：W5 的 X-Trace-ID 和 job.TraceID 字段继续工作。
- 最小侵入：domain 层不依赖 OTel 类型；观测逻辑在 transport/infrastructure 层。
- 安全约束：pprof 和 metrics 端点仅绑定 localhost（PRD 11.4）。

## 决策

### Trace SDK

- 使用 OpenTelemetry Go SDK（`go.opentelemetry.io/otel` v1.44+）。
- P0 默认 stdout exporter（JSON 输出到 stderr），无需外部 collector。
- 代码预留 OTLP exporter 切换能力（通过 `JOBFORGE_OTEL_EXPORTER` 环境变量）。
- 采样策略：`JOBFORGE_OTEL_SAMPLE_RATIO`（默认 1.0 全采样）。

### Trace 传播

- HTTP 层：W3C `traceparent` header（OTel 自动提取/注入）。
- 跨进程（API → Worker）：通过 job 表 `trace_context` 列持久化序列化的 TraceContext。
- 向后兼容：`X-Trace-ID` header 继续支持；`trace_id` 字段保留用于日志关联。

### Metrics

- 使用 OTel MeterProvider + Prometheus exporter（`go.opentelemetry.io/otel/exporters/prometheus`）。
- 指标通过 `/metrics` 端点暴露（绑定 debug 端口 127.0.0.1:6060）。
- 高基数字段（job_id、trace_id）不作为 metric label（PRD 12.1 + code-standards）。

### pprof

- 使用标准库 `net/http/pprof`，注册在独立 HTTP mux 上。
- 与 /metrics 共享 debug 端口（默认 127.0.0.1:6060）。
- 不暴露到外网（PRD 11.4）。

### 架构约束

- `internal/observability` 包封装所有 SDK 初始化。
- domain 层不引入 OTel 类型（code-standards 依赖方向）。
- span 必须在所有路径关闭（`defer span.End()`）。

## 公开契约与数据影响

- 新增 migration `0006_add_trace_context`：jobs 表增加 `trace_context text` 列（nullable）。
- 无公开 API 变更。
- 新增环境变量：`JOBFORGE_OTEL_EXPORTER`、`JOBFORGE_OTEL_SAMPLE_RATIO`、`JOBFORGE_METRICS_ADDR`。

## 后果

### 正面

- 完整 trace 链路：http.submit_job → gateway.claim_jobs → worker.execute → gateway.complete_job。
- 10 个 PRD 指标全部可采集。
- pprof 支持 CPU/heap/goroutine 剖析。
- 零外部依赖即可开发调试。

### 负面与成本

- stdout exporter 在高吞吐时产生大量 JSON 输出（生产应切换 OTLP 或降低采样率）。
- OTel SDK 增加约 5 个直接依赖。
- trace_context 列增加少量存储开销。

## 考虑过的替代方案

### 仅日志 trace_id（W5 方案）

不引入 OTel SDK，继续用 trace_id 字符串关联日志。无法生成 span 时间线、无法可视化调用链、无法满足 PRD 12.2 的 span 要求。不采用。

### 直接使用 Jaeger client

Jaeger 已弃用独立 client，推荐迁移到 OTel。不采用。

### OpenCensus

已被 OpenTelemetry 取代，社区不再活跃。不采用。

## 验证与退出条件

- AT-12 集成测试：4 个 span 共享 trace 且属性完整。
- `curl localhost:6060/metrics` 返回 10 个 jobforge_ 前缀指标。
- `go tool pprof http://localhost:6060/debug/pprof/profile?seconds=5` 可采集 CPU profile。
- `go test -race ./...` 通过。
