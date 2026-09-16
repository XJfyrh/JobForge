# ADR-0017：Run 接纳、执行权与物理调用账本

- 状态：Proposed；独立评审合并后才作为实现依据。
- 日期：2026-09-16。
- 关联：[PRD v0.9](../product/JobForge_PRD_v0.9.md)、[ADR-0013](0013-durable-agent-run-and-step-commit.md)、[0014](0014-supervised-python-executor-and-call-budget.md)、[0015](0015-approved-business-actions-and-receipts.md)、[0016](0016-business-snapshots-and-policy-retrieval.md)。
- 取代范围：无；细化此前未固定的接纳、重试家族额度和线协议边界。旧版本历史结论保持不变。

## 1. 上下文与交付边界

S1-A 已有独立业务服务、不可变快照和真实检索。仍缺少生产 Run、有效 lease 下的逐次发送授权和持久预算。当前 search_policy 实际包含四次 HTTP，现有 Python before_send 钩子没有控制库授权语义。直接接上收费模型会遗漏物理调用、取消与 unknown 窗口。

S1-B 实现 Go 控制服务、Worker RPC、HTTP/SDK 和数据库合同；S1-C 将正式受监管执行器及 DeepSeek 接入该合同。B 的测试 fixture 只能构造确定性网络结果，生产不得注册假模型。S2 的动态工具决策、S3 的完整进程恢复、S4 的批准/写入和 S5 的展示/保留集另行验收。

只调度 runs。步骤和调用不是队列实体；不创建影子 jobs，不并行启动旧调度器，不引入调用 dispatcher、DAG 或外部工作流引擎。新控制服务承载 API、Worker RPC 和唯一有界恢复扫描器，Worker 仅执行已领取 Run 的下一步骤。

## 2. 身份与接纳

### 2.1 五种身份

| 身份 | 作用域与含义 |
|---|---|
| business_request_id/key | tenant 内稳定业务意图；唯一 key，保存原创建时间、7 日 retry 截止、根 Run、固定 profile 和预算账户 |
| run_id | 一次提交或人工 retry 的执行；retry 新 ID、空游标，原 Run 终态不变 |
| attempt_no/fencing_token | 同一 Run 每次 Claim 递增；attempt 与故障 recovery_count 分开 |
| tool_invocation_id | 一次可能执行的逻辑工具；由有效 Worker 申请，重做使用新 ID 并再占次数 |
| physical_call_id | 一次可能发出的 HTTP；唯一 ID，不是供应商的幂等保证，不允许跨进程重放许可 |

Submit 使用必填 Idempotency-Key（1～128 个 ASCII `[A-Za-z0-9._:-]`，首字符字母或数字）。Submit 的操作键作用域为 tenant+submit+key；cancel/retry 为 tenant+目标 Run+操作类型+key。业务请求键使用相同字符约束。tenant 与操作者角色只来自鉴权。

输入为严格 JSON：schema_version=1、ticket_id、business_request_key、profile_id、budget_batch_id、run_timeout_seconds（缺省 3600，1～86400）；显式 null、未知/重复/大小写别名字段、尾随数据、非法 UTF-8 和非整数均拒绝。请求最大 4KiB。profile_id 是不可变登记版本，不能重绑定 hash。

规范请求指纹使用 Go 权威的固定字段顺序、UTF-8 值和长度前缀 SHA-256 编码；整数规范为十进制、不带前导零。输入归一化只填冻结默认值，不 trim、不改大小写、不做 Unicode 归一化。指纹包含接口版本、操作种类、上述字段和 retry 来源；不含 Idempotency-Key、TraceContext 或传输 timeout。profile hash 在接纳记录中独立冻结并核对。跨语言共同 fixture 锁定缺省/显式默认等价；SDK 不自行裁定幂等。

同一业务请求键绑定同一规范 Submit 内容。完全相同业务内容即使换 Submit 操作键，也返回首次根 Run，并原子记住该操作结果；异内容 CONFLICT。不能用新 Submit 键重开执行或重置预算。需要新 Run 只能对 failed/cancelled 使用 retry；新业务意图须新业务请求键，仍受租户/批次总限额。

人工 retry 是单链：一个终态来源 Run 最多一个直接后继，数据库 unique(retry_of) 强制。不同 retry 操作键、相同规范参数返回该后继并登记操作结果；异参数 CONFLICT。后继再次失败须对后继 retry，不能对旧来源另开分叉。已存在后继的重发无需重新 capture 或检查余额；该收紧用于避免同一业务意图并发分叉，不影响自动恢复仍使用原 Run。

### 2.2 跨库接纳顺序

1. 鉴权、严格解码，先读取该作用域已接受操作：同指纹返回首次对象，异指纹 CONFLICT。读取既有操作无需 profile 仍可派发或还有余额，但始终检查当前权限和租户。
2. 未受理时验证不可变 profile、批次授权及有效期、业务键关系。retry 先验证来源资格和业务请求 7 日窗口，继承原 profile/账户/批次；请求只含 schema_version 和可选 run_timeout_seconds，不允许改工单或模型。
3. 在控制事务外使用受信 operator 身份 capture 业务快照，单次 HTTP≤10s、无隐式重试。acquisition key 是域分隔 SHA-256(tenant、submit/retry、操作键、retry 来源)，不是业务意图键。相同操作重发取得同一快照；新 retry 操作可取得新事实。
4. 验证返回的 tenant、ticket、schema、snapshot/content hash、完整版本向量和索引 profile。然后开启短控制库事务，先锁业务请求（不存在时用唯一键插入竞争），再锁来源 Run（retry），再锁操作记录；锁后读取新鲜 DB 时间，重新验证接纳条件与同键结果。原子保存关系、ready Run、初始游标/事件和操作结果。
5. 唯一冲突读取已接受结果并比较，不能盲目成功。业务 capture 已提交但控制事务失败时可能留下孤立快照；同操作后续请求复用。不同 Submit 操作键指向同一业务键时也可能产生未引用快照，返回的仍是首次根 Run。

Run deadline 自首次控制库接纳时间起算；同操作重发不延长。retry 新 Run 可有新 deadline，但不延长原业务请求窗口、批次有效期或未来已经存在的动作许可。没有 initializing 状态或跨库 2PC。

接纳并发默认每 tenant 2、进程总计 8，不设置无界等待队列，满时 429 QUEUE_OVERLOADED；这是每进程资源保护，不声称跨副本全局限频。快照未引用不等于失败时自动删除。本切片无自动清理；演示数据环境可重建，生产长期容量/清理仍未验收。

S4 在 retry 快照捕获前补充已授权动作/回执检查：unknown 不重发/续签，已应用复用；本切片没有授权记录，不能提前宣称该路径通过。

### 2.3 业务版本向量

业务 snapshot metadata 增补 schema_version=1 的 version_vector：ticket={id,revision}；order={id|null,exists,revision|null}；delivery={id|null,exists,aggregate_revision|null}；policy={version,revision,corpus_sha256}；index={id,profile_hash,content_hash}。不存在时 revision 必须 null；有关系但记录不可访问时 id 保留快照中授权关系的期望 ID，exists=false。无订单时 delivery.id=null，exists=false。

向量从已保存快照派生，不二次读取当前源行、不修改快照原 content_hash；snapshot_id+content_hash 始终是权威内容身份。补充字段不改变 S1-A 的首次快照幂等。控制面逐项核验并保存；模型不能填向量。S4 用相同关系/缺失语义比较当前事实，包括原缺失记录后来出现。

## 3. 数据与锁序

控制库只新增 versioned migration，不能放入独立 migrations/business。逻辑表为 business_requests、runs、run_operations、run_attempts、run_steps、run_events、run_approvals、budget_accounts、physical_calls、tool_invocations、worker_sessions、execution_slots；实现可合并字段相同的记录，不省略唯一约束或审计。

Run 子表使用 tenant+run_id 复合外键。唯一点包括业务键、操作作用域、Run+attempt、Run+step sequence/id、Run+event sequence、Run+approval、physical_call_id、tool_invocation_id 和预算 scope+id。审批表在 B 只有 pending 方案绑定；没有写权限。

每事务最多转换一个 Run。正常路径固定锁序为 **Run → 预算账户（family、tenant、batch，范围内 ID 排序）→ 调用/工具记录 → 子记录 → 容量槽（worker、tenant、profile，ID 排序）**。无关 Run 不能先占账户或槽再等 Run。接纳独立采用上一节的 business_request→来源 Run→operation 顺序，普通运行路径不反向锁 business_request；预算开户在业务请求接纳事务内完成、固定同序。管理员只锁账户，不反向等待 Run。

usage 补报可先无锁定位 Run，再按正常顺序加锁重读。账本/账户/业务身份关联禁止 ON DELETE CASCADE；B 不执行自动清理。合法 usage 补报截止为 reserved_at+30日，等号过期返回 CALL_SETTLEMENT_EXPIRED，不改变预留。S5 清理7日后的受保护checkpoint时须保留至少覆盖该窗口的薄 Run/调用身份审计行；unknown 占用不能因删数据返还，超过补报窗仍保留账户累计值和不确定性记录。具体清理器/人工核销是 S5 的独立验收，不由 B 暗中实现。所有有时间语义的条件取行锁后的 clock_timestamp()，等号到期，不使用事务启动时 now() 或 Go 旧时间代替。

索引服务于 ready Claim、running/stopping 到期、retry_wait 到期、approval 到期和 tenant 分页。用真实 PG 的 EXPLAIN (ANALYZE, BUFFERS) 检查；不复制旧 W4 吞吐目标。金额/token/call 使用非负 bigint，线协议值≤2^53−1，乘加先检查溢出。

## 4. 执行权、时间与停止

内部 Worker 使用与公开 API 分离的部署凭据，服务端固定能力/profile/tenant allowlist 和容量上限。Register 发放随机 session，绑定已认证 Worker principal 和进程启动身份；body owner 字符串不构成鉴权。session liveness 60s，Heartbeat 5s 更新。已有活跃 session 不允许同 principal 的重复注册取代它；重发同启动身份返回原 session。过期 session 不授予新执行权；重注册不能扩大配置容量。

Claim 一次最多一个 Run，FOR UPDATE SKIP LOCKED 取 ready 候选；锁后校验 profile、deadline 和容量，在同一事务增加 attempt/token、创建 attempt/event、占槽和写 running/owner/session/lease。初版服务端上限 worker=1、tenant=1、profile=2，配置只能显式调小或由管理员调大，不能来自 Worker 声明。槽为派生计数，可按实际 Run 对账，不能成为任务事实源。

lease≤30s；attempt_deadline=min(claim_time+180s,run_deadline)，Heartbeat 不超过任一上界。每次步骤/预留/正常执行报告必须匹配 owner/session/token、running、有效 lease、profile/snapshot 和预期游标；已到期尚未重领也 STALE_LEASE。旧 Worker 不能因结果已经存在而得到写成功。

运行取消/attempt deadline/Run deadline进入 stopping，保存首次原因；后到 cancel 另记事实。stopping 的 Heartbeat 返回停止控制，不续 lease，不允许新步骤/调用。Go 终止本地执行器后 AcknowledgeStopped 验证原 attempt/session/token 及 stopping，允许 lease 已到期但不得有新 owner/attempt；这是停止确认，不是正常执行授权。重复关闭改用只读 attempt/commit 查询。

停止确认或 lease 到期时依序收敛：已接受 cancel→cancelled；否则 Run 到期→failed/RUN_DEADLINE_EXCEEDED；否则 attempt_timeout 按恢复余额→retry_wait/failed。等待态取消立即结束。awaiting_approval 同刻触及 Run 和许可期限时 RUN_DEADLINE_EXCEEDED 优先；仅许可到期为 APPROVAL_EXPIRED。

临时依赖错误/失联实际安排恢复时 recovery_count+1，默认最多 3，延迟依次 1、2、4 秒；attempt_no 每 Claim 增加。永久参数/协议/profile/预算错误失败，不因 HTTP 429 字样一概重试。网络/5xx/受控临时依赖失败可恢复；执行器协议错误永久失败；可纠正模型输出只走一次登记的协议纠正步骤。扫描≤1s，单轮最多100个候选、逐 Run 短事务。重复 RPC/扫描不再次结算 attempt 或释放槽。

任何关闭 attempt 的事务都同时脱离它的 active_call，将尚未确认的调用保留为 unknown、保留全部 hold、关闭 attempt 和释放槽。新 attempt 不继承旧许可。旧 ObserveCall/停止 ACK/usage 不能清除新 attempt 的指针；usage 唯一允许的变化是自身账本结算。

到期、取消和新授权的先后由同一 Run 锁串行决定。授权先提交时请求可能已在途；取消只阻止后续授权与处理，不能保证供应商停止计算或退款。正常结束/yield 前必须确认本地步骤执行器已完成；历史 unknown 可以保留，不能把仍在执行的调用假报关闭。

## 5. 固定步骤与方案提交

S1-B 仅登记顺序步骤枚举和校验器：读取工单绑定、读取订单/物流、检索政策、模型方案、一次必要的协议纠正、提交方案。具体合理策略/提示词在 C 固定为不可变 profile；B 测试规则仅在测试入口注入。Worker 不提交任意 next cursor；服务端依据当前 profile、已提交结果和注册规则计算下一步。

每次 CommitStep 保存 step_id/sequence/kind、input/profile/snapshot hash、完整 commit_hash、受保护结果及新 cursor_version，和元数据事件原子提交。最多32个持久步骤，总 checkpoint≤256KiB；单工具≤8KiB、chat≤16KiB/1024输出token、方案≤16KiB。超限明确失败，不静默截断已接受证据。引用必须来自同 Run 已提交的工具结果或提交时取得的工单 evidence，格式合法不足以通过。

同内容中间 CommitStep 在仍有效的执行权下幂等，不同内容 STEP_CONFLICT。yield/终态提交释放 lease，丢 ACK 后授权只读 GetAcceptedCommit 返回完整 hash 和 attempt outcome；禁止再用旧 lease 提交或报 Fail。当前新 Worker 读取 checkpoint 复用持久结果，未提交结果可重做且可能再次计费。

有动作建议时同一事务写方案内容/ref/hash、snapshot/version_vector、pending approval 和 permission_expires_at=min(DB时间+1h,run_deadline)，关闭 attempt 为 yielded_approval、清 owner/lease/active_call、释放槽并转 awaiting_approval。明确 no_action 才 succeeded/no_action。B 不产生 applied/rejected，不开放批准/写入的空实现。

受保护内容保存在控制库有界 JSON，引用 `run-step:<run_uuid>:<sequence>`、`run-proposal:<run_uuid>`；只通过当前鉴权 tenant 查询，无公开直链。普通事件和 Trace 仅记录身份、状态、hash、时长与数量，不记录模型/工具全文、秘密或内部推理。

## 6. 三层预算

### 6.1 账户与计数

family 账户覆盖根 Run 和全部人工 retry，是对 ADR-0014 Run 额度的明确收紧；单 Run 使用量另统计。三个账户为 family、tenant、batch；无自动按日/月清零。batch 允许管理员指定多个 tenant allowlist，总额共享，每 tenant 仍有自己的账户。只允许部署管理员开户/提额；普通 Submit 不可传金额。批次到期禁止新接纳/调用，既有记录/结果/usage 仍可读取或结算。

金额使用 CNY microyuan，价格为不可变 profile 中的整数分子/分母与版本，不使用浮点。每层限制次数、token 暴露与费用暴露：

`exposure = known_usage_upper_cost + reserved_or_unknown_upper_bound`

必须满足 exposure+新预留≤limit。费用报告区分保守价表估计和供应商账单；不把估计声称为实际扣费。具体价表、完整输入上界和真实验收 B 在 C 明确后才能收费；B 的合成价表只用于自动测试。

family 上限固定：chat 12、逻辑工具 8、query embedding 8、profile metadata HTTP 16、业务工具 HTTP 8、总物理 HTTP 44、协议纠正1。get_order/get_delivery 各一业务 HTTP；search_policy 每次为 version/tags 两 metadata + embed + search。逻辑工具先 BeginTool 占一次计数；同 ID 重发不能再执行，重做换 ID 再占一次。chat/embed/metadata/business HTTP 各在其物理许可占相应类别。预留后次数永不退回，取消/发送前死亡亦如此。

按已登记步骤逐个许可，不能用一次工具许可绕过其后续子请求。S1-A 的离线免费固定语料准备仍按 ADR-0016 独立有界；Run 前 snapshot capture 单独记接纳元数据、最多一次/请求，不假装属于尚未存在的 Run。内部控制 RPC、SDK 查询不递归记为业务物理调用。

### 6.2 预留、unknown 与结算

1. Worker 仅为下一次 HTTP 请求申请新 ID。ReserveCall 验证当前执行权、step/subcall、input/profile/价格hash、逻辑工具身份、active_call为空、三层上限和批次期限；一次事务预留并设置 active_call。dispatch_expires_at≤当前 lease/attempt/Run/批次截止；call_deadline≤reserve时间+60s、attempt/Run/批次截止，免费工具另受10s上限。dispatch 前检查许可仍有效；已开始的请求可在 Go 正常续租下等待到 call_deadline，丢失 lease 则停止本地等待。许可不因续租而延长，也不将30s lease误作所有请求的固定超时。
2. 相同 ID 只返回既有记录和 newly_reserved=false，不再次产生许可；异内容 CALL_CONFLICT。无法确认首次 Reserve ACK/执行帧是否收到时，保持 unknown，不能重用旧许可发送。新物理发送用新 ID、新额度。
3. Go 将调用 ID、step/input hash、session 和剩余期限传给固定 Python 能力。同进程重复 ID 返回记录或拒绝，不再次发送；新执行器进程不加载旧许可。无供应商 Idempotency-Key 保证时不得暗示请求 exactly-once。
4. ObserveCall 区分传输结果、业务结果与 usage。当前有效 Worker 可关闭 active_call，但不能顺便推进游标；停止/回收路径可将其关闭为 unknown。单个 Go/Python 发送前检查和网络之间仍有竞态，不能声明跨系统原子取消。
5. known usage 需要完整、合法且匹配预留的受信执行报告。第一次结算在三层账户按相同 delta 减少 token/费用保守 hold；次数不退。同 receipt/usage hash 幂等，异内容 CALL_CONFLICT。完整 usage 即使 finish_reason=length 或输出 schema 无效仍可结算；业务步骤独立失败。
6. 无响应、断连、截断、超时、进程死亡、缺少 usage 均保持 unknown 全额 hold，不自动退款或后台重新发送。晚到合法 usage 可以从 unknown 首次转 known。
7. SettleUsage 使用内部 Worker principal 认证并匹配当初调用 session/Run/身份；不要求仍有有效 lease，原 session 过期不阻断其已授权计量补报。该窄接口只能结算自身既有调用，不能新建调用、关闭 active_call、更新游标或 Run 状态；模型或公开 SDK 无此权限。profile 指定 usage 字段及上界，不能接受 caller 自报费用或仅 receipt 字符串。
8. usage 超出预留边界时保存异常计量及真实报告，冻结该 family/tenant/batch 的新授权；不截断数字、返还额度或伪报预算通过。已受理步骤/Run终态不被计量覆盖。该异常属于不再满足硬预算前提，须报告并停止真实验收。

同 Run 一次 active_call 仅限制当前有效本地执行器。故障后的旧远端计算可能与新 attempt 重叠；三层预留保护费用暴露，不提供供应商全局并发保证。

## 7. 公开与内部源协议

### 7.1 HTTP/SDK

| 接口 | 行为 |
|---|---|
| POST /v2/runs | 首创201，幂等200；返回 Run 及 reused |
| GET /v2/runs/{id} | state/outcome/error、attempt/recovery、profile/snapshot、游标摘要、期限、预算和方案引用；failed Run 仍200 |
| GET /v2/runs | 默认20、最多100；tenant内 created_at/id 降序、状态过滤；cursor有版本且绑定tenant/过滤，游标不构成授权 |
| GET /{id}/steps、/{id}/events | sequence升序、after非负、默认20最多100；事件只元数据，步骤详情为受保护内容 |
| GET /{id}/result | 200，available、kind（proposal/no_action/final 或null）、ref（或null）；awaiting_approval 的 proposal 不称 applied |
| POST /{id}/cancel | 独立幂等键；返回已接受操作引用和当前 Run，取消running先stopping；先鉴权/识别旧操作再判断新状态 |
| POST /{id}/retry | 独立幂等键；新 Run201、重发200；仅failed/cancelled及原请求7日内；继承身份/预算，空游标 |

普通 operator 可提交/cancel/retry，reader 只查询。自己租户缺少角色403，跨租户404。SDK 是薄类型封装，无隐藏重试和后台队列。错误 envelope 沿用 nested error；400 INVALID_ARGUMENT，401 UNAUTHORIZED，403 FORBIDDEN，404 NOT_FOUND，409 CONFLICT/ALREADY_TERMINAL/INVALID_TRANSITION，429 QUEUE_OVERLOADED/RATE_LIMITED，500 INTERNAL，503 DEPENDENCY_UNAVAILABLE。

内部执行冲突为 STALE_LEASE、CANCEL_REQUESTED、STOP_REQUESTED、STEP_CONFLICT、CALL_CONFLICT；预算拒绝为 BUDGET_EXHAUSTED，profile缺失为 PROFILE_UNAVAILABLE，计量窗口过期为 CALL_SETTLEMENT_EXPIRED。执行错误存在 Run/error，不把 GET 改成错误。RPC 错误通过 google.rpc.ErrorInfo 的 reason 稳定映射：参数 INVALID_ARGUMENT、鉴权 UNAUTHENTICATED/PERMISSION_DENIED、不存在 NOT_FOUND、冲突/状态/profile/预算/窗口过期 FAILED_PRECONDITION、临时容量 RESOURCE_EXHAUSTED、依赖 UNAVAILABLE、内部 INTERNAL。不靠英文错误文本判断是否恢复。

公开 OpenAPI 源放 api/run/v2/openapi.yaml；Proto package jobforge.agent.v1，沿现有 Buf+protoc-gen-go/go-grpc 生成，禁止手改生成文件。Go HTTP DTO 和 Python SDK 类型按源契约实现，共同请求/响应/错误 fixture 及真 HTTP 跨语言测试检查一致性，不再复制一份不同规范。

### 7.2 Worker 与 Python 帧

Worker RPC 包含 Register、Claim、Heartbeat、GetCheckpoint、BeginTool、ReserveCall、ObserveCall、SettleUsage、CommitStep、FailAttempt、AcknowledgeStopped、GetAcceptedCommit。没有任意 UpdateCursor 或无校验 Complete。恢复/已接受结果查询先于新增额度检查。

Go/Python 帧以 api/executor/v1/schema.json 为事实源，UTF-8 JSON Lines，单帧≤384KiB（容纳最大checkpoint和封装），请求包含version/request_id/run/step/session/profile/hash/剩余毫秒/TraceContext；严格拒绝未知/重复字段、错身份、超大帧和多余 stdout。Go/Python 显式 codec 由共同 fixture 验证，不引入新的运行时 Schema 框架。Schema 和 fixture 与相应实现一同评审，不能只更新说明。

消息顺序为 execute_step → 零或多个串行 call_intent→call_permit→call_observation → step_result；每时刻仅一个待许可 intent/物理调用。intent 只能选择已注册 subcall 和参数hash，目标/方法/路径由可信 profile 与资源绑定推导；不能让 payload 指定 URL。许可携带单次ID和剩余时间；拒绝/取消/到期立即停止后续发送。Go 在等待帧期间仍续租、处理 stop；Python 不持租约。

Python stdout 仅协议、stderr只固定脱敏诊断且有界；Go监管一个 guardian+最多一个固定step进程组。正式生命周期、父进程死亡/guardian死亡、stdout异常、清理时限和真实调用接缝在 C 实现，并复用 S0 进程门禁；本 ADR 不把 source fixture 当作生命周期验收。

## 8. 替代方案、代价与验证

- 从旧 job 包装 Run：产生两套状态与租约，拒绝。
- 每调用独立排队：没有顺序单 Agent 的实际需要，增加第二调度器，拒绝。
- 快照与 Run 的跨库事务/initializing：增加未出现需求的状态与恢复；采用幂等 capture+孤立资源披露。
- 每次人工 retry 重置额度：可绕过业务请求上限；采用共享 family 账户，代价是耗尽后须管理员有审计提额或新业务意图。
- 未收到响应就退款、自动重发：无法证明未发送，违反预算边界；接受 unknown 会使批次提前停止。
- 一次工具预留包含全部后续请求：取消无法阻止后续发送；采用每物理请求许可，代价是额外内部 RPC/事务。

验证按 PRD v0.9 的 B-01～10：真实 PG 的锁等待/事务故障/并发与 race、API/SDK错误映射、调用身份/三层额度/计量、migration与权限、源协议/生成一致性、定向性能。源码与测试不得把缺依赖 skip 标通过。现有 W4 失败、AT-25 跳过、云端与生产留存未验收仍保留。

本决策合并后只表示契约接受。S1-B 实施、S1-C 实际 DeepSeek/工具链、40例评分、S2～S5及完整目标分别报告完成状态。
