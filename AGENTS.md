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
- 不得把缺少代码、服务或依赖误报为检查通过；明确区分已运行、未适用和无法运行。
- 提交前检查 staged diff、敏感信息、迁移安全和文档链接。
