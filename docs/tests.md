# 测试与 CI 门禁

所有命令从仓库根目录执行。先按[开发指南](development.md)安装工具与 SDK，再按本次改动选择检查；每个 PR 的现有 8 项 CI 都须在最新 head 通过。实际结果写明通过、失败、未运行及原因，skip 不计验收通过。

## 按改动选择检查

| 改动 | 必要验证 |
|---|---|
| 文档 | 文件/锚点链接、Linux 大小写、命令与代码一致性、受保护数据/契约未改；无需启动数据库或模型 |
| Go/并发/租约/状态 | 格式、build/vet/lint、相关单元/真实 PG 故障测试；并发修改必须 `go test -race ./...` |
| API/SDK/Proto | 正常/相关失败契约、安装 SDK 的真实 HTTP、Buf lint/兼容性、从源生成代码 |
| Python/执行器输入 | 相关 Python 测试、Ruff、类型检查；共同 schema/fixture 与解析器一致 |
| 业务/检索 | pgvector/HTTP/race、离线数据与语义锚；真实 embedding/检索质量另列 |
| 运行时/恢复/审计 | 共同向量、真实 PG 事务/并发、固定 Linux 进程与联合层、生产镜像边界 |
| SQL migration | 冻结基线检查、SQLFluff、新迁移往返与锁风险；不改已应用 migration |
| 观测配置 | dashboard 生成一致性、promtool 配置和告警规则测试 |

测试验证行为和实际故障，不为简单文案写镜像测试。已通过且没有新改动/失败/未解决风险的检查不重复。

## PostgreSQL 与跨语言前置

Windows 的 Docker Desktop 后端不由 testcontainers 自动启动测试库。运行 `go test ./tests/integration/...` 或全仓 race 前必须：

```powershell
docker compose -f deploy/compose.yaml up -d postgres
$env:JOBFORGE_TEST_DSN='postgres://jobforge:jobforge@localhost:5433/jobforge?sslmode=disable'
.venv/Scripts/python.exe -m pip install --no-deps ./sdk/python
$env:JOBFORGE_TEST_PYTHON=(Resolve-Path .venv/Scripts/python.exe).Path
```

只使用可重建测试库。**同一 DSN 只运行一个可能清理数据库的测试进程**，不同时运行宿主全仓 race 和容器联合层。SDK 未安装/解释器未配置导致的真实 HTTP 契约 skip 不计通过。Linux 未设置 DSN 时可由 testcontainers 启动临时 PG；CI 显式使用 service PG 和已安装 SDK。

旧 Job 耐久事件测试还需要 AOF Redis：

```powershell
docker compose -f deploy/compose.yaml --profile durable-events up -d postgres redis
$env:JOBFORGE_TEST_REDIS_URL='redis://localhost:6379/0'
$env:JOBFORGE_TEST_REDIS_CONTAINER='deploy-redis-1'
```

自定义 Compose project 时改为实际 Redis 容器名。AT-17/NFR-303 使用 stop/start 验证 AOF 恢复，保留 volume；缺 Redis 的 skip 不计耐久层通过。Windows 可用 `pwsh -NoProfile -File tools/test-windows.ps1`，前置和时间诊断见[手册](runbooks/windows-acceptance.md)。

## 常规检查

Windows PowerShell（Go 测试先完成上述前置）：

```powershell
go build ./...
go vet ./...
.tools/bin/golangci-lint.exe run
go test -race -count=1 ./...
.venv/Scripts/python.exe -m pytest sdk/python/tests python/tests tools/agent_probe_data tools/executorprobe tools/support_evaluation
$env:PYTHONPATH='python;sdk/python'
.venv/Scripts/python.exe -m pytest tools/support_recovery tools/support_approval
.venv/Scripts/python.exe -m pytest tools/support_s5 tools/support_lifecycle
.venv/Scripts/ruff.exe check .
.venv/Scripts/ruff.exe format --check .
.venv/Scripts/mypy.exe sdk/python
.venv/Scripts/mypy.exe --platform linux python/jobforge_agent
.venv/Scripts/mypy.exe --platform linux tools/agent_model_probe.py tools/executorprobe/executor.py
$env:MYPYPATH='python;sdk/python'
.venv/Scripts/mypy.exe --platform linux --explicit-package-bases tools/support_evaluation
.venv/Scripts/mypy.exe --platform linux --explicit-package-bases tools/support_recovery tools/agentruntimecheck/recovery_supervisor_check.py
.venv/Scripts/mypy.exe --platform linux --explicit-package-bases tools/support_approval
.venv/Scripts/mypy.exe --platform linux --explicit-package-bases tools/support_s5 tools/support_lifecycle
.venv/Scripts/python.exe tools/check_sqlfluff_baseline.py
.venv/Scripts/sqlfluff.exe lint migrations
.tools/bin/buf.exe lint
```

Linux/macOS 使用 `.venv/bin`、无 `.exe` 的 `.tools/bin` 路径；源码环境变量路径分隔符为 `:`。Proto 修改另运行 `.tools/bin/buf breaking --against '.git#branch=main'` 和 `.tools/bin/buf generate`。`.sqlfluffignore` 只冻结历史基线，新 migration 必须直接通过 lint。

独立业务层启动 `docker compose -f deploy/compose.agent.yaml up -d --wait business-postgres`，配置 `JOBFORGE_BUSINESS_TEST_DSN=postgres://jobforge_business_bootstrap:jobforge_business_bootstrap@127.0.0.1:5434/jobforge_business?sslmode=disable` 和安装 SDK 的解释器后，执行 `go test -race -count=1 ./internal/business ./internal/jsonstrict ./tests/integration/business`。合成向量验证机械合同，真实数据准备/检索按[业务指南](agent-v3/business.md)。

## 固定 Linux 进程与联合检查

Windows 同样使用 Docker Linux 镜像与 `--init`。Go/Python 共享 `CLOCK_BOOTTIME` 必须在同一固定 Linux 容器实测；普通非 Linux 分支或未启用 `JOBFORGE_RUNEXECUTOR_PROCESS_TESTS` / `JOBFORGE_RUNEXECUTOR_INTEGRATION_TESTS` 的 skip 不能替代此层。

[独立进程探针](../tools/executorprobe/README.md)检查固定合成操作，正式运行时使用下面的 `agentruntimecheck` 目标。

```powershell
docker build -f tools/executorprobe/Dockerfile -t jobforge-executor-probe:s0 .
docker run --rm --init --network none --cpus 1 --memory 256m --pids-limit 64 jobforge-executor-probe:s0
docker run --rm --init --network none jobforge-executor-probe:s0 ./clock.test -test.v -test.timeout=30s
docker build --target process-check -f tools/agentruntimecheck/Dockerfile -t jobforge-agent-runtime:process .
docker run --rm --init --network none jobforge-agent-runtime:process
docker build --target python-check -f tools/agentruntimecheck/Dockerfile -t jobforge-agent-runtime:python .
docker run --rm --init --network none jobforge-agent-runtime:python
docker build --target integration-check -f tools/agentruntimecheck/Dockerfile -t jobforge-agent-runtime:integration .
docker run --rm --init --network none jobforge-agent-runtime:integration /app/worker.test -test.v -test.timeout=120s
```

进程层使用实际 guardian/step、FD、Kill/Wait/EOF/Join/组消失与 race；`python-check` 包含当前授权 HTTP、真实 BOOTTIME、回环 TCP 和 Python 全套所需的 executor/support 源契约。联合层连接真实 PG/TCP gRPC、正式 Worker/SDK。S1–S3 的业务 HTTP 为测试替身；S4 `TestRunApprovalExecutorRealBusinessNaturalRecovery` 另连接独立 pgvector 业务库和实际业务 HTTP，验证真实工单/结论/回执事务、提交后丢响应并杀 Go Worker，再按原30s lease自然恢复。embedding 和模型在免费联合层使用合成响应，真实模型验收另列。

完成 PG 前置后，Windows 联合层连接宿主 5433，不能使用 `--network none`：

```powershell
docker run --rm --init --add-host control:127.0.0.1 -e JOBFORGE_RUNEXECUTOR_INTEGRATION_TESTS=1 -e 'JOBFORGE_TEST_DSN=postgres://jobforge:jobforge@host.docker.internal:5433/jobforge?sslmode=disable' -e 'JOBFORGE_BUSINESS_TEST_DSN=postgres://jobforge_business_bootstrap:jobforge_business_bootstrap@host.docker.internal:5434/jobforge_business?sslmode=disable' jobforge-agent-runtime:integration
```

联合层前先启动上述业务 pgvector 库；未设置业务 DSN 导致的 S4 skip 不算联合层通过。Linux 使用可达的 PG 地址，必要时在镜像名之前添加 `--add-host host.docker.internal:host-gateway`。Dockerfile 在测试 target 设置专用开关；`control:127.0.0.1` 供容器内 SDK listener 使用。S3 `TestRunRecovery` 和 S4 动作故障保留自然 30s lease、180s attempt 和三次恢复上限，联合测试最长 1200s，不缩短生产计时。

审计检查包含共同 audit/report/observation 向量、真实 PG 首报告/重放/冲突/冻结/晚到/批次 guard、`TestRunProviderAuditPythonHTTPContract` 和固定进程 `TestRunProviderAuditExecutor` 确认丢失/停发。恢复层验证关闭证明、原预算、迁移和实际进程接管；细节见[审计](agent-v3/provider-audit.md)与[恢复](agent-v3/recovery.md)。

仅测试 integration target 安装合成 registry、固定 loopback origin 和 fault hook。生产 `deploy/Dockerfile.agent-worker` 必须只注册 `support-fixed-v1`/`support-agent-v1`，不含 gold、测试模块/安装器/manifest。所有 profile、输入、manifest 和 Worker 必须匹配同一固定 executor version；不能用替身成功信号代替 ACK/Wait/EOF/Join/组消失。

## 观测、性能与真实模型

观测配置变更运行：

```powershell
.venv/Scripts/python.exe tools/generate_task_dashboard.py
git diff --exit-code -- deploy/grafana/dashboards/jobforge-tasks.json
docker run --rm -v "${PWD}/deploy/prometheus:/etc/prometheus:ro" --entrypoint promtool prom/prometheus:v3.14.0 check config /etc/prometheus/prometheus.yml
docker run --rm -v "${PWD}/deploy/prometheus:/etc/prometheus:ro" --entrypoint promtool prom/prometheus:v3.14.0 test rules /etc/prometheus/alerts.test.yml
.venv/Scripts/python.exe tools/generate_run_dashboard.py
docker run --rm -v "${PWD}/deploy/prometheus:/etc/prometheus:ro" --entrypoint promtool prom/prometheus:v3.14.0 check config /etc/prometheus/run.yml
docker run --rm -v "${PWD}/deploy/prometheus:/etc/prometheus:ro" --entrypoint promtool prom/prometheus:v3.14.0 test rules /etc/prometheus/run-alerts.test.yml
```

旧 Job scale 套件使用 `go test -race -tags scale -count=1 ./tests/scale/...`，数据库/Redis 前置相同；性能比较按[基准说明](benchmark.md)，不改写历史门槛。

真实模型层使用独立[工作流](../.github/workflows/real-models.yml)及[旧模型任务](legacy/real-tasks.md)或 [v3 批次](agent-v3/cloud-batch.md)的明确运行范围。缺 `JOBFORGE_REAL_MODEL_URL` 的 skip 不算真实模型通过。模型协议探针 fixture 不代表真实业务工具，开发集分数不代表保留集泛化；资料/注册能力不构成收费授权。

## CI 质量门禁

权威执行配置为 [ci.yml](../.github/workflows/ci.yml)，本页解释本地等价检查，不取代实际执行结果。8 项 job 全部强制执行：

| Job ID | 检查 |
|---|---|
| `go-lint` | Go build、vet、golangci-lint |
| `go-test` | 全仓 race、真实 PG/AOF Redis、安装 SDK 的 HTTP 契约 |
| `python-lint` | SDK/Agent/探针/数据评分/恢复测试，Ruff、各域 mypy（含 Linux）、SQLFluff 基线与 migration |
| `business-contract` | 独立 pgvector、真实 HTTP/PG/race、业务 runtime 镜像 |
| `executor-process-probe` | 固定 Linux 进程故障/race及 Go/Python 共享 BOOTTIME |
| `proto-lint` | Buf lint |
| `agent-runtime-contract` | 实际进程/PG/gRPC/SDK/恢复/race、S4真实业务HTTP/pgvector事务及生产镜像边界 |
| `observability-config` | dashboard 生成一致性、promtool 配置/告警规则 |
