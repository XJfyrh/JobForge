# 受监管执行器源协议 v1

[`schema.json`](schema.json) 是帧字段与边界的事实源；Go 显式 codec 位于
[`internal/runprotocol`](../../../internal/runprotocol/)，Python 对应
[`protocol.py`](../../../python/jobforge_agent/protocol.py)。
[`fixtures/frames.json`](fixtures/frames.json) 由两端测试共同读取，不是生产模型注册。

每条消息是一个 UTF-8 JSON Lines 帧，包含结尾 LF 后最多 384 KiB。
严格拒绝未知、缺失、重复、大小写别名字段、尾随 stdout、非法 UTF-8、非有限数值和
未配对的 Unicode surrogate；JSON 嵌套最多 64 层。JSON Schema 的 `x-*` 注解描述
标准 JSON Schema 无法表达的字节界限和跨帧约束，由显式 codec/Conversation 执行。
整数使用 JSON 数字，必须是整数词法形式，预算与身份计数不超过 `2^53-1`。

```text
execute_step
  (call_intent -> call_permit -> call_observation)*
step_result
```

所有帧精确绑定首次 `request_id` 和 `binding` 中的 tenant、Worker、Run、step、
session、attempt、token、游标与不可变资源 hash。执行器不能提供 URL、方法、路径、
代码或下一游标。一个步骤同时只有一个待许可 intent/物理调用；search_policy 固定为
version、tags、query_embedding、search 四个子请求，绑定同一逻辑工具执行。

`call_permit.granted=true` 只能来自控制库新预留成功且 `newly_reserved=true` 的
响应。重复预留、丢 ACK、重新启动执行器不能重建发送许可。调用者应先调用
`Conversation.Accept` / `Conversation.accept`，发送前再调用一次
`CanDispatch` / `can_dispatch` 消费本地许可。校验失败、stop、截止到期立即关闭
本地会话，禁止后续发送。外层监管器仍负责在每次网络发送前校验真实 lease，保持
Heartbeat、计算传输后剩余时限以及终止/回收进程；本模块不承诺跨系统原子取消。

`dispatch_ms` 与 `call_ms` 分开：派发最多 30 秒，调用最多 60 秒，免费业务/metadata
HTTP 最多 10 秒；截止时刻相等即到期。执行步骤剩余时限最多 180 秒。
所有时间由调用者注入的本地单调时钟计算，不用这个时钟裁定数据库执行权。
当前控制账本对登记的本地 query embedding HTTP 采用更保守的 10 秒上限，
chat HTTP 上限为 60 秒；它们仍分别需要物理调用许可并占用相应预算。

传输结果、业务结果和 `usage_known` 分开。完整合法 usage 可伴随 rejected 输出；
免费 HTTP 不接受付费 usage，未知响应没有 usage。未知结果保留预算的处理属于控制
账本，帧校验不退款。已过正常执行期限的合法计量使用独立 `SettleUsage` RPC；不能
重新发送过期的 observation 或恢复旧步骤。Schema 不接收供应商价格或 caller 自报费用。

checkpoint、input 和 result 是唯一允许业务 JSON 的字段；对它们递归拒绝重复 key，
字节数按帧内原始 UTF-8 JSON 值计（包括内部空白和转义），分别限制为 256 KiB、
16 KiB 和 8 KiB（工具）/16 KiB（模型、方案）。实际业务 schema、引用来源和
commit hash 由已登记 profile 与控制服务另行校验。

步骤结果由控制服务 `internal/run/checkpoint.go` 的 `StepResult` 验证。当前唯一图标识
为 `bounded_readonly_v1`：`read_ticket → get_order → get_delivery → search_policy →
model_proposal → submit_proposal`；首个模型结果明确要求纠正时，只允许在提交方案前
增加一次 `protocol_correction`。该标识仅登记有界步骤规则；B 的可执行 profile 只在
测试中注入，生产没有模型、提示词或合成成功的默认 profile。

`result` 必须完整包含以下字段，字段名区分大小写；空值与省略不同：

```json
{
  "schema_version": 1,
  "tool_invocation_id": "",
  "physical_call_id": "",
  "evidence_refs": [],
  "content": null,
  "proposal": null,
  "correction_required": false
}
```

工单步骤的 `content` 必须等于接纳时冻结的工单，引用固定为
`business-evidence:<snapshot_uuid>:ticket`，不产生调用。工具步骤必须绑定同一
Run、attempt、step 下已经观察成功的逻辑工具与最后物理调用；政策检索同时核验
四个子调用的完整顺序。订单/物流引用来自相同快照的工具响应，政策引用来自相同
索引的最多三条返回段落。仅格式正确的未返回引用不能用于模型方案。

模型步骤的 `content=null`、顶层 `evidence_refs=[]`、`tool_invocation_id=""`，
`physical_call_id` 绑定已观察 chat。合法 `proposal` 精确包含 `decision`、`summary`、
`evidence_refs`、`action`：`decision=no_action` 时 `action=""`；`decision=proposal`
时 action 只能为 `record_conclusion`、`request_information` 或 `escalate`。引用必须
来自该 Run 已提交的工具证据或接纳工单。首次输出拒绝可用
`correction_required=true, proposal=null` 进入纠正；纠正不能再次要求纠正。
最终 `submit_proposal` 不执行调用，必须重复已接受模型方案的完整内容。

CommitHash 使用长度前缀 SHA-256 指纹，依次绑定域
`jobforge.run.commit.v1`、step ID、十进制 sequence、kind、十进制 cursor_version、
input hash、profile ID/hash、snapshot ID/hash、规范结果 JSON。JSON 对象键排序，
不删除内容；数字使用无冗余零的精确十进制值（不经浮点四舍五入），所以 `1e3`
与 `1000` 在 PostgreSQL JSONB 往返后仍有相同 hash。拒绝非有限值、不成对 surrogate
与 PostgreSQL 不支持的 NUL；步骤数最多 32，受保护工单、版本向量和全部结果累计
最多 256 KiB。工具信封上限 8 KiB，模型/方案信封上限 16 KiB。

重复 CommitStep 先验证当前执行权；过期、取消或已经释放的 lease 不能通过重复
请求恢复写权限。最终方案与 pending approval、快照/版本向量、许可期限、attempt
关闭和容量释放同事务；有建议进入 `awaiting_approval`，明确 `no_action` 才成功。
终态/yield 丢 ACK 使用 `GetAcceptedCommit`，按原 principal/session/attempt/token
只读返回完整结果与 hash，过期 session 不阻止查询，也不续租、不消费额度。

S1-B 仅交付 source、codec 和确定性协议检查。正式 guardian/step 进程监管、
父进程/guardian 死亡、stdout 故障收敛以及真实 DeepSeek 调用属于 S1-C，未由
fixture 或单元测试替代。

```powershell
go test -race ./internal/runprotocol ./proto/jobforge/agent/v1
.venv/Scripts/python.exe -m pytest python/tests/test_protocol.py
.tools/bin/buf.exe lint
.tools/bin/buf.exe generate
```
