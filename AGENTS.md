# AGENTS.md

本文件适用于整个 JobForge 仓库，供 Codex 等编码 Agent 使用。

## 任务开始时必读

Agent 在执行任何修改任务前，必须先读取以下关键文件以获取上下文：

| 文件 | 用途 |
|------|------|
| `docs/product/JobForge_PRD_v0.1.md` | 产品需求与验收语义 |
| `docs/adr/README.md` | 架构决策记录索引 |
| `docs/code-standards.md` | 编码规范与风格要求 |
| `CONTRIBUTING.md` | 贡献流程与协作约定 |
| `docs/development.md` | 本地开发与构建指南 |

若任务涉及特定领域，还需额外读取对应的 ADR 文件（`docs/adr/0NNN-*.md`）。

## 事实来源

1. `docs/product/JobForge_PRD_v0.1.md` 定义产品边界与验收语义。
2. 已接受 ADR 定义 PRD 未覆盖的架构决策；新 ADR 只能取代旧 ADR，不能静默改写历史。
3. 测试和代码必须实现上述契约。若三者冲突，停止扩大改动，先修正文档或提出决策。

## 不可破坏的不变量

- 交付保证是 at-least-once，禁止暗示 exactly-once；外部副作用必须业务幂等。
- PostgreSQL 是 P0 唯一事实源，JetStream 只能通过 outbox 作为 P1 事件适配器。
- Claim 在一个事务中更新 owner、lease、attempt、fencing token 与 state。
- Heartbeat、Complete、Fail 必须匹配 job、owner、fencing token 和允许的当前状态。
- 陈旧 Worker 结果必须返回 `STALE_LEASE`，不得覆盖新状态。
- 状态转换集中在 domain/service 层，transport handler 保持轻薄。
- 核心可靠性测试使用真实 PostgreSQL，并发修改必须通过 `go test -race ./...`。
- 只运行预注册 Handler；不得引入任意代码执行入口或记录秘密、完整敏感 payload。

## 修改要求

- 先阅读相关 PRD、ADR、代码和测试，再做最小范围修改。
- 遵守 `docs/code-standards.md` 和 `CONTRIBUTING.md`。
- 数据库变更只新增 versioned migration，不改写已应用 migration。
- 生成代码不得手工编辑；修改其源文件或生成配置。
- 新增或改变状态、错误码、指标、公开 API、Proto 字段或故障语义时，同步更新文档与测试。
- 影响可靠性语义、公开契约或关键依赖的决策必须通过 ADR。

## 验证与汇报

- 运行与修改范围相称的格式、lint、单元、集成、故障和 race 检查。
- Windows 上运行集成测试（`go test ./tests/integration/...`）前，必须先执行 `docker compose -f deploy/compose.yaml up -d postgres` 并设置 `JOBFORGE_TEST_DSN=postgres://jobforge:jobforge@localhost:5433/jobforge?sslmode=disable`（testcontainers 不支持 Windows 上的 Docker Desktop rootless/WSL2 后端；Linux CI 无需该变量，testcontainers 会自动启动临时 PostgreSQL）。
- 其中机械检查子集（golangci-lint、`go test -race ./...`、ruff、mypy、SQLFluff 历史基线校验、`sqlfluff lint migrations`、buf lint）由 [.github/workflows/ci.yml](.github/workflows/ci.yml) 在每个 Pull Request 上强制执行，检查项清单与 [CONTRIBUTING.md](CONTRIBUTING.md) 验证要求保持一致，详见 [docs/development.md](docs/development.md) 的 “CI 质量门禁” 一节。
- Python SDK 单测与真实 HTTP 跨语言契约也属于 CI 门禁；安装 SDK 后设置 `JOBFORGE_TEST_PYTHON`，否则 Go 契约测试的 skip 不计通过。
- 可观测配置变更还需通过 promtool 配置/告警规则测试，以及 Grafana 仪表盘生成一致性检查；这些检查在 PR CI 中执行。依赖真实模型的验收在独立工作流与本地真实模型层运行，不用替身替代。
- Agent v3 S0 探针的确定性 Python guardrails、Linux 类型检查和独立容器内 Go race/真实进程故障均由 CI 执行；常规 Go 测试因未启用专用进程环境产生的 skip 不计验收通过。模型协议探针使用的工具 fixture 不代表真实业务工具已验收。
- S1-C1 v2共同fixture随Go/Python测试执行；Go/Python共享CLOCK_BOOTTIME由同一固定Linux镜像内的`clock.test`实际验证。Windows非Linux分支或协议fixture不能替代正式Worker进程/真实云端验收。
- S1-C2授权HTTP适配随`python/tests`进入Linux CI；Windows本地使用`tools/agenthttpcheck/Dockerfile`验证实际BOOTTIME和回环TCP。协调者、向量和供应商响应是测试替身，不能据此宣称持久IPC或真实云端通过。
- S1-C3b运行时使用`tools/agentruntimecheck/Dockerfile`的`process-check`与`integration-check`，由`agent-runtime-contract` CI job运行真实Linux进程/race及真实PG/gRPC联合检查；Python全套仍由`python-lint`执行。Windows必须运行相同Linux镜像并带`--init`；缺少`JOBFORGE_RUNEXECUTOR_PROCESS_TESTS`或`JOBFORGE_RUNEXECUTOR_INTEGRATION_TESTS`导致的skip不计通过。联合测试先启动本地postgres并设置DSN，同一DSN只允许一个可能清理数据库的测试进程。
- 生产registry仅登记support-fixed-v1；合成adapter与固定回环供应商origin仅在专用测试target构建时安装，不得进入生产Dockerfile或通过运行时开关启用。正式输入/manifest/全部profile必须匹配固定executor_version；Go保留唯一执行权，ACK/Wait/EOF/Join/组消失屏障不可用替身成功信号代替。support共同schema/fixture与离线开发数据锚校验由CI执行；注册能力不等于收费profile启用或真实云端验收。复现与分层边界见[运行时指南](docs/agent-v3-runtime.md)。
- 不得把缺少代码、服务或依赖误报为检查通过；明确区分已运行、未适用和无法运行。
- 提交前检查 staged diff、敏感信息、迁移安全和文档链接。
