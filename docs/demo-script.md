# 三分钟 Agent/RAG 演示

当前验收由 [PRD v0.6](product/JobForge_PRD_v0.6.md) 定义。提前按[真实任务指南](real-tasks.md)下载固定模型、安装 SDK/检查工具；按[观测指南](observability.md#agentrag-本地观测闭环)启用 obs，并追加 `compose.models-test.yaml` 以便主机 Go 测试使用 `localhost:11435`。下载、编译和首次加载不计入三分钟。演示库使用独立项目与 55433，故障测试库使用 5433。

## 0:00—1:00 提交两个任务并检查实际产物

```powershell
$env:OTEL_EXPORTER_OTLP_ENDPOINT='http://localhost:4318'
.venv/Scripts/python.exe examples/agent_rag.py
```

脚本使用安装好的 Python SDK，经真实 HTTP / Gateway / Worker 执行 `rag.index`、`agent.extract`。输出两份 job_id、result_ref、artifact_url、trace_id；实际读取 6×384 向量、重新检索三个问题、检查采购单字段，并验证提交幂等和跨租户 404。任何断言不满足即失败。

复制输出的 job_id，可用 SDK 查询状态和 attempt 时间线：

```python
from jobforge import JobForgeClient
with JobForgeClient("http://localhost:8080", "dev-api-key") as client:
    job = client.get("替换为刚输出的 job_id")
    print(job.state, job.result_ref, job.attempts)
```

## 1:00—2:00 取消、人工重试与发布后崩溃

使用独立、可重建的测试 PostgreSQL，运行两种真实任务的两个关键窗口；预热后本机约数十秒。完整六类场景与全部门禁另有验收记录。

```powershell
docker compose -f deploy/compose.yaml up -d postgres
$env:JOBFORGE_TEST_DSN='postgres://jobforge:jobforge@localhost:5433/jobforge?sslmode=disable'
$env:JOBFORGE_REAL_MODEL_URL='http://localhost:11435'
$env:JOBFORGE_TEST_OTLP_ENDPOINT='http://localhost:4318'
$env:JOBFORGE_TEST_JAEGER_URL='http://localhost:16686'
go test -race ./tests/integration -run '^TestRealTasksLifecycle/(rag.index|agent.extract)/(cancel_after_publish_manual_retry|crash_after_publish)$' -count=1 -v
```

取消通过真实 API 发出，Worker 通过 Heartbeat 接收；人工 retry 生成新 job_id，保留业务键。crash 场景实际 Kill/Wait Worker PID，等待数据库自然租约过期，再次执行时 token 增长。断言首次产物引用/内容/发布时间不变，恢复后不再调用模型。模型计算可重做，未实现断点续作。

## 2:00—3:00 查看 Trace、指标与告警

打开 [Grafana 任务仪表盘](http://localhost:3000/d/jobforge-tasks)，本地演示账号 admin/jobforge。分别选择 rag.index / agent.extract，查看积压、attempt outcome、执行耗时、重试/DLQ、Worker 存活与租约恢复。将第一步的 trace_id 填入 Trace ID，点击 Open task trace；链路包含 SDK、API、Gateway、Worker、真实模型适配器、产物发布和 Complete。

故障测试输出 `JAEGER VERIFIED` 及恢复 Trace ID，可看到 `scheduler.recover_lease` 与后继 attempt。强杀进程的未结束/未导出 Span 可能丢失，任务 attempt 仍以 PostgreSQL 审计为准。

完整 Compose 运维演练另运行 `examples/observability_acceptance.py --project jobforge-agent-rag`，约需数分钟：真实停止 Collector、模型和 Worker，断言业务可靠性、四种结果指标及告警触发/恢复。该长演练不塞入三分钟。证据与限制见[实施记录](agent-rag-progress.md)。
