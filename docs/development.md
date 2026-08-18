# 开发环境与本地检查

本页记录 JobForge 的开发环境设置、常用检查命令和测试分层。项目使用 Go 1.26 和 PostgreSQL 16。

## 项目结构

```text
jobforge/
├── cmd/jobforge/       # 服务入口
├── internal/
│   ├── api/http/       # HTTP 控制面 API
│   ├── config/         # 环境变量配置
│   ├── domain/         # 领域模型与状态机
│   ├── eventconsumer/  # Redis Source + inbox 事务 + pending/poison 恢复
│   ├── gateway/grpc/   # gRPC Worker Gateway
│   ├── migrate/        # 自动迁移
│   ├── notify/         # LISTEN/NOTIFY fan-out
│   ├── observability/  # OTel tracing + Prometheus metrics + pprof
│   ├── scheduler/      # 调度器
│   ├── store/postgres/ # PostgreSQL 存储层
│   └── worker/         # Worker Runtime
├── migrations/         # 版本化 SQL 迁移
├── proto/              # Protobuf 契约
├── sdk/python/         # Python SDK
├── tests/integration/  # 集成测试
├── tests/scale/        # scale 可靠性套件（-tags scale，AT-13/AT-14）
└── deploy/             # Docker Compose
```

## 已验证环境

| 工具 | 版本 | 安装位置/来源 |
|---|---:|---|
| Go | 1.26.5 | 本机环境，仅作为首次验证记录 |
| Python | 3.12.10；v0.5 门禁另验证 3.14.7 | 本机环境记录；检查工具版本由锁文件固定 |
| golangci-lint | 2.12.2 | `.tools/bin`，官方 Windows amd64 发布包 |
| Buf | 1.72.0 | `.tools/bin`，官方 Windows amd64 发布包 |
| Ruff | 0.16.0 | `.venv`，由 `tools/requirements-lint.txt` 锁定 |
| mypy | 2.3.0 | `.venv`，由 `tools/requirements-lint.txt` 锁定 |
| SQLFluff | 4.2.2 | `.venv`，由 `tools/requirements-lint.txt` 锁定 |
| httpx | 0.28.1 | `.venv`，由 `tools/requirements-lint.txt` 锁定，供 mypy 解析 SDK 依赖 |
| pytest | 9.1.1 | `.venv`，由 `tools/requirements-lint.txt` 锁定，供 mypy 解析 SDK 测试 |

`.venv` 和 `.tools` 都被 Git 忽略。本地安装不会写入系统 PATH，也不应被提交。

## Python 检查工具

Windows PowerShell：

```powershell
python -m venv .venv
.\.venv\Scripts\python.exe -m pip install --requirement .\tools\requirements-lint.txt
```

macOS/Linux：

```sh
python3 -m venv .venv
.venv/bin/python -m pip install --requirement tools/requirements-lint.txt
```

升级工具时先确认新版本支持项目 Python 基线，再更新 requirements、本文版本表和配置，且在同一 Pull Request 中运行完整检查。

`.sqlfluffignore` 是已应用 migration 的冻结历史基线，其有效条目由 `tools/check_sqlfluff_baseline.py` 精确校验。不得改写这些历史 migration，也不得把新 migration 加入 ignore；新 SQL 必须直接通过全量 `sqlfluff lint migrations`。

## Go 与 Proto 检查工具

golangci-lint 和 Buf 使用官方 release 二进制。下载与当前平台匹配的稳定版本，核对发布页 checksum 后放入 `.tools/bin`。不要把该目录加入全局 PATH；从仓库根目录显式调用。

Windows：

```powershell
.\.tools\bin\golangci-lint.exe config verify
.\.tools\bin\buf.exe lint
```

macOS/Linux：

```sh
.tools/bin/golangci-lint config verify
.tools/bin/buf lint
```

## 本地 PostgreSQL

集成测试需要真实 PostgreSQL。使用 Docker Compose 启动：

```sh
docker compose -f deploy/compose.yaml up -d
```

Compose 将宿主机端口 `5433` 映射到容器内 PostgreSQL 的 `5432`，因此从宿主机运行测试时使用：

```text
postgres://jobforge:jobforge@localhost:5433/jobforge?sslmode=disable
```

以上账号密码仅用于本地开发与演示，与 `deploy/compose.yaml` 中的配置一致，不代表任何真实环境凭据。集成测试可通过 `JOBFORGE_TEST_DSN` 环境变量覆盖连接串；未设置时（如 Linux CI）会自动使用 testcontainers 启动临时 PostgreSQL。

## 任务类型目录与 Worker 契约（PRD v0.5，ADR-0010）

API 与 Gateway 都必须设置相同的 `JOBFORGE_TASK_TYPES`。值为逗号分隔静态 allowlist，默认及 Compose 显式值为：

```text
demo.echo,demo.sleep,demo.fail,demo.idempotent_effect,demo.http,pagewise.reindex
```

空目录、空项、重复项或不匹配 `^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$` 的名称会使进程启动失败。启动日志中的 `catalog_size` / `catalog_sha256` 应在 API 与 Gateway 间一致。新增类型可以先发布目录再上线 Worker；移除类型前必须先停止提交、查询并 drain 该类型全部非终态任务，再依次更新 Worker、Gateway 与 API，避免合法存量任务失去消费者。

Register 要求非空 worker ID、正 capacity，以及非空无重复 queues/types；types 必须属于目录。Poll 的 queues/types 必须是登记子集，`max_jobs` 与 `available_capacity` 均在 1..registered capacity；服务端另以 `running+cancelling` 核算剩余 slot。Worker Proto v1 的失败状态附带 `DomainErrorDetail`，stock Runtime 优先读取 `retryable`，旧 Gateway status 回退仍受支持。该能力约束不是身份认证，Gateway 仍只能放在可信网络。

Windows 下定向验证：

```powershell
$env:JOBFORGE_TEST_DSN = "postgres://jobforge:jobforge@localhost:5433/jobforge?sslmode=disable"
go test -count=1 -run 'TestAT2(8|9)|TestAT30|TestAT31' ./tests/integration/
.\.tools\bin\buf.exe lint
.\.tools\bin\buf.exe breaking --against ".git#branch=main"
```

## Demo 持久业务效果（PRD v0.4，ADR-0009）

默认 `jobforge worker` 为 `demo.idempotent_effect` 建立 `MaxConns=2` 的 PostgreSQL pool，因此 Demo Worker 与 Gateway 一样需要 `JOBFORGE_DATABASE_URL`。Worker 不运行 migration；启动 Worker 前由 `jobforge migrate`、API、Gateway 或 Scheduler 将 schema 升级到 0018。核心 `internal/worker` Runtime 与自定义 Handler API 不依赖 PostgreSQL。

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

Compose 只在 Gateway 设置 heartbeat 值，Worker 通过 `RegisterResponse.heartbeat_interval` 采用建议值；不要在 Worker 环境中遗留旧的显式 `10s`，否则它会按设计覆盖 Gateway 建议。job lease 每次 heartbeat 都续租；`workers.last_heartbeat_at` 仍按 `LeaseTTL/3` 条件节流。默认变化与滚动升级/回退步骤见 [5s Heartbeat 发布说明](runbooks/heartbeat-5s-rollout.md)。

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

从 `notify` 切换到 Redis 不是自动双写或零停机升级；执行前阅读[事件 transport 切换运行手册](runbooks/event-transport-switch.md)。

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
| — | `JOBFORGE_TASK_TYPES` | 六种内置 Demo/PageWise 类型 | API/Gateway 部署目录；两者必须同值 |
| `--stale-after` | — | `3×JOBFORGE_LEASE_TTL` | workers-status |
| `--repair` | — | `false` | quota-reconcile |
| — | `JOBFORGE_TENANT_QUOTA_PREFILTER` | `true` | 服务配置：Claim 候选预筛开关（关闭仅损失公平性性能，硬上限不受影响，ADR-0007 §4） |
| — | `JOBFORGE_QUOTA_RECONCILE_INTERVAL` | `5m` | 服务配置：Scheduler leader 配额核对周期（≤0 关闭） |

table 视图不输出完整 payload；CLI 日志不记录 API key、Authorization header（PRD v0.2 NFR-205）。`retry` 克隆新 job_id 并记录 `retry_of_job_id`，原任务终态不可变（AT-16）。

注意：Compose 将宿主机端口映射为 `5433`（见上文“本地 PostgreSQL”），因此对本地 Compose 库运行 `outbox-status`/`workers-status` 时需显式设置 `JOBFORGE_DATABASE_URL=postgres://jobforge:jobforge@localhost:5433/jobforge?sslmode=disable`（与集成测试的 `JOBFORGE_TEST_DSN` 相同连接串）。

## 可选观测 profile（Compose obs）

默认 `docker compose up` 不启动观测组件（ADR-0004 零外部依赖默认，行为不变）。如需在 Compose 环境查看 jobforge_* 指标曲线（PRD v0.2 FR-632），使用可选 obs profile：

```sh
docker compose -f deploy/compose.yaml --profile obs up -d
```

obs profile 额外拉起：

| 服务 | 宿主机端口 | 说明 |
|---|---|---|
| prometheus | 9091（容器内 9090；9090 已被 gateway gRPC 占用） | 抓取六个服务的 `:6060/metrics`，配置见 `deploy/prometheus/prometheus.yml` |
| grafana | 3000 | 自动 provision Prometheus 数据源（admin/jobforge，仅本地演示），通过 Explore 查询 jobforge_* 指标 |

验证：Prometheus targets 页（http://localhost:9091/targets）六个 jobforge-* 目标应为 UP；PromQL 示例 `jobforge_jobs_submitted_total`、`jobforge_outbox_pending`。仅停止观测组件：`docker compose -f deploy/compose.yaml --profile obs stop prometheus grafana`。

## 常用检查

提交前按改动范围运行：

```text
go build ./...
go test -race ./...
go vet ./...
.tools/bin/golangci-lint run
.venv/bin/ruff check .
.venv/bin/ruff format --check .
.venv/bin/mypy sdk/python
.venv/bin/python tools/check_sqlfluff_baseline.py
.venv/bin/sqlfluff lint migrations
.tools/bin/buf lint
.tools/bin/buf breaking --against '.git#branch=main'
```

Windows 将 `.venv/bin` 替换为 `.venv\Scripts`，将无扩展名工具替换为 `.exe`。目录尚未创建时应标记检查“未适用”，不能把“没有输入文件”报告成产品代码通过。

集成测试需要 PostgreSQL 运行中：

```sh
go test -race ./tests/integration/...
```

## Scale 可靠性套件（-tags scale）

`tests/scale/` 以 PRD v0.1 NFR-001/002 字面规模验证可靠性（PRD v0.2 FR-601/602/603，PRD v0.4 FR-806，v0.5 NFR-504/505）：AT-13 每轮启动真实 Worker helper 子进程，在持久效果行作为数据库屏障后调用 OS Kill 并 Wait，默认 100 轮零静默丢失；AT-14 让 10,000 个任务各发生一次重投，以 `demo_idempotent_effects` 主键验证表恰 10,000 行、零重复效果。NFR-306 另以真实 PostgreSQL 量化 5s heartbeat 相对 10s 基线的 job lease 写放大；20k Claim 与多租户公平性守护执行契约改造不影响既有热路径。套件通过 build tag `scale` 与默认测试物理隔离：默认 `go test ./...` 与 CI 均不执行，默认套件时长不受影响（FR-603）。

```sh
# 字面规模运行（需 PostgreSQL；Windows 先设 JOBFORGE_TEST_DSN，见上文）
go test -tags scale -count=1 -timeout 60m ./tests/scale/

# 降采样冒烟（开发反馈循环）
set JOBFORGE_SCALE_KILL_ROUNDS=5
set JOBFORGE_SCALE_IDEMPOTENT_JOBS=200
go test -tags scale -count=1 ./tests/scale/
```

规模参数：`JOBFORGE_SCALE_KILL_ROUNDS`（默认 100）、`JOBFORGE_SCALE_KILL_JOBS_PER_ROUND`（默认 10）、`JOBFORGE_SCALE_IDEMPOTENT_JOBS`（默认 10000）、`JOBFORGE_SCALE_WORKERS`（默认 8）。运行前停止 compose 应用服务，保留测试所需 PostgreSQL；需要 NFR-302 时同时保留 Redis。helper 只存在于 `_test.go`，直接重启当前 test binary，不给生产二进制增加 crash 开关；所有进程都有超时、Kill、Wait 与 cleanup。运行结果与 race 抽样归档于 [可靠性报告](reliability-report.md)。

设置 `JOBFORGE_TEST_REDIS_URL` 时额外执行 NFR-302 事件发布 smoke（`TestScaleNFR302EventPublishSmoke`，参数 `JOBFORGE_SCALE_NFR302_EVENTS` 默认 10000、`JOBFORGE_SCALE_NFR302_WAVE` 默认 1000）；未设置时该测试 skip。

## CI 质量门禁

[CI 工作流](../.github/workflows/ci.yml) 在每个 Pull Request（以及合入 `main` 的 push）上运行上一节“常用检查”中的机械检查子集，与 [CONTRIBUTING.md](../CONTRIBUTING.md) 的验证要求互相引用：

| CI Job | 检查 | 本地等价命令 |
|---|---|---|
| `go-lint` | `go build ./...`、`go vet ./...`、golangci-lint v2.12.2 | `go build ./...`、`go vet ./...`、`.tools/bin/golangci-lint run` |
| `go-test` | `go test -race -count=1 ./...`（真实 PostgreSQL 16 service，覆盖单元、集成与故障测试） | `go test -race ./...` |
| `python-lint` | SQLFluff 历史基线校验、`sqlfluff lint migrations`、`ruff check .`、`ruff format --check .`、`mypy sdk/python`（工具版本由 `tools/requirements-lint.txt` 锁定） | `.venv/bin/python tools/check_sqlfluff_baseline.py`、`.venv/bin/sqlfluff lint migrations`、`.venv/bin/ruff check .`、`.venv/bin/ruff format --check .`、`.venv/bin/mypy sdk/python` |
| `proto-lint` | `buf lint`（Buf 1.72.0） | `.tools/bin/buf lint` |

该清单对应 AGENTS.md “验证与汇报”中的格式、lint、单元、集成、故障与 race 检查；`buf breaking` 仍按改动范围在本地执行，暂不进入 CI。新增或移除 CI 检查项时，必须同步更新本表、AGENTS.md 与 CONTRIBUTING.md。

## 测试分层

- 单元测试：状态转换、错误分类、退避、配额和 Handler 生命周期。
- 数据库集成测试：使用真实 PostgreSQL 验证 claim、事务、租约、持久业务幂等、0018 up/down、outbox，以及 AT-02 真实 Worker 进程 Kill/Wait、AT-24 DB-clock 取消 SLO、AT-28～31 类型/能力/错误契约、默认/非默认 heartbeat/TTL 与 liveness 节流。
- 事件消费集成测试：使用真实 PostgreSQL + Redis 验证 commit-before-ACK、XAUTOCLAIM 多页 cursor、inbox group binding/去重、瞬时 read/processor/ACK 恢复、deleted pending fail-fast、默认五次 poison 和晚建 group backlog；migration 0017 另在独立临时 0016 数据库上通过正式 Migrator 前滚。
- 契约测试：验证 HTTP/gRPC 错误映射、每个 Worker 错误的稳定 detail、deadline、未知 type 零副作用、Register/Poll 子集与容量、重复提交和 Proto 兼容性。
- 故障测试：kill Worker/Scheduler、阻断 heartbeat、ACK 前崩溃和陈旧写入。
- scale 可靠性测试（`-tags scale`）：100 轮故障注入、万级幂等和 NFR-306 heartbeat 写放大的字面规模验证，独立于默认 CI（见“Scale 可靠性套件”一节）。
- 性能测试：固定环境后报告吞吐、延迟、CPU、heap 与 goroutine 稳态。

## 配置来源

- golangci-lint：<https://golangci-lint.run/docs/configuration/file/>
- Ruff：<https://docs.astral.sh/ruff/configuration/>
- mypy：<https://mypy.readthedocs.io/en/stable/config_file.html>
- SQLFluff：<https://docs.sqlfluff.com/en/latest/configuration/setting_configuration.html>
- Buf：<https://buf.build/docs/configuration/v2/buf-yaml/>

## Python SDK 开发

Python SDK 位于 `sdk/python/`，使用 httpx 作为 HTTP 客户端。

### 安装开发依赖

```sh
cd sdk/python
pip install -e ".[dev]"
```

### 运行测试

```sh
cd sdk/python
pytest tests/
```

### 代码检查

```sh
.venv/bin/ruff check sdk/python
.venv/bin/mypy sdk/python
```

## 可观测性开发（W6）

JobForge 使用 OpenTelemetry + Prometheus + pprof 提供完整可观测性（ADR-0004）。

### 环境变量

| 变量 | 默认值 | 说明 |
|---|---|---|
| `JOBFORGE_OTEL_EXPORTER` | `stdout` | Trace exporter 类型（stdout / none） |
| `JOBFORGE_OTEL_SAMPLE_RATIO` | `1.0` | Trace 采样率 [0.0, 1.0] |
| `JOBFORGE_METRICS_ADDR` | `127.0.0.1:6060` | pprof + /metrics 监听地址 |

### Trace 查看

开发环境默认将 span JSON 输出到 stderr。生产可切换 OTLP：

```sh
JOBFORGE_OTEL_EXPORTER=stdout go run ./cmd/jobforge api
```

### Metrics 查看

```sh
curl http://127.0.0.1:6060/metrics
```

### pprof 剖析

```sh
# CPU profile (5秒)
go tool pprof http://127.0.0.1:6060/debug/pprof/profile?seconds=5

# Heap profile
go tool pprof http://127.0.0.1:6060/debug/pprof/heap

# Goroutine
go tool pprof http://127.0.0.1:6060/debug/pprof/goroutine
```

### Span 列表（PRD 12.2）

| Span | 组件 | 属性 |
|---|---|---|
| `http.submit_job` | API | tenant_id, queue, type |
| `scheduler.promote_jobs` | Scheduler | jobs.promoted, jobs.recovered |
| `gateway.claim_jobs` | Gateway | worker_id, max_jobs, jobs.claimed |
| `worker.execute` | Worker | queue, type, attempt, worker_id |
| `gateway.complete_job` | Gateway | worker_id |
| `event.consume` | Reference Consumer | consumer group、redelivered、delivery_count、ACK/pending/poison 结果 |
| `event.process` | Reference Consumer | inbox duplicate、业务事务结果 |
