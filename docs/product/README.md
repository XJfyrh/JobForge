# 当前产品范围

当前主线是 **S2 有界售后 Agent**：[PRD v0.16](JobForge_PRD_v0.16.md)、[ADR-0024](../adr/0024-bounded-support-agent.md)。输入工单，按需只读取证，输出有来源的待审方案。历史开发验收 37/40，安全硬失败 0，见[S2 报告](../evidence/agent-v3-s2-delivery-2026-09-17.md)。

本阶段优先改善可运行性、审阅体验和可靠性，不增加框架，不扩展到其他项目。S3 恢复成本对比、S4 审批写入、S5 保留集与展示仍未交付；当前不能宣称“自动处理完售后工单”。

[PRD v0.6](JobForge_PRD_v0.6.md) 的 Jobs / RAG / 产物 / 事件能力作为兼容底座保留。原有 PageWise 专属集成已被通用任务替代；路线 v1/v2 是历史候选，v3 是当前规划。旧代码不是仅凭名称即可删除的实验：仍有 CLI、SDK、Compose 与 CI 调用。

v0.1～v0.5 保留为历史契约，可靠性不变量仍生效；其中历史 PageWise 示例不再作为当前实现或验收要求。当前执行与未完成事项见[实施记录](../agent-rag-progress.md)。

## Agent v3 增量

Agent v3 使用独立增量合同；接受设计、完成实现和真实验收分别记录，不能把版本号当作当前实现已通过。

| 版本 | 范围 | 状态 |
|---|---|---|
| [v0.7](JobForge_PRD_v0.7.md)～[v0.9](JobForge_PRD_v0.9.md) | v3 路线、业务快照、Run 接纳与调用账本 | 已接受；阶段状态见[实施记录](../agent-v3-progress.md) |
| [v0.10](JobForge_PRD_v0.10.md) / [v0.11](JobForge_PRD_v0.11.md) | DeepSeek 固定流程、执行器确认与退出 | 已接受；[S1完整40案真实验收已闭合](../evidence/agent-v3-s1-delivery-2026-09-17.md) |
| [v0.12](JobForge_PRD_v0.12.md) | provider 持久审计与批次停发 | 合同与审计实现已交付；真实S1范围见最新报告 |
| [v0.13](JobForge_PRD_v0.13.md) | 首批可信 profile、快照约束与40行串行驱动 | Accepted；启动/评分实现与真实40案随PR #51交付 |
| [v0.14](JobForge_PRD_v0.14.md) | S1 收尾新批次与新增累计 5 CNY 授权 | **Accepted（PR #53 合并生效）；不恢复原批，实施与真实验收另行记录** |
| [v0.15](JobForge_PRD_v0.15.md) | 保留历史未知费用全额预留后的独立新批准入 | Accepted（PR #54 合并生效）；历史hold保留，完整40案证据见S1报告；后续预算按维护者最新授权记录 |

| [v0.16](JobForge_PRD_v0.16.md) | S2 有界 Agent、动态取证与方案 | 已实现；真实开发验收 37/40，未开放审批写入 |
