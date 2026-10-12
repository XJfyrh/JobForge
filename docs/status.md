# 当前状态与下一步

更新于2026-10-12。已接受阶段为Agent v3 S4（[PR #60](https://github.com/XJfyrh/JobForge/pull/60)）；S5正式质量未通过，[PR #61](https://github.com/XJfyrh/JobForge/pull/61)未合并。schema9/10最后正式结果为Agent正确12/20、完整证据15/20；新[独立终裁候选](evidence/agent-v3-s5-final-adjudication-2026-10-11.md)开发正确/完整证据38/40、已见回归17/20，但004/018未解决，停在冻结之前，恢复与演示未启动。本页维护实现状态；契约见[产品索引](product/README.md)和[ADR索引](adr/README.md)，分层结果见[证据索引](evidence/README.md)。

| 阶段 | 已实现能力 | 实际验收 |
|---|---|---|
| S0 | 执行器/模型协议可行性探针、v3 设计 | 合成工具与进程探针，范围见[历史证据](evidence/agent-v3-s0-review-2026-09-16.md) |
| S1 | 独立业务快照/政策检索、Run 账本、固定流程、供应商审计 | [40 案真实执行](evidence/agent-v3-s1-delivery-2026-09-17.md)，业务 11/40，安全硬失败 0；固定流程开发基线 |
| S2 | 由模型选择只读工具、累计来源、一次纠错 | [40 案真实验收](evidence/agent-v3-s2-delivery-2026-09-17.md)，业务 37/40，安全硬失败 0；另有真实响应截断注入 |
| S3 | 复用已提交步骤、自然租约接管、未提交步骤有条件重做 | [固定 11 项真实验收](evidence/agent-v3-s3-cloud-2026-10-04.md)，5 个接管机制通过；8 个完整方案中 7 个业务通过，DEV-035-C 失败；3 个按计划取消的对照运行（H0） |
| S4 | 人工审批、Go结论记录、独立动作许可、终态效果核对与回执优先retry | [固定 10 个源样本及 2 个后继](evidence/agent-v3-s4-cloud-2026-10-05.md)，原方案业务质量 10/10、机制 10/10，7 条唯一业务回执；真实层获独立接受 |
| S5 | 同源页面、有限 OTLP span/link、低基数指标与面板、7日终态内容清理、双库停写恢复；独立公平对照 profile 与评分工具 | [免费机制](evidence/agent-v3-s5-free-2026-10-07.md)通过；[prompt-v3正式结果](evidence/agent-v3-s5-v3-real-2026-10-08.md)Agent正确12/20、完整证据15/20，Fixed为1/20、2/20；四类硬失败0，质量未通过，恢复/演示未执行 |

任务页面支持审阅方案，批准后保存处理结论并更新工单标记，实际结果可查业务回执。角色和状态码见[审批指南](agent-v3/approval.md)。默认部署没有收费 profile；启用模型批次需登记不可变 profile、预算和独立凭据。旧 Job API/Ollama 示例使用[独立维护路径](legacy/README.md)。

S5 的剩余范围和门槛见[验收矩阵](agent-v3/s5-acceptance.md)。免费机制通过不替代真实 DeepSeek 方案质量、恢复成本对照及正式20案验收。

2026-10-11完成[固定输入裁决诊断](evidence/agent-v3-s5-decision-diagnosis-2026-10-11.md)：已见20案、四组各三轮共240次调用，原提示A、thinking B、完整政策C、精简提示D正确数分别为31/49/33/19（各60次）。B三轮均改善，但有3次截断和持续语义错误，单次裁决估算费用/平均耗时约为A的10.3/15.4倍。诊断结束时S5累计observed usage估算¥7.103572，held为零；不重发原15条，该诊断未新增生产Run。

随后按[ADR-0033](adr/0033-versioned-final-adjudication.md)实现schema11/12独立终裁，并完成[开发40案和已见v3回归20案](evidence/agent-v3-s5-final-adjudication-2026-10-11.md)。同批初稿到终裁，开发正确数38→38、完整证据39→38；已见回归正确数11→17、完整证据12→17，四类硬失败均零。004缺少冲突定义P05.1并漏送达后的运输冲突，017/018取得P06.1后仍错用未送达时效分支，因此未冻结候选，未启动Fixed开发、新未见集、恢复或演示。当前23库累计449 Runs、1808 chat、observed usage估算¥9.275641，held/unknown/在途和未结束attempt均零，剩余¥10.724359；两批收费进程已退出。免费检查与真实质量分列，提交前完整Go race复跑通过，远程CI未运行。

2026-10-12：按[ADR-0034](adr/0034-s5-final-stage-separation-and-condition-enumeration.md)（Accepted）实现schema13 / `support-agent-prompt-v5` / `linux-v2-adjudication-runtime-2`候选：终裁改为自包含final-only请求（system由17,931降至13,619字节、工具诱导文本归零，载荷移除工具额度与检索导航），调查增加条件类枚举与`definition_retrieved`，离线评分增加主张级/证据级配对差值、**两种**检索闭合（可引用闭合与定义闭合）与终裁推理余量，并补齐(13,12)冻结路径与未知dataset失败关闭。维护者已批准方案与S5阶段上限由¥20提高至¥28（余量¥18.724359；该批准为会话内转录记录，非独立签名收据）。独立设计审查结论`accept_with_conditions`，4项BLOCKING已全部处置并复核：用出货代码在上一轮60案上复算，可引用闭合2/60不可达（DEV-020、S5V3-018）、定义闭合3/60不完整（多出S5V3-004），57完整→55正确、3不完整→0正确；审查另发现失败关闭曾连带拒绝v1历史重评路径，已加显式分支修复。**本候选尚未运行任何收费批次，无真实模型质量结果**；schema11/12的既有结果与上段账本不变。免费层已通过全仓真实PG/AOF Redis的`go test -race`（exit 0，integration 436.0s）、固定Linux三层镜像、Python三组套件（1882+103+107通过）、Ruff/mypy七域、SQLFluff与Buf；远程8项CI未运行。批次5前还须绑定`DATASET_V4`并在evidence.py/support_profile.go/两份profile-schema的dataset_id enum/registration路由测试同步，且冻结必须在本轮改动提交之后创建。

## 限制与未结事项

| 范围 | 未结事项与影响 | 证据 |
|---|---|---|
| 旧 Job Claim 性能 | W4 相对历史基线超过 15% 回退门槛，仍待同环境复验/基线决策；不能用后续不同口径数据认定问题已解决 | [原基准](archive/benchmark-history.md#历史微基准绝对值核对)、[v0.6 审查](archive/agent-rag-review.md) |
| 旧 Job 取消控制流 | AT-25 的可选 ControlStream 尚未实现，用例 skip；已接受的 heartbeat 取消信号 p95≤6s 不能改称推送 1s 保证 | [ADR-0008](adr/0008-cancel-control-channel-heartbeat.md)、[历史可靠性](archive/reliability-history.md) |
| 旧远程 Ollama | 自配远程 Ollama 的连通/鉴权/完整任务路径未验收；v3 的真实 DeepSeek 验收单列，不受此项概括 | [旧模型接入](legacy/real-tasks.md)、[历史审查](archive/agent-rag-review.md) |
| 生产运维 | S5 已有有界内容清理、停写新库恢复与开发观测演练；尚无生产容量治理、在线跨库备份/PITR/HA、长期 Trace 留存或外部告警验收 | [v3 操作](agent-v3/operations.md)、[免费机制](evidence/agent-v3-s5-free-2026-10-07.md) |
| 开发集质量 | S1 的 RQ-06 检索未命中、S2 的 DEV-031/032/033 协议失败、S3 的 DEV-035-C 业务失败均保留；现有公开开发集不能证明未见保留集泛化 | [S1 检索](evidence/agent-v3-s1-support-2026-09-16.md)、[S2](evidence/agent-v3-s2-delivery-2026-09-17.md)、[S3](evidence/agent-v3-s3-cloud-2026-10-04.md) |

费用中的 known 是按冻结费率与 usage 计算的保守账本值，不能称供应商已结算费用；unknown/full hold 不是零费用。各批结果和历史冻结账户以原机器证据为准。
