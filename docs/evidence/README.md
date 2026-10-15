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
- [S5 固定输入裁决诊断](agent-v3-s5-decision-diagnosis-2026-10-11.md)：已见20案、四组各三轮，A/B/C/D正确31/49/33/19（各60次）；优先验证thinking最终裁决，未计正式验收；[机器摘要](agent-v3-s5-decision-diagnosis-2026-10-11.json)。
- [S5 独立终裁开发验证](agent-v3-s5-final-adjudication-2026-10-11.md)：开发正确/完整证据38/40、已见回归17/20；004/018未解决，停在冻结之前；[机器摘要](agent-v3-s5-final-adjudication-2026-10-11.json)。
- [S5 v5候选验证](agent-v3-s5-v5-validation-2026-10-13.md)：终裁回放18/20；开发因DEV-020双闭合缺口停在第20案，原40分母19通过、1失败、20未执行；[机器摘要](agent-v3-s5-v5-validation-2026-10-13.json)，未冻结或进入后继批次。
- [S5 v6受控补证机制](agent-v3-s5-v6-mechanism-2026-10-13.md)：版本化补证、原步骤恢复、两次 thinking 计量与新旧闭合分列；该免费验证未收费。
- [S5 v6开发验证](agent-v3-s5-v6-development-2026-10-13.md)：DEV-020/031补证成功，DEV-033遗漏行动政策后停发；原40分母32通过、1失败、7未执行；[机器摘要](agent-v3-s5-v6-development-2026-10-13.json)。
- [S5 v7状态政策前置读取](agent-v3-s5-v7-mechanism-2026-10-13.md)：权威状态触发一次固定查询，已有来源不重读；全仓 race、5 项 Linux 链路和独立历史重算通过，该机制验证未收费。
- [S5 v7开发验证](agent-v3-s5-v7-development-2026-10-13.md)：正确与完整证据38/40，031/033状态读取修复；026协议失败、038语义退化保留。独立接受开发达标，未正式冻结；[机器摘要](agent-v3-s5-v7-development-2026-10-13.json)。
- [S5 v7已见v3回归](agent-v3-s5-v7-seen-v3-2026-10-13.md)：004首次thinking用满输出容量、未提交final，输入缺P05.1但补证入口可用；第4案后停发，原20分母3通过、1失败、16未执行，018未验证；[机器摘要](agent-v3-s5-v7-seen-v3-2026-10-13.json)。
- [S5 v8完整政策前置读取](agent-v3-s5-v8-mechanism-2026-10-13.md)：固定20段整包、双策略同能力单次终裁；免费机制、历史848请求哈希与新冻结源绑定验证，不代表云端质量通过。
- [S5 v8首案审计停发](agent-v3-s5-v8-audit-stop-2026-10-14.md)：政策包资源标签误记为订单，首案后停发；原1/40、39未执行保留。实际7请求及费用独立复算、记录器修复；[机器摘要](agent-v3-s5-v8-audit-stop-2026-10-14.json)。
- [S5 v8开发回归](agent-v3-s5-v8-development-2026-10-15.md)：27案执行后完整证据门槛不可达，自动停发；原40分母正确/完整各24、3失败、13未执行，硬失败0；[机器摘要](agent-v3-s5-v8-development-2026-10-15.json)。
- [S5 v8三案失败分析](agent-v3-s5-v8-contract-analysis-2026-10-15.md)：定位提示压缩时遗漏的三条输出约束，提出六案12次固定输入对照；保留诊断前的分析与工程验证。
- [S5 v8输出约束对照](agent-v3-s5-v8-contract-probe-2026-10-15.md)：12次全部执行，V8正确及完整证据4/6、候选5/6；候选DEV-016退化，未满足6/6，不晋升生产；[机器摘要](agent-v3-s5-v8-contract-probe-2026-10-15.json)。
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
