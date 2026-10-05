# 当前状态与下一步

更新于 2026-10-05。当前实现与验收阶段为 Agent v3 S4（[PR #60](https://github.com/XJfyrh/JobForge/pull/60)）。本页维护实现状态；已接受契约见[产品索引](product/README.md)和[ADR 索引](adr/README.md)，分层结果见[证据索引](evidence/README.md)。

| 阶段 | 已实现能力 | 实际验收 |
|---|---|---|
| S0 | 执行器/模型协议可行性探针、v3 设计 | 合成工具与进程探针，范围见[历史证据](evidence/agent-v3-s0-review-2026-09-16.md) |
| S1 | 独立业务快照/政策检索、Run 账本、固定流程、供应商审计 | [40 案真实执行](evidence/agent-v3-s1-delivery-2026-09-17.md)，业务 11/40，安全硬失败 0；固定流程开发基线 |
| S2 | 由模型选择只读工具、累计来源、一次纠错 | [40 案真实验收](evidence/agent-v3-s2-delivery-2026-09-17.md)，业务 37/40，安全硬失败 0；另有真实响应截断注入 |
| S3 | 复用已提交步骤、自然租约接管、未提交步骤有条件重做 | [固定 11 项真实验收](evidence/agent-v3-s3-cloud-2026-10-04.md)，5 个接管机制通过；8 个完整方案中 7 个业务通过，DEV-035-C 失败；3 个按计划取消的对照运行（H0） |
| S4 | 人工审批、Go结论记录、独立动作许可、终态效果核对与回执优先retry | [固定 10 个源样本及 2 个后继](evidence/agent-v3-s4-cloud-2026-10-05.md)，原方案业务质量 10/10、机制 10/10，7 条唯一业务回执；真实层获独立接受 |
| S5 | 未实施：产品页面与保留集验收 | 原路线见[归档路线](archive/plans/agent-execution-roadmap-v3.md)，尚无实现/验收结果 |

`awaiting_approval` 表示持久方案可供审阅。旧 schema 1–3 保持只读；新 S4 profile 的批准/动作语义见[审批指南](agent-v3/approval.md)。applied 只证明结论记录/工单标记已提交，不表示客户问题已解决。默认部署没有收费 profile；启用模型批次需登记不可变 profile、预算和独立凭据。旧 Job API/Ollama 示例使用[独立维护路径](legacy/README.md)。

下一阶段为 S5；页面与保留集须另行确定范围并验收，尚无实现或通过声明。

## 限制与未结事项

| 范围 | 未结事项与影响 | 证据 |
|---|---|---|
| 旧 Job Claim 性能 | W4 相对历史基线超过 15% 回退门槛，仍待同环境复验/基线决策；不能用后续不同口径数据认定问题已解决 | [原基准](archive/benchmark-history.md#历史微基准绝对值核对)、[v0.6 审查](archive/agent-rag-review.md) |
| 旧 Job 取消控制流 | AT-25 的可选 ControlStream 尚未实现，用例 skip；已接受的 heartbeat 取消信号 p95≤6s 不能改称推送 1s 保证 | [ADR-0008](adr/0008-cancel-control-channel-heartbeat.md)、[历史可靠性](archive/reliability-history.md) |
| 旧远程 Ollama | 自配远程 Ollama 的连通/鉴权/完整任务路径未验收；v3 的真实 DeepSeek 验收单列，不受此项概括 | [旧模型接入](legacy/real-tasks.md)、[历史审查](archive/agent-rag-review.md) |
| 生产运维 | 长期数据/Trace/指标留存、备份恢复和外部告警未覆盖；审计 30 日晚到窗口是权限合同，不等于存储 TTL | [审计边界](agent-v3/provider-audit.md#验证与运维边界)、[开发观测](legacy/observability.md) |
| 开发集质量 | S1 的 RQ-06 检索未命中、S2 的 DEV-031/032/033 协议失败、S3 的 DEV-035-C 业务失败均保留；现有公开开发集不能证明未见保留集泛化 | [S1 检索](evidence/agent-v3-s1-support-2026-09-16.md)、[S2](evidence/agent-v3-s2-delivery-2026-09-17.md)、[S3](evidence/agent-v3-s3-cloud-2026-10-04.md) |

费用中的 known 是按冻结费率与 usage 计算的保守账本值，不能称供应商已结算费用；unknown/full hold 不是零费用。各批结果和历史冻结账户以原机器证据为准。
