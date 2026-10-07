# 当前状态与下一步

更新于 2026-10-07。已接受阶段为 Agent v3 S4（[PR #60](https://github.com/XJfyrh/JobForge/pull/60)）；S5 实施分支正在审查。本页维护实现状态；契约状态见[产品索引](product/README.md)和[ADR 索引](adr/README.md)，分层结果见[证据索引](evidence/README.md)。

| 阶段 | 已实现能力 | 实际验收 |
|---|---|---|
| S0 | 执行器/模型协议可行性探针、v3 设计 | 合成工具与进程探针，范围见[历史证据](evidence/agent-v3-s0-review-2026-09-16.md) |
| S1 | 独立业务快照/政策检索、Run 账本、固定流程、供应商审计 | [40 案真实执行](evidence/agent-v3-s1-delivery-2026-09-17.md)，业务 11/40，安全硬失败 0；固定流程开发基线 |
| S2 | 由模型选择只读工具、累计来源、一次纠错 | [40 案真实验收](evidence/agent-v3-s2-delivery-2026-09-17.md)，业务 37/40，安全硬失败 0；另有真实响应截断注入 |
| S3 | 复用已提交步骤、自然租约接管、未提交步骤有条件重做 | [固定 11 项真实验收](evidence/agent-v3-s3-cloud-2026-10-04.md)，5 个接管机制通过；8 个完整方案中 7 个业务通过，DEV-035-C 失败；3 个按计划取消的对照运行（H0） |
| S4 | 人工审批、Go结论记录、独立动作许可、终态效果核对与回执优先retry | [固定 10 个源样本及 2 个后继](evidence/agent-v3-s4-cloud-2026-10-05.md)，原方案业务质量 10/10、机制 10/10，7 条唯一业务回执；真实层获独立接受 |
| S5 | 同源页面、有限 OTLP span/link、低基数指标与面板、7日终态内容清理、双库停写恢复；独立公平对照 profile 与评分工具 | [免费机制](evidence/agent-v3-s5-free-2026-10-07.md)通过；[Agent开发回归](evidence/agent-v3-s5-dev-agent-2026-10-07.md)方案及完整证据37/40，四类硬失败0；固定对照及正式20案待运行，未见集未创建/未打开 |

任务页面支持审阅方案，批准后保存处理结论并更新工单标记，实际结果可查业务回执。角色和状态码见[审批指南](agent-v3/approval.md)。默认部署没有收费 profile；启用模型批次需登记不可变 profile、预算和独立凭据。旧 Job API/Ollama 示例使用[独立维护路径](legacy/README.md)。

S5 的剩余范围和门槛见[验收矩阵](agent-v3/s5-acceptance.md)。免费机制通过不替代真实 DeepSeek 方案质量、恢复成本对照及正式20案验收。

## 限制与未结事项

| 范围 | 未结事项与影响 | 证据 |
|---|---|---|
| 旧 Job Claim 性能 | W4 相对历史基线超过 15% 回退门槛，仍待同环境复验/基线决策；不能用后续不同口径数据认定问题已解决 | [原基准](archive/benchmark-history.md#历史微基准绝对值核对)、[v0.6 审查](archive/agent-rag-review.md) |
| 旧 Job 取消控制流 | AT-25 的可选 ControlStream 尚未实现，用例 skip；已接受的 heartbeat 取消信号 p95≤6s 不能改称推送 1s 保证 | [ADR-0008](adr/0008-cancel-control-channel-heartbeat.md)、[历史可靠性](archive/reliability-history.md) |
| 旧远程 Ollama | 自配远程 Ollama 的连通/鉴权/完整任务路径未验收；v3 的真实 DeepSeek 验收单列，不受此项概括 | [旧模型接入](legacy/real-tasks.md)、[历史审查](archive/agent-rag-review.md) |
| 生产运维 | S5 已有有界内容清理、停写新库恢复与开发观测演练；尚无生产容量治理、在线跨库备份/PITR/HA、长期 Trace 留存或外部告警验收 | [v3 操作](agent-v3/operations.md)、[免费机制](evidence/agent-v3-s5-free-2026-10-07.md) |
| 开发集质量 | S1 的 RQ-06 检索未命中、S2 的 DEV-031/032/033 协议失败、S3 的 DEV-035-C 业务失败均保留；现有公开开发集不能证明未见保留集泛化 | [S1 检索](evidence/agent-v3-s1-support-2026-09-16.md)、[S2](evidence/agent-v3-s2-delivery-2026-09-17.md)、[S3](evidence/agent-v3-s3-cloud-2026-10-04.md) |

费用中的 known 是按冻结费率与 usage 计算的保守账本值，不能称供应商已结算费用；unknown/full hold 不是零费用。各批结果和历史冻结账户以原机器证据为准。
