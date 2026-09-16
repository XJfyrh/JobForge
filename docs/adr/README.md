# Architecture Decision Records

现有v0.6实现的增量：[ADR-0011 通用结果与模型业务适配器](0011-general-task-results-and-model-adapters.md)、[ADR-0012 任务观测与 OTLP](0012-task-observability-and-otlp.md)，随 [PR #33](https://github.com/XJfyrh/JobForge/pull/33) 接受。

ADR 用于记录会长期影响 JobForge 架构、公开契约或可靠性语义的决策。PRD 已明确的边界不需要重复创建 ADR；对其补充、取舍或偏离必须记录。

## 文件命名

- 使用 `NNNN-short-kebab-title.md`，编号从 `0001` 递增。
- `0000-template.md` 仅作为模板，不代表一项决策。
- 标题与正文以中文为主；代码、协议名、状态和错误码保留英文。

## 状态

- `Proposed`：正在评审，尚不能作为实现依据。
- `Accepted`：已接受，是当前事实来源。
- `Rejected`：已评审但未采用，保留决策背景。
- `Superseded`：已由更新 ADR 取代，并必须链接新 ADR。

## 生命周期

1. 复制模板并分配下一个未使用编号。
2. 写明上下文、决策、替代方案、后果、兼容性和验证方式。
3. 在 Pull Request 中完成评审；合并后将状态设为 `Accepted` 或 `Rejected`。
4. 不修改已接受 ADR 的历史结论。决策变化时新增 ADR，并把旧 ADR 标记为 `Superseded`。

## 必须创建 ADR 的变更

- 投递保证、状态机、lease、fencing、幂等、重试、DLQ 或取消语义；
- HTTP、gRPC、SDK 或持久化契约的重大变化；
- 数据库事实源、调度模型、关键依赖和部署边界；
- 安全模型、租户隔离或兼容性政策；
- 与 PRD 已固定边界不一致的实现选择。

## 已接受 ADR 索引

路线 v3 的S0详细决策已随 [PR #35](https://github.com/XJfyrh/JobForge/pull/35) 接受：[ADR-0013 单一 Run 与步骤提交](0013-durable-agent-run-and-step-commit.md)、[ADR-0014 Python 执行器与额度](0014-supervised-python-executor-and-call-budget.md)、[ADR-0015 审批与业务回执](0015-approved-business-actions-and-receipts.md)。接受范围为新版本设计，S1～S5仍未实现；现有v0.6运行代码与验收保持原边界。

当前通用 Agent/RAG 增量对应 [PRD v0.6](../product/JobForge_PRD_v0.6.md)，实际验收范围与保留限制见[审查记录](../agent-rag-review.md)。

S1实现契约评审：[PRD v0.8](../product/JobForge_PRD_v0.8.md)、[ADR-0016 业务快照与政策检索](0016-business-snapshots-and-policy-retrieval.md)。ADR-0016仍为Proposed，不提前作为实现依据。

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
