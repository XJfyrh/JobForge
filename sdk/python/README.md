# JobForge Python SDK

Python 3.10+，同步 HTTP 客户端。安装（仓库根目录）：

```sh
python -m pip install './sdk/python[dev]'
```

## Run v2

`RunClient` 对应 [OpenAPI 源](../../api/run/v2/openapi.yaml) 和 ADR-0017；旧 `JobForgeClient` 仍可独立使用。Run 控制服务必须事先登记不可变 profile、业务快照依赖和授权预算批次；S1-B 没有默认收费模型或批准/写入接口。

```python
import os
from jobforge import RunClient

with RunClient(
    os.environ["JOBFORGE_API_URL"], os.environ["JOBFORGE_API_KEY"]
) as client:
    accepted = client.submit(
        ticket_id="T01",
        business_request_key="ticket-T01-review-v1",
        profile_id=os.environ["JOBFORGE_RUN_PROFILE"],
        budget_batch_id=os.environ["JOBFORGE_RUN_BUDGET_BATCH"],
        idempotency_key="submit-ticket-T01-v1",
    )
    run = client.get(accepted.run.run_id)
    print(run.state, run.error, run.budget.family.held_cost_microyuan)
    result = client.result(run.run_id)
    print(result.available, result.kind, result.ref)
```

公开方法为 `submit/get/list/steps/events/result/cancel/retry`。`list` 返回 `items/next_cursor`，`steps/events` 接受 `after/limit` 并返回 `items/next_after`；默认20条、最多100条，SDK不自动翻页或轮询。`steps` 显式读取受保护结果，事件仅含元数据。未有结果时 `available=False`、`kind/ref=None`；`proposal` 只表示待审批方案。

提交必须同时提供业务意图键和独立 `idempotency_key`；相同业务内容换提交键仍复用首次根Run。`cancel(run_id, idempotency_key=...)` 返回操作ID和当前Run，运行中取消先进入 `stopping`。`retry(run_id, idempotency_key=..., run_timeout_seconds=3600)` 为failed/cancelled来源创建或复用唯一后继，保留原终态并共享家族预算。新建retry须在原业务请求7日窗口内；不能通过换键重置额度或创建分叉。

Run响应使用完整、严格类型模型，未知/重复/缺少字段、非法状态/时间、浮点/布尔金额或超过 `2**53-1` 的计数均被拒绝。所有金额是CNY microyuan整数；`used.tokens/cost_microyuan` 是确认用量加保守hold，`known_*` 与 `held_*` 分别展示，不代表供应商实际账单。failed Run查询仍返回HTTP 200，执行失败在 `run.error`。

SDK每次方法调用最多发送一次HTTP请求，不隐式重试、跟随重定向、发送后台工作或自动解引用结果。超时可能已受理，调用方须保留原操作键。新增稳定异常为 `RateLimitedError`、`DependencyUnavailableError`、`ProfileUnavailableError` 和 `BudgetExhaustedError`；`retryable` 只提供调用方策略提示，不会触发发送。TraceContext自动继承当前上下文，也可显式传 `traceparent/tracestate`；span只记录操作和HTTP状态。

确定性SDK测试和源/共同fixture一致性检查使用 `python -m pytest sdk/python/tests`。真实HTTP入口为 `sdk/python/tests/run_http_contract.py <url> <profile_id> <budget_batch_id> <ticket_id>`，由Go测试建立生产router和真实PostgreSQL后调用；没有运行该入口不能把MockTransport单测当成真实服务验收。

## 旧 Job API

```python
import os
from jobforge import JobForgeClient

with JobForgeClient("http://localhost:8080", os.environ["JOBFORGE_API_KEY"]) as client:
    submitted = client.submit(
        queue="default",
        type="rag.index",
        payload={
            "version": 1,
            "corpus_version": "handbook-v1",
            "business_key": "handbook-v1",
        },
        idempotency_key="submit-handbook-v1",
    )
    job = client.get(submitted.job_id)
    print(job.state, job.result_ref, job.attempts)
```

本地 Compose 的 `JOBFORGE_API_KEY=dev-api-key` 仅为合成演示凭据，映射 `dev-tenant`。鉴权通过 Bearer header，payload 不决定租户。`get` 对不存在和其他租户任务均抛 `NotFoundError`。引用可缺省/null，SDK 不自动请求业务存储；业务服务须独立验证租户。

`cancel(job_id)` 取消等待任务或请求运行中任务停止；`retry(job_id)` 为 dead/cancelled 创建新 job ID，保留原任务终态。业务键由 payload 定义，不等同于提交幂等键。

异常均派生 `JobForgeError`，提供 `code`、`message`、`status_code` 和 `retryable`。支持 InvalidArgument / Unauthorized / Forbidden / NotFound / Conflict / AlreadyTerminal / StaleLease / CancelRequested / InvalidTransition / QueueOverloaded / Internal。未知错误码与异常响应安全归为 InternalError。网络失败为 TransportError，超时为 RequestTimeoutError；服务端可能已接受提交，调用方重试应使用相同提交幂等键。SDK 不自行重试，不提供调度循环。

HTTP 2xx 也会校验已知结果字段：错误的 ID/标量类型、状态、时间或 attempt 时间线归为 InternalError，不把原始响应正文带入异常消息或格式化堆栈。缺失可选字段沿用默认值，未知扩展字段忽略，旧响应中缺失/null/空串的 result_ref 均映射为 None。

安装 `opentelemetry-sdk` 并配置自己的 TracerProvider 后，SDK 自动创建 `sdk.submit/get/cancel/retry` span 并传播当前 W3C context。也可通过 `traceparent=` 接入外部上下文；旧 `trace_id=` 继续写 X-Trace-ID，仅作兼容关联。SDK 不配置 exporter、不输出 payload/密钥。

快速测试：`python -m pytest sdk/python/tests`。真实跨语言契约：设置 `JOBFORGE_TEST_PYTHON` 为安装了本 SDK 的解释器，再运行 `go test -run TestPythonHTTPContract -count=1 ./tests/integration`（Windows 先启动 Compose PostgreSQL 并设置 JOBFORGE_TEST_DSN，详见仓库开发指南）。上述 get 可能仍返回执行中状态；安装 `[demo]` extra 后运行 `examples/agent_rag.py` 可等待两个真实任务并验证产物。启动和清理见[真实任务指南](../../docs/real-tasks.md)，Trace/故障演练见[可观测性指南](../../docs/observability.md)。
