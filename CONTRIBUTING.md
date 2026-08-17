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
.venv/bin/python tools/check_sqlfluff_baseline.py
.venv/bin/sqlfluff lint migrations
.tools/bin/buf lint
```

上述 golangci-lint、`go test -race ./...`、ruff（check + format）、mypy、SQLFluff 历史基线校验、`sqlfluff lint migrations` 和 buf lint 均由 [CI 工作流](.github/workflows/ci.yml) 在每个 Pull Request 上强制执行（见 [开发环境](docs/development.md) 的 “CI 质量门禁” 一节），本地清单与 PR 门禁保持一致。`.sqlfluffignore` 只冻结已应用 migration 的既有格式债务；禁止用新增 ignore 条目绕过新 migration 的检查。

Windows 对应的 Python 可执行文件位于 `.venv\Scripts`，Go/Buf 工具位于 `.tools\bin`。可靠性集成测试必须使用真实 PostgreSQL，核心行为不能只由 mock 验证。

Pull Request 至少应包含正常路径和一个相关失败路径的测试；并发相关变更必须通过 race 检测，接口变更必须包含契约或兼容性验证。

## 安全问题

不要在公开 Issue 或 Pull Request 中提交未公开漏洞、凭据或敏感数据。请遵循 [SECURITY.md](SECURITY.md) 的私密报告流程。
