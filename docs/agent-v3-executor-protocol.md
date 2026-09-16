# Agent v3 S1-C1：执行器协议与时钟接缝

[ADR-0018](adr/0018-deepseek-fixed-flow-and-executor.md)随PR #41接受。本切片提供正式执行器要使用的严格codec、顺序校验和计量权限，以及Worker RPC的权威时间/异常标志；尚未交付正式Worker、IPC监管循环、DeepSeek HTTP适配或40例云端执行，不能单凭fixture通过宣称这些验收完成。

## 版本与所有权

`api/executor/v2/schema.json`为新帧的源合同，`fixtures`是Go/Python共同用例；实现分别在`internal/runprotocol/v2`和`python/jobforge_agent/protocol_v2.py`。v1合同、fixture和行为保留，不把新字段静默加入旧严格解码。帧、许可状态机及窄计量接收器不持lease、不启动HTTP或子进程、不写控制库，也不具备独立调度权。

正式执行器必须将普通JSONL与计量JSONL放在不同继承FD，由各自的标准decode与8KiB/384KiB上限处理。codec和状态机只验证输入及权限，真实FD生命周期、Kill/Wait、有限排空和完整usage后有界SettleUsage仍由后续Worker负责。不能从拒绝帧中宽松捞取usage，也不能把异常报告当成新的执行许可。

## 时间

Register、带lease的Claim、idle/active/stop Heartbeat从存储层返回`authority_observed_at`，取自相关阻塞锁后的PostgreSQL时钟。重复Register提供当前观测但不延长原session；空Claim无lease，也不续session。协议新增字段兼容旧Proto；生成代码来自`buf generate`。

`internal/runclock.FromAuthority`计算`RPC发起BOOTTIME + (expires_at - authority_observed_at)`，保守扣除整个RPC耗时；检查原类别的最大TTL、整数溢出和等号到期，向下取整毫秒。它不读取宿主墙钟。`runclock.Now`只在Linux读取CLOCK_BOOTTIME，与同容器/time namespace的Python `time.clock_gettime_ns(time.CLOCK_BOOTTIME)`共用时钟域；非Linux明确返回不支持，Windows使用Linux容器。

v2的`emitted_mono_ms`是剩余期限的发出锚，接收方扣除IPC排队时间。普通执行路径拒绝未来戳、倒退、过期和溢出；正常心跳也不能延长已签发的物理调用截止。窄计量通道可在普通会话停止后接收已经完整取得的原调用计量，不能因此重开步骤。

## 计量与普通结果

普通observation声明`usage_disposition=unknown/reported`和可空`usage_hash`。reported必须与同调用/原参数的`metering_report`汇合并确认settled，才可继续；不能依赖不同FD的接收先后。unknown保留完整hold。已知超界报告立即停止本地派发；`metering_ack`的anomaly或unconfirmed均不恢复执行。

计量逐字段接受safeint，交账本判断超预留异常，不按许可或1024输出token截断。Worker RPC的`measurement_anomaly`明确表示已保存原始计量、冻结三层账户并保留hold；正常晚到usage不因此冻结。原principal/session/attempt/fence及30日窗口仍由数据库核验，补报不能修改Run终态、清除新attempt调用或推进游标。

结算ACK丢失只表示未确认，不证明事务未提交。活进程有界补报之外没有本地receipt journal；提交前进程/主机死亡可能丢失报告，未结算的调用保留unknown全hold。源码状态机不能替代这项真实进程验证。

## 分层验证

Windows按仓库要求先启动测试PostgreSQL，再执行真实RPC/PG测试；源码协议用共同fixtures和反例验证，Python SDK与旧v1仍回归。Linux镜像另外运行Go/Python共享BOOTTIME测试及既有S0进程探针。具体命令和结果随本切片证据记录；真实Worker、网络适配和云端模型层待后续切片运行。

新增直接使用的`golang.org/x/sys/unix`读取内核时钟，沿用仓库既有固定版本；`go mod tidy`同时将此前已直接使用的genproto/rpc归入直接依赖，未升级版本。无数据库migration、未更改Claim SQL或预算转换；历史W4失败、AT-25跳过、生产留存和远程模型未验收继续保留。
