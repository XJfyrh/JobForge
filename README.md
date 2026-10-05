# JobForge

JobForge 是以 PostgreSQL 为事实源的持久 Agent 执行后端。Go 管理 Run、租约、步骤提交和预算；受监管的 Python 执行器按登记策略读取业务证据、调用模型并生成可审阅方案。交付保证为 at-least-once，外部副作用须业务幂等。

当前主线是 Agent v3，已交付固定客服流程、有界 Agent 和已确认步骤恢复（S1–S3）。审批写入与产品页面（S4/S5）尚未实施。实现、真实验收结果和限制统一见[当前状态](docs/status.md)。

## 从这里开始

- [文档导航](docs/README.md)：按接入、开发、排障和契约查找内容。
- [本地开发](docs/development.md)：准备工具、构建，启动无收费 profile 的控制服务。
- [Run API 与 SDK](docs/agent-v3/runs.md)：提交、查询、取消、重试和调用账本。
- [架构与执行权](docs/architecture.md)：控制库、业务库、Go/Python 边界。
- [贡献与验证](CONTRIBUTING.md)：协作要求和按改动选择检查。

已有 Job API 及本地 Ollama Agent/RAG 示例继续可用，使用[旧 Job 路径](docs/legacy/README.md)；它们与 v3 的 Run 服务分开启动。

代码采用 [Apache-2.0](LICENSE)。安全问题按 [SECURITY.md](SECURITY.md) 私密报告。
