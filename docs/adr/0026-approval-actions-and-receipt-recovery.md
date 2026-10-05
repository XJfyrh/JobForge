# ADR-0026：人工审批、签名动作与回执优先恢复

- 状态：**Accepted；实现与验收见[当前状态](../status.md)**。
- 日期：2026-10-05（Asia/Shanghai）；决策者：维护者。
- 对应：[PRD v0.18](../product/JobForge_PRD_v0.18.md)、[ADR-0015](0015-approved-business-actions-and-receipts.md)、[0017](0017-run-admission-and-call-ledger.md)、[0025](0025-confirmed-step-recovery.md)。
- 取代范围：仅新 schema 4/S4 profile，补充0015尚未固定的签名/接收协议、动作物理额度，调整0017的动作阶段 Claim/retry/查询顺序。0025恢复政策扩展到schema 4，原chat豁免谓词不放宽。旧schema 1–3/profile/方案、模型策略、费用分类和历史正文保持。

## 1. 已核对的接缝与选择

本决策扩展 S3 的已有调度与恢复边界；合同本身不作为实现验收证据。

| 当前源码 | 已有事实 / S4 接入 |
|---|---|
| `internal/run/checkpoint.go`、`postgres/checkpoint.go` | CommitStep原子保存方案、pending审批、版本向量并关闭attempt/释放槽；增加批准/拒绝领域决策和动作步骤结果 |
| `internal/run/service.go`、`postgres/admission.go`、`admit.go` | Retry先ResolveAdmission，但新后继目前先检查profile/账户、capture；在这两道新接纳检查之前增加已授权操作的回执路径 |
| `internal/run/postgres/lifecycle.go` | Claim目前只选Executable profile且先检查模型账户/审计guard；动作游标增加仅查询的Claim分支，保持同Run/attempt/fence/slot |
| `internal/runworker/lifecycle.go`、`worker.go` | 每Claim读checkpoint后执行单步；`apply_ticket_resolution`由Go执行，继续共用lease keeper/Heartbeat/Fail/原扫描器，不启动Python写步骤 |
| `internal/run/recovery.go`、`postgres/recovery_guard.go` | schema 3恢复policy与原attempt证明；schema 4保留相同chat判定，动作恢复使用原持久动作，不推导新授权 |
| `internal/business/store.go`、`version_vector.go`、业务0002 | 不可变快照/索引、包含缺失事实的完整向量、物流aggregate_revision已具备；增加首次写入事务和缺失事实并发保护 |
| `cmd/agent-control`、`run/httpapi`、`cmd/support-business` | 当前身份只有tenant/reader/operator；新增稳定审批身份、独立动作reader/writer及业务writer低权限pool |
| `api/run/v2`、`proto/jobforge/agent/v1`、`sdk/python` | 当前没有approval/reconcile/effect或动作RPC；源契约新增后生成Proto，薄transport/SDK同步 |

只调度现有Runs；审批、动作授权、回执视图和物理调用都是Run/业务请求的子记录。一个动作步骤完成原Run，不增加队列、通用权限框架或Python调度器。

## 2. profile、身份与审批

新definition `schema_version=4`，必填 `program.recovery_policy="confirmed_uncommitted_v1"`、`program.approval_policy="ticket_resolution_v1"`，并冻结`action={operation,origin,key_id,public_key_sha256}`；固定executor version `linux-v2-approval-runtime-1`，strategy/adapter仍为`support_agent_v1`/`support-agent-v1`。完整typed definition参与原profile hash，schema 1–3拒绝新字段。新增 `api/support/approval-v1/profile-schema.json` 与共同向量；更新v2 runtime-input中schema 4的读步骤定义，Python仍只执行原模型/只读能力。manifest、Register、Go/Python包和生产构建receipt匹配新版本，不改旧定义/hash。模型提示、决定/方案schema、gold与原恢复安全谓词保持。

公开身份新增受信 `actor_id` 和独立 `role="approver"`。approver仅可查询/审批，不因此获得operator的提交/取消/retry权；operator不获批准权。approver必须有合法、稳定、非秘密actor ID；旧reader/operator配置可继续读取与原操作，不由密钥派生审批者。新部署示例显式配置actor；轮换密钥可绑定同actor。会扩大权限的Worker/capture/业务reader/动作reader/动作writer/public身份凭据复用启动失败；不同租户按原账户共享同一个DeepSeek供应商API key不属于此拒绝范围。

审批请求闭集 `{schema_version:1, decision:"approve"|"reject", proposal_hash}`，必填独立Idempotency-Key。操作域为tenant+`approval:<run_id>`+key；请求指纹 `Fingerprint("jobforge.run.approval.v1", "1", tenant, run_id, actor_id, decision, proposal_hash)`。沿用现有8字节大端长度前缀/UTF-8/SHA-256及键约束。当前鉴权→tenant资源确认→同Run锁下识别已接受操作→新决定检查状态/hash/DB期限；同键异指纹CONFLICT，另键改变决定/actor或方案APPROVAL_CONFLICT。另键相同决定与actor只登记原决定别名，不能改首次身份/时间或再次排队。

复用`run_approvals`的tenant/run复合主键，新增 `decision_operation_id`、`actor_id`、`decided_at`，状态闭集pending/approved/rejected，决定字段成对约束。`decision_operation_id`是首次审批操作UUID，并作为签名中的approval引用；与业务operation ID不同。批准事务保存决定、next step ID/kind/input hash及ready事件；cursor仍指向最后已提交方案，下一序号=cursor+1，input hash绑定原proposal CommitHash和批准指纹。拒绝事务保存决定、succeeded/rejected及首次final引用，没有Claim/写入。取消/到期不改写已接受决定，审批view另返回当前可用性；新决定拒绝，旧操作可在当前鉴权后重放。

旧profile的pending方案只能查询/取消/自然到期，新审批决策明确PROFILE_UNAVAILABLE，不从等待状态推断写权限。新schema 4审批核验持久完整profile身份及原权限期限，不以当前Executable或模型账户为前置；Available使用相同条件。停用profile仍可拒绝或接受原方案；新授权/写许可另行核验当前可执行与账户/审计guard。

## 3. 固定动作及签名源协议

参数由控制面从已提交的完整方案构造：`ticket_id`及原方案的decision/action/conclusion/requested_fields/target_ticket_status/summary/claims/evidence_refs；decision必须为proposal，只准record_conclusion/request_information/escalate及原方案对应状态，不接受客户端在线编辑。现有模型/Go/Python线协议为record_conclusion；政策/gold的record_resolution在原score.py中显式映射为它，保持两者及原映射，不新增别名。业务将完整结论保存在独立有界resolution记录（≤16KiB），工单body只更新revision/status，避免把大方案塞入现有4KiB工单。record_conclusion保留工单状态，另外两种状态沿用原方案允许值，绝不关闭工单。

新源协议放 `api/business-action/v1/schema.json`、`fixtures.json`，Go共享编码/签名放`internal/business`的动作contract文件，供控制面/Worker/接收端使用，不引入循环依赖。JSON闭集、精确字段名、合法UTF-8，拒绝unknown/duplicate/null/尾随/非整数/不规范UUID或hash，动作请求≤32KiB、回执≤4KiB。JSON对象顺序/空白不影响身份；数组顺序保留，不trim或Unicode归一化。

摘要使用原Fingerprint的8字节大端UTF-8长度前缀（包含域本身）及SHA-256。参数摘要为`Fingerprint("jobforge.business.parameters.v1", "1", canonical_parameters)`；向量摘要为`Fingerprint("jobforge.business.version-vector.v1", "1", canonical_version_vector)`。两份JSON先按闭集typed合同校验，再按Go encoding/json的map键字节序、默认字符串escaping紧凑重编码；整数十进制、不带前导零，bool为true/false，nullable字段保留JSON null，不缺省、不转浮点；数组保留原顺序。参数为本节列出的完整八个方案字段加ticket_id，向量采用既有VersionVector的全部身份/存在/版本字段。未知/重复字段在canonicalize前拒绝。

authorization摘要域为`jobforge.business.authorization.v1`，其Fingerprint字段**依次**为：`"1", key_id, "apply_ticket_resolution", tenant_id, business_request_id, business_request_created_at, operation_id, run_id, approval_id, actor_id, decided_at, proposal_hash, parameters_hash, snapshot_id, snapshot_hash, version_vector_hash, authorized_at, permission_expires_at, authorization_expires_at, run_deadline`。时间均是UTC Unix微秒整数十进制（PostgreSQL精度），所有数值在安全整数范围。主体携带完整vector并重算其摘要，不允许只提供摘要代替版本。回执摘要为`Fingerprint("jobforge.business.receipt.v1", "1", canonical_receipt_without_receipt_hash)`，使用同样JSON规则。源README和共同fixture照此冻结字节及逐字段篡改向量；签名不覆盖可变查询/计数字段。

标准库Ed25519签名覆盖authorization摘要的32个原始字节；signature使用无padding base64url、固定64字节。`key_id`选部署受信公钥，签名私钥仅控制面持有，业务接收端只有公钥；profile冻结的origin/key_id/公钥SHA-256必须匹配部署，接收端按key_id+tenant绑定受信公钥，公钥摘要写入构建/启动证据。参数hash由接收端重算；签名主体、参数、快照及HTTP身份tenant必须完全一致。旧ID重放比较整个不可变authorization/parameters内容hash，不能只比较工单或参数。不引入JWT、模型持密钥或动态endpoint；签名密钥轮换需保留原key验证能力，不能重新签名/延长已授权内容。

授权事务锁原Run、核验session/owner/token/有效lease/动作游标/当前批准和新鲜DB期限，仅首次生成随机operation UUID、不可变内容和签名，唯一tenant+business_request防止retry建第二动作。permission来自原方案，authorization expiry不超过permission与原Run deadline；签名一经保存不延长。重取同授权必须先核验当前执行权，只有原Run可请求物理写许可；新Run继承该身份只读查回执。签名不含新Worker fence：接收端不是控制库的全局fencing执行者，授权先提交的在途动作可能在取消后提交。

## 4. 业务事务与最小权限

业务接收 `POST /business/v1/actions/apply_ticket_resolution`，只有tenant绑定action_writer凭据可调用；`GET /business/v1/actions/{operation_id}/receipt`只有独立action_reader凭据可查询。不向模型reader开放动作/回执路径。接收服务启动单独writer低权限pool；原runtime/loader/read pool的权限不扩大。

首次写入顺序：

1. 当前内部HTTP鉴权/严格解码；事务取得下述保护，再查tenant+operation回执。同内容已应用返回首次回执，不要求版本或授权仍有效；异内容ACTION_CONFLICT。
2. 无回执才验签/参数摘要、许可/原Run期限与批准时间关系，读取不可变snapshot核对对象关系、内容hash与全部前置版本；再比当前ticket/order/delivery/policy与原缺失事实，验证原published index的tenant/policy/corpus/profile/content绑定。
3. 最终修改前再次取业务DB时间；等号到期拒绝。原子保存结论、工单revision+1/允许状态及首次回执（operation/content hash、业务请求/授权/批准引用、ticket前后revision、applied_at、retain_until、receipt hash）。先查回执的顺序保证工单新版本不会误拒同操作重发。

缺失源行不能用FOR UPDATE保护。选择**业务库内一个固定事务advisory guard**，覆盖这个小型演示数据域的首次动作和源变更；保护键在新迁移中冻结。所有ticket/order/delivery/policy source INSERT/UPDATE/DELETE及policy index发布由数据库BEFORE STATEMENT trigger先取同guard，再取得目标行锁；Apply在任何receipt/源行读取前取guard。loader的ImportDataset/PublishIndex在各自事务开始即取guard，早于dataset_import/index/源行锁；其它有权限源变更也受statement trigger约束。策略读取在取得guard之后使用READ COMMITTED的新statement视图，不能使用锁前RR旧视图。

顺序为`guard → receipt/来源（ticket → order → delivery → policy → immutable index）→ ticket修改/resolution/receipt`，无需为只读依赖授予UPDATE以获取FOR UPDATE。不存在行插入、政策插入/发布和物流事件修改均与动作串行；现有源revision trigger保持，新增政策revision保护，published index仍不可变。禁止先锁源/索引行再取guard；loader接口与专用fixture写入遵循此序。owner/migration是受信管理者，不构造绕过trigger的生产写路径。

选择单guard是为了无需缺失对象占位表、额外索引版本协调表或串行化全业务模型。代价是不同租户的源变更/动作首次写入也串行；只保护有界短事务，≤40工单、无网络，设置短lock/statement timeout，锁等待耗尽返回DEPENDENCY_UNAVAILABLE。回执GET无guard且只读；动作POST重复仍是有界短事务。真实PG验证并发双向顺序/缺失插入/发布/回滚及定向执行计划，不能把小数据结果声称高吞吐生产能力。

新NOLOGIN业务writer role和专用LOGIN仅有元数据/必要源快照索引SELECT、工单revision/body列UPDATE、resolution/receipt INSERT/SELECT；无源INSERT/DELETE、订单/物流/政策UPDATE、snapshot修改、DDL或owner/loader成员权限。reader只读回执；startup分别检查角色/数据库purpose/schema/权限，错误不泄露DSN。全局角色名称需按业务0001同样检查归属，不覆盖其它角色或密码。

## 5. Go执行、物理额度与恢复

新增内部RPC `GetAction`（有效执行权读取原绑定）、`AuthorizeAction`（仅原Run首次签名/重取）、`ReserveActionCall`（query/write单次物理许可）、`ObserveActionCall`（内容有界的传输事实）、`CompleteAction`（验证当前执行权/游标/真实回执后原子提交动作step、final/applied、关闭attempt/释放槽）。增加StepKind枚举编号9=`apply_ticket_resolution`和checkpoint动作绑定字段；Proto只追加、从源生成。签名/writer key/endpoint不传Python；现有runexecutor.Environment仍仅白名单reader/provider字段，Worker动作client使用独立配置。

动作Claim在持久S4动作游标下可取得**仅查回执**的执行权，使用仍登记且版本/hash匹配的Worker/原持久profile，但不以profile当前Executable或模型账户冻结/余额/有效期为前置；原Run deadline、cancel、session、lease、容量仍强制。控制RPC只有当前动作步骤可进入这一分支，不能ReserveCall(chat)、BeginTool或重新规划。查到回执的Complete不经过模型预算guard；新授权/每次物理写仍检查可执行S4 profile、三层账户未冻结/有效、原checkBatchAuditGuard（含其它Run的待确认chat）、新鲜原permission、原Run和写次数。未知chat/被冻结账户不能借动作阶段请求新的模型或首次写入。

Go动作HTTP使用独立`action_calls`子表，保存operation、lease、step、physical UUID、许可/期限、transport/outcome及内容hash，provider计量明确not_applicable。原physical_calls、/calls、Usage.PhysicalHTTP、三层模型44次上限与全部known/unknown/hold分类保持。authorization子行持久累计原Run最多4次write（初次+最多3次恢复）、4次receipt query；同attempt最多各一次，只有首次许可受理才计数且不退回。GET action-calls和SDK单独暴露最多8条自动调用，不制造provider usage或费用结算；公共reconcile/retry查询使用独立有界许可及effect来源审计，不占原8次额度。

每次可能发送的query/write都先保存新physical_call_id/原operation/content hash/完整当前execution binding、dispatch expiry和≤10s deadline；相同物理ID只返回newly_reserved=false，不能再发送，同ID异内容CALL_CONFLICT。授权ACK/许可ACK不确定不本地猜成功，不重用发送许可，停止续租交自然恢复。未知写调用不退款、不更换operation；授权身份允许业务去重，物理许可本身不允许重放。一次动作attempt最多query一次、write一次，无HTTP客户端隐藏重试。

原授权存在则先复用本地已持久validated applied view；未确认才query。Found=true核验全部身份/hash并完成；404只表示该时点无回执，原非终态Run可在原授权有效且新写门禁通过时，以同操作/签名/参数新物理ID重发。请求超时/5xx可通过既有Fail/自然lease按3次恢复收敛；写入响应丢失后保留未知调用，下一attempt仍先query。许可过期且无回执失败ACTION_AUTHORIZATION_EXPIRED，不续签；步骤提交ACK不确定复用原GetAcceptedCommit有界确认。动作结果首次提交前检查当前lease/owner/token/state/cursor；陈旧原Worker只能保留原调用事实，不能Complete新状态。

批准续跑只增加attempt_no/fence，不增加recovery_count；新动作attempt的真实丢失才按既有CloseAttempt增加恢复次数。S4 schema4的模型恢复继续逐条验证原call自身profile/审计/关闭证明，不豁免同batch历史S1–S3不完整调用。

## 6. 终态效果核对、retry与保留

`action_receipt_views`按tenant+business_request/operation保存独立effect version、首次已应用receipt及查询来源/时间。authorization一旦存在、尚未确认效果即unknown；无授权为none，确认后为applied，不能被随后404/超时降回unknown。回执由受信固定业务endpoint/独立内部身份取得，校验operation/authorization/parameters/proposal/tenant/request/ticket/hash；用户或模型不能上传回执代替查询。

公开 `POST /v2/runs/{id}/reconcile` 只用于终态且有原operation的读取；operator权限，reader/approver仅GET effect。无授权或已有applied可直接返回本地view；需查询时共享≤10s请求deadline、一次业务GET、无隐式重试。租户有持久query gate：`query_id/next_allowed_at/query_until`，按锁后DB时间安装最多1次/s、最多一个≤10s在途许可；当前query完成只按自身ID释放，死亡通过原query_until自然到期，不续期。多副本/新ID也受此gate，429 RATE_LIMITED；它与动作writer权限分离。核对只保存effect子行，既有Run state/outcome/error/result/cursor/事件序列不更新、不重开Run、不发POST。安装许可的短事务只按`source Run KEY SHARE → authorization KEY SHARE → gate → query audit INSERT`锁身份，提前取得两个FK锁；不改变执行事实。保存结果按`query audit → authorization KEY SHARE → effect → gate CAS`，不回头锁Run/账户。网络在事务外；晚到旧查询与新许可不能反转授权/gate锁序。首次查询outcome及首次回执固定，相同重报幂等，异内容ACTION_CONFLICT并保留首次事实。

新retry请求先ResolveAdmission复用已接受后继，再检查tenant/source资格及原7日窗口，读取原业务请求唯一authorization；无授权才走原AdmissionContext/profile/budget/capture。有授权先复用本地validated applied view，否则在控制事务外最多一次GET，受相同租户限频，不消耗模型或原Run4次自动查询额度。Found=true创建新succeeded/applied Run和首次receipt引用，沿用原快照/profile/账户但无需其当前可执行；404创建failed/ACTION_OUTCOME_UNKNOWN后继并继承原授权；timeout/依赖错误返回DEPENDENCY_UNAVAILABLE，不凭错误创建“无回执”结论。任何分支均不重发/续签/改方案。

最终接纳事务仍`business_request → source Run → operation/动作子记录`，重查后继/原授权/源终态/新鲜7日窗口；同源唯一后继、防止并发分叉。源已failed/cancelled不能再新增授权，外部在途提交仍可能在查询后发生，故unknown后继可之后只读reconcile，不回退其终态。原授权前正常retry可以新快照/新模型，但共享family/batch，首次授权后身份锁定。

Run/checkpoint7日、业务operation/receipt至少30日仍按原合同；receipt retain_until至少applied_at+30日并覆盖原请求有效期，不实现删除器。新retry在原创建+7日等号拒绝，已接受retry重放仍返回原对象；首次写入在authorization expiry等号拒绝，即使去重记录缺失也不能复活旧签名。30日保留不设为GET截止。可注入DB时间的专用机制测试与真实PG验证这些边界，不修改生产计时、不拿直接改库时间代替自然故障证据。

## 7. API、迁移与兼容性

公开源OpenAPI/fixture/SDK增加：

| 路径/字段 | 合同 |
|---|---|
| GET/POST `/{id}/approval` | 受保护pending/决定/hash/actor/期限；POST固定请求与幂等返回首次决定及当前Run，200；只能approver |
| GET `/{id}/effect` | none/unknown/applied、operation/hash、effect_version、回执引用及来源；无写权限/签名/凭据 |
| POST `/{id}/reconcile` | schema_version=1，operator；一次只读核对返回effect，不变Run |
| GET `/{id}/result` | 保持available/kind/ref，新增disposition区分none/proposal/approved/applied/rejected/unknown/no_action；none仅表示无已接受结果，no_action仅表示明确提交无需动作；终态首次disposition持久固定，effect单独变更 |
| GET `/{id}/action-calls` | 原Run最多8条自动动作HTTP许可/传输/operation记录；provider计量not_applicable；原/calls保持44条模型工具账本 |

错误envelope不变。公开POST新冲突为409 APPROVAL_CONFLICT/ACTION_CONFLICT，rate为429 RATE_LIMITED；期限新请求使用409 APPROVAL_EXPIRED；旧操作按鉴权重放。执行失败存Run/error：ACTION_CONFLICT、ACTION_AUTHORIZATION_EXPIRED、ACTION_OUTCOME_UNKNOWN及既有预算/profile/dependency错误；GET failed Run仍200。gRPC reason同步稳定映射，ACTION_CONFLICT/期限/unknown为FAILED_PRECONDITION，缺依赖UNAVAILABLE，不按文本分类。业务首次验签失败403 FORBIDDEN、内部租户不存在404、内容/版本409 ACTION_CONFLICT、过期409 ACTION_AUTHORIZATION_EXPIRED；同内容已应用200并返回首次receipt。

仅新增控制`0027_approval_actions`及业务`0003_ticket_resolution`up/down。控制扩展run_approvals/run_operations允许approval；新增action_authorizations/action_calls/action_receipt_views/tenant query gate及独立动作计数约束，原physical_calls不改。所有Run子行tenant复合FK、唯一tenant+business_request/operation；authorization经tenant+授权Run+business_request复合FK绑定原请求（Runs增加相应唯一约束），不另向business_requests创建会在正常Run锁内获取父行锁的FK，避免反转接纳锁序。正常动作锁序为`Run → family/tenant/batch账户（仅需要时）→ authorization/call/effect子行 → attempt/slots`；公开查询短事务使用第6节的身份/gate顺序，不改变Run/账户。签名主体/参数/hash/首次回执不可改写，不CASCADE删除预算/身份。历史审批保持pending/新字段null、历史call不回填/改义；新字段pair/range/content大小与policy约束以PG测试验证。

业务创建resolution/receipt表、专用角色、statement guard与政策revision trigger。保留旧0001/0002及已发布index/snapshot正文，schema ready检查升级为3。DDL设置短lock timeout，新增表无历史payload回填；约束变更验证小锁窗口和新旧数据。down前停新许可、清理全部进程、禁用S4 profile并备份动作/回执审计；存在S4授权/回执时拒绝破坏性down，优先前滚修复。不以代码回滚删除业务去重记录。迁移up/down/0026与业务0002升级、权限拒绝、查询计划和锁序均实际验收。

## 8. 风险、验证与退出

跨控制/业务库不采用2PC；业务已提交而控制未知是预期窗口，稳定操作身份和回执先行修复。取消不撤销已派发动作，终态effect单列。使用单业务guard牺牲源写并发，换取缺失对象与政策变化的可核验保护；网络始终在锁外。独立动作账本回归验证原Calls/限额/审计guard完全不变，不能借动作路径发送chat或减旧hold。签名及current actor校验共同防止模型输入被当作批准；受信Worker/业务服务仍是部署信任边界。

不采用Python写工具、重试换operation、取消后重发、给旧profile默认写policy、只锁现存行或查版本后HTTP写入。免费机制矩阵见PRD S4-01～10/12，实施测试集中在原run/business integration和正式agentruntimecheck，故障hook只在测试/外部验收镜像。至少一项真正业务提交后丢HTTP响应并杀正式Go Worker，使用原30s lease/5s Heartbeat/180s attempt/1-2-4s退避/三次恢复和`--init`，不直接改状态/lease、预录回执或缩短时间证明恢复。

最终8项CI按[测试指南](../tests.md)，同DSN清库测试串行、真实pgvector业务库、SDK实际安装、Buf生成/兼容检查、全仓race、Linux进程/联合层及生产registry边界全部验证；skip单列。S4单独保存审批前原始方案的协议/来源/冻结业务谓词评分与审批/效果安全；旧S1–S3任何写入即安全失败的评分规则保持，不伪装schema/回改Run状态。模型业务失败或未批准导致机制未触达均保留，不挑样到通过。真实层批次范围与执行审查记录保存于独立manifest/验收报告。
