# 验收证据

按交付阶段与候选版本查找报告。每份报告记录当时的源码、环境、样本、结果和原始工件；当前能力汇总见[状态页](../status.md)。

## 最终结果

| 范围 | 报告 | 原机器证据 |
|---|---|---|
| S5 当前最终结果 | [正式Agent18/20、原11项恢复、实际审批与唯一回执恢复](agent-v3-s5-closeout-2026-10-16.md) | [源绑定机器摘要、费用、180秒视频来源](agent-v3-s5-closeout-2026-10-16.json) |
| 公开 Preview | [免费接入、新版视频、许可与发布验证](public-readiness-2026-10-16.md) | 视频/字幕、包产物及来源清单见 [Release](https://github.com/XJfyrh/JobForge/releases/tag/v0.1.0-preview.1) |
| S1 固定流程 | [40 案，业务 11/40](agent-v3-s1-delivery-2026-09-17.md) | [评分](agent-v3-s1-delivery-2026-09-17.json)、[receipt](agent-v3-s1-delivery-receipt-2026-09-17.json) |
| S2 有界 Agent | [40 案，业务 37/40；真实截断注入](agent-v3-s2-delivery-2026-09-17.md) | [评分](agent-v3-s2-delivery-2026-09-17.json)、[receipt](agent-v3-s2-delivery-receipt-2026-09-17.json) |
| S3 步骤恢复 | [11 项：5 个接管通过，DEV-035-C 业务失败](agent-v3-s3-cloud-2026-10-04.md) | [完整机器摘要](agent-v3-s3-cloud-2026-10-04.json) |
| S3 免费机制 | [检查与原失败](agent-v3-s3-free-2026-10-04.md) | 原提交/私有原日志从报告进入 |
| S4 人工审批与回执 | [10 个原方案/机制通过、12 个实际 Run](agent-v3-s4-cloud-2026-10-05.md) | [机器摘要](agent-v3-s4-cloud-2026-10-05.json) |
| S4 免费工程 | [检查分层](agent-v3-s4-free-2026-10-05.md) | 当前 CI / 私有原日志从报告进入 |

机器 JSON 保留原数值、状态和哈希；模型正文、SDK/PG/outbound 导出等私有材料从报告定位。费用区分用量估算、hold与供应商结算，评分保留失败和未尝试的原分母。

### V9 验收的组成证据

- [S5 V9原11项恢复与从头对照](agent-v3-s5-v9-recovery-2026-10-16.md)：5项实际恢复通过，8个完整方案中7个正确，3个H0计划取消；两组可比案例未体现节费，1组因错答不可比；[全部11项机器摘要](agent-v3-s5-v9-recovery-2026-10-16.json)。后续实模审批演示与总体验收见最终结果。
- [S5 V9未见正式V4配对](agent-v3-s5-v9-formal-v4-2026-10-16.md)：Agent正确/完整证据18/20，原门槛通过；Fixed原20案分18+2来源收齐、各11/20，配对胜8/平11/负1；[逐案机器摘要](agent-v3-s5-v9-formal-v4-2026-10-16.json)。后续恢复、实模演示和交付见最终结果。
- [S5 v9原已见V3完整回归](agent-v3-s5-v9-seen-v3-2026-10-15.md)：19/20正确与完整证据，004/018及DEV-020三守卫通过，闭账与批后审阅accepted；[全部20案机器摘要](agent-v3-s5-v9-seen-v3-2026-10-15.json)。020保留不适用的额外状态主张；该报告记录正式冻结前的已见回归。
- [S5 v9/Fixed17完整开发对照](agent-v3-s5-v9-fixed-development-2026-10-15.md)：Agent39/40、Fixed34/40，配对胜6/平33/负1，完整基线与闭账独立接受；[全部40案机器摘要](agent-v3-s5-v9-fixed-development-2026-10-15.json)。后续已见守卫结果见上条。
- [S5 v9完整开发回归](agent-v3-s5-v9-development-2026-10-15.md)：正确和完整证据各39/40、硬失败0，独立接受开发质量及闭账；[全部40案机器摘要](agent-v3-s5-v9-development-2026-10-15.json)。该报告记录正式冻结前的开发结果。

## 历史与组件快照

以下保留早期候选、诊断、中断与组件验证，按需要展开。报告中的“下一步”对应其记录日期。

<details>
<summary>S5 早期候选与诊断（初版至 v8）</summary>

- [S5 prompt-v3正式质量](agent-v3-s5-v3-real-2026-10-08.md)：Agent正确12/20、完整证据15/20，未通过；[原机器摘要](agent-v3-s5-v3-real-2026-10-08.json)。
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
- [S5 v8单句删减对照](agent-v3-s5-v8-description-ablation-2026-10-15.md)：固定18次全部执行，C1/C2均正确及完整9/9、无当轮配对改善，删句收益未获证明；累计usage估算¥15.431865、闭账，未晋升；[机器摘要](agent-v3-s5-v8-description-ablation-2026-10-15.json)。
- [S5 v8时效主张对照](agent-v3-s5-v8-critical-timing-2026-10-15.md)：固定12次全部执行，V8/V8T正确及完整2/6、3/6，候选未达6/6；定位6项timing自身事件集合引用缺失、1项整条timing缺失；[机器摘要](agent-v3-s5-v8-critical-timing-2026-10-15.json)。
- [S5 v8独立引用对照](agent-v3-s5-v8-timing-evidence-2026-10-15.md)：固定12次，V8T正确/完整3/6、V8TE6/6且三次配对改善，支持精确提示候选实施与完整开发回归；累计usage估算¥16.203356、36库闭账；[机器摘要](agent-v3-s5-v8-timing-evidence-2026-10-15.json)。
- [S5 首次Agent开发记录](agent-v3-s5-dev-agent-2026-10-07.md)：[原机器摘要](agent-v3-s5-dev-agent-2026-10-07.json)，保留当时40案结果；后续版本与正式配对见上方最终结果。

</details>

<details>
<summary>S0–S3 探针、组件与中间交付</summary>

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

</details>

<details>
<summary>其他原始机器工件</summary>

- [review-clock-2026-09-15.json](review-clock-2026-09-15.json)
- [windows-clock-fix-2026-09-15.json](windows-clock-fix-2026-09-15.json)

</details>
