# 已确认步骤恢复

[PRD v0.17](../product/JobForge_PRD_v0.17.md)与[ADR-0025](../adr/0025-confirmed-step-recovery.md)定义 schema 3 的持久恢复。`support_agent_v1` 显式声明 `confirmed_uncommitted_v1`，固定 `linux-v2-recovery-runtime-1`；旧 schema 1/2 不借用新豁免。当前验收见[状态页](../status.md)与[S3 报告](../evidence/agent-v3-s3-cloud-2026-10-04.md)。

版本化 profile schema 与共同恢复向量见[源契约](../../api/support/recovery-v1/README.md)。

## 原关闭事务与新执行权

已提交的 `run_steps`、cursor、hash 链与事件继续由 CommitStep 在原事务提交。原 attempt 按可恢复分类关闭时，先锁原 Run，再按 family/tenant/batch、call/attempt 和 slots 的既有顺序清理；schema 3 同事务保存待执行 step 的完整九字段与连续恢复序号。unknown 或 frozen 不阻止关闭清理，也不释放旧 hold。其他关闭与 schema 1/2 的两个新字段均为 null。

新 Claim 仍需自然 lease/Sweep/1、2、4 秒退避及合法 session；最多三次恢复。取消、Run deadline、attempt deadline 的优先顺序和 30 秒 lease、5 秒 heartbeat、180 秒 attempt 保持原值。新 attempt/fence、owner/session 在同一事务授予，不能从旧 Worker 的内存 cursor 继续。

batch guard 在原账户锁内只读其他 Run/attempt，避免反向 Run 锁。豁免必须由原 call 的不可变 profile 决定：原执行身份、step、binding、完整 known report、accepted observation、结算、关闭时间和连续证明全部匹配，且原账本没有 unknown、异常、冲突或冻结。未提交 chat 只豁免同 Run 当前待执行 step；其他 Run 仍被阻断。

恢复必须重新调用该未提交模型步骤，使用新物理 ID、许可和费用。新 Commit 指向真正的新 call；原 call 不能充当 checkpoint。之后 guard 核验完整相同逻辑 step 的新 attempt 提交和新调用审计，因此 cursor 再前进仍能验证旧证明。family/tenant/batch 的次数、known、held 与费用累计不重置。

## 实际进程丢失

S3 协调器只把实际 signal 或 guardian 的既有退出 72，结合 Wait、普通/计量 EOF、reader/writer Join、stderr 上界和组消失，认作执行丢失并停止 Run 续租。已有领域拒绝、坏帧、残片、大小、身份、unknown 或缺少清理事实不能被该分类吞掉。未提交结果不伪造 Fail/Commit，由正式控制扫描自然关闭。

Commit ACK 不确定只允许原身份/hash 的有限 GetAcceptedCommit 查询，总计最多两次/两秒；无论 Found 与否，Worker 放弃本次执行，不能发出下一步许可。新的 Claim/GetCheckpoint 以数据库提交事实复用前缀。原结果、usage 报告和晚到结算各自保持权限边界；旧 Observe/Commit/Heartbeat 不得修改新 active call 或 cursor。

## Migration 与复现

[0026 up](../../migrations/0026_attempt_recovery_proof.up.sql)仅添加 nullable 证明对和 CHECK，无历史回填；原迁移不修改。DDL 用事务内 2 秒 lock_timeout。锁超时整次回滚，release 后可重试；真实 PG 用例核验无半列/半版本状态。[down](../../migrations/0026_attempt_recovery_proof.down.sql)只允许尚无任何证明时回滚；已有证明应前滚修复，禁止删除证据后 down。

Windows PG/SDK 前置、固定 Linux `--init`、专用环境开关和同 DSN 串行见[测试指南](../tests.md)。`TestRunRecovery` 验证自然 30s lease、180s attempt、同 principal session 保护与三次恢复；平台 skip 不计 Linux 通过。专用测试 hook 不进入生产镜像。

## 查询与实验工具

S5 使用 `tools.support_recovery.plan --s5` 生成 plan schema 2，绑定最终 schema 7 候选、原十一项故障/对照意图及持久共享 chat≤132、费用≤¥4的实际批次上限。到方案保存后待审批或 no_action 结束，不批准业务写入；S3 的 plan 1 与历史报告保持原语义。具体版本边界见 [ADR-0028](../adr/0028-versioned-support-evidence-navigation.md)。

使用 `RunClient.get/steps/events/calls/result` 读取状态、已提交前缀、attempt 与原调用/费用；恢复不创建人工 retry 后继。终态方案导出与后续审批等待到期状态分别保存，不改写评分时点结果。

[有限实验工具记录](../archive/s3-experiment-tools.md)说明固定代理、Supervisor、单 Submit、分离计时与受控续执行；它只适用于已完成的十一项历史实验。新实验需重新冻结来源、预算和实际授权，不重启原 attempted 批次。
