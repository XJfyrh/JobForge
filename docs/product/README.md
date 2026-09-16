# 当前产品契约

当前增量为 [PRD v0.6](JobForge_PRD_v0.6.md)，覆盖通用接入、真实 Agent/RAG 任务与可观测闭环。原有 PageWise 集成验收被两个通用真实任务替代；没有旧任务兼容、迁移或弃用安排。

v0.1～v0.5 保留为历史契约，可靠性不变量仍生效；其中历史 PageWise 示例不再作为当前实现或验收要求。当前执行与未完成事项见[实施记录](../agent-rag-progress.md)。

## Agent v3 增量

Agent v3 使用独立增量合同；接受设计、完成实现和真实验收分别记录，不能把版本号当作当前实现已通过。

| 版本 | 范围 | 状态 |
|---|---|---|
| [v0.7](JobForge_PRD_v0.7.md)～[v0.9](JobForge_PRD_v0.9.md) | v3 路线、业务快照、Run 接纳与调用账本 | 已接受；阶段状态见[实施记录](../agent-v3-progress.md) |
| [v0.10](JobForge_PRD_v0.10.md) / [v0.11](JobForge_PRD_v0.11.md) | DeepSeek 固定流程、执行器确认与退出 | 已接受；support 切片已合并，真实40案未验收 |
| [v0.12](JobForge_PRD_v0.12.md) | provider 持久审计与批次停发 | 合同已接受；实现与验收另行交付 |
| [v0.13](JobForge_PRD_v0.13.md) | 首批可信 profile、快照约束与40行串行驱动 | **Accepted（PR #49 合并生效）；未实现/未启用** |
| [v0.14](JobForge_PRD_v0.14.md) | S1 收尾新批次与新增累计 5 CNY 授权 | **Accepted（PR #53 合并生效）；不恢复原批，实施与真实验收另行记录** |
