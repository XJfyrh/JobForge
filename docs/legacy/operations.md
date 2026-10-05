# 旧 Job API：运行配置与运维

本页适用于 `cmd/jobforge` 的 jobs 服务。命令从仓库根目录执行；测试前置见[测试指南](../tests.md)，当前 Run 使用[独立指南](../agent-v3/runs.md)。

## 任务类型目录与 Worker 契约（PRD v0.5，ADR-0010）

API 与 Gateway 都必须设置相同的 `JOBFORGE_TASK_TYPES`。值为逗号分隔静态 allowlist，默认及 Compose 显式值为：

```text
demo.echo,demo.sleep,demo.fail,demo.idempotent_effect,demo.http,rag.index,agent.extract
```

空目录、空项、重复项或不匹配 `^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$` 的名称会使进程启动失败。启动日志中的 `catalog_size` / `catalog_sha256` 应在 API 与 Gateway 间一致。新增类型可以先发布目录再上线 Worker；移除类型前必须先停止提交、查询并 drain 该类型全部非终态任务，再依次更新 Worker、Gateway 与 API，避免合法存量任务失去消费者。

Register 要求非空 worker ID、正 capacity，以及非空无重复 queues/types；types 必须属于目录。Poll 的 queues/types 必须是登记子集，`max_jobs` 与 `available_capacity` 均在 1..registered capacity；服务端另以 `running+cancelling` 核算剩余 slot，migration 0019 的 `idx_jobs_owner_inflight` 只加速该 jobs 聚合，不以 `workers.inflight` 替代。Worker Proto v1 的失败状态附带 `DomainErrorDetail`，stock Runtime 优先读取 `retryable`，旧 Gateway status 回退仍受支持。该能力约束不是身份认证，Gateway 仍只能放在可信网络。

Windows 下定向验证：

```powershell
$env:JOBFORGE_TEST_DSN = "postgres://jobforge:jobforge@localhost:5433/jobforge?sslmode=disable"
go test -count=1 -run 'TestAT2(8|9)|TestAT30|TestAT31' ./tests/integration/
.\.tools\bin\buf.exe lint
.\.tools\bin\buf.exe breaking --against ".git#branch=main"
```

## Demo 持久业务效果（PRD v0.4，ADR-0009）

默认 `jobforge worker` 为 `demo.idempotent_effect` 与真实任务业务存储建立 PostgreSQL pool，因此默认 Worker 与 Gateway 一样需要 `JOBFORGE_DATABASE_URL`。Worker 不运行 migration；启动服务前由 `jobforge migrate`、API、Gateway 或 Scheduler 升级 schema：0018 持久效果表，0019 Poll 索引，0020 有界结果引用，0021 独立业务产物表，0022 对齐 C0/DEL 约束。核心 `internal/worker` Runtime 与自定义 Handler API 不依赖 PostgreSQL。

效果表查询：

```sql
select job_id, result_ref, applied_at
from demo_idempotent_effects
order by applied_at desc
limit 20;
```

`post_effect_delay_ms` 仅供 Demo/故障测试稳定制造效果提交后、Complete 前的窗口，范围 0～60,000ms，且只在首次 applied 后等待；重复投递立即返回。不得把该同库示例解释为跨系统 exactly-once。

## Heartbeat 与取消 SLO（PRD v0.3 M4）

| 环境变量 | 默认值 | 说明 |
|---|---:|---|
| `JOBFORGE_LEASE_TTL` | `30s` | job lease TTL；不因 M4 改变 |
| `JOBFORGE_HEARTBEAT_INTERVAL` | `5s` | Gateway 注册建议值；Worker 未显式配置时采用该值，Worker 显式本地配置优先 |

Compose 只在 Gateway 设置 heartbeat 值，Worker 通过 `RegisterResponse.heartbeat_interval` 采用建议值；不要在 Worker 环境中遗留旧的显式 `10s`，否则它会按设计覆盖 Gateway 建议。job lease 每次 heartbeat 都续租；`workers.last_heartbeat_at` 仍按 `LeaseTTL/3` 条件节流。默认变化与滚动升级/回退步骤见 [5s Heartbeat 发布说明](../runbooks/heartbeat-5s-rollout.md)。

AT-24 使用真实 PostgreSQL、HTTP Cancel、gRPC Gateway 和 Worker Runtime，20 个确定性随机相位样本验证 DB-clock signal p95≤6s，并单独报告 API→context 与 Handler stop：

```powershell
$env:JOBFORGE_TEST_DSN = "postgres://jobforge:jobforge@localhost:5433/jobforge?sslmode=disable"
go test -race -count=1 -v -run TestCancelAT24HeartbeatSignalSLO ./tests/integration/
go test -tags scale -count=1 -v -run TestScaleNFR306HeartbeatWriteAmplification ./tests/scale/
```

## 耐久事件与 Redis（Compose durable-events，PRD v0.3 §10.2）

外部事件 transport 由 `JOBFORGE_OUTBOX_TRANSPORT` 选择：默认 `notify`（v0.2 兼容，非耐久）；耐久交付需 `redis_streams` 与 Redis：

```powershell
# Windows/Docker Desktop：启动 PostgreSQL + AOF Redis（命名 volume）
docker compose -f deploy/compose.yaml --profile durable-events up -d postgres redis
$env:JOBFORGE_TEST_DSN = "postgres://jobforge:jobforge@localhost:5433/jobforge?sslmode=disable"
$env:JOBFORGE_TEST_REDIS_URL = "redis://localhost:6379/0"
```

- 耐久事件集成测试（AT-17/18/19/20、NFR-303，`tests/integration/durable_events_test.go` 与 `tests/integration/event_consumer_test.go`）在 `JOBFORGE_TEST_REDIS_URL` 缺省时 **skip 而非失败**；Linux CI 的 testcontainers 模式会自动拉起 AOF Redis。
- AT-17/NFR-303 通过 `docker stop/start` 重启 broker 验证 AOF 恢复，目标容器名由 `JOBFORGE_TEST_REDIS_CONTAINER` 指定（默认 `deploy-redis-1`）；重启必须保留命名 volume，禁止用 `down -v` 后把全新实例误报为重启恢复。
- 运行 publisher 子命令启用 redis_streams：设 `JOBFORGE_OUTBOX_TRANSPORT=redis_streams` 与 `JOBFORGE_REDIS_URL`（Compose 内默认指向 `redis://redis:6379/0`）；Redis URL 与认证信息不进入日志/trace/metrics（NFR-309）。

完整 reference consumer demo：

```powershell
$env:JOBFORGE_OUTBOX_TRANSPORT = "redis_streams"
docker compose -f deploy/compose.yaml --profile durable-events up -d --build
```

`consumer` 服务仅属于 durable-events profile；默认部署不依赖 Redis。Consumer 通过 `consumer_inbox_binding` 将数据库/schema 绑定到一个逻辑 group，并在 PostgreSQL 中执行 `consumer_inbox` + `consumer_demo_effects` 同事务，commit 后才 ACK。独立业务效果的其他 group 必须使用独立 schema/数据库；复用会在消费前 fail closed。

本地 Compose 把每个 Consumer 的 `/metrics` + pprof 映射到 loopback 随机宿主端口，避免 `--scale consumer=2` 冲突。用 `docker compose -f deploy/compose.yaml port --index 1 consumer 6060`（第二实例使用 `--index 2`）查询实际端口；生产仍应只绑定内网或 localhost。Compose 不覆盖 `JOBFORGE_CONSUMER_NAME`，各容器使用自身 `<hostname>-<pid>`。

| 环境变量 | 默认值 | 约束/说明 |
|---|---:|---|
| `JOBFORGE_CONSUMER_GROUP` | `jobforge-reference-v1` | 1～128 个安全字符；一个 inbox 对应一个逻辑 group |
| `JOBFORGE_CONSUMER_NAME` | `<hostname>-<pid>` | 1～128 个安全字符；同 group 实例应不同 |
| `JOBFORGE_CONSUMER_BLOCK_TIMEOUT` | `2s` | 单 entry 阻塞读取上限 |
| `JOBFORGE_CONSUMER_PENDING_SCAN_INTERVAL` | `5s` | PEL 扫描周期；与新消息读取交替 |
| `JOBFORGE_CONSUMER_PENDING_MIN_IDLE` | `30s` | 必须大于 process timeout |
| `JOBFORGE_CONSUMER_PROCESS_TIMEOUT` | `10s` | 单次 PostgreSQL 业务事务上限 |
| `JOBFORGE_CONSUMER_MAX_DELIVERIES` | `5` | 仅永久 decode/schema/Handler 错误消耗该上限 |
| `JOBFORGE_CONSUMER_RETRY_BASE` | `1s` | 瞬时基础设施错误退避起点 |
| `JOBFORGE_CONSUMER_RETRY_MAX` | `30s` | 瞬时错误退避上限 |
| `JOBFORGE_CONSUMER_POISON_STREAM` | `<event-stream>:poison` | 必须与源 stream 不同；只存有界元数据，不复制 payload |

`JOBFORGE_REDIS_STREAM_MAXLEN` 默认 0。非零值保持兼容，但 Redis 裁剪不保护 PEL；若尚未 ACK 的 payload 被删除，Consumer 会记录 `pending_payload_deleted` 并退出。生产启用裁剪前必须准备基于 PostgreSQL outbox 高水位的人工恢复方案，不能把退出后的 PEL=0 当作成功消费。

从 `notify` 切换到 Redis 不是自动双写或零停机升级；执行前阅读[事件 transport 切换运行手册](../runbooks/event-transport-switch.md)。

## 运维 CLI（jobforge ctl）

`jobforge ctl` 是纯客户端运维入口（PRD v0.2 FR-620/621），复用 HTTP API 与 Bearer API key 鉴权，不新增服务端特权路径：

```sh
# 任务查询与操作（需 API URL + key）
jobforge ctl list --state dead --queue default --limit 20 --output table
jobforge ctl get <job_id>            # 详情 + attempt 时间线
jobforge ctl cancel <job_id>
jobforge ctl retry <job_id>          # dead/cancelled 人工重试（克隆新 job_id）

# outbox 积压视图（只读，需数据库连接串，不走 HTTP API）
jobforge ctl outbox-status

# worker 注册表与存活状态（只读，需数据库连接串）
jobforge ctl workers-status [--stale-after 90s]

# 租户配额计数核对（PRD v0.3 FR-724，只读；--repair 以 jobs 聚合为事实源覆盖修复）
jobforge ctl quota-reconcile [--repair]
```

凭据与连接参数：

| 参数 | 环境变量 | 默认值 | 适用命令 |
|---|---|---|---|
| `--api-url` | `JOBFORGE_API_URL` | `http://localhost:8080` | list/get/cancel/retry |
| `--api-key` | `JOBFORGE_API_KEY` | 无（必填） | list/get/cancel/retry |
| `--output` | — | `table`（可选 `json`） | 全部 |
| — | `JOBFORGE_DATABASE_URL` | `postgres://jobforge:jobforge@localhost:5432/jobforge?sslmode=disable`（代码默认值） | outbox-status / workers-status |
| — | `JOBFORGE_TASK_TYPES` | rag.index、agent.extract 与五种诊断 Demo 类型 | API/Gateway 部署目录；两者必须同值 |
| `--stale-after` | — | `3×JOBFORGE_LEASE_TTL` | workers-status |
| `--repair` | — | `false` | quota-reconcile |
| — | `JOBFORGE_TENANT_QUOTA_PREFILTER` | `true` | 服务配置：Claim 候选预筛开关（关闭仅损失公平性性能，硬上限不受影响，ADR-0007 §4） |
| — | `JOBFORGE_QUOTA_RECONCILE_INTERVAL` | `5m` | 服务配置：Scheduler leader 配额核对周期（≤0 关闭） |

table 视图不输出完整 payload；CLI 日志不记录 API key、Authorization header（PRD v0.2 NFR-205）。`retry` 克隆新 job_id 并记录 `retry_of_job_id`，原任务终态不可变（AT-16）。

注意：Compose 将宿主机端口映射为 `5433`（5433），因此对本地 Compose 库运行 `outbox-status`/`workers-status` 时需显式设置 `JOBFORGE_DATABASE_URL=postgres://jobforge:jobforge@localhost:5433/jobforge?sslmode=disable`（与集成测试的 `JOBFORGE_TEST_DSN` 相同连接串）。

## 可选观测 profile（Compose obs）

默认 `docker compose up` 不启动观测组件（ADR-0004 零外部依赖默认，行为不变）。如需在 Compose 环境查看 jobforge_* 指标曲线（PRD v0.2 FR-632），使用可选 obs profile：

```sh
docker compose -f deploy/compose.yaml --profile obs up -d
```

obs profile 额外拉起：

| 服务 | 宿主机端口 | 说明 |
|---|---|---|
| prometheus | 9091（容器内 9090；9090 已被 gateway gRPC 占用） | 抓取七个服务的 `:6060/metrics`，配置见 `deploy/prometheus/prometheus.yml` |
| grafana | 3000 | 预置任务仪表盘与 Prometheus / Jaeger 数据源（admin/jobforge，仅本地演示） |
| otel-collector | 4318（localhost） | OTLP/HTTP 接收与有界批量转发 |
| jaeger | 16686（localhost） | Trace 查询；开发内存存储，重启清空 |

验证：Prometheus targets 页（http://localhost:9091/targets）七个 jobforge-* 目标应为 UP。需导出 Trace 时额外设置 `JOBFORGE_OTEL_EXPORTER=otlp`，主机 Python 设置 `OTEL_EXPORTER_OTLP_ENDPOINT=http://localhost:4318`。完整命令、故障脚本、指标口径与仪表盘见[可观测性指南](observability.md#agentrag-本地观测闭环)。Grafana 文件 provider 每 20s 轮询，兼容 Docker Desktop 挂载。仅停止观测组件：`docker compose -f deploy/compose.yaml --profile obs stop prometheus grafana otel-collector jaeger`。
