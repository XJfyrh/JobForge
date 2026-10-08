# 验收证据

结果按源码机制、真实进程/数据库、真实模型与业务质量分层；较低层结果不能代替较高层验收。当前交付范围见[状态页](../status.md)。

## 最终结果

| 范围 | 报告 | 原机器证据 |
|---|---|---|
| S1 固定流程 | [40 案，业务 11/40](agent-v3-s1-delivery-2026-09-17.md) | [评分](agent-v3-s1-delivery-2026-09-17.json)、[receipt](agent-v3-s1-delivery-receipt-2026-09-17.json) |
| S2 有界 Agent | [40 案，业务 37/40；真实截断注入](agent-v3-s2-delivery-2026-09-17.md) | [评分](agent-v3-s2-delivery-2026-09-17.json)、[receipt](agent-v3-s2-delivery-receipt-2026-09-17.json) |
| S3 步骤恢复 | [11 项：5 个接管通过，DEV-035-C 业务失败](agent-v3-s3-cloud-2026-10-04.md) | [完整机器摘要](agent-v3-s3-cloud-2026-10-04.json) |
| S3 免费机制 | [检查与原失败](agent-v3-s3-free-2026-10-04.md) | 原提交/私有原日志从报告进入 |
| S4 人工审批与回执 | [10 个原方案/机制通过、12 个实际 Run](agent-v3-s4-cloud-2026-10-05.md) | [机器摘要](agent-v3-s4-cloud-2026-10-05.json) |
| S4 免费工程 | [检查分层](agent-v3-s4-free-2026-10-05.md) | 当前 CI / 私有原日志从报告进入 |
| S5 prompt-v3正式质量（未通过） | [Agent正确12/20、完整证据15/20；恢复/演示未运行](agent-v3-s5-v3-real-2026-10-08.md) | [逐案、配对、费用与工件摘要](agent-v3-s5-v3-real-2026-10-08.json) |

机器 JSON 保留原数值/状态/哈希；原始模型正文、SDK/PG/outbound 和秘密仅在仓库外私有材料中。known/hold 不等于已结算账单，失败/未尝试不从分母删除。

## 历史与组件快照

以下记录保留当时的探针、独立层检查、中断和修复；不是多份当前阶段台账。最终报告链接相关历史，未结事项统一在状态页维护。

- [S5 免费机制、观测、恢复与本地检索](agent-v3-s5-free-2026-10-07.md)：[机器摘要](agent-v3-s5-free-2026-10-07.json)。
- [S5 正式v2历史结果](agent-v3-s5-real-2026-10-08.md)：Agent正确/完整证据10/20、Fixed各1/20；[原机器摘要](agent-v3-s5-real-2026-10-08.json)保留。
- [S5 正式v2输入诊断](agent-v3-s5-input-diagnosis-2026-10-08.md)：87次请求绑定、20案完整事实/政策/UTC复核；新增收费0，旧正式质量不变。
- [S5 首次Agent开发记录](agent-v3-s5-dev-agent-2026-10-07.md)：[原机器摘要](agent-v3-s5-dev-agent-2026-10-07.json)，保留当时40案结果；当前候选与正式配对见上方S5本轮报告。

- [DeepSeek 接入调查、独立复审与环境阻塞](agent-v3-deepseek-and-blocker-2026-09-16.md)
- [Docker 恢复后 S0 复验与合并](agent-v3-docker-recovery-2026-09-16.md)
- [S0 本地主模型试验结论](agent-v3-model-probe-2026-09-16-summary.md)
- [Agent v3 S0 本地模型协议试验](agent-v3-model-probe-2026-09-16.md)
- [S0 独立审查与修复记录](agent-v3-s0-review-2026-09-16.md)
- [S1-A 真实业务与政策检索验收（2026-09-16）](agent-v3-s1-business-2026-09-16.md)
- [S1-C3b：固定执行器与持久控制确认验证](agent-v3-s1-c3-runtime-2026-09-16.md)
- [S1 收尾新批：未知 chat 计量阻塞，保持停止](agent-v3-s1-closeout-2026-09-17.md)
- [首批失败后的最小修复](agent-v3-s1-cloud-fixes-2026-09-16.md)
- [S1 云端运行与评分入口](agent-v3-s1-cloud-launch-2026-09-16.md)
- [首批真实 DeepSeek：未通过，已停止](agent-v3-s1-first-cloud-2026-09-16.md)
- [S1 连续短任务心跳饥饿修复](agent-v3-s1-heartbeat-2026-09-17.md)
- [S1 历史未知费用准入与诊断增量](agent-v3-s1-held-usage-2026-09-17.md)
- [S1 供应商持久审计：实现与验证](agent-v3-s1-provider-audit-2026-09-16.md)
- [S1 收尾：独立环境最小复现记录](agent-v3-s1-reproduction-2026-09-17.md)
- [S1 support 固定流程与开发数据 v2 验证](agent-v3-s1-support-2026-09-16.md)
- [Agent v3 S1-B：Run Claim 与调用账本定向基线](agent-v3-s1b-performance-2026-09-16.md)
- [S1-B Run / 调用账本验证](agent-v3-s1b-runs-2026-09-16.md)
- [Agent v3 S1-C1：协议、时间与计量验证](agent-v3-s1c1-protocol-2026-09-16.md)
- [Agent v3 S1-C2：授权 HTTP 与 DeepSeek 模块验证](agent-v3-s1c2-http-2026-09-16.md)
- [Agent v3 S1-C3a：普通观察 ACK 验证](agent-v3-s1c3a-ack-2026-09-16.md)
- [执行器停止通知竞态修复](executor-stop-publication-2026-09-17.md)

<details>
<summary>其他原始机器工件</summary>

- [review-clock-2026-09-15.json](review-clock-2026-09-15.json)
- [windows-clock-fix-2026-09-15.json](windows-clock-fix-2026-09-15.json)

</details>
