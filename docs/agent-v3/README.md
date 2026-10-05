# Agent v3 指南

按使用顺序阅读；实现与验收状态统一见[当前状态](../status.md)。

1. [业务依赖](business.md)：快照、真实 embedding 和政策检索。
2. [Run 接入](runs.md)：控制服务、SDK、执行权和预算排障。
3. [运行时](runtime.md)：固定 Linux Worker、manifest、秘密和进程屏障。
4. [批次操作](cloud-batch.md)：固定流程部署、有限预算、启动与导出。
5. [动态 Agent](support-agent.md)：只读工具选择与方案。
6. [恢复](recovery.md)：已提交前缀、自然接管和有限实验工具。

修改内部接缝时再看[协议与时钟](executor-protocol.md)、[受控 HTTP](authorized-http.md)和[供应商审计](provider-audit.md)。验证命令集中在[测试指南](../tests.md)，结果进入[证据索引](../evidence/README.md)。
