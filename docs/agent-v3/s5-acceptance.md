# S5 验收矩阵

S5 于 2026-10-16 完成。本页将[PRD v0.19](../product/JobForge_PRD_v0.19.md)及[ADR-0027](../adr/0027-s5-observability-and-data-lifecycle.md)的验收项对应到实际证据；当前交付见[状态页](../status.md)。

## 验收矩阵

正式 V9 候选（Agent schema 18 / Fixed schema 17）在新未见20案上达到正确18/20、完整证据18/20、四类硬失败0；Fixed各11/20。原11项恢复对照与一案实际人工审批/唯一回执恢复均已完成，见[S5最终结果](../evidence/agent-v3-s5-closeout-2026-10-16.md)。开发、已见回归和未见正式集的逐案结果分别保存在[证据索引](../evidence/README.md)。

| ID | 必须证明 | 方法/证据 | 当前状态 |
|---|---|---|---|
| S5-01 | 同源提交/查询/步骤/实际来源/方案/预算/独立效果 | Go HTTP、安装SDK、真实浏览器截图/记录 | 免费全功能实链与[本次真实DeepSeek方案、审批和唯一业务效果](../evidence/agent-v3-s5-closeout-2026-10-16.md)分别通过 |
| S5-02 | 批准、拒绝、版本冲突、取消与unknown；服务端权限与文本安全 | Playwright真实浏览器；operator/approver、异源与恶意文本正反例 | 免费浏览器及服务端检查通过 |
| S5-03 | SDK→控制→Worker→Python→模型/业务父子/link；审批/恢复有限span | 实际OTLP导出和Run/attempt关联机器核验 | 原140Run/790普通调用关联证明保留，本案另7普通HTTP全部关联；实际attempt1/3经run.http父子边关联原SDK，恢复GET operation一致。SIGKILL根span缺失不伪造补齐 |
| S5-04 | 积压/审批/Worker/调用/预算/步骤/恢复/动作指标、可用面板及告警 | 实际Prometheus/Grafana查询；配置/规则验证 | 实际14面板查询、配置及规则通过；S5启动器接线修复后私网抓取HTTP200、Worker up |
| S5-05 | collector失联不阻断执行、不泄密 | 停collector后正式链路完成，调用/Run/OTLP证据 | 免费链路到方案待审9.703秒；已恢复collector |
| S5-06 | 7日终态内容清理；活动/待审批保留；30日晚到/unknown/frozen不返还 | 新建专用PG库受控老化、并发/等号/API/外键/预算快照 | 真实PG及race检查通过 |
| S5-07 | 业务身份/完整回执至少30日、旧键不复活 | 业务内容本阶段完整保留；恢复克隆库受控老化、receipt-only重放 | 原签名/回执重放及31日关系老化检查通过 |
| S5-08 | 停写双库备份、新库恢复、样本/预算/旧身份防重、无新收费或写入 | 仓库外dump/manifest/SHA-256、原新库校验报告 | 控制31+业务11表完整相等；disabled SDK及receipt-only检查通过 |
| S5-09 | 干净Linux Compose与Windows Docker宿主复现 | 固定镜像/SDK/source/build receipt及三分钟演示 | Windows Docker Linux Compose实际实模审批/故障恢复通过，180秒原速剪辑含闭账只读回看；SDK首段0字节保留失败，原回执/Trace核验。未另称原生Linux宿主演示 |
| S5-10 | 候选/基线/评分/模型/上限/阈值在打开20例前冻结 | 不可变freeze/manifest摘要；开发回归单列 | V9/Fixed在新20案创建前冻结于b6328c1；V4新场景家族及manifest独立审查accepted，实际镜像/源码/评分来源绑定 |
| S5-11 | 正式20例全分母：Agent≥16/20，证据≥90%，四类硬失败0 | 实际DeepSeek、MiniLM/pgvector、业务HTTP/PG、正式Go/Python/SDK；原输出/逐案/配对胜平负 | 未见正式V4 Agent正确18/20、完整证据18/20，门槛通过；Fixed各11/20，硬失败0。原v3失败保持历史结果 |
| S5-12 | 同策略恢复/从头成本和约20条检索诊断 | 冻结故障时点、全部调用/known/held/活跃耗时、真实MiniLM | 本地20查询19命中；[原11项恢复对照](../evidence/agent-v3-s5-v9-recovery-2026-10-16.md)完成，5项恢复机制通过，完整方案7/8正确、3个H0计划取消；两组可比C费用更高，另一组错答不可比，不宣称节费 |
| S5-15 | 无模型控制开销、checkpoint提交p95和恢复耗时 | 有限固定机器/样本/并发测量，原始数值与环境 | 100次只读p95 17.184ms；33次CommitStep RPC p95 151.262ms；自然恢复31.015秒 |
| S5-13 | 适用工程检查及最新 head 的 `CI required` | 按完整差异由检查选择器选择 build/vet/lint/race/PG/Linux/Python/SQL/Buf/observability/链接检查 | 已有本地证据按各报告保留；当前改动的检查状态见[状态页](../status.md)，远程见[PR检查](https://github.com/XJfyrh/JobForge/pull/61/checks) |
| S5-14 | 当前文档、简短成果与演示可按实际证据复核 | README/导航/状态/架构/开发/测试/操作/证据索引 | [最终结果及机器摘要](../evidence/agent-v3-s5-closeout-2026-10-16.md)、180秒视频/字幕/来源SHA完成；实际结果与录制失败均明确保留 |
| S5-16 | 逐案可判定性：主张级与证据级配对回退、**两种**检索闭合、终裁推理余量分别可见，且跨案汇总在受审代码内 | `adjudication_stages` 的 `claim_level_delta`、marker两种闭合、`reasoning_budget`和实际 `final_input_closure`，以及 `score.py` 的 `adjudication_measurement`；当前摘要schema3，逐调用容量/期限与两阶段闭合分列，不改验收口径 | v9调查40案完整、已提交final39案；Fixed已提交final38案。两组全部80个实际final请求重建闭合；失败未提交不回填通过，Fixed无marker的调查指标不计通过。详见[完整开发对照](../evidence/agent-v3-s5-v9-fixed-development-2026-10-15.md)。 |

## 审查点与执行顺序

以下规则用于之后单独授权的新实验；S5 本轮已结束。工具入口见[共享工作流](../../tools/support_s5/host/README.md)，具体停止规则见[ADR-0039](../adr/0039-s5-startup-review-and-automatic-health.md)。

1. 每轮确定一个问题，提出假设并做最小诊断，取得支持后实现并完整回归。
2. 一次启动审查：维护者批准具体范围和金额，独立审查核对实现、免费检查及同一份 manifest。
3. 获准后自动执行既定名单并逐案健康检查；第10案自动继续。普通质量失败保留并继续；安全异常、门槛不可达或预定义系统性故障停发。保存成功、失败、未执行与费用，不自动重试或补样。
4. 批后一次结果审阅；修复只复核原问题、影响和新增差异。名单、候选、模型、费用或权限变化重新绑定，合并要求最新 head 的 `CI required` 通过。

本轮累计授权为28,000,000 microyuan（[ADR-0034](../adr/0034-s5-final-stage-separation-and-condition-enumeration.md)），单批上限4,000,000 microyuan（[ADR-0035](../adr/0035-s5-batch-reservation-and-v5-replay.md)）。执行前核对本批与全阶段的known+held及下一调用完整预留；授权按历史批次累计，新账户不重置。阶段仅设费用上限，数量照常记录，各Run的协议和循环限制继续生效。

候选开发门槛为正确至少36/40、完整案例证据至少38/40、四类硬失败0；达到后冻结候选和基线，再创建新未见集。正式门槛为正确至少16/20、完整证据至少18/20、四类硬失败0，Fixed只作配对基线。原分母始终保留，未执行不计通过。

正式v1/v2/v3/v4现已使用，后续回归按已见材料处理。旧候选的错误、停止与费用见[历史实施记录](../archive/agent-v3-progress.md)和[各版本报告](../evidence/README.md#历史与组件快照)。原[收费manifest](s5-paid-plan.json)及[240 Run计划](../evidence/agent-v3-s5-paid-plan-240-2026-10-07.json)保留当时范围。
