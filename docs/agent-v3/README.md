# Agent v3 指南

按使用顺序阅读；实现与验收状态统一见[当前状态](../status.md)。

S5工程能力与正式模型质量已分别完成验收证据，包含实际人工审批及唯一回执故障恢复；见[当前状态](../status.md)和[最终结果](../evidence/agent-v3-s5-closeout-2026-10-16.md)。历史失败、停止批次和原分母继续保留。

1. [业务依赖](business.md)：快照、真实 embedding 和政策检索。
2. [Run 接入](runs.md)：控制服务、SDK、执行权和预算排障。
3. [运行时](runtime.md)：固定 Linux Worker、manifest、秘密和进程屏障。
4. [批次操作](cloud-batch.md)：固定流程部署、有限预算、启动与导出。
5. [动态 Agent](support-agent.md)：只读工具选择与方案。
6. [恢复](recovery.md)：已提交前缀、自然接管和有限实验工具。
7. [人工审批](approval.md)：稳定审批身份、结论记录、独立效果与回执优先恢复。
8. [页面与运维](operations.md)：任务页面、有限 Trace、指标/告警、内容过窗与双库停写恢复。
9. [S5 验收矩阵](s5-acceptance.md)：免费证据、收费审查、冻结及正式20案的独立门槛。
10. [S5 候选 v5 交接说明](s5-v5-candidate-handover.md)：schema 13 的实施状态、已验证范围、剩余工作与已知陷阱。

修改内部接缝时再看[协议与时钟](executor-protocol.md)、[受控 HTTP](authorized-http.md)和[供应商审计](provider-audit.md)。验证命令集中在[测试指南](../tests.md)，结果进入[证据索引](../evidence/README.md)。
