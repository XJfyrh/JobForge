# 文档导航

当前开发主线是 Agent v3。先看[状态与下一步](status.md)，再按任务选择指南；产品契约和验收结果分别记录。

| 要做什么 | 从哪里开始 | 按需深入 |
|---|---|---|
| 理解项目 | [架构](architecture.md) | [产品契约索引](product/README.md)、[ADR 索引](adr/README.md) |
| 准备环境、构建 | [开发指南](development.md) | [贡献要求](../CONTRIBUTING.md)、[编码规范](code-standards.md) |
| 接入 Run API/SDK | [Run 指南](agent-v3/runs.md) | [Python SDK](../sdk/python/README.md)、[OpenAPI](../api/run/v2/openapi.yaml)、[Proto](../proto/README.md) |
| 准备业务数据与检索 | [业务依赖](agent-v3/business.md) | [开发语料](../examples/support-agent/README.md) |
| 部署执行器 | [运行时](agent-v3/runtime.md) | [受控 HTTP](agent-v3/authorized-http.md)、[执行器协议](agent-v3/executor-protocol.md) |
| 运行有界模型批次 | [批次操作](agent-v3/cloud-batch.md) | [动态 Agent](agent-v3/support-agent.md)、[恢复](agent-v3/recovery.md) |
| 排查调用与费用 | [供应商审计](agent-v3/provider-audit.md) | [Run 故障判断](agent-v3/runs.md#调用预算与故障判断) |
| 使用任务页面、观测及恢复数据 | [页面与运维](agent-v3/operations.md) | [S5 验收](agent-v3/s5-acceptance.md)、[公平对照工具](../tools/support_s5/README.md) |
| 验证改动 | [测试指南](tests.md) | [Windows 手册](runbooks/windows-acceptance.md)、[基准](benchmark.md) |
| 核对验收 | [证据索引](evidence/README.md) | [可靠性证据](reliability-report.md)、[离线评分工具](../tools/support_evaluation/README.md) |
| 使用既有 Job API | [旧 Job 指南](legacy/README.md) | 启动、扩展 Handler、事件与可观测性 |
| 查历史决策过程 | [归档索引](archive/README.md) | 已替代路线、阶段记录、长报告 |

[Agent v3 专题索引](agent-v3/README.md)汇总运行指南。`product/` 和 `adr/` 保存版本化契约；`evidence/` 保存验收快照和机器证据；`archive/` 不作为当前状态来源。
