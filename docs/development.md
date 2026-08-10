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
| Python | 3.12.10 | 本机环境，仅作为首次验证记录 |
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

`tests/scale/` 以 PRD v0.1 NFR-001/002 字面规模验证可靠性（PRD v0.2 FR-601/602/603，AT-13/AT-14）：AT-13 为 100 轮 Worker kill 零静默丢失，AT-14 为 10,000 任务重复投递零重复副作用。套件通过 build tag `scale` 与默认测试物理隔离：默认 `go test ./...` 与 CI 均不执行，默认套件时长不受影响（FR-603）。

```sh
# 字面规模运行（需 PostgreSQL；Windows 先设 JOBFORGE_TEST_DSN，见上文）
go test -tags scale -count=1 -timeout 60m ./tests/scale/

# 降采样冒烟（开发反馈循环）
set JOBFORGE_SCALE_KILL_ROUNDS=5
set JOBFORGE_SCALE_IDEMPOTENT_JOBS=200
go test -tags scale -count=1 ./tests/scale/
```

规模参数：`JOBFORGE_SCALE_KILL_ROUNDS`（默认 100）、`JOBFORGE_SCALE_KILL_JOBS_PER_ROUND`（默认 10）、`JOBFORGE_SCALE_IDEMPOTENT_JOBS`（默认 10000）、`JOBFORGE_SCALE_WORKERS`（默认 8）。运行前仅保留 postgres 服务（停止 compose 应用服务，避免争用）；运行结果与 race 抽样归档于 [可靠性报告](reliability-report.md)。

## CI 质量门禁

[CI 工作流](../.github/workflows/ci.yml) 在每个 Pull Request（以及合入 `main` 的 push）上运行上一节“常用检查”中的机械检查子集，与 [CONTRIBUTING.md](../CONTRIBUTING.md) 的验证要求互相引用：

| CI Job | 检查 | 本地等价命令 |
|---|---|---|
| `go-lint` | `go build ./...`、`go vet ./...`、golangci-lint v2.12.2 | `go build ./...`、`go vet ./...`、`.tools/bin/golangci-lint run` |
| `go-test` | `go test -race -count=1 ./...`（真实 PostgreSQL 16 service，覆盖单元、集成与故障测试） | `go test -race ./...` |
| `python-lint` | `ruff check .`、`ruff format --check .`、`mypy sdk/python`（工具版本由 `tools/requirements-lint.txt` 锁定） | `.venv/bin/ruff check .`、`.venv/bin/ruff format --check .`、`.venv/bin/mypy sdk/python` |
| `proto-lint` | `buf lint`（Buf 1.72.0） | `.tools/bin/buf lint` |

该清单对应 AGENTS.md “验证与汇报”中的格式、lint、单元、集成、故障与 race 检查；`sqlfluff lint migrations` 与 `buf breaking` 仍按改动范围在本地执行，暂不进入 CI。新增或移除 CI 检查项时，必须同步更新本表与 CONTRIBUTING.md。

## 测试分层

- 单元测试：状态转换、错误分类、退避、配额和 Handler 生命周期。
- 数据库集成测试：使用真实 PostgreSQL 验证 claim、事务、租约、幂等与 outbox。
- 契约测试：验证 HTTP/gRPC 错误映射、deadline、重复提交和 Proto 兼容性。
- 故障测试：kill Worker/Scheduler、阻断 heartbeat、ACK 前崩溃和陈旧写入。
- scale 可靠性测试（`-tags scale`）：100 轮故障注入与万级幂等的字面规模验证，独立于默认 CI（见“Scale 可靠性套件”一节）。
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
