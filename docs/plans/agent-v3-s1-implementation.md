# S1 实施计划与证据索引

- 起点：`8708308`，S0已完成；2026-09-16开始S1。
- 规范：[PRD v0.8](../product/JobForge_PRD_v0.8.md)、[ADR-0016](../adr/0016-business-snapshots-and-policy-retrieval.md)。保持路线v3全范围，切片合并不等于阶段完成。
- 2026-09-17 当前状态：**S1 未完成，PR #51 保持 Draft，S2～S5 未开始**。PR #52 停止竞态修复与 PR #53 / [ADR-0022](../adr/0022-s1-closeout-cumulative-authorization.md) 新授权已合并；修复后新批次因 `CHAT_USAGE_UNKNOWN` 停止。14 案方案完成、DEV-015 中断、25 案未尝试，业务 9/40；安全证据 14 案通过、1 案不完整、25 案未执行。见[最新证据](../evidence/agent-v3-s1-closeout-2026-09-17.md)。后文切片顺序保留为阶段历史，不能读作当前尚无云端调用。

| 顺序 | 产物 | 验收/合并条件 | 当前状态 |
|---|---|---|---|
| 契约 | 业务快照、工具HTTP、pgvector与准备身份 | 独立上下文审查；ADR按PR流程接受 | PR #37已接受 |
| S1-A | 独立业务服务、迁移/角色、40例开发数据、Python工具适配、索引准备与搜索 | 真PG/HTTP、tenant/版本/大小边界、固定真实embedding与20查询、重启持久性；适用CI | [PR #38](https://github.com/XJfyrh/JobForge/pull/38) 已合并；两份独立审查和最终head八项CI通过；[证据](../evidence/agent-v3-s1-business-2026-09-16.md) |
| S1-B | 最小Run/lease/内部协议、API/SDK、持久调用账本 | 有效执行权、并发预留、重发身份、unknown占额、取消/超时；不新增调度语义 | 契约经 PR #39 接受；实现[PR #40](https://github.com/XJfyrh/JobForge/pull/40)已合并；Windows全仓race、真实PG/HTTP/SDK和128Run基线、三份独立审查及最终七项CI通过；[运行指南](../agent-v3-runs.md)、[验收映射](../evidence/agent-v3-s1b-runs-2026-09-16.md) |
| S1-C | DeepSeek固定profile、受监管执行器、合理固定流程、评分器 | 实际云端与工具链；40开发例全量报告；方案与写入分开；费用硬上限 | C1/C2、普通 ACK、正式运行时、support 策略及 provider 持久审计已分别合并；PR #51 实现完整评分与云端启动，`dbacee7` 八项 CI 通过。首批及修复后新批次均未完成 40 案；[新批次外部计量阻塞](../evidence/agent-v3-s1-closeout-2026-09-17.md)，不能合并 PR #51 或宣称 S1 完成 |

实施目录意向：`internal/business`与独立命令保存业务领域/服务/PG/HTTP；`migrations/business`维护业务迁移；Python执行器与工具适配不依赖队列存储；数据与gold分别打包。公开源schema、具体目录和生成工具在对应实现PR确定，避免复制多套同义契约。

关键依赖顺序：业务snapshot先于Run绑定；有效Run lease和持久账本先于任何收费调用；方案进入awaiting_approval需要原子保存最小审批绑定并释放lease，即便批准/写入实现留在S4。S1不能将有写建议的proposal误标为succeeded/applied。

真实模型与确定性检查分开记录。历史W4失败、AT-25跳过和生产长期留存未验收保持原结论；已有真实 DeepSeek 调用不等于完整 40 案验收通过。技术选择与受控原始证据另存于仓库外 `E:\JobForge-notes\2026-09-16-agent-v3-s1`；公开报告不归档凭据、完整模型输入输出或秘密。

## 正式执行器的后续切片（阶段历史）

[PRD v0.11](../product/JobForge_PRD_v0.11.md)/[ADR-0019](../adr/0019-executor-confirmation-and-exit-contract.md)已通过PR #44两份独立审查及七项CI并合并，定义C2到真实IPC的观察确认、关闭后失败和计量收尾接缝。按以下顺序推进，保留正式机制与真实模型验收的区分：

1. C3a同步内部v2源schema/两端codec/fixture/C2 hooks，验证明确ACK及唯一顺序，见[证据](../evidence/agent-v3-s1c3a-ack-2026-09-16.md)；不新增兼容双模式。
2. 固定Linux guardian/step supervisor与Go单并发Worker可在接口冻结后并行，实现唯一lease/RPC所有权、取消、Kill/Wait、结果屏障。
3. 用真实PG/gRPC/正式进程和合成HTTP故障服务验证许可、失联、计量及Commit窗口，补固定镜像/Windows运行入口。
4. 再接`support_fixed_v1`真实业务、方案schema/来源/模板、版本化数据/索引、provider审计持久桥和冻结评分，运行DeepSeek的40例有界批次。

前三项只验执行机制，不能替代第四项的真实模型/业务结果。Claim的跨队列trace来源、provider身份持久审计、S2动态Agent、S3恢复对比及S4/S5仍按各自范围实现，不借模块通过提前完成。

## C3b 之后的最小切片（阶段历史）

1. [PRD v0.12](../product/JobForge_PRD_v0.12.md)/[ADR-0020](../adr/0020-provider-audit-and-batch-stop.md)先审查 provider 持久报告、定价资格、普通确认汇合、原 session 晚到权限、只读查询及基于已有调用事实的批次停发合同。没有独立审计调度器或新增收费额度。
2. 按已接受 ADR-0018 实现 support_fixed_v1 条件图、六字段模型输入输出、八字段持久方案与来源/模板；独立 Agent 审查开发标签，发布消除注释歧义的新政策/索引版本，并冻结评分器。另一条实现线在 ADR-0020 接受后补齐审计桥、正式 profile/manifest 和 SDK 串行驱动。
3. 先完成确定性 PG/HTTP/实际进程故障及生产隔离，再核对当时官方价格/账号事实，在既有唯一共享 5 CNY、6h、容量 1 的批次中执行全部 40 案。未知费用、未确认报告或外部阻塞时保留所有未尝试行并停止收费；不另建额度绕过，也不以替身完成 C-07。

完整 Trace、生产长期留存与 S2～S5 仍单列。C3b 的合成供应商、真实步骤恢复和八项 CI 不替代真实云端业务基线。

PR #47已合并接受ADR-0020（d449bc7，两份独立复审及八项CI通过）。support固定流程、开发数据v2独立审查与真实索引/检索已执行，分层结果见[新证据](../evidence/agent-v3-s1-support-2026-09-16.md)；完整评分冻结和40案云端仍未完成。

## 首批启动接缝合同（阶段历史）

[PRD v0.13](../product/JobForge_PRD_v0.13.md)/[ADR-0021](../adr/0021-first-cloud-batch-admission-and-launcher.md)随 **PR #49 独立审查通过并合并时接受**，定义可信 profile 生成/登记、Capture 后的资源接纳校验，以及单 Worker/40行 SDK 驱动的[精简实施映射](agent-v3-first-cloud-batch.md)。接纳后原版本/hash 不变，新 Retry 仍受同一冻结资源约束，已接受回执优先返回；awaiting_approval 是S1方案完成而非业务写入。该合同不改变已接受的5 CNY/6h/停批合同，不表示audit实现合并或收费前置已完成。

当前新增批次适用 PRD v0.14 / ADR-0022 的累计授权，原批不恢复。新批次已知 48,653 microyuan、未释放 hold 2,105,344 microyuan；HTTP 200 响应头后正文不完整，无法确认 usage。即使算术上仍有额度，必要持久确认不完整也不得换批继续；保留全部失败、未知项与未尝试行，等待外部缺口解决，不追加收费探针。
