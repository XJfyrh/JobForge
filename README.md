# JobForge

JobForge 是一个面向 Agent、RAG 与通用后台任务的分布式任务编排平台，目标是提供可恢复、可观测、可隔离的可靠执行底座。

> 当前状态：PRD v0.1（Draft），准备进入 S0 契约与状态机评审。仓库目前仅包含产品与工程规范，尚无可运行的产品代码。

## 核心边界

- 交付语义是 **at-least-once**，不宣称 exactly-once；外部副作用由业务幂等保护。
- PostgreSQL 是 P0 的唯一事实源，任务状态、租约和 attempt 必须在数据库事务中保持一致。
- Worker 的 Heartbeat、Complete 与 Fail 必须校验 owner 和 fencing token，拒绝陈旧写入。
- 先交付 lease、重试、DLQ、取消、崩溃恢复和故障验证，再考虑完整 UI 或事件扩展。
- 只运行预注册 Handler，不允许上传或执行任意代码。

## 计划中的组成

- Go 控制面、Scheduler、Worker Gateway 与 Worker Runtime
- PostgreSQL 队列内核与 versioned migrations
- gRPC Worker 协议和 HTTP 控制面
- Python SDK 与真实 Agent/RAG 集成演示
- OpenTelemetry、metrics、pprof、故障注入和 benchmark

## 文档导航

| 文档 | 用途 |
|---|---|
| [产品需求文档](docs/product/JobForge_PRD_v0.1.md) | 产品边界、状态机、接口、验收标准与里程碑 |
| [代码与注释规范](docs/code-standards.md) | Go、Python、SQL、Proto、测试和注释约定 |
| [开发环境](docs/development.md) | 本地检查工具、安装方式与验证命令 |
| [ADR 说明](docs/adr/README.md) | 架构决策的创建、接受和取代流程 |
| [贡献指南](CONTRIBUTING.md) | 分支、提交、评审与合并要求 |
| [安全策略](SECURITY.md) | 漏洞报告方式与敏感信息要求 |

## 协作方式

仓库采用 GitHub Flow 与 Conventional Commits。开始修改前请阅读 [CONTRIBUTING.md](CONTRIBUTING.md)；改变可靠性语义、公开接口或关键技术边界时，必须先提交 ADR，并同步更新 PRD、测试和相关文档。

## 许可证

本仓库当前未提供开源许可证。除适用法律另有规定或权利人另行书面授权外，保留全部权利，不授予复制、修改、分发或使用本项目的许可。
