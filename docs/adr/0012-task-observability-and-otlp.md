# ADR-0012：任务完成口径与可选 OTLP 观测链路

- 状态：Proposed（维护者已授权范围的实现候选，PR 评审合并后转 Accepted）
- 日期：2026-09-14
- 关联：PRD v0.6 AT-40～42，补充 ADR-0004、0011

## 背景

Complete/Fail 原指标缺少 queue/type，重复 RPC 和取消被当成新结果；Scheduler 租约恢复也使用空 queue，积压 gauge 没有清零消失的分组。stdout Trace 不能完成后端关联查询。需要保持任务事务独立于观测可用性。

## 决策

1. `AttemptResult.Changed` 和恢复事务返回的逐任务元数据是计数依据。只在事务提交后记录；重复 ACK、失败事务、陈旧 lease 都不增加 attempt/重试/DLQ。尝试 outcome 与 `job_attempts` 一致：succeeded、failed_retry、failed_dead、cancelled、lease_expired。取消过期后的 state 是 cancelled，其 attempt outcome 仍为 lease_expired，单独展示恢复结果。
2. `jobforge_job_latency_seconds` 保留兼容名称，明确只表示 Worker 实测执行时间，包含失败/取消已上报的持续时间，不是排队或端到端时间。租约过期没有可信 Worker 时长，不填充该 histogram。重试仅统计真实 retry_wait；恢复重投单独计 lease_expired。提交计数仅统计新 job（包括人工克隆），幂等重复不计。
3. lease_expired 增加真实 queue/type/resolution；retries 增加 type，error_code 使用固定类别白名单，其余 OTHER。job_id、业务键、模型输入输出不进 metrics label。Scheduler 按上一轮采样清零消失的积压分组，观测失败不覆盖已知值。
4. 使用与现有 Go SDK 同版本的 OTLP HTTP exporter v1.44.0；有界异步 batch queue、短 export timeout，无 WithBlocking。只通过标准 OTEL_EXPORTER_OTLP_* 配置端点和可选凭据，配置值不写日志。stdout/none 默认行为保留；传播固定 W3C TraceContext，禁用业务 Baggage 转发，none 模式仍能传递已有 TraceContext。
5. 可选 Compose obs profile：Collector contrib 0.160.0 接收 OTLP，转发 Jaeger 2.20.0；Jaeger 本地 all-in-one 内存存储便于复现，重启丢 Trace 的边界明确披露。Prometheus/Grafana 保留，增加受版本控制的仪表盘/告警。进程 service.name 区分 API、Gateway、Scheduler、Worker、artifacts；Python 演示可选导出 OTLP。
6. 提交、领取、Worker 执行、模型适配、产物发布、Complete/Fail 在同一 job trace 下；人工 retry 创建新提交 span 并链接原任务 trace。恢复 span 从持久化 TraceContext 建立，附 job_id/attempt/token。进程被强杀可能丢最后一批尚未导出的 Worker span，仍由已导出的 Gateway / Scheduler / 后继 Worker 关联，不承诺崩溃进程完整 span。
7. Metrics 是进程计数，进程 crash 与事务提交之间可能漏报，不能替代 PostgreSQL 审计。Trace/metrics 不承诺事务性持久交付；Collector/Jaeger 停机只影响观测。实际停止后端并执行两个真实任务验证；告警在 Prometheus 中评估，不隐式配置外部通知接收者。

## 验证与替代方案

- 真实 PostgreSQL 验证完成、重复、取消、旧 token、重试耗尽、恢复，核对 metrics 与 attempt/state；积压归零和未知错误标签有定向测试。
- 真实 SDK + 模型场景查询 Jaeger、Prometheus、Grafana，检查原始导出数据不含文档、输出或凭据；实际故障验证告警触发与恢复。
- 不引入独立持久观测队列，也不在业务事务中同步 exporter；这些方案会把监控可用性引入任务可靠性边界。本地 trace 存储不作为生产容量/留存设计。

依据：[OTel Go exporter](https://opentelemetry.io/docs/languages/go/exporters/)、[Collector configuration](https://opentelemetry.io/docs/collector/configuration/)、[Jaeger 2.20 all-in-one](https://www.jaegertracing.io/docs/2.20/getting-started/)。
