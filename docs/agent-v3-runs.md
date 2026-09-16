# Agent v3 Run 接入与调用账本

本指南对应 [PRD v0.9](product/JobForge_PRD_v0.9.md) / [ADR-0017](adr/0017-run-admission-and-call-ledger.md) 的 S1-B。当前分支已实现控制服务、Worker RPC、SDK、步骤持久化与三层调用预算，正在完成验证和独立评审。S1-C 的正式受监管执行器、DeepSeek 真实推理与40例固定流程尚未交付；默认配置没有可执行模型，不能把确定性测试结果称作云端业务验收。

## 服务与数据边界

`agent-control` 同一进程提供 `/v2/runs`、`jobforge.agent.v1` Worker RPC 和每秒一次、每轮最多100候选的恢复扫描。它只调度 Run，不启动历史 jobs 调度器。PostgreSQL 是唯一执行事实源；步骤、工具与物理调用记录都不是独立队列。

业务 `support-business` 继续使用独立数据库。提交先在控制事务外请求幂等快照，然后在控制库保存业务意图、Run、固定版本向量、预算关系和操作键。捕获后控制事务失败可能留下未引用快照；相同操作显式重发复用快照，控制层没有跨库事务或隐式 HTTP 重试。

公开身份与内部 Worker 凭据分开。API 身份决定 tenant/reader/operator；Worker 身份只能领取部署配置的 tenant/profile。请求不能提供模型 URL、费用、工具定义、命令或代码。`bootstrap` 拒绝带业务数据库标记的 DSN；业务权限、生产控制库最小权限部署和长期保留仍需相应运维验收。

## 启动本切片

需要 Docker、Go 和 Python；业务资源准备使用[业务指南](agent-v3-business.md)。下面只创建本项目新的可重建控制数据库（端口5435），不会使用集成测试端口5433。

```powershell
docker compose -f deploy/compose.agent.yaml --profile control build control
docker compose -f deploy/compose.agent.yaml --profile control up -d --wait control-postgres
docker compose -f deploy/compose.agent.yaml --profile control run --rm control bootstrap
docker compose -f deploy/compose.agent.yaml --profile control up -d control
Invoke-RestMethod http://127.0.0.1:8093/health/ready
Invoke-RestMethod http://127.0.0.1:8093/v2/runs `
  -Headers @{Authorization='Bearer dev-agent-north-reader'}
```

本地示例凭据只用于合成数据环境。HTTP/RPC 端口只映射到宿主 loopback。默认 `deploy/agent-run.empty.json` 登记两个租户、一个 Worker、零模型和零预算；查询返回空集合，提交未登记 profile 明确失败。S1-C 接受具体模型价格、输入上界、固定策略和批次后才提供启用配置；不要把单元测试中的合成 profile 放进生产配置。

原生命令为 `go run ./cmd/agent-control bootstrap` / `serve`。配置变量：

| 变量 | 含义 |
|---|---|
| JOBFORGE_AGENT_DSN | 控制 PostgreSQL；bootstrap需要迁移权限 |
| JOBFORGE_AGENT_CONFIG | 最大1MiB的部署 JSON 文件；含登记 profile、Worker 能力、预算开户与 tenant/batch 绑定 |
| JOBFORGE_AGENT_HTTP_ADDR / GRPC_ADDR | 默认127.0.0.1:8093 / :9093 |
| JOBFORGE_AGENT_BUSINESS_URL | 受信业务服务地址，不能来自任务 payload |
| JOBFORGE_AGENT_BUSINESS_KEYS | tenant→业务 operator token 的 JSON，仅从进程环境读取 |
| JOBFORGE_AGENT_API_KEYS | token→{tenant_id,role} 的 JSON |
| JOBFORGE_AGENT_WORKER_KEYS | 固定 Worker principal→token 的 JSON |

`profiles` 保存不可变定义；`enabled_profiles` 表示当前部署可执行集合。不同内容不得覆盖同 profile ID。预算开户只接受固定 UUID、scope/key、有效期、整数上限；重复 `bootstrap` 不清零计数或提高额度。人工 retry 与自动恢复均不能绕过原 family/tenant/batch 的费用和调用上限。

部署 JSON 字段名必须精确匹配，显式 null 不能充当缺省；profile.definition 仅保存有界定义，不执行其中内容。公开 API、Worker 和业务 operator 的 token 必须彼此独立，每个登记 Worker 和业务租户都需对应凭据；缺失、重复、未知角色/租户或控制字符会拒绝启动。关停会取消 HTTP 请求及扫描上下文，并有界等待 RPC 退出。

停止控制组件：`docker compose -f deploy/compose.agent.yaml --profile control stop control control-postgres`。保留数据时不要添加 `--volumes`。若确认整个 Agent v3 本地合成环境都可丢弃，可按业务指南执行完整 `down --volumes` 后重建；该操作同时删除业务数据库和模型缓存，并非只清理一个 Run。

## SDK 与 Worker 契约

```powershell
python -m pip install ./sdk/python
```

```python
from jobforge import RunClient

# profile_id 和 budget_batch_id 必须由管理员实际登记；此示例不发模型请求。
with RunClient("http://127.0.0.1:8093", "dev-agent-north-operator") as client:
    page = client.list(limit=20)
    for run in page.items:
        print(run.run_id, run.state, run.budget.run_usage)
```

提交、取消、retry 都使用独立 Idempotency-Key。相同业务键和规范内容返回首次根 Run；不同内容冲突。一个失败/取消 Run 只能有一个直接 retry 后继，后继继承预算家族、创建新 Run/快照并从空游标执行。对已受理结果的重发不依赖 profile 仍在线、预算批次仍有效或业务捕获服务可用。

源契约位于 [OpenAPI](../api/run/v2/openapi.yaml)、[Proto](../proto/jobforge/agent/v1/agent.proto)、[执行器帧](../api/executor/v1/schema.json)及其共同 fixture。SDK API 见[SDK说明](../sdk/python/README.md)。Worker RPC 必须带 deadline 和内部 Bearer token；稳定错误使用 `google.rpc.ErrorInfo.reason`，不能解析英文消息判断重试。

CommitStep拒绝过大结果或无法验证的模型方案时，RPC状态为 `INVALID_ARGUMENT`，reason分别保留 `CHECKPOINT_TOO_LARGE`、`MODEL_PROTOCOL_ERROR`。这两类结果错误不能被当作临时内部故障无限重试；Worker只可按登记策略使用一次协议纠正，或以同名永久错误结束attempt。未知服务端错误仍统一脱敏为 `INTERNAL`。

步骤只按服务端注册的 `bounded_readonly_v1` 顺序推进。Worker 提交当前身份和受保护输出，服务端核验工具/物理调用观察与实际证据来源，并计算下一游标。最终方案进入 `awaiting_approval` 时原子保存方案/版本向量/许可截止，关闭 attempt 并释放容量；仅 `no_action` 可以直接成功。B 没有批准或业务写入接口。

重复中间 CommitStep 仍须当前有效 lease；最终提交丢 ACK 后使用只读 GetAcceptedCommit，不用旧 lease 再次提交。自动恢复读取原 Run 已提交步骤，未提交步骤可能重做；人工 retry 空游标不代表断点续作。S3 仍须验收正式 Worker/执行器的实际 Kill/Wait 恢复。

## 调用预算与故障判断

每个可能发出的 HTTP 都先 ReserveCall，只有 `newly_reserved=true` 的首次返回可派发。相同 ID 重发只是账本查询；新发送必须新 ID、新预留。search_policy 的版本、标签、向量化、业务搜索四次请求分别授权。免费本地请求仍占物理次数；当前注册本地请求≤10秒，chat≤60秒，并受 lease 派发期限、attempt、Run 和批次截止约束。

三个账户的 token/费用暴露都是已知保守费用加 reserved/unknown 全额 hold。完整可信 usage 可以结算；断连、截断、超时、丢 ACK 或无 usage 均不自动退款。出现超预留用量时保留原始异常、冻结三个账户并保留全部 hold。异常意味着硬上限假设不再成立，真实收费验收须停止排查。

取消阻止后续授权，不能撤回已经派发的外部请求。关闭 attempt 原子脱离 active_call、标记未确认调用 unknown、释放槽，不返还未知费用。晚到计量仅结算其原调用，不能改 Run、游标或新 attempt 的 active_call；补报窗为预留后30日，等号过期。

排查顺序：使用 tenant 身份查询 Run/error、events 和预算；内部检查 attempt/session/fence 与数据库时间；再按 physical_call_id 对照当前观察和 usage。`STALE_LEASE` 需要停止旧执行器，不能通过重新获取旧许可继续发送。`BUDGET_EXHAUSTED` 查看三个账户中哪一层冻结、过期或达到上限；unknown 不是零消费。`PROFILE_UNAVAILABLE` 检查不可变登记及部署 enabled/profile allowlist。没有可用 Worker/profile 的 ready Run 不会任意切换模型，最终仍受 Run deadline 限制。

日志与 Trace 只记录固定路由、身份、状态、数量和 hash，不输出文档、完整模型输入输出、秘密或工具全文。受保护步骤通过已鉴权 tenant 的 `/steps` 查询；`run-step:` / `run-proposal:` 引用不是公开下载地址。

## 可复现测试

```powershell
docker compose -f deploy/compose.yaml up -d postgres
$env:JOBFORGE_TEST_DSN='postgres://jobforge:jobforge@localhost:5433/jobforge?sslmode=disable'
$env:JOBFORGE_TEST_PYTHON=(Resolve-Path .venv/Scripts/python.exe).Path
.venv/Scripts/python.exe -m pip install ./sdk/python
go test -race -count=1 -run '^TestRun' ./tests/integration
go test -race ./internal/run/... ./internal/runprotocol/...
.venv/Scripts/python.exe -m pytest sdk/python/tests python/tests
```

TestMain 使用可重建的测试环境；Run 用例再创建并清理独立数据库。不要用生产 DSN，也不要同时启动多个会清理同一测试 DSN 的 Go 集成测试进程。安装 SDK 并设置解释器后，Python runner 通过真实 TCP HTTP 操作生产 service/store；未设置而产生 skip 不计通过。

`TestRunPhysicalHTTP*` 使用固定故障 HTTP 服务验证丢许可 ACK、服务端实际收到请求后断连/截断、取消与在途响应；`TestRunLedger*` 验证真实 PG 预留/计量事务；测试响应明示为 fixture。它们不证明 DeepSeek 实际取消、价格或业务质量。完整质量门禁依照 [CONTRIBUTING](../CONTRIBUTING.md) 与[开发指南](development.md)。历史 W4 性能门禁失败、AT-25 跳过、远程模型与生产长期留存未验收继续保留。
