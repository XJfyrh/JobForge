# JobForge

JobForge 是一个面向 Agent、RAG 与通用后台任务的分布式任务编排平台，目标是提供可恢复、可观测、可隔离的可靠执行底座。

> 当前状态：S0–W7 全部完成。核心任务生命周期、租约领取、HTTP API、Scheduler、gRPC Worker Gateway、Worker Runtime、性能基准、租户隔离、Python SDK、Agent Demo、OpenTelemetry 全链路 Trace、Prometheus 指标、pprof 诊断、故障注入测试和演示脚本均已可用。

## 项目亮点

| 实现证据 | 可证明的能力 |
|---|---|
| goroutine pool、context、race、泄漏测试 | Go 并发与资源生命周期 |
| `SKIP LOCKED`、lease、索引与事务测试 | PostgreSQL 并发控制与队列设计 |
| gRPC Worker 协议与 fencing token | 跨进程协议、超时与陈旧写保护 |
| 重试、DLQ、幂等、故障注入 | 分布式系统可靠性与工程判断 |
| advisory-lock Scheduler 切换 | 高可用调度与边界控制 |
| 租户配额、限流、背压 | 平台治理与故障隔离 |
| OTel、pprof、benchmark | 可观测性与性能分析 |
| PageWise/Agent 真实接入 | Go 后端能力与 Agent 应用场景结合 |

## 核心边界

- 交付语义是 **at-least-once**，不宣称 exactly-once；外部副作用由业务幂等保护。
- PostgreSQL 是 P0 的唯一事实源，任务状态、租约和 attempt 必须在数据库事务中保持一致。
- Worker 的 Heartbeat、Complete 与 Fail 必须校验 owner 和 fencing token，拒绝陈旧写入。
- 先交付 lease、重试、DLQ、取消、崩溃恢复和故障验证，再考虑完整 UI 或事件扩展。
- 只运行预注册 Handler，不允许上传或执行任意代码。

## 已实现的组成

- **Go 控制面**：HTTP API（创建、查询、取消、手动重试任务）
- **Scheduler**：扫描推进（scheduled/retry_wait → ready）与租约回收（过期 running → ready）
- **gRPC Worker Gateway**：Register、Poll、Heartbeat、Complete、Fail RPC
- **Worker Runtime**：并发任务执行、心跳维持、优雅关闭
- **PostgreSQL 队列内核**：versioned migrations、FOR UPDATE SKIP LOCKED 领取、fencing token
- **事件通知**：PostgreSQL LISTEN/NOTIFY fan-out 广播（ADR-0003）
- **租户隔离**：租户并发上限（FR-302）、队列背压（FR-303）
- **Python SDK**：submit/get/cancel/retry（FR-401）
- **Agent Demo**：demo.echo/sleep/fail/http/idempotent_effect、pagewise.reindex Handler
- **OpenTelemetry Trace**：http.submit_job / gateway.claim_jobs / worker.execute / gateway.complete_job 全链路 span
- **Prometheus 指标**：10 个核心指标（PRD 12.1），通过 /metrics 端点暴露
- **pprof 诊断**：CPU/heap/goroutine 剖析，绑定 localhost:6060
- **故障注入测试**：AT-01～AT-12 全覆盖（崩溃恢复、STALE_LEASE、Scheduler 切换、租户隔离）
- **性能基准**：微基准 + 端到端，W4 冻结基线 + W7 最终验证

## 快速开始

```sh
# 一条命令启动全栈（PostgreSQL + API + Scheduler + Gateway + 2 Worker）
docker compose -f deploy/compose.yaml up -d --build

# 提交第一个任务
curl -s -X POST http://localhost:8080/v1/jobs \
  -H "Content-Type: application/json" \
  -H "X-API-Key: dev-api-key" \
  -d '{"queue":"default","type":"demo.echo","payload":{"message":"hello"}}'

# 查询任务状态
curl -s http://localhost:8080/v1/jobs/{job_id} -H "X-API-Key: dev-api-key"

# 查看指标
curl -s http://localhost:6060/metrics | grep jobforge_
```

服务默认监听：
- HTTP API: `:8080`
- gRPC Worker Gateway: `:9090`
- pprof + /metrics: `:6060`（仅 localhost）

完整演示流程见 [三分钟演示脚本](docs/demo-script.md)。

## 文档导航

| 文档 | 用途 |
|---|---|
| [产品需求文档](docs/product/JobForge_PRD_v0.1.md) | 产品边界、状态机、接口、验收标准与里程碑 |
| [系统架构](docs/architecture.md) | 架构图、组件职责、状态机、部署拓扑 |
| [故障语义](docs/failure-semantics.md) | 故障模型、故障矩阵、恢复路径与测试证据 |
| [可观测性](docs/observability.md) | Trace、Metrics、pprof 使用说明 |
| [性能基线](docs/benchmark.md) | W4 冻结基线 + W7 最终发布数据 |
| [演示脚本](docs/demo-script.md) | 3 分钟可复现演示（PRD 18） |
| [代码与注释规范](docs/code-standards.md) | Go、Python、SQL、Proto、测试和注释约定 |
| [开发环境](docs/development.md) | 本地检查工具、安装方式与验证命令 |
| [ADR 说明](docs/adr/README.md) | 架构决策的创建、接受和取代流程 |
| [贡献指南](CONTRIBUTING.md) | 分支、提交、评审与合并要求 |
| [安全策略](SECURITY.md) | 漏洞报告方式与敏感信息要求 |

## 协作方式

仓库采用 GitHub Flow 与 Conventional Commits。开始修改前请阅读 [CONTRIBUTING.md](CONTRIBUTING.md)；改变可靠性语义、公开接口或关键技术边界时，必须先提交 ADR，并同步更新 PRD、测试和相关文档。

## 许可证

本仓库当前未提供开源许可证。除适用法律另有规定或权利人另行书面授权外，保留全部权利，不授予复制、修改、分发或使用本项目的许可。
