# ADR-0025：完整审计下的未提交步骤恢复

- 状态：**Accepted（2026-10-04 独立方案审查通过，随本决策 PR 合并生效）**。
- 日期：2026-10-04（Asia/Shanghai）。
- 决策者：维护者；实施会话编写草案，原规划会话负责方案审查与结果验收。
- 关联：[PRD v0.17](../product/JobForge_PRD_v0.17.md)、[S3实施计划](../archive/plans/agent-v3-s3-recovery.md)、[ADR-0013](0013-durable-agent-run-and-step-commit.md)、[0017](0017-run-admission-and-call-ledger.md)、[0019](0019-executor-confirmation-and-exit-contract.md)、[0020](0020-provider-audit-and-batch-stop.md)、[0024](0024-bounded-support-agent.md)。
- 拟部分取代范围：**只对新 S3 profile**，增补 ADR-0020 §8.1 的 chat 结束屏障，取代该节“完整审计但原 step 未提交、原 lease 失效时不能自动重新收费恢复”的限定；增补 ADR-0019 §3/§4 的纯进程丢失分类及 ADR-0020 §8.2/§8.3 的相应本地停发处理。S1/S2 profile、未知/异常停发、ACK/Commit/清理屏障和历史 ADR 正文保持。
- 起点：`e2b9bad`；ADR编号至0024，migration至0025。本文不更改生产代码、迁移或已接受合同，不发起收费请求；执行预算由主规划会话按自主S3实施范围选定，见实施计划。

## 1. 问题与选择

现有 `ClaimExecution/CloseAttempt/Sweep` 已分离 attempt 与三次恢复预算；CommitStep 原子保存步骤、游标与事件，正式 Worker 在 Claim 后读取 checkpoint。S3 应补齐这些已有机制的动态 Agent 闭环。

目前 `provider_audit_guard.go` 要求每条旧 chat 有完整 report/known usage/ordinary observation，且原 attempt 的步骤 Commit 或极窄业务终态失败。完整响应和观察已持久、步骤尚未提交时 Worker 丢失，扫描器虽把 Run 回到 ready，新 Claim 仍被 guard 拒绝。仅改 guard 仍不够：`coordinator.finish/receiptFailure` 将 guardian 信号/退出72归为 EXECUTOR_PROTOCOL_ERROR，并对未完整 Commit 的 chat 返回 ErrBatchStopped。

选择版本化、最小的恢复增量：在旧 attempt 上持久保存**恢复时的步骤身份及已安排的恢复序号**；按每条原 call 的持久 profile 验证原审计、关闭事实和恢复绑定。原调用保持未提交及原费用；恢复 Worker 重新执行原待执行步骤，申请新调用身份。纯进程丢失或 Commit 确认不确定只关闭本地执行，是否重新 Claim 由 PG 决定。

## 2. profile 与版本

新增 support definition `schema_version=3`，沿用 S2 模型、提示、动态决定 schema、工具、资源和大小上限，增加必填闭集字段 `program.recovery_policy="confirmed_uncommitted_v1"`。旧 schema 1/2 不接受此字段，缺省政策继续原保守语义；未知/空值/null拒绝。完整 typed definition 继续参与 profile hash，不新增可由请求切换的运行时开关。

新固定 executor_version 为 `linux-v2-recovery-runtime-1`，strategy/adapter 仍为 `support_agent_v1` / `support-agent-v1`。manifest、Register、全部可执行 profile、Go/Python安装包及构建receipt匹配同一版本。内部 v2 帧形状、ProviderAudit、report/observation hash与现有 RPC 保持；恢复 policy 只在 Go 控制/协调路径裁决，不向 Python授予执行权。生产 registry 仍只有两种既有 support adapter。

每条 call 的恢复政策从其 tenant/Run/profile_id/profile_hash、call.profile_hash 及不可变 `agent_profiles.definition` 校验取得。不能只看正在申请的新 Claim/profile，不能将 S3 政策追溯套给同 batch 的 S1/S2 调用。旧 profile ID 不换定义/hash，旧结果及原 session 窄计量仍可查询/补报；新 runtime 不静默执行旧 profile。

模型及价格快照采用实际执行当天的核验日期与原始来源摘要。沿用相同别名不意味着模型不可变；当天无法核对身份/费率或资源不符时保持 profile disabled，不切换模型、提示、索引或价格恢复。

## 3. 最小持久证明

拟新增 migration `0026`，只在 `run_attempts` 添加两个 nullable 字段：

| 字段 | 冻结内容 |
|---|---|
| `recovery_step` | 严格有界 JSON 对象，保存关闭前完整 StepIdentity：step_id、sequence、kind、cursor_version、input_hash、profile_id、profile_hash、snapshot_id、snapshot_hash；不含结果、payload、凭据或发送许可 |
| `recovery_ordinal` | 本次关闭事务实际安排的 recovery_count，整数1..3 |

两者同时 null 或同时非 null；非 null 要求 finished_at 非 null、outcome为 `failed_retry` 或 `lease_expired_retry`、error_code为 `LEASE_EXPIRED`、`TIMEOUT`、`DEPENDENCY_UNAVAILABLE`、`ATTEMPT_DEADLINE_EXCEEDED` 之一、sequence=cursor_version+1且范围符合32步上限。最后一项是现有 failureCode 对 attempt_timeout 的实际持久映射，取消和 Run deadline 不属于此集合。Go用闭集 typed 解码，数据库约束校验成对存在/类型/范围/结束状态。历史行保持 null，不从当前游标、事件、余额或模型正文回填。

只由已有 Fail/AcknowledgeStopped/Sweep 关闭事务在新 S3 profile 下写入：先保存旧资源身份和待执行 StepIdentity，取得新鲜DB时间，按现有 CloseAttempt 处理取消/Run期限/attempt期限/临时故障和三次上限；**只有实际安排 retry 的分支**保存该对字段。同事务关闭旧 attempt、清 active_call/owner/lease、保留 unknown hold、释放slot、更新 Run/recovery_count/退避并追加原 attempt_closed 事件。重复关闭拒绝，不再增加序号或覆盖证明。

这两列记录“该旧 attempt 在此步骤安排过一次恢复”，不声称旧 chat 已审计完整或可立即发送。原 audit/usage/observation 在 physical_calls；finished_at/outcome/原worker/session/fence在 run_attempts；recovery_step/ordinal是持久的恢复安排及步骤绑定。三种事实独立验证，不新增 pending 表、恢复队列或人工 unlock 接口。

完整 StepIdentity 和 call 的原 Lease 重算 `ExecutionBindingHash`，必须等于预留时保存的 execution_binding_hash，且步骤id/kind、profile/snapshot与原call/不可变Run一致。因此后续 Claim 改 owner/token、后续 Commit 改 next_step，均不会丢失旧 call 的原绑定。无需在 physical_calls 再复制整份步骤，也不把 `physical_calls.input_hash` 当步骤 input_hash：现列实际保存物理参数hash。

## 4. 持久 chat guard

原已提交步骤及原窄业务失败分支保留。新分支必须逐条满足以下三组事实，缺任一项拒绝新的 Claim/BeginTool/Reserve；原 report/Observe/Commit/Heartbeat/checkpoint/关闭和窄计量确认仍不递归进入 guard。

### 4.1 原调用审计完整

- 原 profile 明确启用上述 S3 policy，profile/price/原binding合法且一致。
- 原 chat 的首份 report 持久、response_complete=true、HTTP200、兼容身份、nonthinking、完整合法 usage；report/audit/receipt/usage/observation各hash按现有算法可重算。status为known、settled usage与report usage一致、held token/费用为0，没有report冲突或measurement anomaly。
- ordinary observation 已持久，transport=`response`、business=`accepted`、domain error为空，与该 report/audit/usage/hash匹配。新豁免不覆盖 rejected纠正标记或其它业务失败；它们仍走原 Commit/终态分支。
- 三层账户未冻结、无batch_stop_code；申请新许可时仍检查各层期限与剩余额度。不能以 known 标志、单一hash或“进程退出0”替代完整事实。

### 4.2 原 attempt 已关闭并绑定恢复

- call 的 tenant/Run/attempt/worker/session/fence逐项匹配原 attempt；finished_at/outcome/error_code及recovery_step/ordinal完整，属于 §3 的实际 retry 分支。
- 重算原 binding 匹配；关闭时间不早于 reserved_at、原 report_recorded_at 和 observed_at。关闭时没有接受取消或 Run deadline，且未耗尽恢复预算；这些由同 Run 锁下的合法关闭分支证明。
- 若原 attempt仍运行、只丢ACK、关闭为 cancelled/failed_terminal，或恢复证明缺失/错配，则此分支不成立。晚到计量不能补造普通观察或恢复证明。

### 4.3 放行范围与后续稳定性

未提交的豁免仅用于**同 Run、相同完整待执行 StepIdentity**的恢复 Claim/后续BeginTool/Reserve。目标 Run 行已被申请事务锁住：必须仍可恢复/执行、无已接受取消、Run/attempt期限有效、当前cursor/待执行步骤与原recovery_step相同；新 attempt/fence由正式Claim安装。旧attempt不重新发送，不复用旧call/工具许可。

若已有多次丢失，逐条旧 chat 均须满足同样事实。序号规则是：`1 <= 原attempt.recovery_ordinal <= Run.recovery_count <= 3`；新 S3 Run 的所有 retry关闭证明按attempt_no排序必须恰好为1..recovery_count，无重复/缺项，非retry关闭不产生序号。申请恢复Claim时，当前最后一个attempt须已retry关闭，其序号恰等于Run.recovery_count，目标待执行步骤与其证明一致；Claim原子安装attempt_no+1/fence+1。新attempt内尚未提交该步时，最近的前一retry关闭证明仍须满足相同累计值/步骤，且当前attempt/fence严格晚于原call。attempt_no本身不作为恢复预算，正常继续不消耗序号。新请求的次数/费用仍由原账户裁决；三次恢复是上限，不保证有足够次数/金额重做（例如纠错额度已用尽）。

原步骤在新 attempt 成功提交后，guard使用同 Run/step_id 的不可变 run_steps 作为**恢复完成**事实：完整StepIdentity与原recovery_step一致，sequence/输入/资源/commit hash合法，提交attempt晚于原call，结果引用新physical_call_id，且新call的原绑定及审计/普通观察通过现有提交校验。旧call保持原“完整观察但未提交”事实，不能被列为新step的调用。之后新游标/下一案例仍能用原attempt证明+新提交核验，不再从 runs当前next_step推导旧身份。

没有新步骤提交时，其它Run/后续步骤不能借豁免先行。取消、Run到期或恢复耗尽而仍留下未解决的旧chat时，保留阻断及费用；不把某个终态/新batch当作完成证明。比较实验中的从头控制只选择旧chat已提交的故障边界，避免为清除这个阻断修改生产规则。

## 5. 算法、事务与锁序

保持已有固定顺序：`Run → family → tenant → batch账户 → 原call/工具/attempt子行 → worker → tenant → profile slots`。只获取本操作需要的锁；不需要账户的旧关闭路径可以跳过该层，但不得在子行/slot之后再回头锁账户。新 S3 关闭路径在锁子行/slot前取得三层账户，保持与 Claim/Reserve/审计提交相同顺序；它不因冻结拒绝清理，也不返还额度。

Claim：验证session候选→SKIP LOCKED锁目标Run→锁账户与slot→取得新鲜DB时间→重验session/profile/期限/容量及batch guard（带目标Run/待执行步骤）→原子创建attempt、递增attempt/fence、占slot、保存owner/lease/state/事件并返回checkpoint。旧session的活跃登记保护仍为60秒；测试用预登记另一principal接管，或等原session自然到期，不能强制改expires_at。

关闭：锁Run→读取原持久profile→按上述顺序锁必要资源→新鲜DB时间→保存关闭前步骤/原身份→CloseAttempt取消优先→原子保存恢复证明、关闭attempt/active_call、释放slot、Run/事件。Sweep仍每轮最多100候选、逐Run短事务，retry_wait到期由原扫描器推进；不在数据库事务内等待进程或供应商。

新Reserve/BeginTool：锁目标Run→三层账户→当前步骤/重复身份检查→guard检查全部先前chat→原子新增许可/扣额。Reserve的guard在新call插入前运行；同ID只返回newly_reserved=false，不能再次消费许可。恢复后的同step新call与旧call同时保留。

guard在持有batch账户锁时只读其它Run/原attempt/不可变step；**不能再锁其它Run**，否则会与原call的 Run→账户确认反向等待。原S3关闭/报告/新许可竞争由同Run/账户锁串行；其它Run关闭尚未提交时读不到完整证明，保守拒绝本次许可。冻结/冲突事务与新Reserve在同batch锁排他，冻结先提交则新许可为0；许可先提交的在途请求仍可能执行，保留预留与事实。

取消及期限在目标Run锁后用新鲜DB时钟判断，等号到期；STOP不能因迟到ACK/已知usage/证明记录复活。Commit仍在最终写入前重读DB时间，原子提交step/游标/事件。迟到旧owner提交先判执行权，永远不以重复hash返回写成功。

## 6. 正式进程丢失与确认不确定

只对S3 profile区分以下退出事实，不新增Python退出码、FD、provider报告或公开错误码：

| 事实 | S3处理 |
|---|---|
| guardian实际Wait为信号退出，或固定guardian退出72；两通道完整EOF/Join、stderr有界、组消失，无坏帧/残片/尾随、身份/大小/监管错误事实 | 只认定固定执行器丢失；不猜provider结果、不Commit、不Fail(EXECUTOR_PROTOCOL_ERROR)，关闭本地会话并停止该Run续租，交自然lease/Sweep |
| 合法结果/清理完整，但Commit RPC或ACK不确定 | 原身份/hash有界只读GetAcceptedCommit（至多两次、共享2s）；Found=false不证明回滚；不重发业务、不本地算下一步、不Fail，停止Run续租，由已提交事实或自然回收收敛 |
| 已确认yield/终态Commit且ACK丢失 | 授权只读确认完整hash/attempt outcome；已经关闭的attempt不增加recovery_count，不使用旧lease再写 |
| 已知64..70退出、坏帧/hash/绑定/尺寸/尾随、未确认清理、可信provider停止/计量异常/报告冲突 | 保留原明确失败/停批/退出处理；不能被信号或72降为可恢复错误 |
| STOP、取消、失权、各层到期或窄计量晚到 | 原停止优先级与清理/补报权限不变，不延长执行权 |

退出72只是固定guardian对非正常child/监管退出的转述；Go不从stderr解释原因。若清理或协议事实不完整，不能把72当成清理成功。新的“执行丢失”也不是收费许可：已派发chat缺报告、计量/普通观察不完整或本地报告确认不确定时，Worker停止新Claim/许可；即使扫成ready，新Worker仍须通过 §4 的持久证明，不能凭进程内confirmed标志恢复。

完整PG报告/观察可能已经提交而本地ACK丢失。此时新Worker只根据PG事实判定，旧Conversation不重开。已知异常与未清理的Worker仍停止全部Claim/Run续租并退出；新组启动前实际核验旧组消失。Go SIGKILL仍由guardian父EOF清理，guardian死亡由Go清理，固定容器init回收孤儿；100ms TERM宽限、独立Kill/Wait及共享2s清理/有限计量收尾均不改变。

## 7. API、兼容性与成本

不新增调度实体、状态、公开错误码或写接口，不修改现有HTTP/gRPC/Proto字段与执行器v2帧。新profile schema和attempt内部持久元数据必须有源合同/共同向量/SDK查询回归；若实现发现确需公开字段，应先补本决策，不能临时改义。取证用既有SDK的Run/attempt/Steps/Calls，再由受信只读SQL核验未公开的原binding和恢复证明；SDK不裁决恢复。

0026为新增nullable列，无历史回填或新恢复表；小型DDL仍需短锁/lock timeout和真实PG up/down/0025前滚验证。S3正在执行时不能直接down丢证明：先停新许可、清理全部进程并禁用新profile，保留审计/备份，再回滚代码/迁移或选择前滚修复。旧runtime不能读取新profile并静默按旧规则执行。

每次恢复仍占真实次数/费用。保留旧call已知费用，不以新step替换旧账本；unknown/full hold不释放、不变零、不靠新batch绕过。原session合法窄report重放/补报只处理原call及必要冻结，不改当前Run/游标/active_call。

成本是两列有界attempt证明、同batch下额外只读关联及新profile版本维护。复用既有≤44调用/32步骤界限，不先加通用索引或抽象；实现时用定向真实PG执行计划和锁竞争证据确认新增guard代价。没有新查找瓶颈则不扩大为旧队列性能验收。

## 8. 替代方案与退出条件

只检查known、原Run终态或child退出，会缺少普通观察与原步骤绑定；从当前next_step回推，会随Claim/Commit失效；给旧call补假Commit或拿新step假装原call已提交，会破坏结果/审计身份。新增独立恢复调度表、receipt journal或工作流框架没有本次业务需要。释放hold、thaw、自动新batch或profile升级均不采用。

真实PG覆盖完整谓词及逐项缺失/错配、原S1/S2同batch不可豁免、证明在新Claim/多次游标推进后仍有效、并发Claim/Reserve/冻结/取消、恢复与三层预算不重置。正式Linux进程覆盖三种实际死亡与四类提交窗口，至少保留正式30秒自然lease恢复；旧加速改时间测试仅证明事务政策。

完整故障矩阵、检查层次、费用上限及小样本实验预登记见实施计划。先独立审查本合同，再实现/免费验收/独立代码审查/适用CI；真实收费使用主规划会话在自主S3实施范围内选定的新增累计最多5 CNY上限，并须由其明确放行执行清单后开展，不将该金额描述为用户口授。最终保留失败、原始日志、源码/镜像/命令/hash和known/held，未执行/skip单列。S4/S5及生产长期运维不在本决策范围。
