# 贡献指南

感谢关注 JobForge。项目已交付 [PRD v0.1](docs/product/JobForge_PRD_v0.1.md)～[PRD v0.3](docs/product/JobForge_PRD_v0.3.md) 的已实现范围，并以 [PRD v0.4](docs/product/JobForge_PRD_v0.4.md) 补齐持久业务幂等与真实 Worker 崩溃证据；明确标为 P1/未实现的条目不在完成声明内。任何实现都必须先服从 PRD 中已经冻结的可靠性边界。

## 工作流

1. 从最新 `main` 创建短生命周期分支。
2. 分支使用 `<type>/<short-kebab-description>`，例如 `feat/lease-claim`、`fix/stale-complete`、`docs/retry-semantics`。
3. 保持提交聚焦且可独立审查；不要把无关格式化或重构混入功能提交。
4. 发起 Pull Request，关联 Issue 或 ADR，说明风险、兼容性影响和验证证据。
5. 所有评审意见和必需检查通过后，使用 squash merge；合并后的提交仍须符合 Conventional Commits。

初始化仓库的 root commit 是直接提交到 `main` 的唯一历史例外。`main` 应保持受保护状态，禁止日常直接推送。

## 提交规范

格式：

```text
<type>(<optional-scope>): <description>
```

允许的 type：`feat`、`fix`、`docs`、`refactor`、`test`、`perf`、`build`、`ci`、`chore`、`revert`。

- type 与 scope 必须使用小写英文。
- description 可以使用中文或英文，但同一个 Pull Request 内保持一致。
- 使用祈使语气，不以句号结尾，首行尽量不超过 72 个字符。
- 不兼容变更使用 `!` 并在正文写 `BREAKING CHANGE:`；公开 API/Proto 的不兼容变更通常不应直接接受。

示例：

```text
feat(worker): reject stale fencing tokens
docs(adr): 记录 dead 任务人工重试决策
```

## 代码与架构要求

- 遵守 [代码与注释规范](docs/code-standards.md)。
- PostgreSQL 是 P0 唯一事实源；不得把 JetStream 或进程内状态变成核心任务状态来源。
- 状态转换集中在 domain/service 层，HTTP 与 gRPC handler 只负责传输、鉴权、校验和错误映射。
- 数据库变更只能通过新增 versioned migration；已应用 migration 不得改写。
- 任何 goroutine 都必须有明确所有者、取消路径和退出条件。
- 时间、随机数、重试策略和外部副作用应可注入，以支持确定性测试。
- 不得记录 API key、Authorization header、完整敏感 payload 或其他秘密。

## ADR 与文档同步

以下变更必须先创建或更新 ADR：

- 任务状态机、投递保证、租约、fencing、幂等、取消或重试语义；
- 公开 HTTP/gRPC/SDK 契约或兼容性政策；
- 数据库事实源、调度模型、关键依赖或部署边界；
- 偏离 PRD 已固定边界的实现选择。

新增状态、错误码、指标、migration 或公开接口时，要在同一 Pull Request 中更新对应文档。

## 验证要求

仅运行与当前改动范围相关的检查，不伪造通过结果。提交前至少执行：

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

上述 golangci-lint、`go test -race ./...`、ruff（check + format）、mypy、SQLFluff 历史基线校验、`sqlfluff lint migrations` 和 buf lint 均由 [CI 工作流](.github/workflows/ci.yml) 在每个 Pull Request 上强制执行（见 [开发环境](docs/development.md) 的 “CI 质量门禁” 一节），本地清单与 PR 门禁保持一致。`.sqlfluffignore` 只冻结已应用 migration 的既有格式债务；禁止用新增 ignore 条目绕过新 migration 的检查。

Windows 对应的 Python 可执行文件位于 `.venv\Scripts`，Go/Buf 工具位于 `.tools\bin`。可靠性集成测试必须使用真实 PostgreSQL，核心行为不能只由 mock 验证。

SDK 安装：`python -m pip install ./sdk/python`；Go/Python 跨语言契约需设置 `JOBFORGE_TEST_PYTHON` 为安装该 SDK 的解释器路径（CI 显式安装并启用）。本地未设置时该用例 skip，不代表契约通过。

Agent v3 的 S0 探针另运行 `python -m pytest tools/agent_probe_data tools/executorprobe`、`mypy --platform linux tools/agent_model_probe.py tools/executorprobe/executor.py`；真实执行器生命周期与 Linux race 使用 `tools/executorprobe/Dockerfile` 构建镜像，按 CI 的 `docker run --init --network none` 命令执行。常规 Go 测试跳过受平台约束的进程套件不代表通过；专门 CI job 实际执行。模型 guardrail 测试不调用模型，真实模型协议试验与业务验收分别报告。

Pull Request 至少应包含正常路径和一个相关失败路径的测试；并发相关变更必须通过 race 检测，接口变更必须包含契约或兼容性验证。

Agent v3 S1-A另运行`python -m pytest python/tests`、`mypy python/jobforge_agent`；独立业务库启动、`JOBFORGE_BUSINESS_TEST_DSN`和跨语言解释器配置见[业务开发指南](docs/agent-v3-business.md)。CI专门使用固定pgvector镜像运行真实HTTP/PG/race，不以普通Go测试中的依赖skip代替。真实模型层保留20条查询的全部结果和未命中，不能用合成向量证明检索质量。

Agent v3 S1-B新增的 `TestRun*` 使用真实控制PostgreSQL，并通过独立数据库隔离各用例；实际HTTP故障服务与模型替身的边界见 [Run指南](docs/agent-v3-runs.md)。生产 `agent-control` 只启动Run扫描，不并行运行旧jobs调度器。新Proto生成仍使用 `buf generate`，执行器Go/Python共同fixture与已安装SDK真实HTTP均为门禁。Run定向性能只记录新基线，不改变历史W4失败或AT-25跳过结论。

S1-C1的v2执行器codec/计量顺序使用共同fixture，随`go test -race ./...`与`pytest python/tests`执行。Linux共享BOOTTIME验证还需构建上述S0镜像后运行`docker run --rm --init --network none jobforge-executor-probe:s0 ./clock.test '-test.v' '-test.timeout=30s'`；CI明确执行，不能拿Windows非Linux分支测试替代。它只验证时钟域，不代表正式执行器进程或真实DeepSeek已验收，见[协议指南](docs/agent-v3-executor-protocol.md)。

S1-C2受控HTTP/DeepSeek适配随`pytest python/tests`和Python类型检查执行；CI在Linux运行真实BOOTTIME分支。本地Windows还应按[受控HTTP指南](docs/agent-v3-authorized-http.md)构建并执行`tools/agenthttpcheck/Dockerfile`，验证固定Linux时钟和实际loopback TCP。测试的模型、向量、协调者均为替身，不代表云端推理或持久授权已验收。

S1-C3b按[固定运行时指南](docs/agent-v3-runtime.md)构建`tools/agentruntimecheck/Dockerfile`：`process-check`验证真实进程/race/FD清理，`integration-check`验证真实PostgreSQL/TCP gRPC/正式Worker与合成业务HTTP，`python-check`复现Python全套。CI的`agent-runtime-contract`运行进程/联合检查并构建生产镜像核对registry边界；`python-lint`继续运行Python测试。Windows同样使用Linux容器和`--init`；先`docker compose -f deploy/compose.yaml up -d postgres`并设置`JOBFORGE_TEST_DSN`，容器使用可达的宿主5433地址。同一DSN不能并行运行会清理数据库的测试进程。普通Go测试中的平台/专用环境skip不计正式进程通过。

供应商审计增量还需验证 Go/Python 的 typed report 和 observation.v2 共同向量；真实 PG 首份/重放/冲突/晚到、批次 guard 和迁移往返；`TestRunProviderAuditPythonHTTPContract` 使用安装 SDK 查询真实控制库。`integration-check` 默认包含 `TestRunProviderAuditExecutor`，在实际 guardian/FD/gRPC/PG 中注入 Reserve/report/Observe/Commit 提交前阻塞和提交后 ACK 丢失。合成供应商证明执行机制，不能替代 DeepSeek、40 案评分或生产留存。

运行时变更还需核对源码schema/共同fixture、全部profile的固定executor_version、只读manifest、秘密与FD白名单，以及Commit前真实Wait/EOF/Join/组消失。生产registry仅登记support-fixed-v1；测试adapter、测试origin安装器和gold不得进入`deploy/Dockerfile.agent-worker`。support模型/持久方案schema与Go/Python共同fixture随既有测试执行，标准JSON Schema校验使用固定开发依赖jsonschema；离线开发数据与语义锚另运行`python -m pytest tools/support_evaluation`，由CI强制执行，不读取保留集。该层合成供应商只验证机制，不能标记真实DeepSeek、检索质量、40案或整体S1完成。

## 安全问题

不要在公开 Issue 或 Pull Request 中提交未公开漏洞、凭据或敏感数据。请遵循 [SECURITY.md](SECURITY.md) 的私密报告流程。
