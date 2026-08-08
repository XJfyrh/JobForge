# Architecture Decision Records

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

| 编号 | 标题 | 状态 | 日期 |
|------|------|------|------|
| [ADR-0001](0001-implementation-parameters.md) | 实现参数与通道模式 | Accepted | 2026-07-29 |
| [ADR-0002](0002-error-classification.md) | 错误分类与 HTTP/gRPC 映射 | Accepted | 2026-07-29 |
| [ADR-0003](0003-event-notification.md) | 事件通知机制（PostgreSQL LISTEN/NOTIFY） | Accepted | 2026-07-29 |
| [ADR-0004](0004-observability-stack.md) | 可观测性技术选型（OTel + Prometheus + pprof） | Accepted | 2026-07-29 |
| [ADR-0005](0005-scheduler-leadership-lease.md) | Scheduler 领导权租约（advisory lock + epoch fencing） | Accepted | 2026-08-08 |
