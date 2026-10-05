# 架构与执行权

Agent v3 以 PostgreSQL 保存 Run、attempt、步骤、许可和预算事实。Go 控制服务与 Worker 授予并校验执行权，固定 Python 单步执行器根据许可读取业务证据或调用模型。已交付范围见[当前状态](status.md)。

## 组件与数据边界

| 组件 | 职责 | 入口 |
|---|---|---|
| `agent-control` | `/v2/runs`、AgentService RPC、恢复扫描；提交步骤和审计/预算事务 | [Run 指南](agent-v3/runs.md) |
| 控制 PostgreSQL | Run/attempt/session/fence、checkpoint、物理调用、三层预算和审计报告 | [ADR-0017](adr/0017-run-admission-and-call-ledger.md)、[ADR-0020](adr/0020-provider-audit-and-batch-stop.md) |
| `agent-worker` | 固定容量领取与续租、严格 checkpoint 投影、IPC/HTTP 许可确认、实际进程清理 | [运行时](agent-v3/runtime.md) |
| Python guardian/step | 固定 registry 单步执行，双通道报告；不 Claim、不续租、不调度 Run | [协议](agent-v3/executor-protocol.md)、[受控 HTTP](agent-v3/authorized-http.md) |
| `support-business` / 业务 PostgreSQL | 独立快照、订单/物流与 pgvector 政策检索，reader 只读 | [业务指南](agent-v3/business.md) |
| Ollama / DeepSeek | 本地 MiniLM embedding / 主线方案推理，使用登记身份、固定端点和有界请求 | [批次部署](agent-v3/cloud-batch.md) |

提交先在控制事务外捕获幂等业务快照，再保存 Run 和固定版本向量；两库之间没有跨库事务或隐式 HTTP 重试。执行器只得到当前步骤需要的凭据，control token 由 Go 持有。gold、评分器和测试 adapter 不进入生产镜像。

## 可靠性合同

交付保证为 **at-least-once**。Claim 在一个事务授予 owner、lease、attempt、fencing token 与状态；Heartbeat/Complete/Fail 或 Run 的对应提交必须核验完整执行身份和允许的当前状态。陈旧结果返回 `STALE_LEASE`，不得覆盖新执行权。

Run 的下一游标由控制面 CommitStep 计算；Worker 必须等待持久报告/观察确认、当前执行权和 Wait/EOF/Join/组消失后提交。S3 从已提交前缀恢复，未提交步骤可能重做并产生新调用/费用；不恢复模型内部推理。详见[恢复合同](agent-v3/recovery.md)。

每个 HTTP 先持久 Reserve，只有首次许可可发送。family/tenant/batch 累计次数和 known+held 暴露不因 retry 或恢复重置；缺失计量不退款。最终方案停在 `awaiting_approval`，审批写入尚未实施。

## 既有 Job API

`cmd/jobforge` 的 API、Gateway、Scheduler、Worker、outbox publisher 与 reference consumer 继续服务旧 jobs。其任务目录、产物和取消/事件合同独立于 v3 Run；`agent-control` 不同时启动旧 jobs 调度器。

旧路径采用 PostgreSQL 核心状态与 outbox，默认 notify、可选 Redis Streams 耐久事件适配；JetStream 只能作为 P1 outbox 适配器。操作入口见[旧 Job 指南](legacy/README.md)，详细状态与故障合同见[旧 Job 故障语义](legacy/failure-semantics.md)。
