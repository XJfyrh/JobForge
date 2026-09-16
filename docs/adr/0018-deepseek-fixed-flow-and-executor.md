# ADR-0018：DeepSeek固定流程与正式受监管执行器

- 状态：部分Superseded；原决策随[PR #41](https://github.com/XJfyrh/JobForge/pull/41)于2026-09-16接受（`01177e9`）。[ADR-0019](0019-executor-confirmation-and-exit-contract.md)经PR #44接受后，只取代普通observation后的继续执行时序并补充退出/收尾；其余模型、预算、业务策略及本文历史正文仍有效。契约接受不等于实现或真实模型验收通过。
- 日期：2026-09-16。
- 关联：[PRD v0.10](../product/JobForge_PRD_v0.10.md)、ADR-0013～0017。
- 取代范围：细化ADR-0014的正式进程、DeepSeek与计量边界；在S1-C新策略中增补ADR-0017的有限条件分支与方案schema，并升级尚未上线的执行器帧合同。Run仍是唯一调度实体，旧报告不改写。

## 1. 模型和预算

维护者已授权自主实现与真实云端验证，并指定DeepSeek优先。本次实施选择首批总上限 **5 CNY**，作为有界尝试额度；没有账户充值、自动增资或跨供应商兜底。该额度不是40例完成承诺，只有真实账本结算后才可能继续使用释放的差额。

固定provider为DeepSeek官方HTTPS `https://api.deepseek.com`，固定非流式Chat Completions、`model=deepseek-flash`、`thinking.type=disabled`、`max_tokens=1024`、JSON object输出、temperature=0；不传图片、思考、stream或动态工具定义。S2工具决策另行细化。请求体≤64KiB，消息业务内容≤16KiB，原始响应≤64KiB，模型内容≤16KiB；超过上限明确失败，不截断后假装完整。不会仅靠字节数猜token数。

2026-09-16官方价格页标明模型版本DeepSeek-V4.1-Flash，高峰每百万tokens miss输入2元、hit输入0.04元、输出8元；一律按高峰价，预留不假定cache hit。官方模型元数据给出完整上下文1,048,576 tokens，以此作保守输入上界再加1024输出：单次hold为 **2,105,344 microyuan**。完整上下文与输出共享窗口，该预留刻意较宽，不声称两者可同时占满。金额使用整数microyuan并向上取整，保留价格/hash和响应身份；供应商实际微额舍入未保证相同。

family、tenant、batch继续原子共享上限；批次两次unknown即可能不足以再发送，不能退回未知预留。首次受理的身份重发、恢复和人工retry不创建免费额度。首批串行运行，Worker/tenant/profile并发均为1，批次包含全部40例和必要纠正/恢复，有效期6小时。每家族仍为12chat/8工具/8embedding/16metadata/8业务HTTP/44物理HTTP/一次纠正，token上限12,595,200；每租户20例按此乘20，批次按40乘算（480chat、320工具、320embedding、640metadata、320业务HTTP、1760物理HTTP、40纠正、503,808,000 tokens）。三层各自费用上限均为5,000,000 microyuan，而共享batch令总暴露不超过5元；不会把两个tenant额度相加当作10元。实际token/计费受响应后结算约束，本地embedding零费用仍计物理请求。

官方没有Flash不可变请求快照或原子价格锁定API。profile保存官方名、已观察版本、参数、endpoint、执行器、策略/schema、定价日期/hash，批次开始前复核官方资料与账号可用性；响应model/fingerprint作为审计线索，不宣称它们排除了别名漂移。发现不兼容身份、价格变化、usage超界或配置失效时停止新真实调用。无法确认价格或计费上界时不运行收费验收。

来源：[价格](https://api-docs.deepseek.com/zh-cn/quick_start/pricing/)、[发布说明](https://api-docs.deepseek.com/zh-cn/news/news260910/)、[模型上下文元数据](https://api-docs.deepseek.com/quick_start/agent_integrations/codex/)、[Chat API](https://api-docs.deepseek.com/zh-cn/api/create-chat-completion/)。网页可能变化，记录核验日期和实际配置；网页可用或鉴权200不等于推理验收。

## 2. 进程所有权与派发

Go Worker独占session/lease/心跳及gRPC，领取后先读已提交checkpoint，再启动固定Linux guardian。一个Run同一时刻至多一个guardian+一个step；按步骤重建进程也必须先完整Wait旧组。Python没有控制库凭据、不Claim、不续租、不排队、不重试Run。无用户可控shell、模块、文件路径、endpoint或工具注册。

guardian独立响应父控制EOF和信号，step只执行已绑定的单步。Go保持5s心跳并处理STOP/失联；正常终止有宽限随后Kill整个组并Wait。Go SIGKILL后guardian因控制管道EOF清理；guardian死亡后Go清理step；固定容器使用init回收孤儿。只承诺固定项目进程，不支持逃逸组的任意用户程序。

stdout仅帧、stderr只记录有界数量和固定错误码，不保存原始诊断。所有管道读写、后台读者、心跳与子进程有deadline/取消/等待路径。部署注入DeepSeek key和固定业务凭据，不能通过日志、帧、profile定义、结果、命令行或测试输出传递秘密。执行镜像不含gold。

每个真实HTTP经过call_intent→控制库ReserveCall→唯一新许可→一次发送→observation；Go验证step、subcall、hash绑定、工具身份和顺序。丢ACK或重复许可没有发送权，任何重新发送使用新ID和额度。search_policy的版本、tags、embedding、search仍分别授权。正式Python适配补齐异步许可与观察钩子，不让发送钩子返回后绕过剩余期限。

固定adapter构造不可变PreparedRequest，body只序列化一次并用同一bytes发送。参数hash为既有长度前缀Fingerprint函数的域 `jobforge.run.physical-input.v1` 加profile_hash、snapshot_hash、subcall、method、固定path、SHA256(body_bytes)；GET无body使用空bytes。凭据、TraceContext、调用ID与网络timeout不进入业务参数hash。endpoint/path/方法从固定部署和资源绑定推导；模型不能填URL。Go不从opaque hash独立证明向量或模型请求全文，可信边界是受监管的固定Python adapter，而非恶意任意Worker。业务validator（版本、digest、向量形状、响应绑定等）通过后才能报告accepted并申请下一个物理调用。

执行器退出且完整Wait、stdout无残片/尾随帧、stderr未越界后才能Commit。Commit ACK不确定时只做有界GetAcceptedCommit核对完整hash；暂时Found=false不证明原事务未提交，不能立刻Fail或重发业务调用。无法确认则停止本地执行，交给已提交事实或lease回收收敛。幂等控制RPC确认至多两次、共享2秒期限；不因此重新发送业务/模型HTTP。费用结算收尾使用下节独立窄权限，不延长执行权。

DB执行权以PG时钟裁定。为避免宿主墙钟偏移及IPC等待重置相对期限，Register/Claim/Heartbeat补充服务端authority观测时间；Go用RPC发起单调时间加服务器剩余期限作保守截止。新版帧固定同一Linux容器/time namespace的CLOCK_BOOTTIME毫秒时钟，并携带emitted_mono_ms，剩余时间以发出戳为锚，接收方扣掉管道排队时间；拒绝未来戳、溢出、过期和混时钟。Reserve使用reserved_at与dispatch/call截止同样映射。任何上游截止已过均不能派发，heartbeat不能延长原物理调用截止。

## 3. 计量与输出分离

完整usage需精确非负safeint：prompt_tokens=cache_hit+cache_miss，total_tokens=prompt+completion，可选cached_tokens出现时必须等于hit；reasoning明细不重复计费。非思考输出不因reasoning明细缺省而伪称已观测零。usage缺失、矛盾、断连、截断、超时均保留unknown。完整合法usage即使JSON/方案不合法也独立结算；原始模型全文不进入普通日志/Trace。

S1-B普通call_observation的codec/Conversation按许可输出上限拒绝超界，不能承载异常计量归档。S1-C采用 `api/executor/v2`、version=2，v1源与fixture保留为历史合同，正式Worker仅使用v2。提供独立继承FD上的有界 **metering_report/ack** 帧及窄计量接收状态：只允许当前监管过的原request/binding/physical_call_id，无动作/游标/许可字段；计量结构只限safeint与内部一致性，不按已预留token截断。原执行会话已停止、deadline已过或拒绝普通帧后，可提交本地已完整取得的计量；不会为等新计量延长进程寿命或重新发送HTTP。正常输出单帧仍≤384KiB、stderr≤8KiB；计量帧≤8KiB，每个物理调用至多一个不同报告，重复只确认。

Go只将该报告交原调用SettleUsage，数据库再次校验原principal/session/attempt/fence和30日窗口；报告不能重开Conversation、改Run终态、清新attempt active_call或提交步骤。异常由既有三层冻结逻辑保存；Worker RPC补充measurement_anomaly明确标志，usage_known=false不能被误读成免费或允许重发。重复计量幂等，异内容冲突。C不增加本地receipt journal：只承诺存活进程的有界补报，PG提交前Go/主机死亡可能失去报告并保留unknown全额hold，不能承诺凭空恢复usage。关闭执行后，对已收完整报告使用Worker拥有的独立≤2秒收尾context，永不授权新调用。收尾超时只表示提交未确认，不能假定事务没提交；未结算调用继续全额hold，已提交known/异常冻结不得改回unknown。以后只允许原身份/hash幂等确认，不产生新发送。正常TERM宽限100ms，随后Kill组、Wait直接guardian，并由init回收孤儿；必须核验旧组无存活进程后才启动下一组。网络补报不能延迟Kill。

正常observation与metering可以分离，但Go必须按已解析的物理调用身份处理计量，不能从任意损坏stdout中捞取字段。帧版本/共同fixture/schema同时升级；不绕过标准decode或把非法帧当trusted usage。Go/Python两端测试覆盖超界、停止后、重复、异身份及带敏感尾随内容。

v2普通observation以usage_disposition=unknown/reported及usage_hash=null/完整hash声明计量处置，不重复携带v1的usage对象。reported必须与标准解码的同调用metering_report汇合并取得有界SettleUsage确认，才允许后继许可、纠正标记或步骤提交；unknown明确保留hold，不等待未来供应商响应。丢报告、错hash、超界或结算未确认均停止后续执行与本次真实验收。metering_ack区分settled/anomaly/unconfirmed，后两者没有继续执行权。普通EOF/step_result不能跳过计量reader有限排空和Join。普通执行帧仍按实际收到时点检查原截止，等待计量不能重置期限；异常计量不能恢复被拒或过期结果。

## 4. 固定流程与方案

S1-C登记新的support固定策略，沿用现有step种类，服务端从已提交事实决定有限下一步骤。get_order确认无关联订单时跳过get_delivery，否则补查；政策查询由固定代码基于工单与已有事实构造，≤512 UTF-8字节、top-k=3。不允许读取gold或按case ID直接选择结论；不扩展为通用DAG。

Proposal在新策略schema中增加 `conclusion`、`requested_fields`、`target_ticket_status`、有界 `claims`，每条claim指向实际返回的证据和结构位置。持久方案保留decision/action/summary/evidence_refs，其中summary和完整引用由可信代码从模型结构生成，模型不能提供自由摘要。core只执行注册schema/图和来源边界，具体政策判断与评分保留在隔离评分器。方案仍不可直接写入；no_action要求空写动作。建议目标为记录结论/无动作保持原状态、补充信息awaiting_information、升级escalated；实际工单在C不因方案而改变。

冻结动作映射：gold的record_resolution对应record_conclusion，escalate_human对应escalate，request_information同名，none对应no_action及空action。不能把insufficient强制映射到补充信息（DEV-027为关键异常升级且字段空集）。新策略名为 `support_fixed_v1`，方案schema为 `support-proposal-v1`；旧bounded_readonly_v1的严格字段不被静默改写。源JSON Schema与Go/Python共同fixture在实现PR交付。

### 4.1 方案结构与评分边界

模型输出恰含以下六个非null字段，拒绝重复/未知JSON键、非法UTF-8和尾随内容。它不能输出tenant、URL、代码、自由理由或别名映射。

| 字段 | 类型与闭集 |
|---|---|
| decision | proposal / no_action |
| action | proposal对应record_conclusion / request_information / escalate；no_action对应空字符串 |
| conclusion | on_time / delayed / disputed / insufficient / conflicting |
| requested_fields | 0～5个唯一值：ticket.order_id、order.delivery_id、delivery.usable_tracking_events、delivery.delivered_event、ticket.problem_description |
| target_ticket_status | open / awaiting_information / escalated / informational_only |
| claims | 1～4个不重复的严格对象，种类如下 |

每个claim公共必填 `kind`、`refs`；refs为1～8个唯一短引用，至少一个实际返回的政策段落。其余字段只有表中所列，全部必填且非null，不接受任意predicate表达式。

| kind | 额外字段及允许值 |
|---|---|
| timing | test=delivered_not_late / delivered_late / outstanding_not_overdue / outstanding_overdue_lt48 / outstanding_overdue_ge48；event_id |
| dispute | type=non_receipt / wrong_address / unauthorized_recipient / unauthorized_safe_place；delivered_event_id |
| critical | status=lost / damaged / returned_to_sender；event_id |
| missing | field取requested_fields同一词表 |
| conflict | type=pre_handover / same_time / source_key / post_delivery / order_delivery；event_ids，前四类恰2个不同ID，order_delivery恰空数组 |
| correction | recovery_event_id、corrected_event_id，两者不同 |
| ticket_status | mode=informational_no_action / preserve_escalated |

别名T绑定已捕获工单，E1/E2绑定本Run实际返回的订单/物流evidence；只接受源schema列出的字段指针。P01.1～P10.2仅在该段落确实被本Run检索返回时进入别名表。event ID长1～128且符合既有ValidIdentifier，须在绑定E2中唯一存在，再由可信代码展开真实数组pointer；不能用另一tenant的同名事件。短引用确定性展开为完整ref和source pointer，去重完整引用≤32；展开后的方案仍≤16KiB、checkpoint总计≤256KiB。别名优化不改变1024输出token上限，超限如实失败。

生产仅校验结构/来源，不加载gold、不纠正业务结论。模板忠实表达模型的建议/主张，不能额外宣称已解决、关闭、退款或通过审批；模板本身也要有反例测试。离线评分器重新计算时序、48小时边界、当前/被更正事件、政策优先级和目标状态；核对必要claim覆盖且任一额外不支持主张均失败，不能用大量正确引用稀释错误。DEV-024的same_time/source_key、DEV-012/015的充分争议证据允许显式等价集合，不能强迫唯一答案串。

客户描述/承运商注释语义只在评分侧登记带实体/版本/原文SHA256/UTF-8跨度的锚，闭集为customer_dispute、vague_problem、carrier_source_key、carrier_correction。评分时从实际返回文本重算hash与位置；case ID只用于定位评测对象，不是通过开关。未登记或不支持的语义计失败，不调用额外收费judge。注入安全由实际调用轨迹、账本与业务写入检查证明，模型自报“已忽略”不作证据。

### 4.2 纠正与错误

第一次模型结构/引用不合法可以提交明确纠正标记，进入唯一protocol_correction；纠正仍新物理调用和额度。普通Conversation在rejected observation后保持失败关闭原则：step_result为error/OUTPUT_INVALID，只有首次model_proposal、完整rejected、当前有效lease、严格correction_required=true且proposal=null的标记能由Go提交“已观察的可纠正失败”。其余error.result不Commit，纠正再次失败则MODEL_PROTOCOL_ERROR永久结束。不得修补输出为成功、自动丢字段/证据或循环纠正。usage与纠正决定分开，停止/超时不以纠正绕过。

错误映射固定：INPUT_INVALID→INVALID_ARGUMENT，坏帧/错身份→EXECUTOR_PROTOCOL_ERROR，大小越界→CHECKPOINT_TOO_LARGE，完整无效模型方案→MODEL_PROTOCOL_ERROR；已知临时网络/依赖与超时分别为DEPENDENCY_UNAVAILABLE/TIMEOUT。收到STOP、失权或本地取消只停止，不把它猜成供应商故障。识别可信usage超界时立即锁存本Worker停止新Claim/许可/纠正并终止本地进程，同时在有界收尾提交SettleUsage；仅收到measurement_anomaly确认才声明数据库冻结成功。未确认则停止真实验收并报告，不自动退款或恢复收费。当前仍有效的Run以MODEL_PROTOCOL_ERROR永久失败，已失权只停止，已终态不改写。

## 5. 数据、评分与验收

消除P01与P05/P06的注释事实歧义时创建新政策/语料/索引版本；明确carrier事件注释可提供被指定规则认可的事件关系陈述，任何命令均不授予权限，用户描述不成为carrier更正。新真实embedding/20查询报告追加，不覆盖旧19/20及RQ06未命中。

40开发例gold在首次运行前独立审核并冻结，记录审核主体为Agent而非虚构人类。机器评分包括action/conclusion/字段集合/建议目标状态与结构化claim的实际来源和谓词；采用结构化claim可信渲染可读summary，不允许自由摘要增加未经验证的业务承诺。每次运行保留全部40行、失败/未尝试、原引用、版本和费用；不修改分母或利用保留集调参。C是可比较的开发基线，不追加质量百分比门槛；须40例均实际执行且安全硬失败为0，业务错误逐例计失败并报告。S5的保留集80%/90%门槛仍独立适用，不能拿开发结果替代。

先运行确定性真实PG/HTTP和Linux进程故障，再运行真实模型层。原S0探针是回归，不等于正式Worker通过。S1-C验证正式进程基本失联/清理；完整恢复点、跨Worker恢复与从头重执行成本对比仍由S3交付。各切片合并后继续S2～S5，不能以40例开发基线代替整个目标完成。
