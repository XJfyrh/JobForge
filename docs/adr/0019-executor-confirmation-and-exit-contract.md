# ADR-0019：执行器观察确认与固定退出合同

- 状态：Accepted；随[PR #44](https://github.com/XJfyrh/JobForge/pull/44)于2026-09-16合并接受（`d11dd2f`）。契约接受不代表新增执行能力或验收通过。
- 日期：2026-09-16。
- 关联：[PRD v0.11](../product/JobForge_PRD_v0.11.md)、[ADR-0018](0018-deepseek-fixed-flow-and-executor.md)。
- 取代范围：取代ADR-0018中普通observation后的继续执行时序；补充关闭后的失败传递和进程收尾。模型、价格、账本、固定业务策略及其它既有决策不变，旧ADR正文保留。

## 1. 上下文

C1已提供内部v2的严格codec、独立计量和BOOTTIME；C2的`DispatchHooks.observe`要求控制面确认才返回。实际代码没有observation ACK。若Python等待observe返回才提出下一个intent，而Go以“下一许可隐式确认”实现，就会循环等待；若直接把write当确认，则最后一步可能越过持久化屏障。

另一个接缝是`DispatchError`的size_limit/stop会关闭普通Conversation，现有error/OUTPUT_INVALID不能无损表达大小失败。正式IPC不得因此重开会话或误触发纠正。计量快照也不等于Go已接收，必须明确FD、进程、网络收尾各自的所有者。

## 2. 明确观察ACK

在内部v2普通通道新增`call_observation_ack`，必须恰含公共字段（version、kind、request_id、完整binding、emitted_mono_ms）与`call_sequence`、`physical_call_id`、`observation_hash`。不包含结果、许可、期限增量或可执行输入。只表示一份观察已按原身份确认；不设可继续执行的负ACK。

`observation_hash`与现有控制账本`ObserveCallRequest.Hash()`一致：长度前缀SHA256 Fingerprint，域`jobforge.run.observation.v1`，依次为transport_outcome、十进制http_status、**映射后的领域error_code**、business_outcome、usage_hash；unknown处置的最后项为空字符串。完整binding、request/call/sequence另外严格匹配原帧，不能只比较payload hash。

生成RPC请求和ACK hash前先使用同一固定错误映射：OUTPUT_INVALID→MODEL_PROTOCOL_ERROR、INPUT_INVALID→INVALID_ARGUMENT、PROTOCOL_ERROR→EXECUTOR_PROTOCOL_ERROR；空串、TIMEOUT、DEPENDENCY_UNAVAILABLE、PROFILE_UNAVAILABLE、BUDGET_EXHAUSTED保持不变。STOP_REQUESTED、STALE_LEASE、CALL_CONFLICT只用于控制拒绝/停止，不转成业务observation或猜测provider结果。两端共同fixture校验原wire错误与映射后DB/hash字节，禁止Python按原OUTPUT_INVALID而Go按MODEL_PROTOCOL_ERROR分别计算。纠正仍检查原wire OUTPUT_INVALID及严格marker，不因领域映射而放宽。

Go仅在对应ObserveCall RPC明确成功、返回原调用/参数/工具身份一致且当前执行权仍有效后发送一次ACK。RPC确认可按ADR-0018在共享2秒内至多两次，重发的是同一控制事实，不是HTTP或许可。无法确认、身份不符或截止已过则停止，不发送ACK。对reported observation，Go先等待同调用/同hash的标准计量及settled确认，再以原usage调用ObserveCall；unknown传false/nil。SettleUsage不能代替ObserveCall，也不能将reported悄悄转成unknown。

两端Conversation增加观察确认等待：收到observation之后，必须同时满足普通ACK和（若reported）metering settled，才可恢复idle/合法error-result状态。对于reported，Go只有在SettleUsage已确认且ObserveCall已确认后才能发送普通ACK，其emitted_mono_ms不得早于对应settled ACK；免费/unknown只要求ObserveCall确认，不伪造计量或等待不存在的settlement。独立FD不保证接收顺序：原report/observation和两类ACK都允许接收反序。若reported的普通ACK先被Python读取，先验证完整身份/hash/原期限，至多保存当前单调用的一份有界pending ACK，待同调用settled ACK标准校验后汇合；等待期间不恢复idle、不发intent/result，不延长deadline。错hash、anomaly、unconfirmed、过期或停止清除普通继续权，迟到汇合不能复活。不能把reader调度顺序误当Go的RPC先后事实。

`DispatchHooks.observe`改为返回ACK帧，由持有原Conversation的dispatcher验证；不能创建另一权限机或只依赖hook自报成功。C2当前先等待settle返回、再发observation的串行实现可以保留，但协议接收端和共同fixture必须覆盖双FD反序，不依赖该具体发送优化证明一般接收正确性。

免费调用、unknown usage、rejected和最后一次HTTP同样等待ACK。后续intent/result的emitted_mono_ms不得早于ACK，接收时仍检查原call/step截止；ACK不延长任何时间。错binding、hash、call、sequence、方向时钟、重复或迟到ACK关闭普通执行；重复控制RPC的幂等性不等于重复IPC帧可再次使用。已停止的会话不因迟到ACK或settled恢复。

## 3. 固定退出事实

固定step和guardian使用以下退出合同。guardian仅转述其直接固定child实际Wait所得的正常退出码；信号、未知码或guardian自身未验证的内部失败统一按监管故障处理，不能伪造provider结果。Go不解析stderr正文。

| 退出码 | 固定含义 | Go可采用的错误类别 |
|---:|---|---|
| 0 | 正常退出，必须另有恰一个合法普通step_result | 不单凭退出码成功；只有完整屏障通过才考虑Commit |
| 64 | 登记输入不合法 | INVALID_ARGUMENT |
| 65 | 协议/响应结构不合法 | EXECUTOR_PROTOCOL_ERROR |
| 66 | profile/模型/资源身份不兼容 | PROFILE_UNAVAILABLE |
| 67 | 输入、响应、内容或结果大小越界 | CHECKPOINT_TOO_LARGE |
| 68 | 已知依赖不可用 | DEPENDENCY_UNAVAILABLE |
| 69 | 物理请求或控制等待超时 | TIMEOUT |
| 70 | 不可纠正/第二次无效模型方案 | MODEL_PROTOCOL_ERROR |
| 71 | 本地停止，不代表已确认用户取消 | 只停止；按Go已有STOP/失权事实收敛，不凭此制造取消或失败 |

固定进程代码是受信任部署的一部分，退出码不是任意用户程序可选择的接口。非0退出不提交普通结果、不能授予新调用、退款或更新游标。Go已锁存STOP/失权时只进行允许的停止确认和窄计量，不再Fail/Commit。其余仍有效的执行权下，Go按既有领域错误策略调用FailAttempt，不自行决定恢复次数。

Go在发送拒绝许可或关闭控制管道之前，先锁存已确认的原领域拒绝原因。特别是ReserveCall返回BUDGET_EXHAUSTED并非用户STOP/失权：不授予发送权，清理Kill/Wait后若执行权仍有效则FailAttempt(BUDGET_EXHAUSTED)；不等Python重新上报预算原因，也不被随后退出71、缺结果EOF或通用65覆盖。相同原则适用于Go已经确认的profile/参数等永久拒绝；停止期间后来收到真实STOP/失权仍立即禁止Fail/Commit。独立坏帧等诊断可另记固定类别，不能制造继续权或改成可恢复的依赖失败。

若仅收到71而Go没有已知STOP/失权/领域拒绝，则只认定本地执行已放弃。清理后停止该Run租约的Heartbeat，不保持无执行进程的无限续租；不伪造取消或Fail原因，不本地重发步骤，交既有lease/Sweep回收。会话级存活心跳不授予此Run执行权，未来重新领取必须经服务端新Claim/新attempt/fence。

Go先收到普通EOF时，记录“结果待定”，同时继续原deadline/watchdog；在固定child/guardian实际Wait完成前不抢先用“缺结果”覆盖已定义退出事实。坏帧、尾随帧、stderr越界或残留进程本身仍是监管失败，不能被退出0或某个已知码消除。任何本地大小事实采用CHECKPOINT_TOO_LARGE，权限停止仍优先禁止提交；其它协议违规保留EXECUTOR_PROTOCOL_ERROR。同时故障保留首个已观测原因及停止标志，不能把未知原因说成已确认供应商失败。

第一次model_proposal完整输出被拒绝时，仍沿用ADR-0018的唯一可纠正路径：观察ACK已确认、usage若reported已settled、原期限/执行权有效、严格correction_required=true/proposal=null，发送error/OUTPUT_INVALID step_result后正常退出0。Go独立校验窄标记；大小、TIMEOUT、停止、错身份、坏帧、缺ACK不能进入此路径。protocol_correction再次失败为70。错误闭集及公开domain错误码不因此扩展。

## 4. 结果、计量与进程收尾

单步finalizer复用dispatcher原Conversation，最多产生一个普通结果；最后已确认observation身份由hooks/协调者保存不可变副本，覆盖免费和unknown调用，不能从paid usage列表推断。StepResult绑定原调用、工具及资源，不允许Python决定next cursor。read_ticket/submit_proposal不发HTTP；其余具体schema和策略仍遵循注册合同。

Python发最终结果后退出，不能等待Go Commit ACK：Go必须先确认正常退出、普通EOF且无残片/尾随、stderr总字节≤8KiB、独立计量reader有限排空/Join、旧进程组无活进程，再在仍有效的执行权下Commit。Go继续按ADR-0018处理Commit不确定：只读GetAcceptedCommit至多两次、共享2秒，Found=false不是原事务未提交证明，不立即Fail或重发业务。

普通和计量使用独立、显式继承的双向管道，每条通道恰一个reader和writer所有者，有界buffer/event队列和取消/Join路径。guardian不得继承Go控制写端的副本；step只继承列明FD，不能持有父EOF探测所依赖的写端。guardian的父EOF/信号处理不受业务协程、下游满管道或模型HTTP阻塞。

收到TERM后，step只停止HTTP/授权并尽力发送已经完整捕获的原报告，不等待供应商新数据。100ms宽限后Go独立Kill整个组并Wait；网络结算不得延迟Kill。Go SIGKILL由guardian的控制EOF清理；guardian死亡由Go清理组；固定容器init回收孤儿。启动下一组前必须确认旧组消失。不能声称支持逃逸组的任意用户程序。

Kill后的进程组确认、管道EOF/reader Join共享2秒本地清理期限，与计量RPC期限独立。若仍不能确认清理完成，Worker停止全部Claim及Run续租、不得Commit或启动下一组，报告固定的监管清理失败并退出；由容器init/部署重启处理，实际再次验收前确认无残留。不能把超时返回写成Kill/Wait成功。严重宿主/内核阻塞下不承诺SIGKILL必在期限内完成；这种环境故障是验收阻塞，保留实际证据。

停止后仍由标准DecodeMetering接收有界原调用报告，不能从坏stdout捞usage。Kill/Wait后对管道内已完整帧有限排空，reader到EOF并Join；只对Go已完整收到、绑定其曾授权的原call/参数的报告使用独立总计≤2秒SettleUsage收尾。每调用至多一份不同报告，重复只幂等确认。半帧、未接收或收尾未确认仍可能unknown全hold；不增加journal，不为计量延长进程生命或授权新动作。

可信usage超界立即锁存Worker停止Claim/许可/纠正并开始本地终止；收到measurement_anomaly才声明数据库三层已冻结。未确认停止真实验收，不假定已退款、已冻结或事务未提交。计量没有改Run终态、恢复普通会话或清新attempt active_call的权限。

## 5. 版本、部署与替代方案

协调更新尚未正式部署的`api/executor/v2`源schema、Go/Python codec、共同fixture和C2 hooks，不新增v3或双模式。当前没有正式v2在途任务；这是明确的内部序列不兼容修订，不能对已部署公共消费者套用同一理由。新实现使用新的不可变executor_version及profile hash，只有对应固定Go/Python镜像组合可注册/Claim；旧profile ID不得原地换内容。v1历史合同和公开HTTP/SDK/gRPC保持不变，PostgreSQL仍唯一事实源。

拒绝以下替代：下一permit隐式确认会与C2等待顺序形成循环；计量ACK无法覆盖免费/unknown观察且不清active_call；pipe flush只能证明传输。另一层request/reply包装重复binding/期限合同，没有减少复杂度。停止后新增terminal_failure帧也可表达事实，但固定退出码已经能在当前受信进程范围内表达闭集失败，避免新增具有关闭后权限的第三通道。stderr永远不是控制协议。

正式镜像安装固定Python包或固定绝对bootstrap，不能依赖用户PYTHONPATH或动态模块。Go独占控制token，Python仅继承本profile/tenant必要的模型/业务凭据；部署白名单传入，秘密不放argv、frame、profile、log或trace。模型/输入不能改变工具、URL、命令或租户。

## 6. 后果与验证

增加一个明确的普通ACK，代价是每HTTP一轮本地确认和同期限内控制RPC；这保证最后一次调用也有可验证持久屏障。协议模块测试仍不能证明PG已提交，需真实PG/gRPC/进程故障验证[PRD映射](../product/JobForge_PRD_v0.11.md)。新增共同fixtures明确拒绝旧无ACK序列，覆盖双FD顺序、迟到/错hash/重复、停止后计量及固定退出优先级；运行适用race/Buf/Python/SQL和固定Linux镜像。

provider identity/reasoning目前只有C2内存审计，持久schema/传输/存储须在真实C-01前另行交付，不能从receipt hash还原并声称已保存。Claim当前trace_context来源未贯通，完整跨队列trace另行定义；现有HTTP透传不代替。support业务图/schema/模板、真实模型批次和评分仍属后续实现。历史W4失败、AT-25跳过、远程模型和生产留存未验收原样保留。
