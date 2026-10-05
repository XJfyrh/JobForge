# S3 步骤恢复：有限真实模型验收

2026-10-04，固定 **11 项**全部执行并获独立接受：**5 个真实故障接管机制通过；8 个完整方案中 7 个业务通过、DEV-035-C 业务失败；3 个对照运行按计划取消（H0）**。十一项来源/协议/安全通过，没有业务写入。[PR #58](https://github.com/XJfyrh/JobForge/pull/58)交付，最终 head 的[8 项 CI](https://github.com/XJfyrh/JobForge/actions/runs/37196767188)通过。

## 范围与结果

同一冻结 schema 3 profile、batch、模型/提示/数据/索引/评分器，累计上限 5 CNY / 6 小时 / 11 Runs。前三对为 DEV-002/027/035 的恢复 C、取消 H0、从头 H1，另有 DEV-002-F03 和 DEV-035-F07。首轮运行前七项，受控续执行仅完成原 ordinal 8–11，不重跑或替换案例、不重置预算。

| 项目 | 实测 |
|---|---|
| 接管机制 | DEV-002-C/F06、DEV-027-C/F05、DEV-035-C/F05、DEV-002-F03、DEV-035-F07 全部通过 |
| 前缀/重做 | 已提交前缀无重放；未提交模型步骤用新 physical ID/许可/费用重做，原 known 保留 |
| 业务 | 8 个完整方案中 **7 通过、DEV-035-C 失败**：方案遗漏必需说明，并包含证据不支持的判断（`REQUIRED_CLAIM_MISSING` / `UNSUPPORTED_CLAIM`）；恢复机制通过另计 |
| 安全/来源/协议 | 11 项通过，事实/权限前后摘要一致 |
| 调用/费用 | 50 chat、139 physical，known **118,126 microyuan（0.118126 CNY）**；held/unknown/anomaly 0 |
| 运行状态 | 原评分时点 8 个 awaiting_approval、3 个 cancelled；收费执行与控制服务已停止 |

known 是冻结声明价与 observed usage 的保守计算值，**不是供应商结算发票**，包含故障前、未提交重做与 H0 费用。未转用 S2 余额，未 thaw/退款/新建替代 batch。

## 故障与耗时

F05 丢失真实 Commit ACK，F06 在 ACK 正常返回后、下一许可前停止 Go Worker，F03/F07 在持久报告/观察已确认而 step 未提交时杀进程；F07 明确 ACK 已入普通管道、Python 未消费。实际 signal、Wait/EOF/Join/组消失与自然控制扫描共同证明接管，未改数据库时间或缩短生产 TTL。

| 自然接管 | DEV-002-F03 | DEV-035-F07 |
|---|---:|---:|
| Kill→原 attempt 关闭 | 28.935962s | 29.785749s |
| 关闭→新 Claim | 10.505058s | 14.987568s |
| Kill→新 Claim | 39.441020s | 44.773317s |
| Claim→方案结束 | 6.412539s | 10.677748s |
| 整个 Run | 67.279806s | 71.560235s |

故障前 lease 是 SDK 观测样本，非最后 heartbeat；首个新 step 时点是 Commit，非执行开始。自然 30s lease、5s heartbeat、180s attempt/退避/session 保护保持，少量开发样本不构成生产 SLO。

## 成本对照

| 案例 | C chat / known microyuan | H0+H1 chat / known microyuan | 结论 |
|---|---:|---:|---|
| DEV-002 | 4 / 9,404 | 7 / 8,715 | 少 3 chat，金额高 689 |
| DEV-027 | 5 / 14,319 | 6 / 12,846 | 少 1 chat，金额高 1,473 |
| DEV-035 | 6 / 21,022 | 10 / 24,190 | C 业务失败，不作收益比较 |

前两对缓存命中不同，调用减少没有带来金额减少；不外推总体节省或性能收益。

## 实际失败与报告修正

首轮在第七项停止，外部 driver 错把已提交且完整审计的纠正标记当成不完整案例。只修复工具取证，生产 Worker/guard/SDK、提示、gold 和 **DEV-035-C 业务失败均未改变**。首次续执行又因 preflight 重复校验导致 PG 快照超过 120s 门槛，在 Submit/Worker 前退出；另一次准备遇 stderr 文件名冲突也零提交。原失败目录/报告保留，最终只执行剩余四项。

第一份合并报告用错 business-before 时间，首七项 safety 为 unverified。只读核验四份业务/权限摘要一致后，改用原首轮 before 和续执行 after 覆盖完整窗口；原错误报告保留、调用新增 0。准备中旧五条等待审批 Run 自然 deadline 到期，只追加原 Sweep 状态/事件，不改原步骤/调用/预算或评分。

## 证据

[机器摘要](agent-v3-s3-cloud-2026-10-04.json)保留全部逐行结果、计量、分离时点、初次失败和长哈希。原 SDK/PG/outbound/模型正文/审批材料位于仓库外私有目录，最终执行容器实际退出 0、PID 0、清理错误为空，旧库/卷和失败未删除。

[免费检查](agent-v3-s3-free-2026-10-04.md)、[恢复指南](../agent-v3/recovery.md)及[原提交完整报告](https://github.com/XJfyrh/JobForge/blob/ef0cab8523470f3c9ced77f683a8b14ac3778ab3/docs/evidence/agent-v3-s3-cloud-2026-10-04.md)分别提供机制、操作和深层材料。当前下一步/共同限制统一见[状态页](../status.md)；本次只验收有限开发样本的恢复机制与实际模型结果。
