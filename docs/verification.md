# 验证指南

先选验证层，再运行对应检查。所有数据库必须是新建可重建测试资源；同一控制库不得并行运行清表套件。不要将现有环境 DSN 自动当成测试库。

| 层 | 证明什么 | 不证明什么 |
|---|---|---|
| Python / Go 单元与共同 fixture | 校验、协议、预算计算、来源、安全拒绝 | 真实模型质量 |
| PostgreSQL / Redis / pgvector + SDK HTTP | 事务、隔离、租约、恢复、实际接口 | 模型推理质量 |
| 固定 Linux 执行器与联合机制 | 真实进程、FD、ACK、gRPC、清理屏障 | 真实供应商验收 |
| 收费模型 | 冻结版本的模型行为与实际 usage | 未跑的保留集或生产 SLA |

云环境已有 `.tools/go/bin/go` 时先执行 `export PATH="$PWD/.tools/go/bin:$PATH"`。Python 使用 `.venv/bin/python`，安装 SDK 与 Agent 包后运行：

```sh
.venv/bin/python -m pip install --no-deps -e ./sdk/python -e ./python
.venv/bin/python -m pytest -q sdk/python/tests python/tests tools/agent_probe_data tools/executorprobe tools/support_evaluation
.venv/bin/mypy python/jobforge_agent
.venv/bin/mypy --platform linux tools/agent_model_probe.py tools/executorprobe/executor.py
MYPYPATH=python:sdk/python .venv/bin/mypy --explicit-package-bases tools/support_evaluation
```

## 最快完整演示

```sh
bash tools/demo-support.sh
# 同时验收 Worker 和全部联合故障机制：
bash tools/demo-support.sh --all
```

只要求 Linux Docker Engine / BuildKit；测试镜像安装当前 SDK 和 Agent，不依赖宿主虚拟环境。创建独立内部网络、全新 PG 和受限非 root runner，运行时不转发宿主模型配置、不访问外网、不发布端口。输出结构化 `DEMO` 方案/步骤/调用摘要和日志目录。成功或失败均按本次 ID 清理容器、匿名数据库卷和网络；保留构建缓存及镜像。不可把该合成模型结果当作 DeepSeek 质量验收。

若 Docker Hub 限流，可设置 `JOBFORGE_BUILD_REGISTRY=mirror.gcr.io`（工具链仍按同一 digest 锁定）。企业代理需要额外公共 CA 时，设置 `JOBFORGE_BUILD_CA=/path/to/public-ca.crt`；只通过 BuildKit secret 挂载公共证书，不传密钥、不关闭 TLS 验证。当前云环境对应已有 `CODEX_PROXY_CERT` 路径。该设置仅用于构建，不授权收费调用。vfs 存储会复制镜像层，首次构建建议预留至少 12 GiB；不要用全局 prune 清理其他任务资源。

## 隔离服务验收（Linux）

```sh
bash tools/test-linux.sh
```

脚本需要本机 Docker、Go 和已安装依赖的 `.venv`。它先用 pip（或已有 uv）从当前源码重装 SDK 与 Agent 包，避免旧 wheel 混入验收；Go 缓存默认位于仓库 `.cache`。它不接受现有 DSN，清除继承的 JobForge/模型配置，新建独立 PostgreSQL、pgvector 与 AOF Redis，仅绑定 loopback 随机端口，执行完整 Go race 与已安装 SDK 的 HTTP 契约。正常退出、失败或收到 INT/TERM 时仅清理本次创建的容器及其匿名卷，不执行全局 prune。日志与逐项 skip 留在 `.cache/verification/`。硬 Kill 或宿主故障无法执行 trap，应依据 `jobforge.purpose=isolated-verification` 标签人工核对遗留资源，不能批量清理别人的运行。

这层不运行收费模型、固定 Linux 执行器镜像或 scale；这些层按下文独立运行，skip 不能算验收。AT-25 ControlStream 尚未实现，空壳 skip 测试已移除；功能限制仍保留，现有 heartbeat 取消测试继续执行。

## 机械检查

按修改范围执行机械检查：

```text
go test -race ./...
go vet ./...
.tools/bin/golangci-lint run
.venv/bin/ruff check .
.venv/bin/ruff format --check .
.venv/bin/mypy sdk/python
.venv/bin/python -m pytest sdk/python/tests
.venv/bin/python tools/generate_task_dashboard.py
# 检查生成后的 dashboard diff；CI 另通过固定 Prometheus 镜像运行 promtool。
docker run --rm -v "$PWD/deploy/prometheus:/etc/prometheus:ro" --entrypoint promtool prom/prometheus:v3.14.0 test rules /etc/prometheus/alerts.test.yml
.venv/bin/python tools/check_sqlfluff_baseline.py
.venv/bin/sqlfluff lint migrations
.tools/bin/buf lint
```

机械门禁由 [CI 工作流](../.github/workflows/ci.yml) 执行；变更检查项时同步本页。`.sqlfluffignore` 只冻结已应用 migration 的既有格式债务；禁止用新增 ignore 条目绕过新 migration 的检查。

Windows 对应的 Python 可执行文件位于 `.venv\Scripts`，Go/Buf 工具位于 `.tools\bin`。可靠性集成测试必须使用真实 PostgreSQL，核心行为不能只由 mock 验证。

SDK 安装：`python -m pip install ./sdk/python`；Go/Python 跨语言契约需设置 `JOBFORGE_TEST_PYTHON` 为安装该 SDK 的解释器路径（CI 显式安装并启用）。本地未设置时该用例 skip，不代表契约通过。

## 专用运行时与模型层

普通 Go 测试有意跳过下列依赖场景；必须分别报告，不能把 skip 算通过。当前分支的逐项映射见[77 项 skip 复核](evidence/reviewable-agent-skips.md)。

| 专项 | 运行入口 | 关键边界 |
|---|---|---|
| S0 探针与 BOOTTIME | `tools/executorprobe/Dockerfile`；CI `executor-process-probe` | 真实 Linux 子进程、FD、退出和同一时钟域；不能替代正式 Worker |
| 正式进程 | `tools/agentruntimecheck/Dockerfile --target process-check`；按[运行时指南](agent-v3-runtime.md)运行 | 非 root、`--init --network none`，实际 Kill/Wait/EOF/Join/组消失 |
| Worker 与联合机制 | `bash tools/demo-support.sh --all` | 新建隔离 PG、真实 gRPC/Worker/已安装 SDK；包括故障注入、预算、审计、固定与动态流程和 launcher |
| Linux 受控 HTTP | [受控 HTTP 指南](agent-v3-authorized-http.md)与 CI | 实际 loopback TCP、固定时钟；供应商和 embedding 是合成响应 |
| 旧 Jobs 真实模型 | [Jobs 指南](jobs-guide.md)、[真实任务](real-tasks.md) | 需要固定 Ollama 后端；普通服务检查不启动、不声称覆盖 |
| S2 真实供应商与检索质量 | [S2 运行指南](agent-v3-support-agent.md)、[业务指南](agent-v3-business.md) | 冻结身份、价格、预算，真实检索和逐案评分；先核验秘密和当次授权 |

SDK 测试应同时包含当前源码与已安装包的真实 HTTP；API 改动覆盖正常和拒绝路径，并发改动加 race。模型/审计变动还需 Go/Python 共同 report/observation 向量、真实 PG 首报告/冲突/晚到/冻结和批次屏障。运行时变动同步核对 schema、profile executor_version、只读 manifest、秘密与 FD 白名单及生产 registry 边界。

生产 registry 只含 `support-fixed-v1` 与 `support-agent-v1`；合成 adapter、测试 origin、gold 只进测试镜像。默认 compose 不启用收费 profile；已接受合同、枚举或历史报告不能代替当前验收。保留集和 scale 另按各自指南执行，不默认触发。

## 正式供应商验收（需单次批准）

离线预算回归：`.venv/bin/python -m pytest tools/test_provider_acceptance.py`，
类型检查：`.venv/bin/mypy tools/run_provider_acceptance.py`；已纳入 CI。

真实收费测试默认 skip。专用镜像为 `tools/agentruntimecheck/Dockerfile` 的
`provider-check` target，启动器为 `tools/run_provider_acceptance.py`。
它要求 `--execute-approved`、新 `--receipt-dir` 和
`--prior-exposure-microyuan`（累计已知费用加全部未释放 hold，不是新预算）。
仅在既有安全凭据、固定代理/只读 CA、本次明确批准及价格重新核实后执行；
不把 CLI 标志当成用户批准，也不自动重跑未知费用批次。

本次已停止，结果与剩余额度以[正式 Worker 验收证据](evidence/worker-real-provider-2026-09-30.md)为准。
