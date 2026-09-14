# 真实 Agent/RAG 任务：启动、验收与清理

需要 Docker Compose、Go 1.26、Python 3.12。无需付费 API key。模型后端为 Ollama 0.32.5；固定模型共约 444 MB（不含运行镜像），首次加载需要额外时间。业务产物存入独立 `task_artifacts` 表，队列内只存引用。数据集为仓库内合成内容，见 `internal/tasks/fixtures`。

## A. 从空模型卷启动 Compose

```powershell
docker compose -f deploy/compose.yaml --profile models up -d --build
docker compose -f deploy/compose.yaml --profile models exec ollama ollama pull all-minilm:22m
docker compose -f deploy/compose.yaml --profile models exec ollama ollama pull qwen2.5:0.5b
python -m venv .venv
.venv/Scripts/python.exe -m pip install './sdk/python[demo]'
.venv/Scripts/python.exe examples/agent_rag.py
```

Linux/macOS 对应解释器为 `.venv/bin/python`。API `8080`，业务产物 API `8081`，Gateway `9090`；默认鉴权为 `Authorization: Bearer dev-api-key`。第二演示租户的 key 为 `other-api-key`，只用于验证隔离。模型容器不向宿主机开放端口，不启用 models profile 时其他服务仍能运行；模型相关任务会明确失败，不用替身回退。

独立演示与测试同时运行时，设 `$env:JOBFORGE_POSTGRES_PORT='55433'`，用 `docker compose -p jobforge-agent-rag ...` 建独立项目/卷，避免测试的 `5433` 数据库被演示 Worker 使用。集成测试会重建其 DSN 指定数据库的表。

## B. 已有本地 Ollama 或可信远程后端

本机执行 `ollama pull all-minilm:22m` 和 `ollama pull qwen2.5:0.5b`。Windows Docker Desktop 使用以下地址连接宿主机模型服务：

```powershell
$env:JOBFORGE_OLLAMA_URL='http://host.docker.internal:11434'
docker compose -f deploy/compose.yaml up -d --build
```

Ollama 必须监听容器可访问的地址；若默认只监听 localhost，请使用 A 的隔离容器方式，或按 Ollama 官方说明配置监听。原生 Go 进程默认使用 `http://localhost:11434`，可单独运行 `migrate`、`api`、`gateway`、`scheduler`、`worker`、`artifacts` 子命令；分别配置不冲突的 `JOBFORGE_METRICS_ADDR`。

可信远程 Ollama 兼容 origin 可由 `JOBFORGE_OLLAMA_URL` 配置，原生进程支持 `JOBFORGE_OLLAMA_API_KEY`。禁止在 payload 中指定 URL、路径或模型。远程服务必须提供相同的 `/api/tags`、`/api/embed` 和 `/api/chat` 接口及确切模型；通用 OpenAI API / Ollama Cloud 不是这里宣称的已验证兼容后端。

## 固定模型与资源界限

| 用途 | 模型 | manifest SHA-256 |
|---|---|---|
| 384 维真实 embedding | all-minilm:22m（F16） | `1b226e2802dbb772b5fc32a58f103ca1804ef7501331012de126ab22f67475ef` |
| JSON Schema 抽取 | qwen2.5:0.5b（Q4_K_M） | `a8b0c51577010a279d933d14c2a8ab4b268079d44c5c8830c0a93900f1827c67` |

适配器每次推理前核对 tags 中的 digest；上游同名标签发生变化会返回 `MODEL_VERSION_MISMATCH`，不能悄悄换模型。更新模型需新版本、验收和业务键。代码中的 seed=42、temperature=0、num_predict=256、num_ctx=2048 固定；不承诺跨硬件逐字节确定性，首次业务产物持久化后保持不可变。

每次模型操作总计≤60s（包含 manifest 检查），受任务剩余 deadline 限制；索引最多两次批量 embedding，抽取最多两次 chat。输入≤64 KiB、分块≤64、抽取输出≤16 KiB、产物≤2 MiB。查询端单请求≤60s、四路并发。取消断开 HTTP 并停止后续处理，但不能撤回已开始的后端计算或已发布产物。

## 分层验收

快速层实际使用 PostgreSQL；模型响应测试明确使用替身，不计真实业务验收：

```powershell
docker compose -f deploy/compose.yaml up -d postgres
$env:JOBFORGE_TEST_DSN='postgres://jobforge:jobforge@localhost:5433/jobforge?sslmode=disable'
$env:JOBFORGE_TEST_PYTHON=(Resolve-Path .venv/Scripts/python.exe).Path
go test -race ./internal/tasks ./tests/integration -run 'Test(TaskArtifacts|PythonHTTPContract|AT34)'
```

安装仓库工具依赖后另跑真实层。可使用上面的 Compose 模型卷，追加只绑定 localhost 的测试端口（无需另安装本机 Ollama）：

```powershell
docker compose -p jobforge-agent-rag -f deploy/compose.yaml -f deploy/compose.models-test.yaml --profile models up -d --no-deps ollama
```

请使用实际启动时的相同 project 名；未使用独立项目时省略 `-p jobforge-agent-rag`。模型测试端口默认 11435，可用 `JOBFORGE_MODEL_TEST_PORT` 覆盖。

```powershell
.venv/Scripts/python.exe -m pip install -r tools/requirements-lint.txt
$env:JOBFORGE_REAL_MODEL_URL='http://localhost:11435'
go test -race ./tests/integration -run '^TestRealTasks(SDK|Lifecycle)$' -count=1 -v
```

已有本机 Ollama 时也可指向 `http://localhost:11434`。联合 Jaeger 断言及 Compose 故障脚本见[可观测性指南](observability.md#自动化故障复现与排障)。

SDK 验收实际检查持久化向量、检索命中、输出字段和租户隔离。Lifecycle 针对两个任务各跑六场景，用测试专属代理注入一次 503 或阻塞真实模型响应；不生成虚假推理数据。生产二进制没有故障参数。发布前/后执行 OS Kill/Wait，再由生产恢复事务按数据库时间回收租约。人工 retry 是新 job_id，payload 中业务键保持，因此复用已发布效果；缺少产物时从头重做。

## 清理

```powershell
docker compose -f deploy/compose.yaml --profile models --profile obs down
```

保留卷即可保留任务/产物/模型。仅对自己创建的可重建项目，添加 `--volumes` 才删除这些数据；使用独立项目时保留原 `-p jobforge-agent-rag`。本机 Ollama 模型由用户管理，可用 `ollama rm all-minilm:22m qwen2.5:0.5b` 删除本轮模型。

接口依据：[Ollama Embed API](https://docs.ollama.com/api/embed)、[Structured outputs](https://docs.ollama.com/capabilities/structured-outputs)。模型目录：[MiniLM](https://ollama.com/library/all-minilm:22m)、[Qwen2.5](https://ollama.com/library/qwen2.5:0.5b)。
