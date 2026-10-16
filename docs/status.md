# 当前状态与下一步

公开接入入口见[免费Run](quickstart.md)、[业务边界](support-overview.md)、[部署适用性](deployment.md)和[新版公开演示](demo.md)。本轮只新增免费机制检查与展示材料，原冻结模型质量和费用结果保持下述口径。

2026-10-16完成[S5最终执行与质量闭环](evidence/agent-v3-s5-closeout-2026-10-16.md)：**工程能力已完成，正式模型质量达标**。V9原未见20案正确18/20、完整案例证据18/20，达到原16/18门槛，四类硬失败0；Fixed原20案各11/20，配对胜8/平11/负1。Fixed不套Agent候选门槛。候选在新20案创建前冻结，正式结果、恢复及实际审批演示分别有独立结果收据。交付检查与合并事实以[PR #61](https://github.com/XJfyrh/JobForge/pull/61)最新记录为准。

Fixed原批完成18案后因宿主监控timeout异常停止。维护者明确授权后，原未执行019/020在新窗口、新控制库各补跑一次且均正确；原18案未重跑，停止批次不重开，合并摘要保留逐案registration/评分来源。Agent残余006/011保留为质量失败，不重试、不补样、不改gold，不启动新候选。

[原11项恢复对照](evidence/agent-v3-s5-v9-recovery-2026-10-16.md)已完成：5项实际恢复机制通过，8个完整方案中7个正确，3个H0按计划取消。两组可比案例的checkpoint续跑费用均高于中断前缀加从头执行，另一组因错答不可比，不宣称节费。实际演示新增1Run/4chat、估算¥0.039035；最终50库累计727Runs/2,924chat，observed usage估算**¥23.325083**，原¥28剩余**¥4.674917**。held/unknown chat/在途/未结束attempt均0，所有本批容器退出、锁释放。原丢ACK动作POST仍保留unknown，最终效果由唯一回执确认。费用不是供应商已结算账单。

正式40实际Run/220HTTP的OTLP关联全部通过，连同此前开发与已见回归累计140Run/790HTTP；Trace采集未新增模型调用。恢复工具新增明确的V9入口、真实故障边界、全历史费用核对与Trace接线。相关Python、代理race、类型和quick检查通过；一项Linux监督器测试在Windows跳过。Linux fixture lint已修复并经独立复核，运行源码`28242e6`的[全部所选CI及CI required](https://github.com/XJfyrh/JobForge/actions/runs/38848905178)通过，包括Linux进程、真实PG/race、恢复及镜像检查。后续文档提交仍以对应最新head检查为准。

隔离Linux Compose实际演示中，维护者批准具体Run/方案后发生唯一业务写入，真实SIGKILL/Wait和自然lease/Sweep后只查询回执，同一Run成功、工单版本2→3、模型无重放，SQL与SDK祖先Trace一致。[三分钟演示及原始材料](evidence/agent-v3-s5-closeout-2026-10-16.md)注明SDK首段录制失败及闭账只读回看，不为录像补跑模型。S5没有剩余收费实验或新候选开发；完整开发Agent39/40、Fixed34/40及已见V3的19/20、其他历史失败和未执行分别保留。

| 阶段 | 已实现能力 | 实际验收 |
|---|---|---|
| S0 | 执行器/模型协议可行性探针、v3 设计 | 合成工具与进程探针，范围见[历史证据](evidence/agent-v3-s0-review-2026-09-16.md) |
| S1 | 独立业务快照/政策检索、Run 账本、固定流程、供应商审计 | [40 案真实执行](evidence/agent-v3-s1-delivery-2026-09-17.md)，业务 11/40，安全硬失败 0；固定流程开发基线 |
| S2 | 由模型选择只读工具、累计来源、一次纠错 | [40 案真实验收](evidence/agent-v3-s2-delivery-2026-09-17.md)，业务 37/40，安全硬失败 0；另有真实响应截断注入 |
| S3 | 复用已提交步骤、自然租约接管、未提交步骤有条件重做 | [固定 11 项真实验收](evidence/agent-v3-s3-cloud-2026-10-04.md)，5 个接管机制通过；8 个完整方案中 7 个业务通过，DEV-035-C 失败；3 个按计划取消的对照运行（H0） |
| S4 | 人工审批、Go结论记录、独立动作许可、终态效果核对与回执优先retry | [固定 10 个源样本及 2 个后继](evidence/agent-v3-s4-cloud-2026-10-05.md)，原方案业务质量 10/10、机制 10/10，7 条唯一业务回执；真实层获独立接受 |
| S5 | 同源页面、有限 OTLP span/link、低基数指标与面板、7日终态内容清理、双库停写恢复；独立公平对照 profile 与评分工具 | [最终结果](evidence/agent-v3-s5-closeout-2026-10-16.md)：正式Agent正确/完整证据18/20，Fixed各11/20；原11项恢复与一案实际人工审批/唯一业务回执恢复完成，工程能力与正式质量分别通过 |

任务工作台采用统一的雾蓝灰背景、大标题与细线列表，支持已加载任务筛选、提交、事实来源、审批确认及独立结果核对；执行与账本技术详情移出业务页面。新建方式由部署 `ui_options` 绑定，未配置或停用时不提供可用的新建操作，见[页面与配置](agent-v3/operations.md#页面与身份)和 [ADR-0041](adr/0041-business-workbench-presentation.md)。批准后保存处理结论并更新工单标记，实际结果可查业务回执。角色和状态码见[审批指南](agent-v3/approval.md)。默认部署没有收费 profile；启用模型批次需登记不可变 profile、预算和独立凭据。旧 Job API/Ollama 示例使用[独立维护路径](legacy/README.md)。

S5 的剩余范围和门槛见[验收矩阵](agent-v3/s5-acceptance.md)。免费机制通过不替代真实 DeepSeek 方案质量、恢复成本对照及正式20案验收。

2026-10-11完成[固定输入裁决诊断](evidence/agent-v3-s5-decision-diagnosis-2026-10-11.md)：已见20案、四组各三轮共240次调用，原提示A、thinking B、完整政策C、精简提示D正确数分别为31/49/33/19（各60次）。B三轮均改善，但有3次截断和持续语义错误，单次裁决估算费用/平均耗时约为A的10.3/15.4倍。诊断结束时S5累计observed usage估算¥7.103572，held为零；不重发原15条，该诊断未新增生产Run。

随后按[ADR-0033](adr/0033-versioned-final-adjudication.md)实现schema11/12独立终裁，并完成[开发40案和已见v3回归20案](evidence/agent-v3-s5-final-adjudication-2026-10-11.md)。同批初稿到终裁，开发正确数38→38、完整证据39→38；已见回归正确数11→17、完整证据12→17，四类硬失败均零。004缺少冲突定义P05.1并漏送达后的运输冲突，017/018取得P06.1后仍错用未送达时效分支，因此未冻结候选，未启动Fixed开发、新未见集、恢复或演示。当前23库累计449 Runs、1808 chat、observed usage估算¥9.275641，held/unknown/在途和未结束attempt均零，剩余¥10.724359；两批收费进程已退出。免费检查与真实质量分列，提交前完整Go race复跑通过，远程CI未运行。

2026-10-12：按[ADR-0034](adr/0034-s5-final-stage-separation-and-condition-enumeration.md)（Accepted）实现schema13 / `support-agent-prompt-v5` / `linux-v2-adjudication-runtime-2`候选：终裁改为自包含final-only请求（system由17,931降至13,619字节、工具诱导文本归零，载荷移除工具额度与检索导航），调查增加条件类枚举与`definition_retrieved`，离线评分增加主张级/证据级配对差值、**两种**检索闭合（可引用闭合与定义闭合）与终裁推理余量，并补齐(13,12)冻结路径与未知dataset失败关闭。维护者已批准方案与S5阶段上限由¥20提高至¥28（余量¥18.724359；该批准为会话内转录记录，非独立签名收据）。独立设计审查结论`accept_with_conditions`，4项BLOCKING已全部处置并复核：用出货代码在上一轮60案上复算，可引用闭合2/60不可达（DEV-020、S5V3-018）、定义闭合3/60不完整（多出S5V3-004），57完整→55正确、3不完整→0正确；审查另发现失败关闭曾连带拒绝v1历史重评路径，已加显式分支修复。**本候选尚未运行任何收费批次，无真实模型质量结果**；schema11/12的既有结果与上段账本不变。免费层已通过全仓真实PG/AOF Redis的`go test -race`（exit 0，integration 436.0s）、固定Linux三层镜像、Python三组套件（1882+103+107通过）、Ruff/mypy七域、SQLFluff与Buf；远程8项CI未运行。批次5前还须绑定`DATASET_V4`并在evidence.py/support_profile.go/两份profile-schema的dataset_id enum/registration路由测试同步，且冻结必须在本轮改动提交之后创建。

2026-10-13：按[ADR-0035](adr/0035-s5-batch-reservation-and-v5-replay.md)将各收费批次上限统一为¥4，累计仍为¥28；修复schema13复用已见包作为正式集的入口遗漏，新增实际v5终裁投影回放及原始来源/请求/账本绑定。20次固定输入回放正确与完整证据18/20（原17/20，017修复），不是生产Run或正式验收。随后完成开发前10案独立语义审阅，并在同一进程继续；DEV-020只取到P02.1/P07.1/P07.2，缺少必需的P03.1/P03.2，终裁识别缺失事实仍无法满足引用。逐案守卫立即停发，开发实际20 Runs/101 chat、正确与完整证据各19/40，剩余20案未提交；四类硬失败0。本轮两批新增估算¥1.316121；最终25库累计469 Runs、1929 chat、observed usage估算¥10.591762，held/unknown/在途/未结束attempt均0，余量¥17.408238，收费与配套进程均已退出。Python相关169项、外部守卫32项、诊断及全仓Go race、静态检查与三层镜像来源核验通过，远程CI未运行。下一步按ADR-0034退出分支提出协议级调查闭合的新ADR，不再追加本候选调用；[完整报告与机器证据](evidence/agent-v3-s5-v5-validation-2026-10-13.md)。

2026-10-13：维护者批准 [ADR-0036](adr/0036-s5-controlled-policy-supplement.md)。schema 14 / prompt-v6 实现一次受控补证、独立再次裁决与逐调用计量，保留 marker 原缺口并新增最终输入闭合；旧435个实际请求哈希不变。[免费机制](evidence/agent-v3-s5-v6-mechanism-2026-10-13.md)及独立复审通过，包括全仓真实PG/Redis race、固定Linux四项实链及生产镜像来源隔离。随后新增宿主最终闭合守卫，188项S5/driver/host测试、Ruff/mypy与镜像核验通过，独立放行新开发40案。前10案独立审阅后同进程继续；DEV-020/031分别通过missing_facts/ticket_status补证修复，DEV-033仍未申请可用P08.1，行动错误且双闭合缺失，第33案后停止。原40分母32正确/完整、1失败、7未执行；独立核验177个请求哈希、费用及无业务写入。新增估算¥1.053030，26库累计502 Runs/2106 chat、¥11.644792，held/unknown/在途/未结束attempt均0，余量¥16.355208；本批全部进程已退出。下一步评估权威工单状态触发的窄政策阅读义务，须新版本决策，不续跑或追加本候选调用；[完整结果](evidence/agent-v3-s5-v6-development-2026-10-13.md)。

当前候选按 [ADR-0037](adr/0037-status-policy-read-prerequisite.md)实现 schema15 / prompt-v7 / executor4：Go 在首次 thinking 前补齐权威特殊状态所需政策，已取得则不重读，保留一次可选补证。[免费机制](evidence/agent-v3-s5-v7-mechanism-2026-10-13.md)通过全仓真实PG/Redis race、5项固定Linux链路和Python 1,892项检查（另12 skip）。新开发40案实际212 chat/517物理调用，新增usage估算¥1.207045；开发结束时27库累计542 Runs/2,318 chat、¥12.851837，held/unknown/在途/未结束attempt均0，余量¥15.148163。独立重算原导出、评分和212个请求哈希一致，业务未写入，全部收费及配套进程已退出。最终输入39案已测且闭合，026无final不计通过。远程CI与该批OTLP未验收；[完整结果与机器摘要](evidence/agent-v3-s5-v7-development-2026-10-13.md)。

随后保持v7源码和镜像不变，独立放行原已见v3 20案回归。实际4 Runs/24 chat/72物理调用，001–003正确且证据完整；004首次thinking的16,384 completion全部为reasoning，91.338秒后返回协议失败、未提交final。实际输入缺P05.1但补证入口可用，不能据此断言缺定义造成容量耗尽。守卫在第4案后停止，005–020未执行；最终输入闭合仅3案已测，004不计通过。新增usage估算¥0.313183，28库累计546 Runs/2,342 chat、¥13.165020，held/unknown/在途/未结束attempt均0，余量¥14.834980。24请求哈希与原导出评分独立复核一致，业务无写入、回执为空，全部收费及配套进程已退出。44项私有守卫/driver检查通过，不续跑或进入Fixed、新未见集、恢复、演示；[回归结果与原因分析](evidence/agent-v3-s5-v7-seen-v3-2026-10-13.md)。

## 限制与未结事项

| 范围 | 未结事项与影响 | 证据 |
|---|---|---|
| 旧 Job Claim 性能 | W4 相对历史基线超过 15% 回退门槛，仍待同环境复验/基线决策；不能用后续不同口径数据认定问题已解决 | [原基准](archive/benchmark-history.md#历史微基准绝对值核对)、[v0.6 审查](archive/agent-rag-review.md) |
| 旧 Job 取消控制流 | AT-25 的可选 ControlStream 尚未实现，用例 skip；已接受的 heartbeat 取消信号 p95≤6s 不能改称推送 1s 保证 | [ADR-0008](adr/0008-cancel-control-channel-heartbeat.md)、[历史可靠性](archive/reliability-history.md) |
| 旧远程 Ollama | 自配远程 Ollama 的连通/鉴权/完整任务路径未验收；v3 的真实 DeepSeek 验收单列，不受此项概括 | [旧模型接入](legacy/real-tasks.md)、[历史审查](archive/agent-rag-review.md) |
| 生产运维 | S5 已有有界内容清理、停写新库恢复与开发观测演练；尚无生产容量治理、在线跨库备份/PITR/HA、长期 Trace 留存或外部告警验收 | [v3 操作](agent-v3/operations.md)、[免费机制](evidence/agent-v3-s5-free-2026-10-07.md) |
| 开发集质量 | S1 的 RQ-06 检索未命中、S2 的 DEV-031/032/033 协议失败、S3 的 DEV-035-C 业务失败均保留；现有公开开发集不能证明未见保留集泛化 | [S1 检索](evidence/agent-v3-s1-support-2026-09-16.md)、[S2](evidence/agent-v3-s2-delivery-2026-09-17.md)、[S3](evidence/agent-v3-s3-cloud-2026-10-04.md) |

费用中的 known 是按冻结费率与 usage 计算的保守账本值，不能称供应商已结算费用；unknown/full hold 不是零费用。各批结果和历史冻结账户以原机器证据为准。
