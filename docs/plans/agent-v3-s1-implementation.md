# S1 实施计划与证据索引

- 起点：`8708308`，S0已完成；2026-09-16开始S1。
- 规范：[PRD v0.8](../product/JobForge_PRD_v0.8.md)、[ADR-0016草案](../adr/0016-business-snapshots-and-policy-retrieval.md)。保持路线v3全范围，切片合并不等于阶段完成。

| 顺序 | 产物 | 验收/合并条件 | 当前状态 |
|---|---|---|---|
| 契约 | 业务快照、工具HTTP、pgvector与准备身份 | 独立上下文审查；ADR按PR流程接受 | 评审准备 |
| S1-A | 独立业务服务、迁移/角色、40例开发数据、Python工具适配、索引准备与搜索 | 真PG/HTTP、tenant/版本/大小边界、固定真实embedding与20查询、重启持久性；适用CI | 未实现 |
| S1-B | 最小Run/lease/内部协议、API/SDK、持久调用账本 | 有效执行权、并发预留、重发身份、unknown占额、取消/超时；不新增调度语义 | 未实现 |
| S1-C | DeepSeek固定profile、受监管执行器、合理固定流程、评分器 | 实际云端与工具链；40开发例全量报告；方案与写入分开；费用硬上限 | 未实现 |

实施目录意向：`internal/business`与独立命令保存业务领域/服务/PG/HTTP；`migrations/business`维护业务迁移；Python执行器与工具适配不依赖队列存储；数据与gold分别打包。公开源schema、具体目录和生成工具在对应实现PR确定，避免复制多套同义契约。

关键依赖顺序：业务snapshot先于Run绑定；有效Run lease和持久账本先于任何收费调用；方案进入awaiting_approval需要原子保存最小审批绑定并释放lease，即便批准/写入实现留在S4。S1不能将有写建议的proposal误标为succeeded/applied。

真实模型与确定性检查分开记录。历史W4失败、AT-25跳过、远程模型和生产留存未验收保持原结论。技术选择与执行证据另存于仓库外 `E:\JobForge-notes\2026-09-16-agent-v3-s1`；不归档凭据、完整模型输入输出或秘密。
