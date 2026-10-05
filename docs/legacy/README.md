# 既有 Job API 与 Agent/RAG

此路径使用 `cmd/jobforge`、`/v1/jobs`、WorkerService 和本地 Ollama。它继续可用，与[Agent v3 Run](../agent-v3/README.md)的控制服务、执行器和业务库独立。

| 任务 | 指南 |
|---|---|
| 启动本地模型与真实任务 | [真实 Agent/RAG 任务](real-tasks.md) |
| 使用 API/SDK | [SDK 旧 Job API](../../sdk/python/README.md#旧-job-api)、[Job 契约](../product/JobForge_PRD_v0.5.md) |
| 演示持久结果与崩溃窗口 | [演示脚本](demo-script.md) |
| 新增预注册 Handler/业务产物 | [任务扩展](task-extension.md) |
| 配置目录、heartbeat、Redis 和运维 CLI | [运行配置](operations.md) |
| 判断重试/取消/陈旧结果 | [故障语义](failure-semantics.md) |
| 查看 Job Trace/指标/pprof | [可观测性](observability.md) |

共同不变量仍是 PostgreSQL 唯一事实源、at-least-once 与业务幂等。历史性能/控制流/远程 Ollama/生产运维限制统一见[限制清单](../status.md#限制与未结事项)。
