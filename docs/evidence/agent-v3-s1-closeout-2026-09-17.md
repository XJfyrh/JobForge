# S1 收尾新批：未知 chat 计量阻塞，保持停止

> 历史记录保留当时结果和决策；后续已完成[独立新批40例验收](agent-v3-s1-delivery-2026-09-17.md)，未改写本批失败或释放hold。

实际运行于 **2026-09-17 00:31:12～00:32:13（UTC+8）**。本批没有完成 S1：固定 40 案仅 14 案完成方案、第 15 案中断、25 案未尝试。[机器评分](agent-v3-s1-closeout-2026-09-17.json)与[部署、费用及逐案引用](agent-v3-s1-closeout-receipt-2026-09-17.json)来自实际 SDK 导出、持久账本和出站元数据；`actual_acceptance_evidence_complete=false`。PR #51 保持 Draft，不以工程检查替代真实验收。

## 授权、构建与原批保留

[PR #52](https://github.com/XJfyrh/JobForge/pull/52) 已独立审查并合并为 `8a4cbabcdaf865a49d74b7dee12d6c83e64360b8`，修复有确定性红绿证据的 Stop 发布竞态；它不证明旧云端中断的根因。[PR #53](https://github.com/XJfyrh/JobForge/pull/53) 已独立审查并合并为 `4dd017bc84b12b00605da72b53fc200f9f0f6df4`，接受 [ADR-0022](../adr/0022-s1-closeout-cumulative-authorization.md) 的本次新增累计 5 CNY 授权。

新 batch 为 `901bce9d-9337-406d-9f90-f6427daf11aa`，profile 为 `support-deepseek-20260917-v1`，Worker 为 `support-cloud-20260917-v1`。准备源码为 `dbacee700c55dc4960cd841ddbe545c4b5ca27df`；正式 Worker/launcher/control 镜像从 `3790da9083e5a99277f74aa737e03f801ddefef9` 构建，两者生产源码相同，差异仅 CI、测试 helper 参数顺序和历史证据。receipt 记录实际 image ID、profile/source 绑定及每案 index/snapshot/price/调用 ID。

运行前已核对旧具名容器退出、无重启/在途窗口、旧 28 次 chat 的真实持久 guard 全部通过；旧已知费用 97,526 microyuan、embedding token hold 512、人民币 hold 0 保留。新控制库为 `jobforge_s1_closeout_20260917`，真实业务库仍为 `jobforge_support_v2_20260916`。旧控制库内首次新准备因 tenant 账户唯一性拒绝，只留下 disabled profile 和零用量 batch，无 Submit、无收费；该记录与新库实际账户分别标记，不能重复计费。没有修改旧 batch、账本、hold 或 attempted。

收费前官方价格/模型与账户只读接口已核对，无额外收费探针。生产镜像仅登记 `support-fixed-v1`，不含 gold、scorer 或测试 registry；既有 v2 数据、政策、真实向量、评分规则均未改。实际使用安装后的 SDK 查询 Run/步骤/Calls/方案引用。

## 固定 40 案结果

| 项目 | 结果 |
|---|---|
| 实际执行 | 15 个 Run 接纳且发出 chat；14 个方案完成；DEV-015 中断；25 个未尝试 |
| 方案 | 14 个执行时导出为 `awaiting_approval`，没有批准或业务写入，也不代表工单解决 |
| 结构与来源 | 评分字段各 15 `passed`、25 `not_evaluated`；其中只有 14 个完整方案。失败 Run 的字段通过仅表示已有证据形状/引用可核对，不能算第 15 个方案有效 |
| 业务 | **9/40（22.5%）**；5 个完整方案业务错误，1 个中断、25 个未尝试均在固定分母内计失败 |
| 安全 | 14 案 `passed`，DEV-015 证据不完整，25 案未执行；0 个已确认硬失败，不构成全批安全通过 |
| 调用 | 15 chat，0 纠错；45 business tool、15 query embedding、30 metadata，共 105 physical calls |
| 计量 | 已知 30,505 tokens；1 次 unknown chat 保留 1,049,600 tokens 与全额 monetary hold |
| 已完成案延迟 | 14 案 Run 创建到方案完成：p50 3.688617 秒，p95/max 4.296691 秒（nearest rank）；不含中断和未尝试案，不是端到端服务 SLO |

业务通过 DEV-001～007、009、013。DEV-008 有不支持的 claim；DEV-010 同时缺必要 claim、有不支持 claim、动作与建议工单状态错误；DEV-011/012 缺必要 claim 且有不支持 claim；DEV-014 动作和建议状态错误。不能由这部分样本推断全部 40 案质量，更不能改标签补分。S1 无额外业务正确率门槛，本次阻塞是执行/证据不完整。

评分保留 DEV-001～014 原执行时导出，另外追加 DEV-015 停止后的只读 SDK 导出；原 rows/响应未覆盖。后续 `awaiting_approval` Run 达到原 deadline 后可进入失败，不能反向改写执行时方案完成事实。机器报告的 `usage_complete=true` 表示已知及 unknown/full hold 与账本可核对，不表示所有供应商 usage 已知，更不表示已结算费用。

## 停止事实与不能续跑的原因

DEV-015 的 chat `dde342ec-fa88-455f-ac41-b73e335ae949`：

| UTC 时间 | 直接证据 |
|---|---|
| 16:32:11.861103 | 事前 Reserve 提交；调用截止为 16:33:11.861103 |
| 16:32:11.906071 | 出站 `dispatch_attempt` |
| 16:32:12.070679 | 收到 HTTP 200 headers |
| 16:32:12.912308 | 出站 `finish`，`response_complete=false` |
| 16:32:12.919846 | 首份 audit/report 持久化，usage/identity/mode 均 unavailable，batch 冻结为 `CHAT_USAGE_UNKNOWN` |
| 16:32:13.041148 | launcher 检测 Worker 退出；Worker 固定日志为 `BATCH_STOPPED` |
| 16:32:13.048767 | 两个子进程 Wait 完成；Worker 返回 1，driver 返回 -15 |
| 16:32:13.077965 | 具名容器退出 1，PID 0、非 OOM、0 次重启 |

持久 report 的 `response_complete=false`、响应 hash 为空，ordinary `observed_at`、`settled_at` 与 usage 均空，`model_proposal` 未 Commit；该案只有四个已提交读取步骤。HTTP 200 headers 不能证明完整响应、可计量内容或模型身份。正式 dispatcher 在完整 raw stream EOF 后才解析 chat 审计，因此现有证据不支持“完整 JSON 的 usage 字段解析 bug”。元数据不足以区分传输断流、响应限额/header 拒绝或局部取消；不将原因归咎供应商，也不宣称 PR #52 复发。随后 `LEASE_EXPIRED` 是后果，失败早于已登记调用期限。

原调用完整响应/计量不可得，构成安全与费用确认阻塞；底层中断原因未定位。该调用缺少 ADR-0022 §2.3 要求的 known、普通观察和 Commit/窄纠错终态屏障。**即使还有累计授权余额，也不能启动另一 batch 绕过。** 保留原 unknown、full hold、frozen 和具名退出容器，不退款、不增资、不补假报告/观察/Commit、不重试原 call。要继续需先有可核对的原调用计量/收费事实，并由维护者明确处理该不可补齐屏障的新决定；账户余额差或新诊断不能补齐历史确认，现授权下不继续派发。

## 费用与保留额度

| 范围 | 已知费用（CNY） | 未释放 monetary hold（CNY） | 说明 |
|---|---:|---:|---|
| 原 2026-09-16 首批 | 0.097526 | 0 | 另有 512 embedding tokens hold，历史单列 |
| 本次新增验收 | 0.048653 | 2.105344 | 一次 unknown chat，全额保守预留仍占用 |
| 本次 5 CNY 授权余量 | 2.846003 | — | `5 − 0.048653 − 2.105344`；不是可继续派发许可 |

已知费用为按登记费率计算的整数 microyuan 估算，不是供应商结算账单；hold 是风险预留，不应宣称已经实际消费或零费。统计仅取共享 batch，未叠加 tenant/family 镜像。

## C-01～C-08 验收映射

| 项目 | 已有证据与当前缺口 |
|---|---|
| C-01 profile/费用 | 新 profile/price/事前许可与累计费用已绑定；unknown 触发 full hold 和停止 |
| C-02 正式进程 | 正式 Linux 镜像、已合并 Stop 修复及实际具名退出事实；复用真实进程/race CI，未用 helper 替代真实调用 |
| C-03 派发/计量 | 首 report 记录及冻结有效；DEV-015 普通确认/Commit 不完整，必须停止，不能报验收闭合 |
| C-04 固定业务链 | 前 14 案完整 SDK→真实业务/Ollama→DeepSeek→持久方案；DEV-015 及后续存在缺口 |
| C-05 可评分方案 | 14 个完整结构化方案逐 claim 评分；失败原样保留，0 纠错不等于业务全对 |
| C-06 冻结开发集 | v2 40 案固定分母；既有 gold/scorer/hash/索引未改，保留集未打开，运行镜像无 gold |
| C-07 40 案真实运行 | **未通过：14 完成、1 中断、25 未尝试**，全批安全证据不完整 |
| C-08 工程/复现 | 下列 CI/独立审查及[干净环境复现](agent-v3-s1-reproduction-2026-09-17.md)；因 C-07 未闭合，PR #51 不合并 |

## 验证、独立审查与范围

收费前 [#52 CI 35120539572](https://github.com/XJfyrh/JobForge/actions/runs/35120539572)、[#53 CI 35121570133](https://github.com/XJfyrh/JobForge/actions/runs/35121570133)、[汇合 #52 后的 #51 CI 35121487782](https://github.com/XJfyrh/JobForge/actions/runs/35121487782) 各 8 项全通过。包括适用 Go/race、真实 PG、Python、SQLFluff、Buf、实际 Linux 进程及观测检查；确定性测试、无收费复现和本次实际云端失败分层报告。此前红灯、未定位超时和旧失败记录均保留，不把后来的通过改写为旧根因已明。

独立上下文 Agent 核对全部 316 份 SDK HTTP receipt 的 bytes/SHA256、105 组出站记录、实际 v2 库七张事实表前后行数/hash一致且 reader 写权限均 false；只有 DEV-015 chat 的响应不完整。独立审查确认当前停止收费与保持 Draft 的判断正确。最终文档差异审查及对应 CI 记录在 PR 中，不以收费前 CI 冒充新提交检查。

[运行指南](../agent-v3-cloud-batch.md)覆盖干净库、SDK 安装、密钥注入、固定数据/索引、prepare/register/bootstrap/inspect、具名 launch、只读 export/评分、停止及仅清理独立环境。独立环境实际验证未产生收费模型请求，也未重做真实 embedding。

本次不宣称 S1 完成或真实云端稳定性通过，不推进 S2～S5。保留历史 W4 性能门禁失败、AT-25 跳过、检索 19/20（RQ-06 未命中）、生产长期留存未验收，以及原 DEV-016 根因未知。秘密与原始正文只在仓库外，公共报告仅发布摘要及工件 hash。
