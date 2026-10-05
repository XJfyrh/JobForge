# Run API、SDK 与调用账本

本页说明控制服务接入、持久步骤与预算排障。合同为 [PRD v0.9](../product/JobForge_PRD_v0.9.md)、[ADR-0017](../adr/0017-run-admission-and-call-ledger.md)及[供应商审计](provider-audit.md)；当前交付与真实验收统一见[状态页](../status.md)。

## 服务与数据边界

`agent-control` 同一进程提供 `/v2/runs`、`jobforge.agent.v1` Worker RPC 和每秒一次、每轮最多100候选的恢复扫描。它只调度 Run，不启动历史 jobs 调度器。PostgreSQL 是唯一执行事实源；步骤、工具与物理调用记录都不是独立队列。

业务 `support-business` 继续使用独立数据库。提交先在控制事务外请求幂等快照，然后在控制库保存业务意图、Run、固定版本向量、预算关系和操作键。捕获后控制事务失败可能留下未引用快照；相同操作显式重发复用快照，控制层没有跨库事务或隐式 HTTP 重试。

公开身份与内部 Worker 凭据分开。API 身份决定 tenant/reader/operator/approver，审批者还绑定稳定 actor_id；Worker 身份只能领取部署配置的 tenant/profile。请求不能提供模型 URL、费用、工具定义、命令或代码。`bootstrap` 拒绝带业务数据库标记的 DSN；业务权限、生产控制库最小权限部署和长期保留仍需相应运维验收。

## 启动控制服务

命令从仓库根目录执行，需要 Docker、Go 和 Python。业务资源按[业务指南](business.md)准备；控制数据库使用 5435，集成测试库使用 5433。

```powershell
docker compose -f deploy/compose.agent.yaml --profile control build control
docker compose -f deploy/compose.agent.yaml --profile control up -d --wait control-postgres
docker compose -f deploy/compose.agent.yaml --profile control run --rm control bootstrap
docker compose -f deploy/compose.agent.yaml --profile control up -d control
Invoke-RestMethod http://127.0.0.1:8093/health/ready
Invoke-RestMethod http://127.0.0.1:8093/v2/runs `
  -Headers @{Authorization='Bearer dev-agent-north-reader'}
```

本地示例凭据只用于合成数据环境。HTTP/RPC 端口只映射到宿主 loopback。默认 `deploy/agent-run.empty.json` 登记两个租户、一个 Worker、零模型和零预算；查询返回空集合，提交未登记 profile 明确失败。启用配置由[批次准备](cloud-batch.md)冻结模型价格、输入上界、策略和预算；不要把单元测试的合成 profile 放进生产配置。

原生命令为 `go run ./cmd/agent-control bootstrap` / `serve`。配置变量：

| 变量 | 含义 |
|---|---|
| JOBFORGE_AGENT_DSN | 控制 PostgreSQL；bootstrap需要迁移权限 |
| JOBFORGE_AGENT_CONFIG | 最大1MiB的部署 JSON 文件；含登记 profile、Worker 能力、预算开户与 tenant/batch 绑定 |
| JOBFORGE_AGENT_HTTP_ADDR / GRPC_ADDR | 默认127.0.0.1:8093 / :9093 |
| JOBFORGE_AGENT_BUSINESS_URL | 受信业务服务地址，不能来自任务 payload |
| JOBFORGE_AGENT_BUSINESS_KEYS | tenant→业务 operator token 的 JSON，仅从进程环境读取 |
| JOBFORGE_AGENT_API_KEYS | token→{tenant_id,role,actor_id?} 的 JSON；approver必填稳定actor |
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

提交、取消、retry 都使用独立 Idempotency-Key。相同业务键和规范内容返回首次根 Run；不同内容冲突。一个失败/取消 Run 只能有一个直接 retry 后继并继承预算家族。首次动作授权前，新后继捕获新快照并从空游标执行；已有授权时仅走[回执优先路径](approval.md)，无重新规划/写入。对已受理结果的重发不依赖 profile 仍在线、预算批次仍有效或业务捕获服务可用。

源契约位于 [OpenAPI](../../api/run/v2/openapi.yaml)、[Proto](../../proto/jobforge/agent/v1/agent.proto)、[执行器帧](../../api/executor/v2/schema.json)及其共同 fixture。SDK API 见[SDK说明](../../sdk/python/README.md)。Worker RPC 必须带 deadline 和内部 Bearer token；稳定错误使用 `google.rpc.ErrorInfo.reason`，不能解析英文消息判断重试。

CommitStep拒绝过大结果或无法验证的模型方案时，RPC状态为 `INVALID_ARGUMENT`，reason分别保留 `CHECKPOINT_TOO_LARGE`、`MODEL_PROTOCOL_ERROR`。这两类结果错误不能被当作临时内部故障无限重试；Worker只可按登记策略使用一次协议纠正，或以同名永久错误结束attempt。未知服务端错误仍统一脱敏为 `INTERNAL`。

步骤只按服务端注册的有限策略推进，生产 support 支持 `support_fixed_v1` 与 `support_agent_v1`，按不可变 profile 选择。Worker提交当前身份和受保护输出，服务端核验工具/物理调用观察与实际证据来源，并计算下一游标。最终方案进入 `awaiting_approval` 时原子保存方案/版本向量/许可截止，关闭attempt并释放容量；`no_action` 可以直接成功。新 schema 4 的[审批路径](approval.md)可拒绝完成或批准后继续原 Run 的登记动作；旧方案不获得写权限。

重复中间CommitStep仍须当前有效lease；最终提交丢ACK后使用只读GetAcceptedCommit。自动恢复读取原Run已提交步骤，未提交步骤可能重做；首次授权前的人工retry为空游标。恢复规则、runtime 版本和未提交模型步骤的条件见[恢复指南](recovery.md)，不恢复模型内部推理进度。

## 只读调用审计

`GET /v2/runs/{run_id}/calls` 和 `RunClient.calls(run_id)` 返回一次事务一致抓取，按ordinal升序最多44项，最终编码含换行≤256KiB；违反硬界限返回固定INTERNAL错误，不截断。没有分页、cursor、limit或tenant参数；reader/operator只能读当前租户Run，跨租户404。该查询不预留额度，也不触发报告、结算或模型请求。

每行包含预留预算、known/held、报告与观察的独立时间/hash、有限provider元数据。`observed_usage`是供应商完整计数；`settled_usage`只表示与冻结价格兼容的已知计量。不兼容模型的计数仍可观察，但不按原价释放hold。reasoning缺省、0、正数或不可用分别保留；reasoning已经含在输出计数中，不二次加费。业务JSON拒绝可以有正常计量。

`audit_status`明确区分 `legacy_not_collected`、`not_applicable`、`missing`、`recorded`。缺报告不证明请求未发出，也不伪造unavailable报告。完整响应摘要不能重建原文；SDK不输出或抓取原始模型内容。共享batch只公开冻结状态和固定原因，不返回其它租户的触发Run/call或Worker权限身份。晚到报告会改变后续视图，证据导出应保留 `captured_at`、报告hash与抓取文件hash。

新审计policy下，每次新Claim/BeginTool/Reserve受已持久chat报告、计量、普通observation及步骤结束屏障约束。首报告不可覆盖；不同报告冲突仅冻结batch，不能把第二份计数算成新财务事实或异常。仅首份结构完整计数超原上限时冻结三个账户。原session晚到确认限原调用30日窗口，不恢复租约或执行权。详见ADR-0020；本层没有解冻、增额或自动审计清理接口。

本增量的unknown停批针对chat。本地query_embedding若没有可用usage，保留既有unknown全额token hold；它没有DeepSeek audit，不伪造report，也不因此产生CHAT_USAGE_UNKNOWN。只有真实响应、业务校验、普通ACK和原期限均满足才能继续后续search。若存在完整embedding usage，则必须先确认同一usage-only report_hash的settled，再汇合普通ACK；完整计数超界仍触发既有异常处理。metadata/business免费调用也继续等待普通ACK。

## 调用预算与故障判断

每个可能发出的 HTTP 都先 ReserveCall，只有 `newly_reserved=true` 的首次返回可派发。相同 ID 重发只是账本查询；新发送必须新 ID、新预留。search_policy 的版本、标签、向量化、业务搜索四次请求分别授权。免费本地请求仍占物理次数；当前注册本地请求≤10秒，chat≤60秒，并受 lease 派发期限、attempt、Run 和批次截止约束。

三个账户的 token/费用暴露都是已知保守费用加 reserved/unknown 全额 hold。完整可信 usage 可以结算；断连、截断、超时、丢 ACK 或无 usage 均不自动退款。出现超预留用量时保留原始异常、冻结三个账户并保留全部 hold。异常意味着硬上限假设不再成立，真实收费验收须停止排查。

取消阻止后续授权，不能撤回已经派发的外部请求。关闭 attempt 原子脱离 active_call、标记未确认调用 unknown、释放槽，不返还未知费用。晚到计量仅结算其原调用，不能改 Run、游标或新 attempt 的 active_call；补报窗为预留后30日，等号过期。

排查顺序：使用 tenant 身份查询 Run/error、events 和预算；内部检查 attempt/session/fence 与数据库时间；再按 physical_call_id 对照当前观察和 usage。`STALE_LEASE` 需要停止旧执行器，不能通过重新获取旧许可继续发送。`BUDGET_EXHAUSTED` 查看三个账户中哪一层冻结、过期或达到上限；unknown 不是零消费。`PROFILE_UNAVAILABLE` 检查不可变登记及部署 enabled/profile allowlist。没有可用 Worker/profile 的 ready Run 不会任意切换模型，最终仍受 Run deadline 限制。

日志与 Trace 只记录固定路由、身份、状态、数量和 hash，不输出文档、完整模型输入输出、秘密或工具全文。受保护步骤通过已鉴权 tenant 的 `/steps` 查询；`run-step:` / `run-proposal:` 引用不是公开下载地址。

## 验证与证据

真实 PG 预留/结算、HTTP 故障、安装 SDK 的契约以及 Linux 进程检查按[测试指南](../tests.md)执行。同 DSN 的清理测试串行；测试供应商响应只证明执行机制。真实模型与质量结果见[证据索引](../evidence/README.md)。
