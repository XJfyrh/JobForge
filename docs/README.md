# 文档导航

第一次了解项目，从[业务说明](support-overview.md)或[三分钟演示](demo.md)开始；想直接运行，进入[免费 Run](quickstart.md)。下表按读者任务选择材料。

| 要做什么 | 从哪里开始 | 按需深入 |
|---|---|---|
| 理解项目 | [业务说明](support-overview.md) | [工程亮点](engineering.md)、[架构](architecture.md) |
| 首次体验 | [免费Run](quickstart.md) | [SDK/HTTP示例](../examples/run-api/README.md)、[公开演示](demo.md) |
| 判断业务与部署范围 | [业务边界](support-overview.md) | [部署参考](deployment.md)、[工程亮点](engineering.md)、[术语表](glossary.md) |
| 准备环境、构建 | [开发指南](development.md) | [贡献要求](../CONTRIBUTING.md)、[编码规范](code-standards.md) |
| 接入 Run API/SDK | [Run 指南](agent-v3/runs.md) | [Python SDK](../sdk/python/README.md)、[OpenAPI](../api/run/v2/openapi.yaml)、[Proto](../proto/README.md) |
| 准备业务数据与检索 | [业务依赖](agent-v3/business.md) | [开发语料](../examples/support-agent/README.md) |
| 部署执行器 | [运行时](agent-v3/runtime.md) | [受控 HTTP](agent-v3/authorized-http.md)、[执行器协议](agent-v3/executor-protocol.md) |
| 运行有界模型批次 | [批次操作](agent-v3/cloud-batch.md) | [动态 Agent](agent-v3/support-agent.md)、[恢复](agent-v3/recovery.md) |
| 准备 S5 开发回归 | [共享工作流](../tools/support_s5/host/README.md) | [启动审查与健康检查](adr/0039-s5-startup-review-and-automatic-health.md) |
| 排查调用与费用 | [供应商审计](agent-v3/provider-audit.md) | [Run 故障判断](agent-v3/runs.md#调用预算与故障判断) |
| 使用任务页面、观测及恢复数据 | [页面与运维](agent-v3/operations.md) | [S5 验收](agent-v3/s5-acceptance.md)、[公平对照工具](../tools/support_s5/README.md) |
| 验证改动 | [测试指南](tests.md) | [Windows 手册](runbooks/windows-acceptance.md)、[基准](benchmark.md) |
| 核对交付与验收 | [当前状态](status.md) | [证据索引](evidence/README.md)、[可靠性证据](reliability-report.md)、[离线评分工具](../tools/support_evaluation/README.md) |
| 使用既有 Job API | [旧 Job 指南](legacy/README.md) | 启动、扩展 Handler、事件与可观测性 |
| 查历史决策过程 | [归档索引](archive/README.md) | 已替代路线、阶段记录、长报告 |

[Agent v3 专题索引](agent-v3/README.md)汇总接入和运行指南。开发环境的 Bash 与 PowerShell 命令在[开发指南](development.md)分别列出，Windows 专属诊断在[运行手册](runbooks/windows-acceptance.md)。

## 文档用途

| 材料 | 回答的问题 |
|---|---|
| 首页、业务说明、演示、英文摘要 | 项目能帮谁解决什么问题，有什么可见结果？ |
| Quickstart、API/SDK 示例 | 怎样完成第一次操作？需要哪些配置？ |
| Agent 指南、架构与工程亮点 | 怎样接入、运行，关键机制如何工作？ |
| 部署参考与 runbooks | 环境是否适合，怎样部署、排障和恢复？ |
| [产品契约](product/README.md)、[ADR](adr/README.md) | 哪些行为必须满足，为什么这样设计？ |
| [验收证据](evidence/README.md) | 某个版本在什么环境下得到了什么结果？ |
| [历史资料](archive/README.md) | 当时怎样推进，哪些方案被替代？ |

当前交付在状态页维护；指南按任务更新；完成的计划和交接移入归档。政策、fixture、gold 与机器报告按数据和契约输入维护。
