# 正式运行时的输入投影

依据 [ADR-0018](../../../docs/adr/0018-deepseek-fixed-flow-and-executor.md)、[ADR-0019](../../../docs/adr/0019-executor-confirmation-and-exit-contract.md)及 [ADR-0020](../../../docs/adr/0020-provider-audit-and-batch-stop.md)。这是内部 v2 `execute_step` 的登记输入，审计版本协调升级源合同与固定镜像；不引入新的存储 checkpoint 或调度语义。

[源码 schema](runtime-input.schema.json) 的根对象恰为 `{"input": execute_step.input, "checkpoint": execute_step.checkpoint}`。它描述字段形状；字节上限、原始 JSON 严格性、资源绑定、提交链及部署 allowlist 仍由代码验证。JSON Schema 的 `integer` 不区分数字词法 `1` 与 `1.0`，运行时只接受整数词法。

## 输入与部署

`input` 恰有六个非 null 字段：

| 字段 | 合同 |
| --- | --- |
| `schema_version` | 整数 `1` |
| `executor_version` | 固定 `linux-v2-audit-runtime-1` |
| `adapter_id` | 现有 `ValidIdentifier` ASCII 1～128 字符规则；来自可信部署 manifest |
| `tool_invocation_id` | `get_order/get_delivery/search_policy` 为 Go 已确认 BeginTool 的 UUID；其余四种步骤恰为 `""` |
| `expected_response_model` | `deepseek-flash`；来自不可变 profile，不能由请求 payload 指定 |
| `provider_audit_policy` | `deepseek-audit-v1`；来自同份 profile |

不接受额外或重复字段。输入没有 query、messages、endpoint、模块、命令、价格或秘密。Go `Selection` 只接收已经由 Worker 核对的部署选择；输入 schema 不注册 adapter。Python 固定 registry 还会核对 manifest 与可用 adapter。`bounded-readonly-mechanism-v1` 仅是测试构建中的机制 adapter，不证明真实业务链或云端模型验收。

## Checkpoint

`checkpoint` 恰有 `cursor_version/next_step/steps/snapshot`。投影沿用 [Agent RPC](../../../proto/jobforge/agent/v1/agent.proto) 字段名：所有整数输出为 JSON 数字，StepKind 输出领域字符串，`*_json` bytes 输出严格 JSON 对象，不能输出 ProtoJSON 字符串整数或 base64。

- `next_step` 恰对应 RPC StepIdentity 的九个字段，执行时必填。终态 checkpoint 不可启动执行。
- `steps` 每项恰有 `step/commit_hash/result_json/result_ref`，长度等于 cursor；序号从 1 连续递增，历史 cursor 为 sequence−1；UUID 不得重复。下一个序号为 cursor+1，cursor 范围 0～31。
- 所有步骤沿用同一 profile/snapshot identity；next_step 完整匹配 v2 binding。`result_ref` 恰为 `run-step:<run_uuid>:<sequence>`。
- BuildExecute 要求原 Claim checkpoint 存在，并将最新 checkpoint 的不可变 profile、snapshot/ticket/index 身份及两个资源 JSON 对象与原 Claim 比较。对象使用既有规范化器比较，允许等价空白和字段顺序；原 Claim 的旧游标不妨碍读取后继步骤。
- Go 使用现有 `run.CanonicalStepResult` 和 `run.CommitHash` 验证每个完整结果；使用 `run.NextInputHash` 验证初始和每一后继输入链。Python验证字符串哈希链，不另写任意 decimal canonicalizer；新结果仍由 Go 规范化/hash并交控制面验证。
- `snapshot` 恰有 RPC 的八个字段。`ticket_binding_json` 是现有 `business.Ticket`（没有另加 schema_version）；`version_vector_json` 是现有 `business.VersionVector`，其索引字段是 `index.id/profile_hash/content_hash`。工单 tenant/id/revision/policy、可空 order 关联、缺失事实与索引身份必须一致。缺失事实保留明确的 null；不可把 null revision 改成 0。
- 七字段 StepResult 保持原合同，`content/proposal` 的合法 null 保留。已提交 proposal 仅接受两个注册格式：原严格四字段，以及 ADR-0018 的 support 严格八字段；后者引用 [support 源 schema](../../support/v1/schema.json) 的 persistedProposal，验证时离线注册其 URN，不从网络加载 schema。格式识别不授予新策略，提交仍由 profile.strategy 决定。快照投影不包含完整订单、物流及索引正文，因此不会声称重新计算完整 snapshot_hash。

## 大小与所有权

先检查 RPC 原 JSON 字节，再检查最终 JSON 编码后字节。`ticket_binding_json/version_vector_json` 分别 ≤8 KiB；read_ticket 和工具结果 ≤8 KiB，其余结果 ≤16 KiB；input ≤16 KiB；完整 checkpoint ≤256 KiB；普通帧包含 LF ≤384 KiB。空白压缩不能使原本超限的 RPC 通过；JSON 转义扩大后也必须重新检查。超限返回现有 `CHECKPOINT_TOO_LARGE`，不截断历史或正文。

`runinput.BuildExecute(lease, checkpoint, selection, requestID, emittedMonoMS, stepDeadlineMonoMS)` 返回独立 Frame。它不读取时钟、不发 RPC、不生成 UUID、不续租、不决定下一游标；remaining_ms 由调用者提供的两个 BOOTTIME 样本相减，范围 1～180000。trace_context 忠实沿用 Claim，经既有 v2 codec 验证。Worker 仍须通过原 Conversation 后才能发送。

## 共同向量

[runtime-input.json](fixtures/runtime-input.json) 顶层包含 `schema_version`、`valid`、`invalid`、`invalid_json`；前两种案例是 `{name,frame}`，最后一种是 `{name,json}`，json 是完整 LF 结尾原始帧字符串，用于重复键等 JSON 对象不能表达的错误。

向量数据明确为合成机制数据。有效 accepted 链由 Go 的真实领域 canonicalizer/ledger fingerprint 生成，Go 与 Python 都消费同一文件。重新生成并验证：

```text
go test ./internal/runinput -update-runtime-fixtures -count=1
go test -race ./internal/runinput ./internal/runprotocol/v2
```

正常测试不修改 fixture。向量和单元测试不能替代正式 Linux 进程、PostgreSQL 提交确认、真实检索、收费模型或 40 案验收。
