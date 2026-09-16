# Agent v3 S1-C3a：普通观察 ACK 验证

日期：2026-09-16；基线为PR #44合并提交`d11dd2fcdc737c92ce7d11e30665ce3ab6c455e4`。[PRD v0.11](../product/JobForge_PRD_v0.11.md)与[ADR-0019](../adr/0019-executor-confirmation-and-exit-contract.md)已通过独立审查接受。本次同步尚未正式部署的内部v2，公开HTTP/SDK/gRPC、v1及数据库合同不变；没有migration、依赖升级或收费调用。

合并状态：[PR #45](https://github.com/XJfyrh/JobForge/pull/45)最终`118ca82d`通过两份独立最终复审与[七项CI](https://github.com/XJfyrh/JobForge/actions/runs/35087095197)，合并提交`193623659de8fe41bfc4f5f0cabc048a167148e4`与审查代码树一致。以下保持C3a当时的验收范围，后续正式进程单列[C3b证据](agent-v3-s1-c3-runtime-2026-09-16.md)。

## 验收映射

| 要求 | 本切片实现与证据 | 仍需后续验证 |
|---|---|---|
| C3-01 观察确认 | Go/Python codec与原Conversation验证普通ACK的完整身份、调用序号及领域观察hash；dispatcher必须等ACK才释放业务结果 | 真实ObserveCall提交前阻塞/提交后丢ACK的PG/IPC窗口 |
| C3-02 顺序与期限 | 共同合法/非法帧、双通道汇合、免费/unknown/rejected、重复/迟到/过期和停止后计量；每个search子调用均有真实TCP等待/失败测试 | 真实双FD和完整Go supervisor/Worker |
| C3-03～05 退出、终止、提交 | 本切片没有实现固定退出码、guardian、Kill/Wait或Commit屏障 | 正式进程及故障测试；不能由模块通过替代 |
| C3-06 部署 | 只有一套更新后的内部v2，无旧无ACK兼容模式；固定Linux验证镜像实际执行同源码 | 新executor_version/profile与正式Worker镜像尚未注册或部署 |

共同源有13个合法帧、54个非法帧、109段会话和31个观察hash向量。原前11合法帧保持；原49会话的8条成功和41条负例保留原测试意图，结果拒绝场景补足无关ACK，避免被“缺ACK”提前遮蔽。Go另将向量与真实领域`ObserveCallRequest.Hash()`比较；这里的领域对象是内存验证，不是数据库提交证据。

确认等待仍使用原call/step截止，下一intent/result也必须在前一call截止前接收。新intent及时接受后清除前一call的继续截止，后续permit定义新期限。独立FD可反序接收；首次有效settled的发出戳决定因果屏障，重复计量ACK不能改写它。停止清除普通继续权，但保留原调用完整计量的窄补报能力。

## 检查记录

| 检查 | 实际结果 |
|---|---|
| Windows全部Python | `pytest sdk/python/tests python/tests tools/agent_probe_data tools/executorprobe -q`：审查补覆盖后最终863 passed，0 skip；初版852通过亦保留。只测确定性逻辑/真实TCP，不调用模型 |
| 固定Linux镜像全部业务Python | `tools/agenthttpcheck/Dockerfile`按最终源码重建，685 passed，0 skip；初版674通过亦保留。实际BOOTTIME/loopback TCP，运行时`--init --network none --cpus 1 --memory 256m --pids-limit 64` |
| 定向协议 | Go `go test -race -count=1 ./internal/runprotocol/...`通过；共同Python协议292项通过，真实TCP工具原86项通过，审查后新增第二子调用11项已包含最终全套 |
| Go静态检查 | 全仓build、vet、golangci-lint通过，0 issues |
| Python静态检查 | Ruff check/format 48文件通过；mypy SDK/业务25文件、Linux业务/探针15文件通过 |
| SQL/Proto | SQLFluff 3项历史基线、migration lint、Buf lint通过；无SQL或Proto变更 |
| 可观测配置回归 | 仪表盘重生成无内容diff；promtool配置及5条规则测试通过，无观测配置修改 |
| Windows全仓race | 第三轮最终退出0：1049个测试/子测试通过事件、29包通过、0失败，5个测试skip与11个无测试包单列。首轮scheduler计数失败、第二轮AT-22样本记录竞态及修复保留如下 |
| 独立审查/PR CI | 两份独立上下文预审无剩余P1/P2，覆盖缺口已修复；最终head复核和七项CI以PR记录为准，不预先宣称通过 |
| 正式IPC/PG观察确认、DeepSeek及40案 | 未运行；模块结果不能替代 |

Linux镜像基础digest沿用C2的`sha256:782412e85d0f0984994c290652577d4018aff08145c85b262bb63dc0c7522254`，审查补覆盖后镜像manifest为`sha256:9ef21427010dbac81f477e5b4cf2c11c3fd6cc955e7c4f3b6a1532cb0f0074bb`。它只复制Python模块/测试及协议源，不含模型凭据或业务gold。

独立预审发现真实TCP ACK矩阵只列search的第1/3/4子调用，但证据写“每个子调用”。补入第2个profile_tags→query_embedding屏障的11种等待/确认/错误/超时/取消场景，再全量重跑Windows及Linux；生产实现不变。最终head审查及CI状态另列。

首轮race失败是旧`TestRecoverExpiredLeasesConcurrentNoDoubleRecovery`对共享测试库全局回收数断言10，实际14。真实PG审计显示同事务的14个job全部只有一次回收：目标队列10个、此前AT-23队列1个、quota reconcile队列3个；额外4个的60秒租约恰好在前一个回收语句之后到期。修复给该测试独立UUID schema/pool，`search_path`排除public，迁移、两条锁连接、造数及所有断言均使用该pool。仍精确要求总数10、state_version=3、10条审计与10条attempt记录，不改生产SQL。定向真实PG race连续20次通过，schema清理后无残留；完整重跑结果单列，首次失败保留。

5个测试skip为`TestCancelAT25ControlStreamDegradation`、`TestRealTasksLifecycle`、`TestRealTasksSDK`、`TestBusinessWorkerProcessHelper`、`TestWorkerProcessHelper`；另11包无测试文件。两个Helper是子进程入口，真实模型套件本轮未启用，AT-25保留未验收。SDK真实HTTP及独立业务PG/HTTP契约实际运行。

第二轮`TestQuotaAT22SustainedFairness`报告598个样本，实际Complete计数及PG succeeded均为600。旧测试在Enqueue/reanchor已让任务可领取之后才登记计时，消费者抢先领取时静默跳过缺失样本。修复在已知job ID、Enqueue之前登记单调起点，缺失计时显式失败；测量准确命名为提交开始→Claim（ready→claim的保守上界），包含入库耗时。600分母、p95≤1秒、max≤2秒及drain要求保持。定向race三轮，每轮30秒、20 jobs/s、600样本/完成/成功；p95依次44.6832/61.6917/63.8912ms，max依次57.5736/97.0222/88.3095ms，全部通过。它修复测试观测竞态，不改变生产配额或公平性算法。

## 定向性能

新增`BenchmarkExecutorV2Session`在计时外读取fixture、构造JSONL，计时内对每段完整会话执行decode、Conversation校验、许可消费和结果收尾，包含标准校验所需的分配。chat为1次HTTP；search为4次HTTP并保留原fixture的两条重复计量帧。它没有运行HTTP、模型、数据库或真实管道。

基线与候选使用同一benchmark文件，各自运行合同要求的完整合法序列。基线chat为7帧/8事件、search为18帧/22事件；候选为8帧/9事件、22帧/26事件。同机Windows/amd64、Go1.26.5、Ryzen7 7840HS、GOMAXPROCS=1、`-cpu=1 -benchtime=1s -benchmem`，各5轮base→candidate交替，其他验收已停止后测量。候选是本PR实现，benchmark文件在基线工作树逐字节一致。

| 完整会话 | 基线中位数（最小～最大） | 候选中位数（最小～最大） | B/op中位数 | allocs/op |
|---|---|---|---|---|
| chat，1 HTTP | 1.029 ms（1.003～1.263） | 1.173 ms（1.130～1.252） | 364159→415291 | 7793→8819 |
| search，4 HTTP及迟到重复计量 | 2.799 ms（2.621～2.920） | 3.282 ms（3.174～4.281） | 962018→1162327 | 20585→24651 |

完整会话中位数分别增加约14.0%和17.3%，包含必需的新确认帧及校验/hash成本；不宣称性能改善或历史门禁转绿。样本有明显调度波动，5轮本地微基准不提供端到端SLO结论。两树harness SHA256均为`37bf50c07fb680ab4f45448d51baff7b025c3a75294c94a4075d4afb9bef7c9f`，时间原样本如下（ns/op，按交替轮次）：

| 会话/版本 | 1 | 2 | 3 | 4 | 5 |
|---|---:|---:|---:|---:|---:|
| chat基线 | 1039997 | 1028990 | 1002818 | 1262500 | 1009402 |
| chat候选 | 1172743 | 1206366 | 1142260 | 1130227 | 1252115 |
| search基线 | 2828993 | 2704758 | 2798858 | 2620980 | 2919673 |
| search候选 | 3174138 | 4280773 | 3282243 | 3187602 | 3676788 |

完整命令输出保留于仓库外`s1c3a-perf-{base,candidate}-{1..5}.txt`及summary。真实PG/IPC延迟须在正式Worker层另测。

## 未验收与历史结论

测试协调者、模型及业务向量是替身，loopback TCP是真实传输；未访问DeepSeek、未消费云端费用，未运行40案业务评分。正式Go Worker、guardian/固定step、PG持久确认/提交窗口、provider身份持久审计、跨队列TraceContext与S2～S5继续待实现。

历史[W4性能门禁失败](../worker-capacity-performance.md)、[AT-25跳过](../agent-rag-review.md)、远程模型与生产长期留存未验收原样保留。本地协议微基准和新增模块通过均不能改变这些结论。原始检查、决策和审查另存仓库外`E:\JobForge-notes\2026-09-16-agent-v3-s1`，不包含秘密或完整模型输入输出。
