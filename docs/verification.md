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

Agent v3 的 S0 探针另运行 `python -m pytest tools/agent_probe_data tools/executorprobe`、`mypy --platform linux tools/agent_model_probe.py tools/executorprobe/executor.py`；真实执行器生命周期与 Linux race 使用 `tools/executorprobe/Dockerfile` 构建镜像，按 CI 的 `docker run --init --network none` 命令执行。常规 Go 测试跳过受平台约束的进程套件不代表通过；专门 CI job 实际执行。模型 guardrail 测试不调用模型，真实模型协议试验与业务验收分别报告。

Pull Request 至少应包含正常路径和一个相关失败路径的测试；并发相关变更必须通过 race 检测，接口变更必须包含契约或兼容性验证。

Agent v3 S1-A另运行`python -m pytest python/tests`、`mypy python/jobforge_agent`；独立业务库启动、`JOBFORGE_BUSINESS_TEST_DSN`和跨语言解释器配置见[业务开发指南](agent-v3-business.md)。CI专门使用固定pgvector镜像运行真实HTTP/PG/race，不以普通Go测试中的依赖skip代替。真实模型层保留20条查询的全部结果和未命中，不能用合成向量证明检索质量。

Agent v3 S1-B新增的 `TestRun*` 使用真实控制PostgreSQL，并通过独立数据库隔离各用例；实际HTTP故障服务与模型替身的边界见 [Run指南](agent-v3-runs.md)。生产 `agent-control` 只启动Run扫描，不并行运行旧jobs调度器。新Proto生成仍使用 `buf generate`，执行器Go/Python共同fixture与已安装SDK真实HTTP均为门禁。Run定向性能只记录新基线，不改变历史W4失败或AT-25跳过结论。

S1-C1的v2执行器codec/计量顺序使用共同fixture，随`go test -race ./...`与`pytest python/tests`执行。Linux共享BOOTTIME验证还需构建上述S0镜像后运行`docker run --rm --init --network none jobforge-executor-probe:s0 ./clock.test '-test.v' '-test.timeout=30s'`；CI明确执行，不能拿Windows非Linux分支测试替代。它只验证时钟域，不代表正式执行器进程或真实DeepSeek已验收，见[协议指南](agent-v3-executor-protocol.md)。

S1-C2受控HTTP/DeepSeek适配随`pytest python/tests`和Python类型检查执行；CI在Linux运行真实BOOTTIME分支。本地Windows还应按[受控HTTP指南](agent-v3-authorized-http.md)构建并执行`tools/agenthttpcheck/Dockerfile`，验证固定Linux时钟和实际loopback TCP。测试的模型、向量、协调者均为替身，不代表云端推理或持久授权已验收。

S1-C3b按[固定运行时指南](agent-v3-runtime.md)构建`tools/agentruntimecheck/Dockerfile`：`process-check`验证真实进程/race/FD清理，`integration-check`验证真实PostgreSQL/TCP gRPC/正式Worker与合成业务HTTP，`python-check`复现Python全套。CI的`agent-runtime-contract`运行进程/联合检查并构建生产镜像核对registry边界；`python-lint`继续运行Python测试。Windows同样使用Linux容器和`--init`；先`docker compose -f deploy/compose.yaml up -d postgres`并设置`JOBFORGE_TEST_DSN`，容器使用可达的宿主5433地址。同一DSN不能并行运行会清理数据库的测试进程。普通Go测试中的平台/专用环境skip不计正式进程通过。

供应商审计增量还需验证 Go/Python 的 typed report 和 observation.v2 共同向量；真实 PG 首份/重放/冲突/晚到、批次 guard 和迁移往返；`TestRunProviderAuditPythonHTTPContract` 使用安装 SDK 查询真实控制库。`integration-check` 默认包含 `TestRunProviderAuditExecutor`，在实际 guardian/FD/gRPC/PG 中注入 Reserve/report/Observe/Commit 提交前阻塞和提交后 ACK 丢失。合成供应商证明执行机制，不能替代 DeepSeek、40 案评分或生产留存。

运行时变更还需核对源码schema/共同fixture、全部profile的固定executor_version、只读manifest、秘密与FD白名单，以及Commit前真实Wait/EOF/Join/组消失。生产registry仅登记support-fixed-v1与support-agent-v1；测试adapter、测试origin安装器和gold不得进入`deploy/Dockerfile.agent-worker`。support模型/持久方案schema与Go/Python共同fixture随既有测试执行，标准JSON Schema校验使用固定开发依赖jsonschema；离线开发数据与语义锚另运行`python -m pytest tools/support_evaluation`，由CI强制执行，不读取保留集。该层合成供应商只验证机制，不能标记真实DeepSeek、检索质量、40案或整体S1完成。
