# 产品契约索引

当前主线为 Agent v3，使用基础 PRD 与已接受增量共同定义合同。后续版本仅取代明确说明的范围，不能把全部旧合同视为失效。实现与验收结果统一见[当前状态](../status.md)。

| 版本 | 合同范围 |
|---|---|
| [v0.1](JobForge_PRD_v0.1.md) | 基础可靠性、状态机与 P0 边界 |
| [v0.2](JobForge_PRD_v0.2.md) | HA、租户与运维 |
| [v0.3](JobForge_PRD_v0.3.md) | 耐久事件与并发治理 |
| [v0.4](JobForge_PRD_v0.4.md) | 持久幂等效果与真实 Worker 崩溃 |
| [v0.5](JobForge_PRD_v0.5.md) | 任务目录与 Worker 执行契约 |
| [v0.6](JobForge_PRD_v0.6.md) | 既有 Job Agent/RAG 闭环 |
| [v0.7](JobForge_PRD_v0.7.md) | v3 Run/恢复/审批总体设计 |
| [v0.8](JobForge_PRD_v0.8.md) | 业务快照与政策检索 |
| [v0.9](JobForge_PRD_v0.9.md) | Run 接入、执行身份与账本 |
| [v0.10](JobForge_PRD_v0.10.md) | 固定模型流程与执行器 |
| [v0.11](JobForge_PRD_v0.11.md) | 观察确认与退出 |
| [v0.12](JobForge_PRD_v0.12.md) | 供应商审计与停批 |
| [v0.13](JobForge_PRD_v0.13.md) | 可信批次登记与启动 |
| [v0.14](JobForge_PRD_v0.14.md) | S1 收尾累计授权 |
| [v0.15](JobForge_PRD_v0.15.md) | 保留未知费用的独立新批准入 |
| [v0.16](JobForge_PRD_v0.16.md) | 有界动态 Agent |
| [v0.17](JobForge_PRD_v0.17.md) | 已确认步骤恢复 |
| [v0.18](JobForge_PRD_v0.18.md) | S4 审批、受控写入与回执恢复 |

v0.1–v0.6 的 Job 可靠性与现行通用任务范围继续有效，历史 PageWise 示例的替代范围以 v0.6 为准。v0.7 的审批设计已接受，但对应写入能力尚未实现；不能从契约版本号推断交付。架构补充见[ADR 索引](../adr/README.md)，旧路线过程见[归档](../archive/README.md)。
