# Agent v3：执行器协议、观察确认与时钟

[ADR-0018](adr/0018-deepseek-fixed-flow-and-executor.md)随PR #41接受；[PR #42](https://github.com/XJfyrh/JobForge/pull/42)提供严格codec、顺序校验、计量权限和Worker RPC时间接缝，见[C1证据](evidence/agent-v3-s1c1-protocol-2026-09-16.md)。C2的[受控HTTP适配](agent-v3-authorized-http.md)已合并。[ADR-0019](adr/0019-executor-confirmation-and-exit-contract.md)经PR #44接受，C3a同步内部v2的明确观察ACK和两端状态校验并随PR #45合并，见[C3a证据](evidence/agent-v3-s1c3a-ack-2026-09-16.md)。正式Worker与IPC监管循环现见[C3b运行时](agent-v3-runtime.md)，其实际验证单列；40例云端执行仍未验收。

## 版本与所有权

`api/executor/v2/schema.json`为新帧的源合同，`fixtures`是Go/Python共同用例；实现分别在`internal/runprotocol/v2`和`python/jobforge_agent/protocol_v2.py`。v1合同、fixture和行为保留，不把新字段静默加入旧严格解码。帧、许可状态机及窄计量接收器不持lease、不启动HTTP或子进程、不写控制库，也不具备独立调度权。

正式执行器将普通JSONL与计量JSONL放在不同继承FD，由各自的标准decode与8KiB/384KiB上限处理。codec和状态机只验证输入及权限，真实FD生命周期、Kill/Wait、有限排空和完整usage后有界SettleUsage由[C3b Worker](agent-v3-runtime.md)负责。不能从拒绝帧中宽松捞取usage，也不能把异常报告当成新的执行许可。

## 时间

Register、带lease的Claim、idle/active/stop Heartbeat从存储层返回`authority_observed_at`，取自相关阻塞锁后的PostgreSQL时钟。重复Register提供当前观测但不延长原session；空Claim无lease，也不续session。协议新增字段兼容旧Proto；生成代码来自`buf generate`。

`internal/runclock.FromAuthority`计算`RPC发起BOOTTIME + (expires_at - authority_observed_at)`，保守扣除整个RPC耗时；检查原类别的最大TTL、整数溢出和等号到期，向下取整毫秒。它不读取宿主墙钟。`runclock.Now`只在Linux读取CLOCK_BOOTTIME，与同容器/time namespace的Python `time.clock_gettime_ns(time.CLOCK_BOOTTIME)`共用时钟域；非Linux明确返回不支持，Windows使用Linux容器。

v2的`emitted_mono_ms`是剩余期限的发出锚，接收方扣除IPC排队时间。普通执行路径拒绝未来戳、倒退、过期和溢出；正常心跳也不能延长已签发的物理调用截止。窄计量通道可在普通会话停止后接收已经完整取得的原调用计量，不能因此重开步骤。

## 计量与普通结果

当前内部版本按 [ADR-0020](adr/0020-provider-audit-and-batch-stop.md)升级为 typed provider report：计量 ACK 关联 report_hash，普通 observation.v2 另绑定 audit_hash；完整矩阵见[源合同](../api/executor/v2/README.md)和[持久审计指南](agent-v3-provider-audit.md)。chat unknown、身份/模式不兼容或报告冲突停批。非 chat unknown 保持普通 ACK/full hold，不能把 chat 的新停止规则泛化到免费业务和未知 embedding。下述确认顺序与原期限约束仍适用。

普通observation声明`usage_disposition=unknown/reported`和可空`usage_hash`。每次HTTP都必须收到同身份/序号/观察hash的`call_observation_ack`，免费、unknown、rejected与最后一次调用也适用。reported还必须与同调用/原参数的`metering_report`汇合并确认settled；unknown保留完整hold，不伪造计量。已知超界报告立即停止本地派发；`metering_ack`的anomaly或unconfirmed均不恢复执行。

观察hash复用控制账本的长度前缀SHA256：observation.v2域、transport outcome、十进制HTTP状态、映射后的领域错误、business outcome、usage hash、audit hash；unknown的usage项与非chat的audit项为空字符串。OUTPUT_INVALID→MODEL_PROTOCOL_ERROR、INPUT_INVALID→INVALID_ARGUMENT、PROTOCOL_ERROR→EXECUTOR_PROTOCOL_ERROR，控制拒绝码不得伪装业务观察。hash不含身份，因此两端仍分别严格验证完整binding、request、call和sequence。共同向量与源合同见[内部v2说明](../api/executor/v2/README.md)。

两个FD可反序接收，但普通ACK发出不能早于observation和首次有效settled ACK。至多保存当前调用一份pending普通ACK，双屏障齐备前不恢复idle或发出结果。重复普通ACK属于协议错误；计量重复的既有幂等规则保留。停止后仍可接收标准完整原调用计量，废弃普通ACK不阻碍这项窄补报，也永不恢复普通执行。

ACK与汇合检查原call/step截止，等号过期。确认后第一个intent或result还必须在前一call截止前被接收，发出时间不能早于ACK；及时接受新intent后，其新permit定义新call截止，不把旧call截止继承到整次新请求。首次intent与零HTTP步骤没有虚构的前一call期限。此处执行ADR-0019的保守截止约束，等待ACK不会重新获得完整timeout。

`DispatchHooks.observe`返回具体ACK Frame，由原dispatcher Conversation验证后才释放业务结果。普通ACK只表示控制事实确认，不授予下一次HTTP、续期或提交步骤。模块测试hooks是替身；C3b Go桥接在真实SettleUsage/ObserveCall明确成功且执行权仍有效后发送，不能把pipe write或本地codec成功当作数据库已提交。

计量逐字段接受safeint，交账本判断超预留异常，不按许可或1024输出token截断。Worker RPC的`measurement_anomaly`明确表示已保存原始计量、冻结三层账户并保留hold；正常晚到usage不因此冻结。原principal/session/attempt/fence及30日窗口仍由数据库核验，补报不能修改Run终态、清除新attempt调用或推进游标。

结算ACK丢失只表示未确认，不证明事务未提交。活进程有界补报之外没有本地receipt journal；提交前进程/主机死亡可能丢失报告，未结算的调用保留unknown全hold。源码状态机不能替代这项真实进程验证。

## 分层验证

Windows按仓库要求先启动测试PostgreSQL，再执行真实RPC/PG测试；源码协议用共同fixtures和反例验证，Python SDK与旧v1仍回归。Linux镜像另外运行Go/Python共享BOOTTIME测试及既有S0进程探针。C3a沿用C2固定Linux镜像检查实际BOOTTIME和TCP；还提供`go test ./internal/runprotocol/v2 -run '^$' -bench '^BenchmarkExecutorV2Session$' -benchmem -cpu=1`测完整本地codec/状态校验会话，不把它当作IPC、数据库或模型吞吐。

历史C1直接使用的`golang.org/x/sys/unix`读取内核时钟，沿用仓库既有固定版本；当时未新增migration或修改Claim/预算转换。当前审计增量新增0024迁移并在新Claim/工具/调用前检查批次屏障，具体见审计指南。历史W4失败、AT-25跳过、生产留存和远程模型未验收继续保留。
