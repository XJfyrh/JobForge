# Agent v3 实施记录

**2026-09-17 当前状态：S1 未完成，真实云端新批次因 `CHAT_USAGE_UNKNOWN` 停止；[PR #51](https://github.com/XJfyrh/JobForge/pull/51) 保持 Draft，S2～S5 未开始。** 分阶段记录保留当时事实；其中“尚无推理”等历史描述不代表当前状态。最新结果见[收尾验收与阻塞证据](evidence/agent-v3-s1-closeout-2026-09-17.md)，不得以工程 CI 或部分方案完成代替完整 40 案验收。

## 起点记录（历史）

- 开始日期：2026-09-16。
- 维护者已确认[路线 v3](plans/agent-execution-roadmap-v3.md)，S0契约与探针已交付，后续为S1。
- 起点：5c82834；分支 XJfyrh/agent-v3-s0。
- 审查入口：[已合并 PR #35](https://github.com/XJfyrh/JobForge/pull/35)、[独立审查与修复记录](evidence/agent-v3-s0-review-2026-09-16.md)。合并提交 `1bf92c2`。
- 开始时仅有此前规划的 README 与 docs/plans、docs/research 未提交改动，全部保留。
- 2026-09-16维护者纠正模型方向：云端主chat用于Agent开发、正式评测和演示，本地chat为可选扩展。本地成功不再作为S0退出或契约PR的阻断条件；云端实际调用仍未验收。
- 2026-09-16后续指令指定DeepSeek优先。只读鉴权/余额接口检查通过，尚未发起推理请求；Docker Desktop启动失败，按维护者“遇到阻塞停止后报告”的要求暂停S1，见[本轮审查与阻塞记录](evidence/agent-v3-deepseek-and-blocker-2026-09-16.md)。
- 同日维护者重启Docker后阻塞解除；当前补丁已完成Windows全仓真实依赖/race与固定容器11类进程场景复验，独立审查无阻断后合并PR #35。见[恢复验收记录](evidence/agent-v3-docker-recovery-2026-09-16.md)。

## 阶段状态

| 阶段 | 状态 | 当前证据 |
|---|---|---|
| S0 契约与关键试验 | 已交付并合并 | PRD v0.7、ADR-0013～0015接受；执行器11类真实进程/race通过；模型探针32项确定性回归通过；独立审查与六项CI通过 |
| S1 业务与基线 | 未完成；新批次外部计量阻塞，PR #51 保持 Draft | PR #50 审计、#52 停止竞态修复、#53 新批次累计授权已合并。新批次 14 个 awaiting_approval 方案、DEV-015 中断、25 案未尝试；业务 9/40，安全证据 14 案通过、1 案不完整、25 案未执行。见[新证据](evidence/agent-v3-s1-closeout-2026-09-17.md)；[首批失败](evidence/agent-v3-s1-first-cloud-2026-09-16.md)与[提示修复](evidence/agent-v3-s1-cloud-fixes-2026-09-16.md)保留 |
| S2 Agent 与预算 | 未开始 | S1-B提供Run/账本基础；动态Agent与真实云端预算仍未验收 |
| S3 步骤恢复 | 阶段未开始；S1-C3b已实测基础恢复机制 | 固定流程的真实Worker SIGKILL、新Claim和checkpoint恢复已实跑；动态Agent的S3故障矩阵与真实云端效果仍未验收 |
| S4 审批与写入 | 未开始 | 无新审批/写入验收 |
| S5 评测与展示 | 未开始 | 无新保留集、页面或观测验收 |

## S0 发现与决定

- attempt_no 和 recovery_count 分离，避免正常审批继续耗尽故障次数。
- 有效 lease 以数据库时间严格判定；匹配 owner/token 但已到期仍拒绝新步骤。
- 接收端先查相同 operation_id 的已有回执，再验首次写入版本，避免成功重发被新版本误拒。
- 取消终态后只读核对回执，不重新派发业务动作。
- stopping保存首次停止原因及独立cancel_requested；后到取消/总期限仍阻止恢复，Heartbeat不能无限延长停止阶段。
- 工具统一读取业务snapshot_id，物流聚合版本覆盖新增事件；动作事务复核完整版本向量。
- Python 执行器以有界进程组监督固定 step；关键进程退出能力由 Linux Docker 实测决定。
- 起始容器只有 qwen2.5:0.5b 与 all-minilm:22m；新增免费 qwen3:4b 冷/暖两次首调用均60s超时。qwen3:1.7b 完成16次真实chat，三个完整工具循环0/3、单步纠正2/2，独立结构化输出值/引用检查失败。未放宽预算、引入第三模型或调用收费接口。小模型旧抽取结果不代表新 Agent 能力。
- 云端主线已确定，维护者后续指定DeepSeek优先，取代此前百炼首选候选；S1计划并行开发云端适配、最小持久调用账本、业务服务和检索。S2真实Agent验收仍须完成。[本地历史结论及费用提案](evidence/agent-v3-model-probe-2026-09-16-summary.md)不代表云端通过或费用授权。

## S0验证记录（历史，不被S1覆盖）

| 检查 | 实际状态 | 范围/证据 |
|---|---|---|
| Windows Go build/vet/golangci-lint | 通过 | 全仓编译/静态检查，0 issues；不等于Linux进程race |
| Linux执行器真进程/race | 当前补丁通过 | Docker恢复后按固定镜像、1CPU/256MiB/64PID运行11个真实场景及1个纯协议测试，无skip/fail/race warning；[当前源码hash与记录](../tools/executorprobe/docker-recovery-2026-09-16.txt)。旧失败/基线记录保留 |
| Python SDK与两个探针guardrails | 通过 | 当前补丁合计93项，SDK51项、模型32项、执行器10项；不调用真实模型 |
| Ruff check/format、Mypy | 通过 | 全仓Ruff；SDK7文件和两个探针入口Linux平台类型检查 |
| SQLFluff | 通过 | 3条历史基线有效，migration lint通过；没有新增业务migration |
| Buf lint/breaking | 通过 | 对main检查；未修改Proto |
| Prometheus/Grafana配置 | 通过 | promtool配置和5条规则测试；仪表盘重新生成无内容diff |
| 文档/证据格式 | 通过 | UTF-8、JSON、末尾换行、本地链接与常见秘密格式检查无错误；交付前再次核对最终diff |
| qwen3:4b冷/暖真实请求 | 失败 | 各1次请求，均超过60s；[模型记录](evidence/agent-v3-model-probe-2026-09-16.md) |
| qwen3:1.7b真实请求 | 失败 | 三个工具循环0/3，独立结构化结果校验失败；单步纠正2/2不能代替完整通过 |
| 全仓Go集成/race、PR CI | 当前代码通过 | Windows `go test -race -count=1 ./...`退出0，355个测试通过事件、5个明确skip；实际PG/Redis/SDK HTTP契约。另[ace6b745 CI](https://github.com/XJfyrh/JobForge/actions/runs/35054474424)六项全部通过；squash后的1bf92c2具有相同代码树 |
| 云端主chat | 只读鉴权检查通过；推理未调用、未验收 | DeepSeek模型列表与余额接口HTTP 200、账号可用；尚无chat/completions请求，不代表模型能力、工具调用或业务质量通过 |
| 新业务/审批/步骤恢复/保留集 | 未运行 | S1～S5能力尚未实现，不能由S0探针替代 |

执行器首次把解释器启动混入500ms/3s超时导致失败；COPY后仍复现，不能归因于Windows挂载。现用有界总启动/执行deadline和started后的1s步骤计时，分别记录启动与清理耗时。探针仅证明固定受监管进程组在所测场景可行，不是完整生产执行器。

审查后的模型探针统一unknown停止和证据先落盘，修复仅经确定性回归测试；原始真实模型报告仍对应18a9302基线，没有替换为修复后结果。stderr补丁在Docker恢复后完成固定容器完整进程/race，Windows宿主全仓真实依赖测试也已通过；真实模型专层未运行。当前没有修改核心热路径，未新增性能门禁结论。

DeepSeek账号/凭据只读检查已通过，推理费用上限及调用账本尚未落地，没有推理调用。S0的模型工具fixture只检查模型协议，不作为真实业务验收。详细ADR在PR #35合并后标为Accepted；这只表示设计接受，S2及整体目标仍需真实云端业务验收。此调整不把既有失败改成通过。

原有远程模型与生产长期留存未验收、W4性能门禁失败、AT-25跳过继续保留在[旧审查记录](agent-rag-review.md)与历史报告。本轮没有重新验收或改写它们，亦不作为v3通过证据。仓库外决策档案位于 `E:\JobForge-notes\2026-09-16-agent-v3-s0`。

## S1启动（历史记录）

2026-09-16从 `8708308` 开始，当前只提交[PRD v0.8](product/JobForge_PRD_v0.8.md)、[ADR-0016草案](adr/0016-business-snapshots-and-policy-retrieval.md)及[切片计划](plans/agent-v3-s1-implementation.md)，尚无S1实现。先落真实业务HTTP/快照与固定本地embedding的pgvector检索，再以有效Run租约和持久账本接入DeepSeek及固定流程；不建立绕过账本的收费探针。后续决定与验证另存 `E:\JobForge-notes\2026-09-16-agent-v3-s1`。

## S1-A 合并与 S1-B 契约（历史记录）

2026-09-16，[PR #38](https://github.com/XJfyrh/JobForge/pull/38) 已合并，提交 `6924b71`。最终审查 head 为 `db8206cb563707a9d26036c3f66e75fc38c1134e`，两份独立上下文审查无剩余阻断；[机械CI](https://github.com/XJfyrh/JobForge/actions/runs/35059776718)七项和[原有真实模型工作流](https://github.com/XJfyrh/JobForge/actions/runs/35059776725)一项通过。后者不证明新 DeepSeek Agent 通过。

Windows全仓race有405个测试通过事件、5个明确skip；新增业务真实PG/HTTP/Python契约实际通过。独立新模型卷真实生成20段向量并完成20查询，Hit@3=19/20、MRR@3=0.808333；RQ06未命中保留。最终补丁修复float32极小/极大向量边界，定向race和最终容器HTTP 400检查通过；详见S1-A证据及仓库外归档。当前Docker恢复正常，业务服务和两个PostgreSQL实例可用。

接下来按 [PRD v0.9](product/JobForge_PRD_v0.9.md) / [ADR-0017提案](adr/0017-run-admission-and-call-ledger.md) 审查 S1-B 的接纳、Run租约、共享家族额度及逐物理调用合同。尚无生产Run/账本、正式执行器或收费chat请求，S1整体仍未完成。历史W4失败、AT-25跳过、远程模型和生产留存未验收不变。

## S1-B 实现验证与合并

2026-09-16，契约 [PR #39](https://github.com/XJfyrh/JobForge/pull/39) 已合并（`696404e`）。实现分支交付独立 `agent-control`、Run HTTP/SDK/Worker RPC、原子执行权、检查点和三层逐调用账本。Windows全仓race通过765个测试事件，5个测试skip明确保留；后补接纳/Claim/审批与128Run定向性能另已实际运行。新控制Compose从空库完成bootstrap和健康/鉴权查询。详见[需求映射与证据](evidence/agent-v3-s1b-runs-2026-09-16.md)。

实现[PR #40](https://github.com/XJfyrh/JobForge/pull/40)已合并为`f63300c`；最终head `43241c9`经过三份独立审查，修复一项RPC结果错误映射P2后无阻断，[七项CI](https://github.com/XJfyrh/JobForge/actions/runs/35072274898)全部通过。S1-B完成，整个S1尚未完成。正式执行器与DeepSeek收费调用未开始；真实模型、完整Agent质量、生产留存以及历史W4失败和AT-25跳过继续分别披露。

## S1-C 契约准备

[PRD v0.10](product/JobForge_PRD_v0.10.md)与[ADR-0018](adr/0018-deepseek-fixed-flow-and-executor.md)细化云端profile、正式执行器、独立异常计量、固定流程与可核对开发评分。[PR #41](https://github.com/XJfyrh/JobForge/pull/41)经两份独立上下文审查与[七项CI](https://github.com/XJfyrh/JobForge/actions/runs/35074431057)通过，已合并为`01177e9`。[协议/时钟/RPC接缝](agent-v3-executor-protocol.md)已随PR #42合并为`b9ba908`；最终七项CI、两独立复审和Windows901项测试/子测试通过。当前实现[受控HTTP与DeepSeek适配](agent-v3-authorized-http.md)；未运行DeepSeek推理，不把40案离线标签核对当作业务验收。

## S1-C2合并与C3契约调查

2026-09-16，受控HTTP/DeepSeek模块[PR #43](https://github.com/XJfyrh/JobForge/pull/43)合并为`5647d7c`。最终`4eed461c`修复独立审查的超时分类P2后，两份新上下文复审无剩余问题，七项CI通过；本地Windows全Python663项、Linux容器485项、真实依赖全仓race901项测试/子测试事件通过，5个skip明确保留。详见[证据](evidence/agent-v3-s1c2-http-2026-09-16.md)。尚无DeepSeek推理、正式Worker或40案结果。

C3实施调查发现：现有v2缺普通observation持久ACK，已关闭会话的大小错误缺类型化退出路由；管道write不能替代数据库确认。因此先提出[PRD v0.11](product/JobForge_PRD_v0.11.md)与[ADR-0019](adr/0019-executor-confirmation-and-exit-contract.md)，通过PR独立审查后才同步源合同/codec并实现正式进程。provider身份持久审计和跨队列trace来源也尚未交付，不能拿C2内存audit/HTTP透传当作通过。技术调查和决策继续归档仓库外。

## S1-C3契约接受与C3a实现

[PR #44](https://github.com/XJfyrh/JobForge/pull/44)最终head `3761fd9`通过两份独立契约审查及七项CI，已合并为`d11dd2f`；本次仅将对应PRD/ADR状态改为Accepted并标注ADR-0018被取代的精确时序范围，保留旧正文。C3a同步v2 schema、Go/Python codec/Conversation、共同fixtures及C2 observe hook，普通ACK与reported计量确认齐备后才继续。免费/unknown/最后调用同样受约束，停止后窄计量不能重开执行。

协议与真实TCP模块验证和同环境本地codec/会话性能另见[C3a证据](evidence/agent-v3-s1c3a-ack-2026-09-16.md)。正式FD/guardian/Worker、PG确认窗口和云端40案没有因该切片完成；W4失败、AT-25跳过及生产留存未验收继续保留。

## S1-C3a合并与C3b正式进程

[PR #45](https://github.com/XJfyrh/JobForge/pull/45)的最终head `118ca82dd4874786f262412ed75166ee5559ea9c`经两份独立上下文最终复审无剩余问题，[七项CI](https://github.com/XJfyrh/JobForge/actions/runs/35087095197)通过，已合并为`193623659de8fe41bfc4f5f0cabc048a167148e4`。第三次Windows全仓race通过1049个测试/子测试事件，5个skip保留；此前真实发现并修复的测试隔离和延迟采样竞态、失败原始日志继续留存。

C3b实现[固定运行时](agent-v3-runtime.md)：Go唯一控制面执行权、严格输入投影、Python单步/guardian、固定FD与有限清理、持久确认和Commit屏障、executor_version门禁。Linux实际进程race和Python、首轮真实PG/gRPC联合机制检查已运行，最终检查与恢复证据见[C3b记录](evidence/agent-v3-s1-c3-runtime-2026-09-16.md)。新增专门CI覆盖正式进程层，普通平台skip不能代替。当前生产registry为空，测试注册器/固定loopback模型只进入测试镜像；无收费调用，support策略/provider审计/跨队列trace/真实40案及整体S1仍未完成。

[PR #46](https://github.com/XJfyrh/JobForge/pull/46)最终head `512f0fc56ca3d1e87e12d3b99bf3328e396d1ca2`经两份独立上下文正式复审无剩余P1/P2，[八项CI](https://github.com/XJfyrh/JobForge/actions/runs/35093384805)全部成功，合并为`0829a4c04a3a123a6ffe6c374e326859b79df1a8`；审查与合并代码树一致。Windows全仓race通过1231个测试/子测试事件，20个具名skip明确保留，15个已由固定Linux进程/协调器/真实PG层覆盖；Python Windows945/Linux775通过。真实Worker SIGKILL重新Claim、已提交步骤复用、unknown预留及可信usage超界冻结/永久失败均实跑。合成供应商不是DeepSeek验收；下一步仍是support固定业务策略、provider持久审计与共享5元批次的40案真实运行。

## S1 support固定流程与provider审计合同

[PR #47](https://github.com/XJfyrh/JobForge/pull/47)最终head `e43e1249c0784acf57e3ef4911087a5caeeb28d1`经两份独立上下文复审及[八项CI](https://github.com/XJfyrh/JobForge/actions/runs/35096592640)通过，合并为 `d449bc709b99ad78d3563b2924802e535541a08a`。ADR-0020仅接受合同；同hash重放/不同hash冲突必须先于新财务判定，末次纠正失败的批次屏障严格绑定原attempt/session/fence/step。

随后实现support固定策略、闭集方案和生产adapter；开发数据v2保持40条expected不变，经独立Agent审查并重新完成真实向量化/索引/20查询。实际19/20、RQ-06未命中保留；全仓Windows race与正式Linux进程/PG检查通过，详见[分层证据](evidence/agent-v3-s1-support-2026-09-16.md)。当前仅完成该实现切片，收费profile未启用。provider持久审计、完整评分器及真实40案仍在后续工作中，S1整体未完成。

[PR #48](https://github.com/XJfyrh/JobForge/pull/48)最终head `1046f33fc00bb3c64543d71a97fb2c2c37625723`经两份独立上下文实现审查无P1/P2，[八项CI](https://github.com/XJfyrh/JobForge/actions/runs/35100509379)和旧真实模型工作流通过，合并为 `bffe5665f266f0510de94d7bf13a3f8dbc963df2`，树与审查head一致。后者不代表新DeepSeek通过；收费profile仍未启用，继续完成provider审计持久桥和评分。

## S1 provider 持久审计实现

按已接受 ADR-0020 完成有界 typed report、原调用绑定、首报告原子持久/定价、冲突与晚到权限、跨 FD ACK、持久批次 guard 和租户隔离的 Calls HTTP/SDK。实际 Windows/固定 Linux/PG/安装 SDK 与进程故障检查、性能退化和修复后的结果见[分层证据](evidence/agent-v3-s1-provider-audit-2026-09-16.md)。[PR #50](https://github.com/XJfyrh/JobForge/pull/50) 的最终提交 `577d79f` 经独立完整审查和诊断差量复核，[八项 CI](https://github.com/XJfyrh/JobForge/actions/runs/35110868145) 全部通过，合并为 `1d50170`。40案真实云端及整个S1仍未完成；历史W4失败、AT-25跳过与生产留存未验收不变。

## S1 收尾修复与新批次阻塞（2026-09-17）

[PR #52](https://github.com/XJfyrh/JobForge/pull/52) 已合并为 `8a4cbab`，[CI 35120539572](https://github.com/XJfyrh/JobForge/actions/runs/35120539572) 八项通过；[PR #53](https://github.com/XJfyrh/JobForge/pull/53) 已合并为 `4dd017b`，[CI 35121570133](https://github.com/XJfyrh/JobForge/actions/runs/35121570133) 八项通过，接受 [ADR-0022](adr/0022-s1-closeout-cumulative-authorization.md) 的新增累计 5 CNY 授权。PR #51 的 `dbacee7` 通过[CI 35121487782](https://github.com/XJfyrh/JobForge/actions/runs/35121487782)全部八项；随后 `b094e5b` 合入授权合同。这些工程结果不表示真实云端验收通过。

新 batch `901bce9d…` 于 2026-09-17 00:31:12～00:32:13（UTC+8）实际运行。14 案方案进入 `awaiting_approval`，DEV-015 中断，25 案未尝试；业务评分为 9/40，安全证据为 14 案通过、1 案不完整、25 案未执行。方案完成不表示已批准、写入业务或解决工单；不能将部分安全结果写成全批通过。完整结果见[证据](evidence/agent-v3-s1-closeout-2026-09-17.md)及[机器报告](evidence/agent-v3-s1-closeout-2026-09-17.json)。

中断调用取得 HTTP 200 响应头后未取得完整响应体，usage 无法确认，批次以 `CHAT_USAGE_UNKNOWN` 停止。新增已知费用为 48,653 microyuan（0.048653 CNY），未释放金额 hold 为 2,105,344 microyuan（2.105344 CNY）；hold 不代表供应商账单，unknown 不能当零费。按 ADR-0022，缺少必要持久确认时不得换 batch 绕过。已停止收费，PR #51 保持 Draft，S1 未闭合；历史首批证据、W4 失败、AT-25 跳过及生产长期留存未验收继续保留，不推进 S2～S5。
