# JobForge Python SDK

Python 3.10+，同步 HTTP 客户端。安装（仓库根目录）：

```sh
python -m pip install './sdk/python[dev]'
```

```python
import os
from jobforge import JobForgeClient

with JobForgeClient("http://localhost:8080", os.environ["JOBFORGE_API_KEY"]) as client:
    submitted = client.submit(
        queue="default",
        type="rag.index",
        payload={
            "input_version": 1,
            "corpus_id": "handbook-v1",
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

安装 `opentelemetry-sdk` 并配置自己的 TracerProvider 后，SDK 自动创建 `sdk.submit/get/cancel/retry` span 并传播当前 W3C context。也可通过 `traceparent=` 接入外部上下文；旧 `trace_id=` 继续写 X-Trace-ID，仅作兼容关联。SDK 不配置 exporter、不输出 payload/密钥。

快速测试：`python -m pytest sdk/python/tests`。真实跨语言契约：设置 `JOBFORGE_TEST_PYTHON` 为安装了本 SDK 的解释器，再运行 `go test -run TestPythonHTTPContract -count=1 ./tests/integration`（Windows 先启动 Compose PostgreSQL 并设置 JOBFORGE_TEST_DSN，详见仓库开发指南）。真实模型任务与故障演示见仓库任务扩展指南。
