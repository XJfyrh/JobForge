# 测试与 CI 门禁

所有命令从仓库根目录执行。先按[开发指南](development.md)安装工具与 SDK。本地按需要验证；PR 按完整差异选择门禁，由最新 head 的 `CI required` 汇总。实际结果区分通过、失败、未选择和未运行；选中的验收层不能把环境 skip 当成通过。

## 统一入口

使用已安装工具与 SDK 的 Python，Windows 可将下面的 `python` 换成 `.venv/Scripts/python.exe`，Linux 为 `.venv/bin/python`。

```sh
python -m tools.check quick
python -m tools.check affected --base origin/main --plan
python -m tools.check affected --base origin/main
python -m tools.check full
```

- `quick`：SDK／流程守卫测试、Ruff、Go 的 internal/cmd/tools 测试。清除继承的 JobForge 环境变量，不连接数据库、不启动模型；用于编辑反馈，不作为数据库／Linux 进程验收。
- `affected`：比较分支与 base 的 merge-base，叠加暂存、未暂存和未跟踪文件，执行相关检查。`--plan` 只显示范围与命令。重命名同时考虑原路径与新路径；无法解析 base 时失败，不静默减少范围。
- `full`：全部免费检查，包含真实 PG、AOF Redis、业务 pgvector、固定 Linux 进程及生产镜像边界。只继承测试库／Redis配置，清除收费模型与手工恢复的环境开关。数据库前置见下文；命令在缺少所需 DSN／Redis／SDK 时提前失败。
- `--only python-lint` 等可运行一个 CI 检查组。统一入口不安装工具、不管理数据库容器、不启动收费模型；集成测试会重建指定的可丢弃测试库。安装／数据库准备只做一次，完整本地检查串行使用测试 DSN，多会话必须各自使用独占测试容器和 DSN。

CI 与本地共享 [tools/check.py](../tools/check.py) 的 Python／SQL、Go race 和镜像检查命令。Windows 全量检查出现 Linux 专用 skip 时，仍须有对应固定 Linux 层的实际结果。

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

工作台另运行 `node --test internal/run/httpapi/ui/tests/*.test.mjs`（Node.js 22，无 npm 依赖），验证进度与独立效果、未知费用、原始来源精确解析。CI 的 Go 静态检查 job 同时运行该组；浏览器验证覆盖登录、身份切换、审批确认、提交与移动布局。

## PostgreSQL 与跨语言前置

Windows 的 Docker Desktop 后端不由 testcontainers 自动启动测试库。以下是单会话默认前置；并行会话改用各自独占的测试容器、DSN和固定Redis端口。运行 `go test ./tests/integration/...` 或全仓 race 前先准备：

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

自定义 Compose project 时改为实际 Redis 容器名。Redis 必须使用固定宿主端口；Docker 动态发布端口会在 stop/start 后变化，统一入口会提前拒绝这种配置。AT-17/NFR-303 使用 stop/start 验证 AOF 恢复，保留 volume；缺 Redis 的 skip 不计耐久层通过。Windows 的时钟异常或旧 Job 完整验收可用 `pwsh -NoProfile -File tools/test-windows.ps1`；它包含 60s 时钟采样与全仓 race，不是日常 quick 入口。前置和时间诊断见[手册](runbooks/windows-acceptance.md)。

## 常规检查

常规命令统一在 `tools/check.py` 维护，避免本地与 CI 的测试列表漂移。`python -m tools.check full --plan` 可查看全部命令；`--only go-lint`、`--only python-lint`、`--only proto-lint --base origin/main` 分别运行静态／Python和SQL／Proto检查。Proto 检查包含 Buf lint 和相对 base 的 breaking；生成代码仍使用 `buf generate`。`.sqlfluffignore` 只冻结历史基线，新 migration 必须直接通过 lint。

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

联合层前先启动上述业务 pgvector 库；未设置业务 DSN 导致的 S4 skip 不算联合层通过。Linux 使用可达的 PG 地址，必要时在镜像名之前添加 `--add-host host.docker.internal:host-gateway`。Dockerfile 在测试 target 设置专用开关；`control:127.0.0.1` 供容器内 SDK listener 使用。S3 `TestRunRecovery` 和 S4 动作故障保留自然 30s lease、180s attempt 和三次恢复上限。上述镜像默认联合套件最长 1500s，包含新增版本回归；统一入口和 CI 拆分后的每组最长 1200s，均不改变生产计时。

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

S5登记与离线评分按[S5工具](../tools/support_s5/README.md)：开发40案、正式v1/v2/v3各20案分别核验，`tools/support_evaluation/test_registration_routing.py`覆盖两策略及数量/未知dataset拒绝，Go准备器另覆盖外部20案路径。正式Agent须正确≥16、完整案例证据≥18、四类硬失败0；普通质量失败仍完成Fixed基线，再停恢复/演示。原SDK输出、实际PG与业务/回执前后审计必须保留；输入重建仅核验实际request parameter hash，不调用模型，也不计为修复后的质量通过。

独立终裁候选按 [ADR-0033](adr/0033-versioned-final-adjudication.md) 验证：真实 PostgreSQL 的 `TestRunAdjudication` 覆盖新 Agent/Fixed 许可、初稿隔离、终裁失败、unknown hold、晚到计量及两阶段恢复；固定 Linux 镜像中的 `TestRunSupportAgentExecutor/adjudication` 覆盖安装后的长终裁、取消、空推理、截断和超长内容。`tools/support_s5/test_adjudication.py` 检查阶段未知计量不作零、初稿不能替代最终分数；旧版本仍运行原合同用例。以上机制测试的模型响应均为合成，真实开发和已见回归另列。

完整政策候选按 [ADR-0038](adr/0038-policy-bundle-before-adjudication.md) 验证：真实 PG 的 `TestRunPolicyBundle` 覆盖双策略图、工具/HTTP 计量、提交幂等、未提交与已提交恢复、取消/失租/额度；业务库 `TestCompletePolicyBundle` 覆盖完整有序 20 段、缺段/额外别名/跨租户拒绝和独立 32 KiB HTTP 上限。固定 Linux 的 `TestRunSupportAgentExecutor/adjudication_bundle` 包含 Agent、Fixed、终裁取消三项。Python `test_support_agent_v8.py` 与 S5 `test_bundle.py`、`test_freeze.py` 验证严格来源、单一 final envelope、初稿隔离、实际终裁双闭合、旧版本隔离及新冻结源码绑定。

## CI 质量门禁

执行配置为 [ci.yml](../.github/workflows/ci.yml)，路径映射为 [ci_checks.py](../tools/ci_checks.py)。PR 始终执行路径／锚点和选择器回归检查，再运行被选择的组；main push 与手动触发运行全部免费检查。Go race、真实数据库和故障合同保留，普通说明文件不会启动这些服务。

| 改动范围 | 自动选择 |
|---|---|
| 普通说明 Markdown／README | 文档检查；PRD、ADR、政策语料不按普通说明处理 |
| S5验收、S5/host/diagnosis README；support profile-schema.json | Python契约与生成一致性检查；schema JSON不启动数据库或镜像 |
| 历史机器摘要 JSON | 文档及 Python 检查，不重跑模型 |
| 离线评分／审批／S5工具 | Python及外部镜像来源检查；恢复工具另选联合层 |
| Go业务、运行时Python、SDK、API、迁移、可执行数据 | Go build/vet/lint、全仓race、Python、业务pgvector、Linux联合层及镜像边界 |
| Proto | 上述共享契约检查，加 Buf lint／breaking |
| 时钟／S0进程探针 | 对应Go检查、固定进程与BOOTTIME检查 |
| 观测配置与dashboard生成器 | Python、生成一致性、promtool配置及规则测试 |
| PRD／ADR、依赖、CI／检查器、未知路径 | 全部免费检查 |

这是一份保守的文件依赖映射，不靠标签或标题猜测风险。`go-test` 继续全仓 `-race -count=1`，用同源 `GO_TEST_SKIP_PATTERN` 排除交给 Linux 分片的用例和显式人工恢复用例，不缓存真实数据库测试结果。新增路径未登记时先全跑，再明确归属。

运行时联合套件分 `core`、`recovery`、`deadlines` 三组，在不同 runner、PostgreSQL／pgvector服务及容器文件系统中并行。由原测试名前缀自动收集，慢恢复与deadline组显式分配，新增匹配用例默认进入core；不改变自然30s lease、180s attempt或测试断言。生产／operator镜像检查拆成独立job，不再排在长故障测试之后。pip与固定运行时构建使用缓存。

分片包含 Adjudication、PolicyBundle、StatusPolicy、ContentCleanup 家族，任何选中用例 SKIP 或没有 PASS 都失败。`MANUAL_RESTORE_TESTS` 显式列出两个 `TestRunS5Restored*`：它们要求独占历史恢复副本、原签名动作与专用开关，按生命周期恢复流程另验，未运行不计通过。

16份部署 profile schema 由 `python -m tools.support_schemas.generate` 逐字节核验；只有显式 `--write` 才写产物。共享约束与版本差异在 `tools/support_schemas/`；Go严格解码、生产builder及全部dataset枚举通过后，由 `go test ./internal/run -run TestSupportDeploymentSchemas -update-support-schema-vectors` 更新向量。Python核对各schema原字节SHA-256与向量绑定，再按对应schema验证；改schema须重生Go向量，该向量路径会触发Go门禁。同源整理不改历史schema字节。

`CI required` 检查计划和所有 job 结果：必要 job 缺失、失败、取消或意外跳过均失败；未选择的 job 允许 skipped。仓库合并规则应只要求这个稳定状态。首次迁移时，先让新 workflow 在默认分支存在，再配置该必需状态，避免主分支还没有相应检查时阻断合并。

PR 路径选择看整条 PR 差异；代码后追加报告仍可能选择原代码的检查。日志和复核记录优先链接 Actions artifact／PR说明；需入库的后续阶段报告可独立成小PR。不能只看最后一个提交就忽略此前未经验证的代码。

独立[真实模型工作流](../.github/workflows/real-models.yml)只对旧模型任务／Worker／验收接入及其依赖触发，普通 examples README 不触发；v3收费批次、scale性能和阶段质量验收继续按各自指南执行，不包含在 `full` 的免费检查中。
