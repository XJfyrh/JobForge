# JobForge PRD v0.17：S3 持久步骤与故障恢复

- 日期：2026-10-04（Asia/Shanghai）；状态：**Accepted（独立方案审查通过，随本决策 PR 合并生效）；实现与验收另行记录**。
- 对应：[ADR-0025](../adr/0025-confirmed-step-recovery.md)、[实施与验收计划](../plans/agent-v3-s3-recovery.md)。
- 起点：`main@e2b9bad`（[PR #56](https://github.com/XJfyrh/JobForge/pull/56)）。S2 冻结40案业务37/40、安全40/40完整且硬失败0；不重做S1/S2。

## 产品结果

正式动态售后 Agent 在 Go Worker、Python step 或 guardian 丢失后，经自然租约回收，由持有新执行权的 Worker 从 PostgreSQL checkpoint 继续。已提交步骤不重新调用模型或工具；未提交步骤只能在审计与预算条件允许时重新执行，使用新的调用身份并可能再次计费。交付保证仍为 at-least-once，不承诺函数内部续作、确定性模型重放或供应商请求恰好一次。

SDK 能查询原 Run 的持久步骤、方案、attempt、恢复次数、物理调用与 known/held 费用。提交方案进入 awaiting_approval 仍只表示待审批，不执行批准或业务写入。

## 范围与合同增量

1. 复用 `runs/run_steps/run_attempts`、CommitStep/GetCheckpoint、唯一 Run 扫描器及正式 `runworker/runexecutor`，继续 `support_agent_v1` / `support-agent-v1`。没有第二调度器、影子任务、通用工作流框架或清 checkpoint 开关。
2. 新 S3 不可变 profile 增加版本化恢复 policy，精确放宽“成功响应、计量及普通观察均已持久，原步骤未 Commit，原 attempt 已持久安排恢复”的窗口。原调用必须使用自己的持久 profile 判断政策；当前 S3 请求不能放宽同 batch 的历史 S1/S2 调用。
3. 原 attempt 保存关闭时的完整待执行 StepIdentity 与恢复序号，关联原 call 的 execution binding。该证据不能依赖后来会推进的 `runs.next_step_*`。放行只用于同 Run 的原待执行步骤；后续步骤与下一案例须能核验该步骤在新 attempt 的实际提交。旧 call 不被伪装成 checkpoint，不删除旧审计或费用。
4. 新 profile 下，实际清理已完成、无坏帧/尺寸/身份等错误事实的非正常进程退出，仅作为执行丢失，停止 Run 续租并交自然 lease 回收。信号/退出72不能被猜成 provider 失败；明确协议错误、未知或未确认计量、冻结及 STOP 保留原停止优先级。
5. profile/snapshot/模型/工具/提示/语料/索引固定。资源不可得或版本不匹配明确拒绝，不静默升级恢复。新 profile 使用新的固定 executor_version；旧定义/hash不改写。
6. 每次 Claim 在一个事务中安装 owner/session/lease/attempt/fence；DB时钟判定到期，旧 owner/token 即使有结果仍返回 STALE_LEASE。步骤、游标与事件原子提交；取消、Run deadline 优先于 attempt timeout 恢复。
7. 最多三次恢复、1/2/4秒退避、三层次数/token/费用额度及全 Run 一次纠错不重置。新物理发送需要新 physical_call_id；重做读工具需要新 tool_invocation_id。未知 chat、报告/观察不完整、冲突、provider 异常、超额或冻结不获恢复豁免，full hold 不记零、不退款、不解冻，不自动换 batch 继续。

具体持久字段、锁序、谓词及兼容性以 ADR-0025 为准。历史 ADR 正文与 S1/S2 profile 的保守语义保持，契约接受不能代替实现证据。

## 验收

| ID | 必须证明的结果 |
|---|---|
| S3-01 | 真实 PostgreSQL + 正式 Linux Go Worker/guardian/step 在发送前、观察后 Commit 前、Commit 落库 ACK 丢失、Commit 后下一步前等窗口恢复或明确停止；每个窗口的预期见实施计划 |
| S3-02 | 实际 Kill/Wait、EOF/Join/组消失与自然 lease/Sweep/Claim；至少有正式30秒lease的自然恢复证据，不手工缩短 lease/session/退避冒充通过 |
| S3-03 | 数据库步骤/游标/hash、真实 HTTP 调用计数及安装 SDK 查询共同证明已提交前缀不重跑；未提交重复调用全部计入账本 |
| S3-04 | 旧 owner/token、旧 call 的普通观察/提交晚到拒绝；合法原 usage 窄补报不推进游标、不清新 active_call；相同 hash 重复、STEP_CONFLICT 与 yield/终态只读确认仍符合既有合同 |
| S3-05 | 原 profile 政策、持久恢复绑定、并发 Claim/Reserve、取消/各层期限、三次恢复及三层额度、unknown/full hold/冻结均有正反例；缺事实的执行丢失不能获得新收费权限 |
| S3-06 | 固定少量已开放开发案例，真实主后端、业务 HTTP、MiniLM embedding、pgvector、持久账本及 SDK；checkpoint续跑与独立从头重执行控制使用相同模型/提示/策略/数据/profile/故障边界，保留模型非确定性与所有失败 |
| S3-07 | 报告完整调用分类、前缀复用、新增/重复请求、known/held、恢复/活跃/总耗时、方案与业务评分；独立审查及最终适用 CI 通过后交付 |

免费机制层可以用明确合成模型输出控制路径，但不能作为真实供应商或业务成本证据。真实云端小样本证明本次实验，不外推总体节省率或重申40案质量。没有减少调用或模型输出失败时如实报告，不通过挑案例、删失败、改 gold 或读 S5 保留集制造收益。

## 费用与退出边界

主规划会话依据维护者自主规划和实施 S3、处理常规技术选择的授权，选定新增累计 **最多5 CNY**、有限6小时窗口与预登记实验清单作为执行上限；这不是用户直接口授的金额。S2 的20 CNY及其余额不转为S3额度，S1/S2所有known/held继续单列。不充值、自动增资或增加无关收费探针。

收费前须通过合同审查、免费工程层、独立实现审查和适用 CI，重核执行当天官方价格/模型、账号与资源，登记新 profile 及有限 batch，并由主规划会话明确放行预登记执行清单。本轮仅完成文档，未发收费请求；尚未执行的真实云端层继续标为未执行。

本阶段不扩展 S4 审批写入、S5 页面/保留集/生产长期留存，不重跑40案、不改业务 gold。历史 W4 性能失败、AT-25 跳过与 RQ-06 未命中不被本次恢复证据覆盖。
