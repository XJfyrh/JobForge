# AGENTS.md

适用于整个 JobForge 仓库。

## 修改前阅读

先读[文档导航](docs/README.md)、[产品契约索引](docs/product/README.md)、[ADR 索引](docs/adr/README.md)、[编码规范](docs/code-standards.md)、[贡献指南](CONTRIBUTING.md)和[开发指南](docs/development.md)。按任务阅读对应版本 PRD/ADR 与代码/测试；基础可靠性合同见 [PRD v0.1](docs/product/JobForge_PRD_v0.1.md)，实现状态以[当前状态](docs/status.md)为准。

## 事实来源与不变量

- 版本化 PRD 定义产品和验收合同，已接受 ADR 补充架构决策；状态页只记录实现。冲突时停止扩大改动，先修正文档或提出决策。
- 交付为 at-least-once；外部副作用须业务幂等，不得暗示 exactly-once。
- PostgreSQL 是 P0 唯一事实源；JetStream 只能通过 outbox 作为 P1 适配器。
- Claim 在一个事务更新 owner、lease、attempt、fencing token 和 state；Heartbeat、Complete、Fail 核验 job/Run、owner、token 与允许状态。陈旧结果返回 `STALE_LEASE`，不得覆盖新状态。
- 状态转换集中在 domain/service 层，transport handler 保持轻薄；Go 保有 Run 执行权，Python 只执行登记单步。
- 只运行预注册 Handler/adapter；禁止任意代码入口，不记录秘密或完整敏感 payload。生产 registry 仅含 `support-fixed-v1`、`support-agent-v1`，测试 adapter/origin/fault hook 与 gold 不进入生产镜像。

## 修改要求

交付可运行的最小业务闭环，不为假设场景增加抽象或兜底。修改状态、错误码、指标、公开 API/Proto 或故障语义时同步文档和测试；可靠性、公开契约或关键依赖决策通过新 ADR，不静默改写历史。数据库只新增 versioned migration，生成代码从源重新生成。

政策 Markdown、fixture、gold、manifest、原始证据和机器报告属于数据/契约输入，不能当普通文档重排或格式化。历史 PRD/ADR 保留正文语义。

## 验证与汇报

按[测试指南](docs/tests.md)运行与改动相称的检查；核心可靠性使用真实 PostgreSQL，并发修改必须通过 `go test -race ./...`。每个 PR 的[现有 8 项 CI](.github/workflows/ci.yml)均须通过。已通过且没有新风险的检查不重复。

Windows 集成测试前必须 `docker compose -f deploy/compose.yaml up -d postgres` 并设置 `JOBFORGE_TEST_DSN=postgres://jobforge:jobforge@localhost:5433/jobforge?sslmode=disable`。SDK 要安装并设置 `JOBFORGE_TEST_PYTHON`；真实 Linux 进程使用固定镜像、`--init` 与专用测试环境开关；同一 DSN 只允许一个可能清理库的测试进程。具体命令及故障/审计/观测/模型层路由由测试指南维护。

明确区分实际通过、失败、未适用和无法运行。平台/依赖/专用环境造成的 skip 不计验收通过；合成模型/向量不能替代真实业务或云端验收。unknown/full hold 不记成零费，observed usage 不称已结算费用。提交前检查 staged diff、秘密、迁移安全和文档链接。
