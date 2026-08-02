# JobForge 系统架构

本文档描述 JobForge 分布式任务编排平台的系统架构。产品边界与验收语义见 [PRD](product/JobForge_PRD_v0.1.md)，架构决策见 [ADR 目录](adr/README.md)。

## 架构概览

```mermaid
flowchart LR
    Client[应用 / Python SDK] -->|HTTP| API[API Server]
    API --> PG[(PostgreSQL)]
    Scheduler[Scheduler] --> PG
    Worker1[Worker 1] -->|gRPC| Gateway[Worker Gateway]
    Worker2[Worker 2] -->|gRPC| Gateway
    Gateway --> PG
    Scheduler -.->|LISTEN/NOTIFY| Gateway
    API -.->|LISTEN/NOTIFY| Gateway
```

**核心约束**：

- 交付语义为 **at-least-once**，不承诺 exactly-once；外部副作用由业务幂等保护。
- PostgreSQL 是 P0 唯一事实源；任务状态、租约和 attempt 在数据库事务中保持一致。
- 只运行预注册 Handler，不允许上传或执行任意代码。

## 组件职责

| 组件 | 职责 | 不负责 |
|---|---|---|
| API Server | 提交、查询、取消、人工重试；API key 鉴权；参数校验；队列背压 | 执行业务任务 |
| Scheduler | scheduled/retry_wait → ready 推进；过期 lease 回收；advisory-lock 单活 | 存储业务结果 |
| Worker Gateway | gRPC Worker 会话管理；Poll/Heartbeat/Complete/Fail 事务；fencing 验证 | 运行用户 Handler |
| Worker Runtime | 并发池执行；Handler 注册；context/deadline 传播；心跳维持；优雅退出 | 决定任务最终状态 |
| PostgreSQL | 任务状态、租约、attempt、Worker、outbox 的唯一事实源 | 执行任意业务代码 |
| Observability | OTel tracing；Prometheus metrics；pprof 诊断 | 业务逻辑 |

## 数据流

### 任务提交与执行

```text
1. Client → POST /v1/jobs → API Server
2. API Server → INSERT jobs (state=ready/scheduled) → PostgreSQL
3. API Server → NOTIFY jobforge_events → PostgreSQL
4. Scheduler → 扫描 scheduled/retry_wait 到期 → UPDATE state=ready
5. Worker → Poll RPC → Gateway → SELECT FOR UPDATE SKIP LOCKED → PostgreSQL
6. Gateway → 返回 ClaimedJob (lease + fencing token) → Worker
7. Worker → 执行 Handler → 外部副作用
8. Worker → Heartbeat RPC (周期性) → Gateway → UPDATE lease_until
9. Worker → Complete/Fail RPC → Gateway → UPDATE state + INSERT job_attempts
```

### 故障恢复流

```text
1. Worker 崩溃 → 心跳停止 → lease 过期
2. Scheduler → 扫描 running WHERE lease_until < now() → UPDATE state=ready
3. 新 Worker → Poll → 重新领取 (fencing_token 递增)
4. 旧 Worker 恢复 → Complete with old token → Gateway 返回 STALE_LEASE
```

## 状态机

```mermaid
stateDiagram-v2
    [*] --> scheduled: run_at > now
    [*] --> ready: run_at <= now
    scheduled --> ready: 到达 run_at
    ready --> running: claim + lease
    running --> succeeded: complete
    running --> retry_wait: retryable fail
    running --> dead: fatal fail / attempts exhausted
    running --> ready: lease expired
    retry_wait --> ready: backoff 到期
    scheduled --> cancelled: cancel
    ready --> cancelled: cancel
    retry_wait --> cancelled: cancel
    running --> cancelling: cancel request
    cancelling --> cancelled: worker ack / lease expiry
```

**终态**：succeeded、dead、cancelled。终态不可逆转。

**并发规则**：

- Claim 事务使用 `FOR UPDATE SKIP LOCKED`，原子更新 lease_owner、lease_until、attempt、fencing_token、state。
- Heartbeat/Complete/Fail 必须匹配 job_id + lease_owner + fencing_token + 允许的当前状态。
- Complete 与 Cancel 竞争采用"事务先提交者生效"规则。

## 部署拓扑

JobForge 使用单二进制多子命令模式，避免过早拆分微服务：

```text
┌─────────────────────────────────────────────────────────┐
│                    deploy/compose.yaml                    │
├─────────────────────────────────────────────────────────┤
│                                                          │
│  ┌──────────┐  ┌───────────┐  ┌─────────┐  ┌────────┐ │
│  │ postgres │  │    api    │  │scheduler│  │gateway │ │
│  │  :5432   │  │   :8080   │  │         │  │ :9090  │ │
│  └──────────┘  └───────────┘  └─────────┘  └────────┘ │
│                                                          │
│  ┌──────────┐  ┌──────────┐                             │
│  │ worker-1 │  │ worker-2 │   (独立进程，可水平扩展)     │
│  └──────────┘  └──────────┘                             │
│                                                          │
└─────────────────────────────────────────────────────────┘
```

- **API / Scheduler / Gateway**：同一 Go 二进制的不同子命令（`jobforge api`、`jobforge scheduler`、`jobforge gateway`）。
- **Worker**：独立进程，通过 gRPC 连接 Gateway，可启动多个实例。
- **PostgreSQL**：唯一强依赖外部服务。
- **观测**：pprof + /metrics 绑定 `127.0.0.1:6060`（PRD 11.4）；OTel stdout exporter 默认零外部依赖。

## 关键设计决策

| ADR | 决策 | 核心要点 |
|---|---|---|
| [ADR-0001](adr/0001-implementation-parameters.md) | 实现参数 | lease TTL 30s、heartbeat 10s、scan 1s；unary long-poll；人工重试克隆新 job_id |
| [ADR-0002](adr/0002-error-classification.md) | 错误分类 | 4 类错误（client/server/business/transient）→ HTTP/gRPC 映射 |
| [ADR-0003](adr/0003-event-notification.md) | 事件通知 | PostgreSQL LISTEN/NOTIFY fan-out；不引入外部 MQ |
| [ADR-0004](adr/0004-observability-stack.md) | 可观测性 | OTel SDK + stdout exporter；Prometheus /metrics；pprof localhost |

## 目录结构

```text
jobforge/
├── cmd/jobforge/           # 单二进制入口（api/scheduler/gateway/worker/migrate 子命令）
├── internal/
│   ├── api/http/           # HTTP 控制面 API（chi router）
│   ├── config/             # 环境变量配置
│   ├── domain/             # 领域模型、状态机、错误定义
│   ├── gateway/grpc/       # gRPC Worker Gateway
│   ├── migrate/            # 自动迁移（embed SQL）
│   ├── notify/             # LISTEN/NOTIFY fan-out
│   ├── observability/      # OTel tracing + Prometheus metrics + pprof
│   ├── scheduler/          # 调度器（promote + recover 循环）
│   ├── store/postgres/     # PostgreSQL 存储层（pgx v5）
│   └── worker/             # Worker Runtime + demo handlers
├── proto/jobforge/worker/v1/  # gRPC proto 定义
├── sdk/python/             # Python SDK（httpx）
├── migrations/             # Versioned SQL migrations
├── tests/integration/      # 集成测试 + 故障注入测试
├── benchmarks/             # 微基准 + 端到端基准
├── deploy/                 # Docker Compose + Dockerfile
└── docs/                   # 文档（本文件、PRD、ADR、规范）
```

## 技术栈

| 层级 | 技术 |
|---|---|
| 语言 | Go 1.26 |
| HTTP | go-chi/chi v5 |
| gRPC | google.golang.org/grpc + protobuf |
| 数据库 | PostgreSQL 16 + jackc/pgx v5 |
| Proto 管理 | buf v2 |
| 可观测性 | OpenTelemetry SDK + Prometheus client_golang |
| Python SDK | httpx |
| 容器化 | Docker + Docker Compose |
| Lint | golangci-lint + ruff (Python) |

## 相关文档

- [故障语义与恢复](failure-semantics.md) — 故障模型、故障矩阵和恢复路径
- [可观测性体系](observability.md) — Trace、Metrics、pprof 使用说明
- [性能基线](benchmark.md) — W4 冻结基线 + 最终发布数据
- [演示脚本](demo-script.md) — 3 分钟可复现演示
- [开发环境](development.md) — 本地开发、测试和检查命令
