# S3 免费实现与故障检查（2026-10-04）

本轮实现[已接受恢复合同](../adr/0025-confirmed-step-recovery.md)，本页仅记录免费机制检查。真实模型 D4 是 S3 完成的必要步骤；现已按两轮具体放行完成原固定十一项并获独立验收，结果另见[云端证据](agent-v3-s3-cloud-2026-10-04.md)。免费合成响应的检查边界保持原样。合同 PR [#57](https://github.com/XJfyrh/JobForge/pull/57) 以 `1594bd0179c37e44093d7d4df371806c634356fe` 通过八项 CI，合并为 `30cc7b6b73f8ddc22ca8bf4e3d4c6184a7768d57`；本功能分支从该 main 开始。运行时与实验准备见[恢复指南](../agent-v3-recovery.md)。

## 已运行的定向结果

环境为 Windows PowerShell、Docker Desktop Linux、开发控制 PostgreSQL 16；测试各用例使用独立数据库，同一 DSN 同时只有一个可能清理库的进程。正式安装的 Go Worker/固定 Python guardian/step、真实 PG/TCP gRPC/SDK HTTP 与清理屏障是真的；业务、embedding、供应商响应均为专用测试 fixture。它们不证明真实 DeepSeek、业务检索质量或云端节省。

仓库外原日志位于 `E:/JobForge-notes/2026-10-04-agent-v3-s3`，失败文件不被后续通过覆盖。实际命令各自记录退出码，不能用 PowerShell 最后一个成功命令掩盖前面失败。

| 定向检查 | 结果 | 原始日志 |
|---|---|---|
| 真实 PG 关闭/审计、完整九字段、legacy 混合 profile、连续序号、并发 Claim/Reserve/旧报告、迁移；已安装 SDK 真实 HTTP | 通过，28.675s | `recovery-pg-04.log` |
| 实际 step 许可前/完整确认后、guardian、Go Worker 死亡；发送前 unknown 和自然同 principal session 保护 | 五模式通过，230.82s | `natural-loss-01.log` |
| Commit 真实落库后 ACK 丢失及下一 BeginTool 前停机 | 两模式最终通过；38.45s / 38.68s | `natural-boundaries-01.log` 的 `commit_ack`；`after-commit-repair-01.log` |
| 原固定 180s attempt 截止，心跳继续维持 lease，真实组清理后自然恢复 | 通过，190.09s | `natural-boundaries-repair-01.log` |
| 连续四次实际 step 丢失、原自然 30s lease 和 1/2/4s 退避 | 通过，131.66s；仅三个 ordinal proof | `natural-boundaries-01.log` 的 RecoveryLimit |
| 外部真实代理/Supervisor F06/F07，经正式 Worker、真实 PG/gRPC、自然恢复和完整后续 | 通过，总 76.39s | `external-supervisor-03.log` |
| S3 inspector 缺 migration 26 拒绝、二 principal 各自的 startup/session 历史 | 通过，1.654s | `recovery-inspect-02.log` |
| 免费工具恢复分配新 IDs、三层 counters/known/held 保留；3 scope×3 chat/token/费用边界 | 通过，22.157s | `recovery-budget-01.log` |
| 原 batch 锁内的实际 SELECT 执行计划比较 | 通过，2.965s | `recovery-queryplan-01.log` |
| Windows SDK/Agent/探针/开发评分/S3 全套 Python | 1780 passed、12 skipped，126.18s；skip 不计 Linux 验收 | `python-full-01.log` |
| S3 清单、单 Submit、失败 Wait 后导出、配置/profile/batch 漂移、物理子调用归类与分离计时报告合同 | 22 passed；Linux mypy 八文件通过 | 本会话逐命令输出，CI 同步执行 |
| 代理真实 loopback 转发鉴权/成功上游、准确下一许可与原窗口、原子且独占完整 JSON 发布 | Go race 通过，2.813s（最终增量随全仓门禁） | 本会话定向输出 |

F06 外部链路在首次 search_policy 的成功 Commit ACK **正常返回后**，阻断下一完整相同 step 的 Reserve，实际杀死 Go Worker、Wait 并确认原组消失；自然恢复 31.45s，已提交前缀无重发。F07 先 SIGSTOP 实际 step，再释放原持久 Observe ACK，实测普通管道 948 字节；未读取 ACK，`python_ack_consumed=false`，实际杀 step 后确认组消失并 Wait Worker，自然恢复 35.14s。入管不称 Python 已消费。

## F01–F17 证据映射

“复用”指原真实事务/进程检查仍被本轮适用全套执行；其中原测试使用手工时钟边界的部分不被改称自然恢复。最终全套结果另列，定向通过不提前代表整个 CI 通过。

| ID | 新增/复用证据 | 验证边界 |
|---|---|---|
| F01 | 新 NaturalLoss `step_before_permit` | 新 runtime、实际 step Kill、HTTP 0、自然恢复待执行原 step |
| F02 | 新 `reserved_before_send`、ReservedFreeTool；复用 LostReservationACK | chat 已占次数且 unknown/full hold，实际发送 0、后续阻断；免费工具新许可/IDs 保留旧未报告记录 |
| F03 | 新 `step_after_confirmed` / `worker_after_confirmed`、ConfirmedPendingAndSuccessorCommit | 原完整 report/observation/结算与关闭 proof；新 call/费用/Commit，继续两步仍可核验 |
| F04 | 新 ProofCannotRescueIncompleteAudit；复用 ProviderAuditExecutorConfirmationWindows | 缺 report/observation、错误观察、冲突/超预留均拒绝；真实提交/ACK 窗口另有既有联合层 |
| F05 | 新 CommitBoundary `commit_ack`、RecoveryCommitACKUncertainty | 原 Commit 一次，有界只读确认；不发下一许可，自然接管已提交前缀 |
| F06 | 新 CommitBoundary `after_commit_ack`、ExternalProxyAndSupervisor/F06 | 正常 ACK 后下一许可前实际停机；无前缀重发 |
| F07 | 新实际 step 许可前/完整确认后 Kill、外部 F07 | 真实 guardian/step 行为、实际 queued ACK 与 consumed=false；协议负例由新 runtime 专测和协调器合同限制 |
| F08 | 新 `guardian_after_confirmed`；复用 process-check | guardian 死亡而 step 在组中，实际 Kill/Wait/EOF/Join/组消失 |
| F09 | 新 `worker_after_confirmed` / `reserved_before_send` / 外部 F06 | 实际 Go SIGKILL、FD 3 EOF、原组消失、自然租约接管；unknown 不放行 |
| F10 | 新 NaturalLoss、CommitBoundary、RecoveryLimit | 实际 30s lease、1/2/4s 退避、新 attempt/fence；同 principal 保护自然结束，不 UPDATE 计时 |
| F11 | 新 ConfirmedPendingAndSuccessorCommit、ConcurrentClaimReserveAndOldReportReplay | 旧报告在新 active call 存在时真实重放；旧 Observe/Commit/Heartbeat 被拒，新状态与预算不动 |
| F12 | 新 Go/Python recovery vectors、schema/version/source prepare；复用输入/manifest/资源 mismatch 合同 | 新 policy/hash/runtime 必须匹配；旧定义/历史读不升级。真实云端资源层未运行 |
| F13 | 新 ExecutorAttemptDeadline；复用 StopPrecedence、ClaimRechecksDeadlineAfterAccountContention、CancelWinsBeforeCommit | 至少一例实际 180s；精确取消/总期限/锁等待用原事务检查，不能当自然场景 |
| F14 | 新 ExecutorRecoveryLimit、RejectsMissingGapAndDuplicateOrdinals、MigrationRejectsPartialAndNonRetryProofs | 四次真实 Kill、最多三次恢复、连续序号、第四次 terminal null、重复 close 陈旧 |
| F15 | 新 ReservedFreeTool、DoesNotResetAnyScopeAtChatTokenOrCostBoundary、ProofCannotRescueIncompleteAudit；复用 ledger/audit freeze | 原累计费用/次数/hold 不重置、无退款/thaw；冻结与 incomplete 审计阻断 |
| F16 | 新 LegacyPendingCallCannotUseNewProfilePolicy、EveryProofIdentityFieldIsRequired、ConcurrentClaimReserveAndOldReportReplay | schema 1/2 原 call 无 S3 豁免；九字段/序号与交叉 step 拒绝；同 batch 真实并发串行权限 |
| F17 | 复用 CheckpointDuplicateConflictAndExpiredAuthority、终态事务/协调器 confirmation | 相同内容 live lease 幂等、STEP_CONFLICT、旧 lease 陈旧；只读终态确认不发新步骤 |

## 查询与锁等待

`TestRunRecoveryQueryPlansUnderOriginalBatchLock` 在一个实际恢复 Run、两个原/新 chat、两个 attempt 的小 fixture 上，持有原 batch 行锁，读取生产 SELECT 原文并执行 `EXPLAIN (ANALYZE,BUFFERS,FORMAT JSON)`。同投影旧 attempt 限制 join、S3 join、恢复历史查询执行时间分别为 **0.527ms、0.544ms、0.336ms**，规划时间分别 2.100/2.145/0.637ms；计划以索引与 Nested Loop/小内存 Sort 为主，无临时 spill。样本很小且缓存命中，不代表吞吐、生产规模、尾延迟或 SLO，也不声称性能改善；未增加索引或扩做旧队列 benchmark。

上述 EXPLAIN 自持有账户锁，**不是锁等待验收**。实际竞争由新 ConcurrentClaimReserveAndOldReportReplay、MigrationLockTimeoutLeavesNoPartialSchema 与复用 ClaimRechecksDeadlineAfterAccountContention / LedgerReservationRechecksBatchAfterAccountLockWait 覆盖。migration 用真实 FOR UPDATE 阻止 AccessExclusive，实测等待原 2s lock_timeout 返回 55P03、没有半列/半 schema version，释放后再 up 保留原 checkpoint。新许可锁后重新采样数据库时间，审计/关闭仍按 Run→账户→call/attempt→slot 顺序，不锁其他 Run。

## 失败与后续检查

保留的失败包括：初次安装缺 hatchling（后续单测成功不能掩盖该失败）；初次测试 fault 目录不可写；after_commit_ack 错把 BeginTool 前屏障当作已有 child；attempt 表字段名错误；两次外部测试启动命令路径/PowerShell 参数错误；inspector 测试恢复 schema_migrations 缺 name；Ruff 格式与导入错误。均按实际原因修复、定向验证，原日志保留。

Windows 全仓 `go test -race ./...` 已通过，真实PG集成包耗时439.558s（`go-race-full-01.log`）；平台专用skip不计Linux验收。Linux process 全套首次在并行构建负载下两项一秒满队列断言失败，等待所有重构建/race结束后，两项实际重跑通过（4.54s，`process-repair-01.log`）。Linux Python全套首次1365 passed、2 failed；隔离后四个相关IPC tail用例全部通过（1.03s，`python-ipc-repair-01.log`）。这两个原全套失败保留，不改称首次全套通过。

正式Linux schema3协议坏帧/普通片段/计量片段/stderr超限四模式通过（11.37s，`protocol-negative-01.log`）；协调器Linux全套与生产镜像边界通过。默认联合层 `integration-full-01.log` 的自然恢复/原正式执行器与launcher等检查通过，但SDK新HTTP脚本未复制到镜像而失败，整轮退出1，不计全套通过。补齐镜像路径后，该实际安装SDK/PG HTTP合同通过（3.48s）；外部F06/F07增量含信号/Wait/组消失独立时间通过（76.12s），日志 `integration-repair-02.log`。新增S3合同在相同Linux镜像实际22 passed（0.88s，`recovery-python-linux-01.log`）。没有修改生产 lease/deadline 或放宽失败断言。

功能草稿 [PR #58](https://github.com/XJfyrh/JobForge/pull/58) 的 `a59bc6f1f807e1e2f18770646225495a726273cc` 已通过[完整八项CI](https://github.com/XJfyrh/JobForge/actions/runs/37191872335)。初次Python job因新测试未设置Agent源码路径而失败，已补显式 `PYTHONPATH`。原初次联合与CI失败保留，已通过且未变化的分段不反复执行。

最终续执行工具 head `782db17b9eda6af23db3b2b414c81cf844e6bbcf` 已通过[八项强制 CI](https://github.com/XJfyrh/JobForge/actions/runs/37195154303)，包含完整 Linux 真实进程/race、真实 PG 联合与已安装 SDK HTTP。外部工具的 Linux 实际定向测试 63 passed（1.56s，`cloud-v1/continuation/python-linux-04.log`），Linux 平台 mypy 九文件与 Ruff 通过；Windows 62 passed/1 Linux CLI skip 单列。新增纠正标记/续执行检查覆盖原材料字节与身份、实际 PG attempt/关闭状态、账户/known/held/freeze 漂移、自然审批到期、Submit 不确定性不重试、唯一完整入口 preflight，生产源码不变。

D4 实际官方 metadata/价格、保留业务数据/索引核验后，首轮七项中断，具体续执行放行后只完成原四项。全部原失败、零提交失败、自然等待到期和报告窗口修正保留，最终真实十一项结果见[云端证据](agent-v3-s3-cloud-2026-10-04.md)。S3 机制与有限真实模型验收已独立接受，最终代码交付、文档审查及最新 head CI 以 [PR #58](https://github.com/XJfyrh/JobForge/pull/58) 记录为准；S4/S5 未开始。无 S2 余额转用，历史 W4、AT-25、远程模型与生产长期留存结论不改写。
