# 架构决策索引

ADR 记录 PRD 未覆盖的架构、可靠性与公开契约取舍。接受决策与完成实现分开：实现/验收见[当前状态](../status.md)，产品边界见[PRD 索引](../product/README.md)。

## 使用与新增

按任务阅读下表对应 ADR。新增决策使用 [0000-template](0000-template.md)，编号连续、文件名 `NNNN-short-kebab-title.md`，写明上下文、选择、后果、兼容性和验证。

`Proposed` 待审查，`Accepted` 为有效依据，`Rejected` 保存未采用原因，`Superseded` 指向后继。不静默改已接受历史结论；变化以新 ADR 明确取代范围。可靠性/状态机/租约/幂等、公开契约、事实源/调度、关键依赖、部署/安全边界或偏离 PRD 的选择需要 ADR。

## 决策列表

| 编号 | 标题 | 状态 | 日期 |
|------|------|------|------|
| [ADR-0001](0001-implementation-parameters.md) | 实现参数与通道模式 | Accepted | 2026-07-29 |
| [ADR-0002](0002-error-classification.md) | 错误分类与 HTTP/gRPC 映射 | Accepted | 2026-07-29 |
| [ADR-0003](0003-event-notification.md) | 事件通知机制（PostgreSQL LISTEN/NOTIFY） | Accepted | 2026-07-29 |
| [ADR-0004](0004-observability-stack.md) | 可观测性技术选型（OTel + Prometheus + pprof） | Accepted | 2026-07-29 |
| [ADR-0005](0005-scheduler-leadership-lease.md) | Scheduler 领导权租约（advisory lock + epoch fencing） | Accepted | 2026-08-08 |
| [ADR-0006](0006-durable-event-transport.md) | 耐久事件 transport 与 envelope（Redis Streams 首选） | Accepted | 2026-08-10 |
| [ADR-0007](0007-tenant-quota-atomic-counter.md) | 租户配额原子计数（派生计数表 + 事务内原子预留） | Accepted | 2026-08-10 |
| [ADR-0008](0008-cancel-control-channel-heartbeat.md) | 取消控制通道与 Heartbeat 参数（5s 默认，ControlStream 预留） | Accepted | 2026-08-10 |
| [ADR-0009](0009-demo-persistent-effects-and-real-crash-evidence.md) | Demo 持久业务效果与真实进程崩溃证据边界 | Accepted | 2026-08-17 |
| [ADR-0010](0010-task-type-catalog-and-worker-capability-binding.md) | 部署任务类型目录与 Worker 能力原子绑定 | Accepted | 2026-08-18 |
| [ADR-0011](0011-general-task-results-and-model-adapters.md) | 通用结果引用与预注册模型业务适配器 | Accepted | 2026-09-15 |
| [ADR-0012](0012-task-observability-and-otlp.md) | 任务完成口径与可选 OTLP 观测链路 | Accepted | 2026-09-15 |
| [ADR-0013](0013-durable-agent-run-and-step-commit.md) | 单一 Run 与持久步骤提交 | Accepted | 2026-09-16 |
| [ADR-0014](0014-supervised-python-executor-and-call-budget.md) | 受监管 Python 执行器与调用额度 | Accepted | 2026-09-16 |
| [ADR-0015](0015-approved-business-actions-and-receipts.md) | 审批、受控写入与业务回执 | Accepted | 2026-09-16 |
| [ADR-0016](0016-business-snapshots-and-policy-retrieval.md) | 独立业务快照与版本化政策检索 | Accepted | 2026-09-16 |
| [ADR-0017](0017-run-admission-and-call-ledger.md) | Run接纳、执行权与物理调用账本 | Accepted | 2026-09-16 |
| [ADR-0018](0018-deepseek-fixed-flow-and-executor.md) | DeepSeek固定流程与正式受监管执行器 | 部分Superseded by ADR-0019/0020；其余有效 | 2026-09-16 |
| [ADR-0019](0019-executor-confirmation-and-exit-contract.md) | 执行器观察确认与固定退出合同 | 新审计profile的hash/汇合部分Superseded by ADR-0020；其余有效 | 2026-09-16 |
| [ADR-0020](0020-provider-audit-and-batch-stop.md) | provider 审计报告、跨 FD 确认与首批停发 | Accepted | 2026-09-16 |
| [ADR-0021](0021-first-cloud-batch-admission-and-launcher.md) | 首批收费 profile 的可信登记、快照约束与串行启动器 | 部分 Superseded by ADR-0022；其余有效 | 2026-09-16 |
| [ADR-0022](0022-s1-closeout-cumulative-authorization.md) | S1 收尾新批次与累计授权 | 部分由 ADR-0023 取代；其余有效 | 2026-09-17 |
| [ADR-0023](0023-held-unknown-cross-batch-admission.md) | 保留未知费用后的独立新批准入 | Accepted | 2026-09-17 |
| [ADR-0024](0024-bounded-support-agent.md) | 有界售后 Agent | Accepted | 2026-09-17 |
| [ADR-0025](0025-confirmed-step-recovery.md) | 完整审计下的未提交步骤恢复 | Accepted | 2026-10-04 |
| [ADR-0026](0026-approval-actions-and-receipt-recovery.md) | 人工审批、签名动作与回执优先恢复 | Accepted（随本 PR 合并生效） | 2026-10-05 |
| [ADR-0027](0027-s5-observability-and-data-lifecycle.md) | S5 观测关联、内容留存与停写恢复 | Accepted（随本PR合并生效） | 2026-10-07 |
| [ADR-0028](0028-versioned-support-evidence-navigation.md) | 售后 Agent 证据导航的独立候选版本 | Accepted（随本PR合并生效） | 2026-10-07 |

## 增量适用范围

ADR-0018/0019 的确认形状按 ADR-0020 升级，仅适用于新审计 profile；其余流程与清理边界仍有效。ADR-0021 的首批授权由 0022/0023 增补；ADR-0024 仅对 S2 profile 调整固定图/终结判断及 S1 专属准入，ADR-0025 仅对 schema 3/S3 profile 增补有限未提交恢复与进程丢失分类。各精确取代条款以原 ADR 为准，历史正文和旧 profile 不升级。
