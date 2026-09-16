# ADR-0020：provider 审计报告、跨 FD 确认与首批停发

- 日期：2026-09-16；状态：**Accepted，随 [PR #47](https://github.com/XJfyrh/JobForge/pull/47) 完成独立审查并合并时生效；尚未实现或验收**。
- 关联：[PRD v0.12](../product/JobForge_PRD_v0.12.md)，既有 [PRD v0.10](../product/JobForge_PRD_v0.10.md)/[v0.11](../product/JobForge_PRD_v0.11.md)、[ADR-0017](0017-run-admission-and-call-ledger.md)/[0018](0018-deepseek-fixed-flow-and-executor.md)/[0019](0019-executor-confirmation-and-exit-contract.md)。
- 仓库核对：C3b 已通过 PR #46 合并，起点 `0829a4c`；本文定义下一合同，不修改已合并实现，也不声称本文已实现或验收。
- 本文只解决 provider 审计持久化、确认、读取和新首批批次停发；support_fixed_v1 的固定图/schema/模板与评分沿用 ADR-0018，不重新决定，也不扩展为 S2 动态 Agent。

## 1. 背景与选择

C3b 已实现固定进程、有效执行权、逐 HTTP 许可、独立计量、普通观察 ACK 与结果屏障。现有 `deepseek.complete_usage` 只在身份兼容且 usage 完整时返回响应身份；`AuthorizedDispatcher.recorded_audit()` 仅存内存。`SettleUsage` 只接受五字段 UsageReport，PG 没有返回 model/fingerprint/reasoning 的持久值。receipt_hash 不能还原这些元数据。

当前 unknown observation 禁止任何 metering report，metering_report 必有 usage，ACK 只确认 usage_hash；只追加 Python 字段不能建立跨 FD 的持久审计确认。普通 PROFILE_UNAVAILABLE 退出只结束当前 Run attempt，Worker 随后仍可能 Claim；Python 的 stop=True 不是批次停止保证。

选择最小桥：复用现有计量 FD、SettleUsage RPC、physical_calls 行及 batch 的 frozen 标志。报告允许“usage + audit”或“audit-only”；不用新 FD、新上报 RPC、新调度器或独立审计权限机。新的 report_hash 绑定两部分，普通 observation_hash 绑定 audit_hash，汇合仍发生在原 Conversation/Go coordinator。

拒绝的方案：仅把 identity 拼进 receipt_hash（不可查询）；普通日志/Trace 存正文（不安全且不持久）；只在 SDK 报告保存（挡不住进程死亡/重启与独立调用）；把任意审计字段放进计费 usage_hash（破坏已接受身份）；新增事件总线/receipt journal/通用审计服务（当前无需，且不能改善未提交前主机死亡的事实缺口）。

## 2. 精确取代范围与版本

本决策仅对新审计 profile：

1. 细化 ADR-0018 §3 的 metering_report/ack 字段和“报告必为可结算 usage”假设，允许不具定价资格的观测计量及 audit-only；计量 FD 总帧 8KiB 不变。
2. 细化 ADR-0019 §1 的普通 observation hash 与跨 FD 汇合：新增 audit_hash；报告 ACK 仍不替代普通 ObserveCall ACK。
3. 为新首批新增“chat unknown 即停止后续收费”的保守政策，并定义 provider 审计原因的 batch-only 冻结。旧 profile 原先 unknown/full hold 的状态、计量与 late 权限不追溯改变。
4. SettleUsage 在新 profile 下扩展为保存原调用的不可变计量/审计报告，可处理 audit-only；RPC 名称保留，不增加对 Run 的修改权限。Proto 原字段号/语义保留，新增字段的具体合同见 §6。

保持不变：ReserveCall 唯一新许可、Go 独占 RPC/lease/心跳、BOOTTIME/原 deadline、现有固定退出码、普通 ACK、Commit、100ms TERM 宽限、Kill/Wait/EOF/Join/组消失、独立总计≤2s 收尾、30 日原调用结算窗口、unknown 不退款、usage 异常三层冻结、at-least-once、业务幂等和 5 CNY/40 案边界。

内部帧继续 `version=2`，但固定执行器标识升级为 `linux-v2-audit-runtime-1`；新 profile definition 固定 `provider_audit_policy=deepseek-audit-v1`，manifest 与全部 profile 的 executor_version 必须一致。旧 `linux-v2-ack-runtime-1` 的 schema/fixture 只依赖 Git 历史保存，不新增历史 runtime 包/源目录，不保留双 codec 或兼容执行路由；正式部署只运行一套版本。不重写 v1 历史结论。

升级前已预留调用由持久 profile definition 和预留时的版本决定，不由当前进程猜测。仅这些原调用的 usage-only SettleUsage（新字段 absent/空）继续原身份、30日窗口内的窄晚到合同；旧 Worker 不获得新 Claim/Reserve 权限。不能要求旧行补造新 audit/hash。这是原调用可靠性，不是新旧双 scheduler/codec；不设计在途普通 v2 混用迁移。

## 3. ProviderAudit v1：数据与信任

### 3.1 严格对象

DeepSeek chat 的报告必须有一个 `provider_audit` 对象；非 chat 报告该字段恰为 null。本增量不为 Ollama 另设 provider 审计模式，其既有 usage/receipt 验证保持。

ProviderAudit 恰含下表字段，均必填；只有标明 nullable 的字段可为 null，空字符串不能替代缺失。拒绝重复/未知键、scalar null、非法 UTF-8、尾随数据和非整数数字。JSON integer 必须为数字，不能为 ProtoJSON 字符串。该对象原始编码和最终编码分别≤2048 bytes，整个 metering frame 含 LF≤8192 bytes；不截断供应商字段以通过上限。

| 字段 | 类型与含义 |
|---|---|
| schema_version | integer，恰 1 |
| provider | string，恰 `deepseek`；不是 endpoint |
| response_complete | bool；仅表示已取得受上限约束的完整响应 body，不表示 provider/body/业务有效 |
| http_status | integer；完整响应为 100～599，无完整响应为 0（不是猜测的供应商状态） |
| response_sha256 | nullable lowercase SHA-256；完整响应为固定 adapter 实际校验的完整 body bytes 的摘要，否则 null；不对半截 body 生成“完整摘要” |
| identity_state | `compatible` / `incompatible` / `invalid` / `unavailable` |
| response_id | nullable ValidIdentifier（ASCII 1～128） |
| response_model | nullable ValidIdentifier；实际返回值，不替换成请求 model |
| system_fingerprint | nullable ValidIdentifier；观察线索，不是不可变版本锁 |
| created | nullable integer，0～2^53−1；provider 原值，不改写为本地时间 |
| usage_evidence | `complete` / `absent` / `invalid` / `unavailable` |
| reasoning_state | `observed` / `absent` / `invalid` / `unavailable` |
| reasoning_tokens | nullable integer，0～2^53−1；仅 observed 时非 null，且不超过对应完整 usage 的 output_tokens |
| mode_state | `nonthinking` / `unexpected` / `invalid` / `unavailable` |
| audit_hash | 严格 lowercase SHA-256；按 §4 计算 |

这些字段是受监管固定 adapter 的有界事实报告，Go 不从 response_sha256 重建或重新验证 HTTP 原文。可信边界仍是固定 adapter，不支持恶意任意 Worker。

### 3.2 捕获和缺失规则

adapter 只读取一次有界响应，在任何异步 hook 和业务 schema 验证之前冻结可捕获事实。审计提取不得因业务 proposal 无效跳过；也不得为了补齐审计重新请求 provider、等待新响应或延长 child 生存。body 仅在正常验证需要的进程内保留，不新增原文持久化。

- 无完整响应（包括未完成读取、截断、超时、超限）：http_status=0，response_sha256 和全部身份值为 null；identity/usage/reasoning/mode 状态为 unavailable。若 child/Go 在连这份报告也没保存前死亡，PG 显示 missing report，而不是伪造一份 unavailable 报告。
- 完整非 200：保存真实 status 和完整 body hash；身份/计量状态 unavailable、身份值及 reasoning_tokens 为 null。不解析任意 error message 或把错误正文中的字段当 Chat Completion 计量。
- 完整 200：严格解码顶层 Chat Completion。合法完整身份且 model 等于冻结的 expected response model 为 compatible；完整、安全身份但 model 不同为 incompatible。缺失/非法身份字段、重复键、错误 object/top-level shape 或无法严格解码为 invalid。只保存通过各自边界校验的身份值；不合法原值以 null 和 invalid 状态表达，不保存截断原值。顶层无法无歧义解码时全部身份值为 null。
- usage_evidence=complete 当且仅当五个计数/明细满足既有精确 safeint、hit+miss=prompt、prompt+completion=total、可选 cached_tokens 和 reasoning 明细一致性要求，且具有足以构造 receipt 的完整、安全身份。**此处允许安全但不兼容的 model，仅表示计量结构完整，不表示可以定价。** complete 时 report.usage 非 null；其它三态 report.usage 为 null。absent 指合法对象没有 usage；invalid 指出现了但不满足合同；unavailable 指没有可解释的完整 Chat Completion/身份。
- reasoning 的跨字段矩阵固定如下：只有 usage_evidence=complete 且 report.usage 非 null，才允许 reasoning_state=observed 或 absent；明细缺省为 absent/null，显式 0 为 observed/0，合法正数为 observed/该值，服务端必须核对该值≤同份 usage.output_tokens。负数、bool、字符串、超 safeint、超可解释的 completion 或非法明细结构为 invalid/null，并使 usage_evidence 非 complete；不把它转换成 0。usage 为 null 时，reasoning 只能为 invalid/null（已能明确证明该明细非法）或 unavailable/null（无法形成可验证完整计数），不能只因一个孤立字段长得像整数就保存 observed。即使 raw reasoning 本身是安全整数，若 aggregate usage 不完整也不宣称已核对其 output 上界。
- mode_state 只描述固定非思考/无工具 envelope：合法单 choice/assistant 形状且 reasoning_content 缺省/null/空串、tool_calls 缺省/null/空数组、reasoning 为 absent 或 observed/0 时为 nonthinking；非空 reasoning_content/tool_calls 或已合法 observed 的正数 reasoning 为 unexpected；choice/message 结构非法或已观察 reasoning 明细非法为 invalid；无法解释 envelope/计数且无其它明确模式违规为 unavailable。只保存闭集状态，不保存 reasoning/tool 正文。
- finish_reason 不为 stop、content 不是所需 JSON 或 proposal/schema/来源无效，仍按既有输出合同失败；不能仅因此把兼容身份、完整计量改成 unknown。它们不自动等于 provider 模式漂移。大小和停止仍不得进入纠正。

### 3.3 计量与定价资格矩阵

| 事实 | 保存报告 | 对原 usage/账户的操作 | 新首批后续收费 |
|---|---|---|---|
| compatible 身份 + complete usage + nonthinking/缺省 reasoning | audit + usage | 按冻结价格正常结算；超预留则既有 measurement anomaly | 正常计量且普通 ACK/期限有效才可继续 |
| compatible 身份 + complete usage，但业务 JSON/schema/来源失败 | 同上 | 正常计量，与纠正/业务失败独立 | 仅原合同允许的首次纠正；不是全批 provider 停发 |
| compatible 身份 + complete usage，但正数 reasoning/非空 reasoning 或 tools/非法 choice | audit + usage | 合法计量仍按原价格结算，不重复加 reasoning；超预留另按 anomaly | 停批；不得纠正来绕过模式不兼容 |
| incompatible model + 内部完整计数 | audit + usage（观测计量） | **不调用当前 profile 的 UsageCost，不写成 known，不释放 full hold**；若计数同时超原 token 上限，则仍记 measurement anomaly | 纯身份问题仅 batch 冻结；同时超 token 上限则三层异常冻结 |
| identity invalid、usage 缺失/非法、reasoning 非法、无完整响应 | audit-only | unknown/full hold；不编造计数或零费 | 停批 |
| 内部完整 usage 超原 token 预留，不论 model 是否兼容 | audit + 原始完整 usage | 沿用完整计量归档、full hold、三层 measurement anomaly 冻结；不截断，不以此证明原价有效 | 停止 Worker/批次 |

本矩阵区分“结构合法”和“按当前定价可信”。审计不阻塞合法计量的承诺，限**原调用第一份一致报告、兼容身份与完整计量**，且只与业务输出有效性分离；不承诺给错身份、内部矛盾或相互冲突的报告按原价退款。供应商违背合同导致的实际收费不能由本地旧价格证明有上界，应停批并如实报告不确定性。

## 4. Hash 身份与完整绑定

使用既有 Go domain Fingerprint：domain 和每个字段都为 uint64 big-endian UTF-8 byte length + bytes，SHA-256 lowercase。无 JSON canonical 浮点算法。整数为无前导零十进制；bool 为 `1`/`0`；nullable 的 null 编码为空串，合法非 null identifier/hash 从不为空，整数 0 编码为 `0`。共同 fixture 固定下列顺序。

`audit_hash = Fingerprint("jobforge.run.provider-audit.v1", schema_version, provider, response_complete, http_status, response_sha256_or_empty, identity_state, response_id_or_empty, response_model_or_empty, system_fingerprint_or_empty, created_or_empty, usage_evidence, reasoning_state, reasoning_tokens_or_empty, mode_state)`。

`execution_binding_hash = Fingerprint("jobforge.run.call-binding.v1", tenant_id, run_id, worker_id, session_id, attempt_no, fencing_token, step_id, step_sequence, step_kind, cursor_version, input_hash, profile_id, profile_hash, snapshot_id, snapshot_hash)`。

该 binding hash 在新 profile 的 ReserveCall 事务中由**已校验的 Lease/Step** 计算并保存，不能到 late settlement 时从当前 cursor 猜回原步骤。模型 call 未必已有 committed run_step，不能假设事后查 run_steps 总能恢复这些字段。

`report_hash = Fingerprint("jobforge.run.call-report.v1", execution_binding_hash, physical_call_id, parameter_hash, usage_hash_or_empty, audit_hash_or_empty)`。

report hash 不包含 emitted_mono_ms 或 RPC 重试时间。request_id 仍由原 IPC Conversation 核对，是 pipe 相关身份，不进入持久报告 hash；服务器未见该值时不能假装验证过它。DB 从预留行取 binding/parameter/物理调用身份重新计算 report hash，不能仅回显来值。

usage_hash 与 `jobforge.run.usage.v1` 域不变。chat 的非 null usage.receipt_hash 必须能由同份 audit 的实际完整身份重算：现有 `jobforge.deepseek.receipt.v1`、physical_call_id、response_sha256、response_id、response_model、fingerprint、created。Go 再检查 receipt 和 usage hash；不兼容的 response_model 仍以**实际值**参加 hash，但不能因 hash 自洽就获得定价资格。

新普通观察为 `Fingerprint("jobforge.run.observation.v2", transport_outcome, decimal_http_status, mapped_domain_error_code, business_outcome, usage_hash_or_empty, audit_hash_or_empty)`。错误映射继续 ADR-0019，绑定字段继续由原 Frame/Conversation 核对。旧 profile 持久 observation.v1 保留；不回写历史 hash。

## 5. 同一计量 FD 与普通 FD 的汇合

### 5.1 新帧差异

共同 common fields/binding/时钟规则不变。只改变以下字段：

- `metering_report`：原 call_sequence/physical_call_id/parameter_hash 保留；usage 改为 nullable，新增必填 provider_audit（nullable）和 report_hash。至少一项非 null。chat 必须有 audit，非 chat 必须 audit=null 且 usage 非 null。每个原物理调用最多一份不同报告，内容冻结后不得从 unknown 就地补成另一报告；相同报告可幂等确认。
- `metering_ack`：原 usage_hash 替换为必填 report_hash；settlement 闭集为 settled/recorded/anomaly/conflict/unconfirmed。只有 settled 表示可定价 usage 已完成正常结算；recorded 表示审计及可有的不可定价观测计量已保存，**不表示结算或继续权**；anomaly 表示既有三层 measurement anomaly 已确认；conflict 表示冲突停批事务确认；unconfirmed 不作任何持久化/冻结断言。report_hash 用于关联当前提交；只有前三种成功记录处置且服务器返回的实际持久 report hash 与提交值相同，才能这样 ACK。conflict/unconfirmed 不是新报告已持久接受。
- `call_observation`：新增必填 audit_hash（nullable）。新 chat 非 null，非 chat 为 null。usage_disposition 的 reported 仍要求可用于正常继续的完整 usage；不得将 recorded 的不兼容计量包装成 reported + ordinary success。
- `call_observation_ack`：形状不变，observation_hash 使用 §4 新域。

两端 codec、Conversation、PipeHooks、Go coordinator 和 fixture 协调升级，不能依赖 C2 当前“先 settle 后 observe”的实现顺序证明跨 FD 正确。

### 5.2 接收与确认

普通观察可以先到，也可以报告先到。coordinator 只在原 reservation/request/binding/call_sequence/parameter 身份完全匹配后缓存有限帧；最多原 step 固定调用数，不新增队列。normal reported chat 必须同时满足：report/audit/receipt/usage hash 全部一致、SettleUsage 返回实际保存 hash、正常 settled、无 batch stop、原 deadline/执行权仍有效，然后调用 ObserveCall。

ObserveCall 在其事务中重新核对：该 call 已有匹配报告、对应 usage 正常 settled、audit_hash 与报告相同、请求字段产生的 observation.v2 hash 一致、当前 live execution/step/active_call 有效。普通 ACK 只能在这个事务成功之后发送。Go 解码、写入计量 FD、SettleUsage 成功或有 report row 都不能代替普通 ACK。

audit-only / 不兼容计量只获得 recorded，并触发 §8 停发；不再等待一个可能永远不会产生的普通 ACK，不进入纠正或 step_result 成功。普通 FD 已关闭后只接收原调用已捕获的窄报告并清理；即使晚到 settled 也不能重新打开普通 Conversation。

无 report 的非计量免费调用沿用原 unknown observation + ordinary ACK；不能因 `usage_disposition=unknown` 一概禁止 audit-only 报告，也不能反过来取消免费调用的 ordinary ACK。

## 6. Proto、持久事务与幂等

### 6.1 追加字段

建议保留现 `UsageReport` 字段 1～5 与计费 hash。新增 typed `ProviderAudit`（nullable string/int 使用 optional/显式 presence；不用任意 JSON bytes 代替 typed 校验）。枚举都有 UNSPECIFIED，作为输入拒绝。

- `SettleUsageRequest` 保留 execution=1、physical_call_id=2、usage=3；追加 provider_audit=4、report_hash=5。新 profile 必须填 report_hash 并遵守 §5；旧 profile 仍仅接受原 usage-only 形式，不通过空 audit 猜 profile。
- `SettleUsageResponse` 保留 newly_settled=1、reservation=2；追加 persisted_report_hash=3、persisted_audit_hash=4、report_conflict=5、batch_frozen=6、batch_stop_code=7。hash 来自事务后实际行。audit-only 时 newly_settled=false；重放不把“本次没有改变”解释成未保存。
- `CallReservation` 追加 execution_binding_hash=12、persisted_report_hash=13、persisted_audit_hash=14；历史无数据为空串。它们是审计事实，不是新许可。
- `ObserveCallRequest` 保留原 1～9；追加 audit_hash=10（非 chat/旧 profile 为空，新 chat 为完整 hash）。新的普通逻辑不能绕过该字段通过旧路由获得确认。

不增加审计写 RPC，不让 Python 直调 RPC。Proto 字段最终命名/编号应在实现源 schema 与 buf 检查中锁定，若仓库同期分配了号码需顺延且在实现 PR 明示，不能占用已使用号。

### 6.2 最小新增存储

新增 versioned migration，在 physical_calls 上保存：execution_binding_hash、report_hash、call_report（恰 usage/provider_audit 的有界 typed JSON）、report_recorded_at、首次 report_conflict_hash（nullable）。audit_hash 可从 typed report 得出，但若单独索引/存列必须约束与报告相同。旧行均为明确 legacy null，不回填猜测审计。

call_report 保存不兼容 model 的内部完整计数，原 `physical_calls.usage` 代表可定价的 known 计量，或已保留的超 token 上限 measurement anomaly；不能借同一列让不兼容计数变成 known。异常标志与 price_eligible 分开：结构完整计数超界可使三层冻结，即使身份失配禁止按原价结算。原 known/held/status CHECK 语义不静默放宽。新增 batch_stop_code 字段仅用于 scope=batch 的 frozen 原因，值为 §8 闭集；没有 thaw/重置 API。

原 observation 的 HTTPStatus/ErrorCode 若用于新 calls 查询，另存 typed 列（null 表示 legacy 未记录/未 observe），与 observation hash 同事务保存；不能从 hash 反推或从 Run 最终 error 代填每次调用。

### 6.3 首次报告事务

维持既有锁顺序：Run → family/tenant/batch → 原 physical call；不持锁调用网络。服务器读取预留时不可变 profile/价格，核对原 principal/session/attempt/fence、binding/parameter/report/audit/usage/receipt hash、30 日窗口。

一份无冲突的标准报告在**一个事务**内完成：保存 call_report 和实际 report_hash；如果 §3 定价资格成立则执行现有 usage 结算或 anomaly；按 §8 必要条件冻结 batch 并记录原因；然后提交。不能出现“已 ACK report，但 audit 仍在另一个异步事务/队列里”等待的状态。因事务失败返回不确定时，不能声称其中任一步已完成。

定价资格不成立时，不按原价结算；原未知预留转为/保留 unknown，全额 hold 不释放，UsageKnown=false。**measurement anomaly 独立于定价资格，但只评估首次被接纳的报告**：该报告的完整 input/output/total 超原 token 预留，即保存完整计数和 anomaly、冻结三层，不因 model 不兼容而丢弃异常；此时仍不调用原价计算。只有纯身份/模式问题且计数不超界时仅冻结 batch。

完全相同 report_hash 的重放重新校验绑定和 typed 内容一致，返回实际首次报告与当前账户事实，不重新扣计数或释放 hold。收到标准解码通过、原调用认证正确但不同 report_hash 时，保留第一份报告及其财务事实，至多保存第一份冲突 hash，冻结 batch 并返回 report_conflict=true；该停止事务必须提交后才返回 conflict，不能通过返回普通 error 导致事务整体 rollback 而丢掉冻结。

不同报告是原“一次响应/一份不可变报告”合同的冲突，不能挑选其中更低的计数，也不能拼接第二份 identity 与第一份 usage。第二份 usage 的结构合法不自动证明它是该原请求的可信计费事实；本草案选择**不以冲突报告改变财务结算**，明确不承诺在这种信任冲突中“抢救字段继续结算”。即使第二份报告的计数超出预留，也只记录冲突并冻结 batch，不保存第二份计数、不新触发 measurement anomaly 或 family/tenant 冻结。首次 unknown 的 full hold 保留，首次 known/anomaly 的已提交财务事实不撤销，批次停发，报告中披露冲突。若需要进一步人工供应商账单核对，另行设计，禁止自动重查/重发。

坏 wire、错 hash、错原身份、越界对象整帧/请求拒绝，不从未经标准解码的帧抢救 usage。对正常供应商非法 metadata，固定 adapter 应依 §3 表达为合法的 null+invalid，避免“保存不了原始坏字段”阻塞其它可确认事实；这不授权从坏 IPC 解码半份数据。

## 7. 晚到权限、读权限与保留

### 7.1 原 session 的窄晚到权限

沿用 `reserved_at + 30 days`（等于边界即拒绝）和原 session/lease 身份：auth principal 必须仍是登记 worker，仍有该 tenant 权限，DB 原 session 必须存在，request execution 必须与预留行的 worker/session/attempt/fence/run/tenant 完全相同。原 session 可以过期、Run 可以终态、旧 profile 可以被停用；不能用新 session 代替旧身份，也不能由其他 worker/operator 上报。

服务器从预留时的 profile/price/binding 验报告。late report 只保存/确认原报告、计量和必要账户停发；不续租、不改 Run 终态/游标、不清新 attempt active_call、不重新激活 profile。late 不要求当前批次还在 6h 有效期，否则会错误剥夺原计量权；已冻结也不阻止原报告的幂等确认。

报告必须是存活进程此前已完整捕获的事实。沿用独立总计≤2s 收尾、最多两次同身份 RPC 确认，不延长 Kill/Wait 的期限；不引入本地 receipt journal。Go/主机在 PG 提交前死亡仍可能丢失报告而留下 unknown/full hold。新合同不许从已发出的 audit-only 报告升级/重写另一份报告；原报告同内容 late 重确认与旧 profile 既有 usage late 路径仍有效。

### 7.2 只读查询

新增 `GET /v2/runs/{run_id}/calls`，沿用现有 public reader/operator tenant 身份，不接受 tenant 参数；跨租户 Run 返回 404。现有 family 物理调用硬上限为44，单 Run 不会更多，故直接按 ordinal 升序一次返回≤44项，不引入分页/cursor/limit 参数。服务端事务一致只读，并在读取第45项时拒绝违反硬上限的数据；最终响应编码≤256KiB，否则固定错误，不能截断。只读查询不计模型额度，也不能产生 Reserve/Observe/Settle 操作。

返回本 Run 的 physical_call_id/step_id/attempt/ordinal/subcall、参数/profile/price hash、预留与观察/报告时间、保守预留/known/held 口径、usage_known/measurement_anomaly、report/audit hash、typed audit、observed usage 与 settled usage 的明确区分、report_conflict 和固定 batch 停发原因。调用预算的 hold 是暴露，不是账单。实体 schema/SDK fixture 在实现 PR 交付；不能让客户端把有 observed usage 自动当 known。

读取侧通过已冻结的 audit policy、subcall 和 nullable report 明确区分历史未采集、免费调用不适用、新 chat 缺报告、已记录；这些只是事实解释，不新增持久状态机。缺报告不等于未发出、免费或响应 unavailable。不得把后来的晚到结果倒填成首次取证时已经拥有；导出记录抓取时间/hash以便复查后续结算变化。

不公开 worker control token、完整 ExecutionIdentity/fencing token、原始帧、endpoint 凭据或其它租户触发停批的 call/Run 身份。共享 batch 的 frozen/固定原因可见，但只返回本租户本 Run 行。Worker 原认证只用于既有窄写 RPC，不能成为跨租户 public reader；普通用户没有审计写入/解冻接口。

### 7.3 保留

S1 不增加自动审计清理任务。原调用、session、profile、报告及必要 hash 至少保留到原 30 日确认窗口结束，并保留到已发生冲突/未确认批次完成只读证据归档；不得为了重跑清空真实收费 batch。测试可重建库与真实收费库分离。后续更长生产保留、删除、恢复演练按 S5 单独决策；本条不能被表述成生产留存已经验收。

## 8. 固定停止谓词和责任

### 8.1 新首批 profile 的持久原因

标准解码、原身份和 hash 校验通过后，先分流：同 report_hash 仅返回原处置；已有不同 report_hash 为 `REPORT_CONFLICT`，按 §6.3 仅冻结 batch，不评估第二份计数的 anomaly 或价格；仅原调用尚无报告时，才按下列首次报告优先序生成固定 batch_stop_code。同 batch 已有原因采用 first-write-wins，原 call/audit 保留后续具体事实。frozen 一旦 true，任何低成本结算/重放/重新 bootstrap 都不能解冻。

1. `MEASUREMENT_ANOMALY`：结构完整 usage 超出原 token 预留，无论 model 是否兼容；沿用三层冻结。不兼容 model 仍不按原价结算。
2. `PROVIDER_HTTP_REJECTED`：chat 完整 HTTP 非 200；仅 batch 冻结。包括 429/5xx 的首批停批，不自动重复收费；固定记录 status，不猜测供应商错误正文或账号原因。
3. `PROVIDER_IDENTITY_INVALID`：完整 200 的 identity 为 incompatible/invalid；仅 batch 冻结。不兼容内部计数不按原价结算。
4. `PROVIDER_MODE_INVALID`：mode unexpected/invalid 或 reasoning invalid；仅 batch 冻结；其中兼容完整计量仍先按同事务规则结算。
5. `CHAT_USAGE_UNKNOWN`：chat 未获得可定价完整 usage，且未命中以上更具体原因；仅 batch 冻结/full hold。

这是新首批保守停发政策，不改变其它旧 profile 的一般 unknown 语义，也不把 free metadata/business HTTP 的零计量或 embedding 缺少 DeepSeek audit 错判为 chat unknown。

控制面在相同账户锁事务中将 batch.frozen=true 和原因保存。新 Claim 与 Reserve/BeginTool 及正常后续路径必须检查账户冻结；使用既有拒绝分类，不新增 Run 状态。原 transaction 内排他检查保证冻结提交后不会再得到该 batch 的新许可。未确认冻结也不能绕过下面的持久 guard。

**以前一条 chat 的已有事实挡住下一条许可。** 每次新 Claim/BeginTool/Reserve，在持有既有 batch account 行锁时，检查该 batch 所有先前 chat reservation；只要有一条尚未具备以下全部事实，就不得签发新执行/HTTP许可：原 report 已持久、usage 已按兼容价格正常 known 且无 anomaly/停批、对应 ordinary observation 已持久且 hash 匹配，并满足下面两种结束屏障之一。不能只检查 usage_known、report row 或进程内 ACK 标志。该查询由既有 business_requests/Run/physical_calls/run_steps/run_attempts 关联得出，不新增队列、锁租约或 pending 状态表。

- **步骤提交**：引用该 physical_call_id 的原 step 已成功 Commit，内容为正常结果或首次唯一合法纠正标记。
- **二次业务校验终态失败**：仅限 `protocol_correction` 的 ordinary observation 已持久为 rejected / `MODEL_PROTOCOL_ERROR`，原 Run 为 failed，原 run_attempts 行有 finished_at、outcome=`failed_terminal`，且 Run 与 attempt 的 error_code 都为 `MODEL_PROTOCOL_ERROR`。call.attempt_no 必须等于 Run.attempt_no；原 attempt 的 worker/session/fence 必须等于 call，Run 保留的 fence 必须相同。使用终态 Run 未推进的 next_step_id/kind、next_input_hash、cursor_version、sequence=cursor_version+1、profile/snapshot 与原 attempt 身份重算 execution_binding_hash，必须等于该 call 预留值；不能以同 Run 的另一步/另一个 attempt 的失败解除此屏障。现有 FailAttempt 已在 live lease 下逐项验证原 Step，并同事务保存这些终态事实，不新增失败写入接口或伪造 Commit。此分支只允许下一案例，并不恢复失败 Run。

该窄失败分支覆盖“唯一纠正再次产生不合格方案”，令其按单例业务失败计入40案分母，而不伪装成 provider 停发。直接尺寸超限的现有 stop 路径没有 ordinary rejected observation，因此仍阻断本批后续收费；租约失效、取消、超时、依赖失败、EXECUTOR_PROTOCOL_ERROR、另一 step 的 MODEL_PROTOCOL_ERROR 也不能走此分支。不能仅凭 child exit code 或 Run.state=failed 解锁。持有 batch 锁后只读关联旧终态 Run/attempt，不再获取旧 Run 行锁，避免反转 Run→账户的既有锁序；读不到完整已提交事实即拒绝本次新许可。

本 guard 排除“正在确认同一原调用”的 SettleUsage/ObserveCall/CommitStep，以及当前持有执行权的 GetCheckpoint/Heartbeat/失败清理；否则会锁死释放 guard 所需的原事实。Reserve 的 guard 在插入新的 reservation 之前检查先前记录，不让刚插入的本次 call 阻止它自身；同 ID 重放最多返回 newly_reserved=false 的已有事实，绝不重新授权发送。guard 只拒绝新的执行/网络许可，不能当作过期执行权的恢复通道。同一 batch 行锁下的检查+新 reservation 插入确保最多一条尚未越过上述屏障的 chat。显式 capacity=1 不替代该持久检查；另一个 driver 或 Worker 重启也必须经过它。

因此 Reserve ACK 丢失、无报告、报告提交未确认、普通 ACK 丢失、尚未达到任一结束屏障的 chat 都会留下阻断事实，即便 batch.freeze 根本没成功。只有原报告、观察及匹配的步骤提交或上述窄终态失败均已实际持久时，才不再是待确认窗口；例如 Commit ACK 丢失但事务已提交，可以通过只读事实证明屏障完成，不能仅凭重启就认定已完成。若原 step 未提交、不满足上述窄失败且 lease 已失效，本次首批不会自动重新收费恢复该步骤；只读报告保留阻断，40案可能无法完成，不清 call/补假 Commit 来解锁。本合同不增加人工解锁或另建batch绕过路径。

一个已有许可已发出的外部 HTTP 仍不能由后来的冻结撤销；本 guard 只保证不会再获得新许可，不承诺供应商取消或退款。

### 8.2 组件职责

- **Python**：捕获 typed audit/计量；识别这些条件后停止当前普通调用/纠正，只尝试已捕获报告的有界上报与清理。不得把 stop 标志当作 DB 已冻结。
- **Go Worker**：从标准报告与 reservation/profile 独立核对停止条件，即刻锁存不再 Claim/许可/纠正并终止当前组；仍处理已捕获报告的既有≤2s 收尾。仅收到报告持久确认及 batch_frozen/measurement_anomaly 才断言冻结成功。审计-only/unknown 不产生普通继续 ACK。
- **控制面**：原调用事务落审计/符合资格的计量/批次冻结，之后才回确认；冻结检查是最终持久拒绝。报告不能直接修改 Run。Go 在完整清理后仍有执行权且具已知固定失败事实时，才按既有 FailAttempt 路径结束当前 attempt：身份/模式不兼容为 MODEL_PROTOCOL_ERROR；usage 异常仍按 ADR-0018；冲突为 EXECUTOR_PROTOCOL_ERROR。只有“无完整响应”不能猜成供应商协议失败，可保留已知本地 TIMEOUT/DEPENDENCY 分类，否则 Abandoned 交既有回收；已 STOP/失权不写失败。
- **SDK 批次驱动/启动器**：一例一提交，只运行一个登记 Worker；固定容器 `restart=no`。发现 batch frozen、Worker 异常退出、报告/控制确认不确定、显式预算/6h 期限不足或人工前置复核失败，停止提交下一例并保存全部 40 行（含未尝试）。停止后的同一批次不可自动重启 Worker 或用新 session/新 batch 继续。不得提供自动 thaw/top-up/fallback。

### 8.3 未确认和外部前置失败

RPC 超时/断连、report ACK 不确定、坏 IPC/收尾失败只说明本地未确认。Go 本地停止所有新工作，SDK/启动器记录固定 `CONTROL_UNCONFIRMED` 或 `SUPERVISION_FAILED` 并停批，不能声称服务器没有提交、已冻结或退款。对失联的服务器无法保证写入一个停止标志，因此本草案**不声称未确认时存在持久 batch freeze**；进一步收费由 §8.1 的已有 chat reservation/report/observation/结束屏障事实 guard 阻断，而非靠进程内“已停”或 restart=no。单 Worker/restart=no 是首批运维约束，不是持久保证的替代。恢复前只读核对既有事实，不能重发业务来探测。

执行当天官方价格/模型合同无法确认、账号配置失效或审核发现 profile 与实际请求不符，由批次驱动在第一条新收费调用前停止并保持 profile 不启用。运行中发现外部价格变更同样停止本地 Worker/后续提交；不通过审计 RPC 填造一个不存在的 provider 响应，也不凭本地标志声称 DB 已冻结。需要改变预算或解除已确认 frozen，必须另行明确决策/授权，不在本合同提供操作接口。

## 9. 别名、隐私和能力限制

expected response model 与固定请求 model 来自不可变 profile，model mismatch 能被识别；fingerprint 变化本身只是一条观察，不直接证明模型版本或价格变化。供应商可在同一别名/fingerprint 下发生未披露漂移，系统不能检测全部变化，不能提供服务端不可变版本/原子价格锁承诺。当前 profile 价格只在供应商遵守已核对身份/计量合同时给出保守暴露估算，不冒充供应商账单。

审计只含本合同列出的标识、状态、数量和摘要。完整输入/输出、reasoning_content、tool_calls 内容、error message、headers/cookies、Authorization、业务数据和 endpoint secret 均不进入新增持久审计、普通日志或 Trace；stderr 不作 provider 审计协议。异常原值不截断成看似合法值。共同测试使用合成值，不在测试 fixture 放真实调用原文或凭据。

## 10. 实现与验收清单

1. 源 schema/Proto/Go/Python 共同向量：字段/null/size/safeint、四种 reasoning、身份兼容/失配、业务 JSON 失败但合法计量、无完整响应、完整身份但未知计量、receipt/audit/report/observation 两端实际 domain hash。
2. 实际 PG 事务：首次同事务、重复、冲突、不兼容计数不调用 UsageCost/不释放 hold、首次 measurement anomaly 原值及三层冻结、batch-only 冻结、并发 Claim/Reserve 拒绝、旧 session late/窗口等号/旧 profile 被禁用；原已知计量不可回滚成 unknown。首份 known、第二份不同且超 token 上限时必须仅 batch 冻结、首份计量保持，不能被判成新 anomaly。
3. 固定 Linux 正式安装包/实际 FD + PG/gRPC：report/observation 反序、wrong hash、DB 提交前阻塞/提交后丢 ACK、免费调用、ordinary close 后有限计量、最终 report ACK 丢失、Go/child 死亡、metering reader 关闭；无新 HTTP、无第二会话恢复、原 Kill/Wait/Join 屏障保持。
4. HTTP/SDK reader/operator、跨租户拒绝、≤44项及response 总大小、历史未采集/缺报告/已记录区别、共享 batch 不泄漏其它租户身份；报告最终 known+held 不把观测计数当已知费用。
5. 首批启动器 restart=no、单 Worker、40 行预登记、worker/control 不确定即停后续提交、无自动新 batch/增资；分别在 Reserve/report/Observe/Commit 的提交前后丢 ACK，再用新 Worker session/第二 driver 申请新许可，验证只有实际完整持久屏障才允许继续。另验证纠正再次校验失败+正常 known/Observe+原步 failed_terminal 允许下一案例；逐项移除/错配 attempt、Step、hash、error、observed 或终态事实均拒绝，尺寸停止/失权/取消也拒绝。停止不能靠固定 sleep、进程内标记或虚构数据库 freeze。
6. 适用 Go/Python/race/SQL/Buf/API/SDK/容器门禁、旧 profile late 回归和独立审查。真实云端只能在这些工程层与 support/scorer 冻结后，用已批准同批预算执行；确定性替身结果不计 C-07。

## 11. 后果

新增状态只是原报告事实、固定 stop code 与既有账户 frozen，没有第二调度器或可继续权限；代价是一次内部 v2 不兼容细化、typed Proto/PG/只读 API 增量。unknown 首次出现就结束新首批，可能无法完成 40 案，这是明确的保守政策而非成功承诺。未提交前主机死亡和价格别名漂移仍有不可消除的证据限制。

本合同不修改运行代码、不发起真实模型调用。接受不代表实现；上述行为仍须按清单交付代码和实际验证证据。
