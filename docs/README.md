# 文档索引

第一次阅读从[项目入口](../README.md) → [核心概念](concepts.md) → [验证指南](verification.md)开始。

## 当前开发与操作

- [当前范围](product/README.md)：路线、保留能力和未完成事项的唯一导航。
- [S2 使用](agent-v3-support-agent.md)、[云端运行](agent-v3-cloud-batch.md)：准备、启动与查询方案。
- [Run API](agent-v3-runs.md)、[业务快照](agent-v3-business.md)、[受监管运行时](agent-v3-runtime.md)、[供应商审计](agent-v3-provider-audit.md)：按领域查阅。
- [开发环境](development.md)、[代码规范](code-standards.md)、[贡献指南](../CONTRIBUTING.md)。

## 兼容能力

[Jobs 指南](jobs-guide.md)、[原有架构](architecture.md)、[故障语义](failure-semantics.md)、[任务扩展](task-extension.md)、[可观测性](observability.md)描述仍保留的任务底座。Redis 事件、本地模型任务和观测组件按需启动，不是运行 S2 的额外必需框架。

## 决策与证据

- [ADR 索引](adr/README.md)：已接受契约和明确的取代关系。
- [路线 v3](plans/agent-execution-roadmap-v3.md)：未来阶段规划，不是功能完成清单。
- [S1 报告](evidence/agent-v3-s1-delivery-2026-09-17.md)、[S2 报告](evidence/agent-v3-s2-delivery-2026-09-17.md)：历史冻结版本的云端结果。
- [本次阶段记录](evidence/reviewable-agent-release.md)：路线判断、清理依据与本次验证。
- [性能基线](benchmark.md)、[可靠性报告](reliability-report.md)：历史环境和限制仍适用，不推导本次性能。

旧 PRD、路线候选和实施流水保留用于溯源。只在相关决策或回归调查时读取；版本号高不等于功能全部交付。
