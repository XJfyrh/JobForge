# S3 有限真实模型验收（2026-10-04）

固定十一项 D4 已执行并获主规划会话独立验收：五个真实故障接管机制通过，八个完整方案中七个业务通过，DEV-035-C 的业务失败保留；三条 H0 按计划取消。十一项来源、协议和安全检查通过。50 次真实 chat 的保守账本 known 为 **118,126 microyuan（0.118126 CNY）**，held/unknown/anomaly 均为 0；这是冻结声明定价与 observed usage 的计算值，不是供应商结算发票。没有业务写入，收费执行及 S3 控制服务已停止。

[公开机器摘要](agent-v3-s3-cloud-2026-10-04.json)只包含脱敏结果、计数、时点与材料哈希。原 SDK、PG、outbound、模型正文、失败和审批材料留在仓库外私有目录 `E:/JobForge-notes/2026-10-04-agent-v3-s3/cloud-v1`。合同见[恢复指南](../agent-v3-recovery.md)，免费检查见[分层证据](agent-v3-s3-free-2026-10-04.md)。最终代码交付、文档审查及最新 head CI 以 [PR #58](https://github.com/XJfyrh/JobForge/pull/58) 记录为准；S4/S5 未开始。

## 固定范围与两轮来源

原 batch `bb5e8274-8b36-4fb9-96f4-16b8bf77d7d7`，profile hash `39b0d98e7ddccbaa48640e500e6c9073b40704cc703104c2dbfa3db08216db74`，窗口 2026-10-04 09:25–15:25 UTC，累计上限 5 CNY、11 Runs。原清单 SHA256 `15149402b0416ef0cd69f1823a344c053af9082dc94230aeeadf78f8f936263e`。全过程沿用原 batch、三层账户、profile、价格、模型、提示、数据、索引和评分器，没有重置预算、thaw、退款、新批次或替换案例；S2 余额未转用。

首轮生产源码为 `a59bc6f1f807e1e2f18770646225495a726273cc`，已通过[八项 CI](https://github.com/XJfyrh/JobForge/actions/runs/37191872335)，独立放行原十一项后运行前七项。续执行工具源码为 `782db17b9eda6af23db3b2b414c81cf844e6bbcf`，tree `be7441219f6346dab91ecc9facf84edeb4d82ec3`，通过[八项 CI](https://github.com/XJfyrh/JobForge/actions/runs/37195154303)；生产源码仍为 a59bc6f。续执行只允许原 ordinal 8–11：DEV-035-H0、DEV-035-H1、DEV-002-F03、DEV-035-F07，每个意图只 Submit 一次。原七行和 9,334 个原材料冻结，另存续执行目录，不重跑已执行项。

生产镜像 digest 为 `sha256:8653ca204ef9c1a8fb7340dabdffb418ec86a43e7f83b130cf5b42023daad39a`，首轮外部实验镜像为 `sha256:bb2b5da2d8e8733d463433095498bbbb5acee39bd9e11a5dde0d68b469cf33aa`，最终续执行镜像为 `sha256:81282b0ba39e41c0c7aeb8b60928241e8b11695765cc8d909a151efe5f9f704e`。正式二进制及 Agent/SDK 包摘要保持原值，完整摘要在机器文件中。外部镜像没有安装合成 registry、固定供应商 fixture、gold 或评分器。

实际调用前已 GET 官方模型/余额并核对当天价格和保留业务数据/索引。价格快照 SHA256 `5a7b1832592387340f2fc456399b34b89b05f3fa167c2e35909e2fa4afe021e3`；采用 miss 输入 2、hit 输入 0.04、输出 8 CNY/百万 token 的保守声明价。真实 chat 均为 HTTP 200、compatible/nonthinking、`deepseek-flash`；别名不证明供应商模型不可变。

## 全部十一项结果

“finished”表示实验行已结束。三条 H0 原业务评分为 RUN_NOT_COMPLETED，按设计取消，不进入完整方案的八项分母；awaiting_approval 不表示批准、业务写入或解决工单。

| 实验 | 原执行结果 | 业务评分 | 恢复次数 | chat / physical | known microyuan |
|---|---|---|---:|---:|---:|
| DEV-002-C | awaiting_approval | passed | 1 | 4 / 10 | 9,404 |
| DEV-002-H0 | 有意 cancelled | RUN_NOT_COMPLETED | 1 | 3 / 9 | 3,374 |
| DEV-002-H1 | awaiting_approval | passed | 0 | 4 / 10 | 5,341 |
| DEV-027-C | awaiting_approval | passed | 1 | 5 / 15 | 14,319 |
| DEV-027-H0 | 有意 cancelled | RUN_NOT_COMPLETED | 1 | 1 / 2 | 732 |
| DEV-027-H1 | awaiting_approval | passed | 0 | 5 / 15 | 12,114 |
| DEV-035-C | awaiting_approval；原 driver 错误保留 | REQUIRED_CLAIM_MISSING、UNSUPPORTED_CLAIM | 1 | 6 / 16 | 21,022 |
| DEV-035-H0 | 有意 cancelled | RUN_NOT_COMPLETED | 1 | 4 / 10 | 5,389 |
| DEV-035-H1 | awaiting_approval | passed | 0 | 6 / 20 | 18,801 |
| DEV-002-F03 | awaiting_approval | passed | 1 | 5 / 11 | 7,925 |
| DEV-035-F07 | awaiting_approval | passed | 1 | 7 / 21 | 19,705 |

首七项 known 66,306，后四项新增 51,820，合计 118,126 microyuan。另有 known tokens 195,187、query embedding 17、metadata 34、业务工具 HTTP 38、protocol correction 1；physical 139 = chat 50 + 17 + 34 + 38。各 scope 累计已核对，held tokens/费用、unknown chat、measurement anomaly 全为 0，batch 未冻结。逐 call usage/定价独立重算与总账相同；包含故障前、未提交重复调用及 H0 费用。

五个接管为 DEV-002-C/F06、DEV-027-C/F05、DEV-035-C/F05、DEV-002-F03、DEV-035-F07，均复用已提交前缀，无前缀重放。DEV-035-C 的机制通过与最终业务失败分别报告。

## 真实 F03/F07 与自然接管

F03 在旧模型调用完整 report、accepted ordinary observation 和结算已持久后、Commit 前，实际 SIGKILL Go Worker，Wait 返回 -9，并确认原进程组消失。旧 call `c5cdd3b7-7f1c-491c-ad0c-b372ab97e7ff` 的 738 microyuan 保留；待执行 step `7e2cb953-d75c-4151-9a5e-c9e666e7ec66` 为 sequence 2、cursor 1、恢复 ordinal 1。新 attempt 2 重新调用同一逻辑 step，以新 call `4f845f24-c880-4188-949a-aa858faedf1a` 实际 Commit，旧 call 未充当 checkpoint。

F07 实际 SIGSTOP Python step 后释放真实持久 Observe ACK，普通管道有 954 字节 queued、`python_ack_consumed=false`，随后 SIGKILL step、确认组消失并实际 Wait Worker。Worker 返回 0 是协调器清理事实，不能充当业务成功。旧 call `dbf55055-7b27-4c7f-ba5f-3fe9e9c39dba` 的 758 microyuan 保留；同 sequence 2/cursor 1/ordinal 1 的 step `5f6231cc-5fa3-4fc1-835a-c444b94516f9` 由新 attempt 2 的 call `ebcf2a72-3b1c-4c55-92f4-9f3806744aa7` 重新调用并 Commit。两例来源、协议、业务及安全检查均通过。

| UTC 时点 / 耗时 | DEV-002-F03 | DEV-035-F07 |
|---|---|---|
| Kill | 10:32:21.330229 | 10:33:23.477987 |
| Worker Wait 完成 | 10:32:21.333649 | 10:33:23.512705 |
| 组消失确认 | 10:32:21.434628 | 10:33:23.497162 |
| sampled lease_until | 10:32:49.936478 | 10:33:52.371087 |
| 原 attempt 关闭 | 10:32:50.266191 | 10:33:53.263736 |
| 新 Claim | 10:33:00.771249 | 10:34:08.251304 |
| 首个新 step 的 Commit | 10:33:02.064652 | 10:34:09.330343 |
| awaiting_approval | 10:33:07.183788 | 10:34:18.929052 |
| Kill → 关闭 | 28.935962s | 29.785749s |
| 关闭 → Claim | 10.505058s | 14.987568s |
| Kill → Claim | 39.441020s | 44.773317s |
| Claim → 方案结束 | 6.412539s | 10.677748s |
| Kill → 方案结束 | 45.853559s | 55.451065s |
| 整个 Run | 67.279806s | 71.560235s |

lease 是故障前 SDK 观测样本，不是最后一次 heartbeat；首个新 step 时点来自 Commit，不是执行开始。原 30s lease、5s heartbeat、180s attempt、自然关闭/退避/session 保护未缩短或以 SQL 改时间。全十一行分离时点在机器摘要中，小样本耗时不是生产 SLO。

## 成本对照

| 案例 | C chat / known | H0+H1 chat / known | C−从头 known | 可比性 |
|---|---:|---:|---:|---|
| DEV-002 | 4 / 9,404 | 7 / 8,715 | +689 microyuan | 两个完整方案业务通过 |
| DEV-027 | 5 / 14,319 | 6 / 12,846 | +1,473 microyuan | 两个完整方案业务通过 |
| DEV-035 | 6 / 21,022 | 10 / 24,190 | 不作收益比较 | C 业务失败，配对不可比 |

前两对 C 分别少 3/1 次 chat，但金额更高。原 token 明细中 DEV-002 的 C miss 输入 3,826，对照 2,911；DEV-027 的 C miss 输入 5,863，对照 5,013。缓存命中差异使调用减少不等于金额减少。DEV-035 的算术差值不能称节省；不从三例开发样本外推总体金额或耗时收益。

## 原始失败、等待到期和报告修正

首轮到 ordinal 7 停止，实验进程退出 1；七行、28 chat、77 physical、known 66,306 和原业务失败保留。DEV-035-C 的 call `51e2167b-3f4a-48cd-b16a-713f6d3a5402` 为 attempt 2 的 model_decision，HTTP 200、known report/observation/结算完整，business=rejected/MODEL_PROTOCOL_ERROR，无冲突异常。sequence 10 已提交 correction_required=true、proposal=null；sequence 11 protocol_correction 和 sequence 12 proposal 已成功提交。原 Go `committedStep` 允许该已提交纠正分支，外部 driver 却要求每次 chat 均 accepted，触发 S3_CASE_INCOMPLETE。只修复外部工具对完整绑定的已提交纠正标记的取证，未提交 rejected 不豁免；生产 Worker/guard/SDK、prompt/gold 和 DEV-035-C 评分均未改变。

准备期间旧五条 awaiting_approval 的 Run deadline 自然到期。主规划会话另行授权仅启动原控制服务，由既有 Sweep 转为 failed/RUN_DEADLINE_EXCEEDED，两条 cancelled 不变，新增费用 0。实际 SDK/PG 核验旧 Step/attempt/Call/Result、身份、cursor 和全部账本不变，仅等待态 state/error/updated_at 及对应事件变化。88 个最新取证文件单列绑定；事后等待超时不计入原执行耗时或改变原模型评分。最终控制快照为旧五条 failed、三条 cancelled、新三条 awaiting_approval；原评分时点的八条 awaiting_approval 另存。

首次受控续执行在任何 Submit/Worker 前因 PG 快照超过原 120s（实测 165.014404s）而退出：main 与 preflight 重复完整校验 9,334 个原文件。保留失败容器 `jobforge-support-recovery-s3-bb5e8274-continuation`、零提交证明及目录，原 7 Runs/77 calls/28 chat、known 66,306、held 0 和 startup 计数不变。只移除 main 冗余校验，保留收费前唯一完整 preflight、原 120s 门槛及 release/source/runtime 绑定。具体准备脚本另一次因 stderr 文件名冲突停止，也未 Submit/启动 Worker，失败文件保留。新源码/镜像/回执重新冻结并经具体放行后，最终入口只运行原四项。

成功容器 `jobforge-support-recovery-s3-bb5e8274-continuation-4` 使用 init、restart=no，实际退出 0、PID 0，二次清理错误列表为空。原首轮和失败续执行容器保留，S3 control/executor 均已停止，旧数据/卷/历史材料未删除。

第一份合并报告误用了续执行前的 business-before 时间（晚于首七项 outbound），使首七项 safety 为 unverified；该报告和错误保留。只读核验四份 before/after 的业务事实、权限及数据库摘要完全相同、仅 observed_at 不同后，最终报告改用原首轮 before 和续执行 after 覆盖完整窗口。评分器、导出、模型结果及原错误不改，新增调用 0。最终十一项 safety passed；这是报告输入修正，没有模型重跑。

## 材料哈希与审批链

两轮 release、规划放行原文和来源绑定分别保留。最终续执行 release 绑定固定四项、782db17 工具、原 a59bc6f 生产镜像和未重置账本。原文不公开，完整材料哈希、二进制和包摘要见机器文件；关键锚如下：

| 材料（相对私有 cloud-v1） | SHA256 |
|---|---|
| 原 `release.json` / `planning-release-message.txt` | `7a184379fed51533095ade91db6730cbd10ff55539d9195d33d5612b89a9a779` / `9f577d1d3452b21b0c491c602ea44e261c602faa0b09c576664cf007fab34680` |
| 原 report | `42f2b9b91cf4f08629f6f39940827ead99fce9262a99b8d2e2413c447eeba403` |
| 原 control audit / rows | `c19e22979cebb655b858750a3011ebb3e0eb462a85d33d908e72a7b2ec8fd5e1` / `6774fc6401035e97248553989f281e73a45d72f9bd754a49c2f03906d5cf990c` |
| `continuation/failed-launch-proof-03.json` | `18c72785b43e0a0934f598a94faca3d3cf3bb68dae8e47cbd124a2e6f475c4f3` |
| `continuation/manifest-04.json` | `58830be9a34d29489869f0b8c595125d42a0e25df7a6e5e718f836000f5c9974` |
| `continuation/release-04.json` | `9da3d1f43de90543077c32433f79a26803339af93877f86e51e1c18346fc1c58` |
| `continuation/planning-release-continuation-04.txt` | `362df693d192769a1f800a19142828c9171d78878f229e823a5794df950ee7f9` |
| `continuation/combined-report-04.json`（原窗口错误） | `b07ea8dc63df819b2470146b573bc2df129e41ba5cb3e8d49a4ea63284d9c706` |
| `continuation/business-window-verification-05.json` | `089e0df5512b76b56ef83deaa3cb711791add98a45daef4131e8a3dfbdf93fa0` |
| `continuation/final-report-05.json`（最终） | `343a77d729d13bb5f4f14f67d463ffc64bd6fff8fc82ff671171d0709732a6dd` |

本轮完成限定开发样本的 S3 机制与真实模型验收。DEV-035-C 失败、历史 W4/AT-25、远程模型及生产长期留存未验收仍保留；没有启动 S4/S5、审批写入、保留集或新的收费实验。
