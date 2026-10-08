# S5 实施与验收矩阵

更新：2026-10-07。起点 `main@bf941b6`；实施分支 `XJfyrh/feat/agent-v3-s5`。已完成的免费检查见[机制证据](../evidence/agent-v3-s5-free-2026-10-07.md)；合同见[PRD v0.19](../product/JobForge_PRD_v0.19.md)，决策见[ADR-0027](../adr/0027-s5-observability-and-data-lifecycle.md)。

2026-10-08：schema9/10及prompt-v3已完成开发回归、冻结和新正式配对。Agent正确12/20、完整证据15/20，未达16/18门槛；Fixed为1/20、2/20，两策略四类硬失败0。结果、费用与停止事实见[prompt-v3报告](../evidence/agent-v3-s5-v3-real-2026-10-08.md)，恢复/演示未启动。

## 验收矩阵

| ID | 必须证明 | 方法/证据 | 当前状态 |
|---|---|---|---|
| S5-01 | 同源提交/查询/步骤/实际来源/方案/预算/独立效果 | Go HTTP、安装SDK、真实浏览器截图/记录 | 免费真实链路通过；模型响应合成 |
| S5-02 | 批准、拒绝、版本冲突、取消与unknown；服务端权限与文本安全 | Playwright真实浏览器；operator/approver、异源与恶意文本正反例 | 免费浏览器及服务端检查通过 |
| S5-03 | SDK→控制→Worker→Python→模型/业务父子/link；审批/恢复有限span | 实际OTLP导出和Run/attempt关联机器核验 | SDK原提交link 5/5；真实模型首案关联经独立核验；审批等待无常驻span |
| S5-04 | 积压/审批/Worker/调用/预算/步骤/恢复/动作指标、可用面板及告警 | 实际Prometheus/Grafana查询；配置/规则验证 | 实际14面板查询、配置及规则通过；S5启动器接线修复后私网抓取HTTP200、Worker up |
| S5-05 | collector失联不阻断执行、不泄密 | 停collector后正式链路完成，调用/Run/OTLP证据 | 免费链路到方案待审9.703秒；已恢复collector |
| S5-06 | 7日终态内容清理；活动/待审批保留；30日晚到/unknown/frozen不返还 | 新建专用PG库受控老化、并发/等号/API/外键/预算快照 | 真实PG及race检查通过 |
| S5-07 | 业务身份/完整回执至少30日、旧键不复活 | 业务内容本阶段完整保留；恢复克隆库受控老化、receipt-only重放 | 原签名/回执重放及31日关系老化检查通过 |
| S5-08 | 停写双库备份、新库恢复、样本/预算/旧身份防重、无新收费或写入 | 仓库外dump/manifest/SHA-256、原新库校验报告 | 控制31+业务11表完整相等；disabled SDK及receipt-only检查通过 |
| S5-09 | 干净Linux Compose与Windows Docker宿主复现 | 固定镜像/SDK/source/build receipt及三分钟演示 | Windows Docker固定Linux进程通过；干净Linux Compose及实模演示待运行 |
| S5-10 | 候选/基线/评分/模型/上限/阈值在打开20例前冻结 | 不可变freeze/manifest摘要；开发回归单列 | prompt-v3开发正确36/40、完整证据37/40后独立接受并冻结；新20案在冻结后创建并通过新颖性审查 |
| S5-11 | 正式20例全分母：Agent≥16/20，证据≥90%，四类硬失败0 | 实际DeepSeek、MiniLM/pgvector、业务HTTP/PG、正式Go/Python/SDK；原输出/逐案/配对胜平负 | 正式v3 Agent正确12/20、完整证据15/20，Fixed为1/20、2/20；质量未通过，四类硬失败0 |
| S5-12 | 同策略恢复/从头成本和约20条检索诊断 | 冻结故障时点、全部调用/known/held/活跃耗时、真实MiniLM | 本地20查询19命中；实模恢复费用对照待运行 |
| S5-15 | 无模型控制开销、checkpoint提交p95和恢复耗时 | 有限固定机器/样本/并发测量，原始数值与环境 | 100次只读p95 17.184ms；33次CommitStep RPC p95 151.262ms；自然恢复31.015秒 |
| S5-13 | 适用工程检查及最新head全部8项CI | build/vet/lint/race/PG/Linux/Python/SQL/Buf/observability/链接与秘密 | 本地检查通过；最新head状态见[PR检查](https://github.com/XJfyrh/JobForge/pull/61/checks) |
| S5-14 | 当前文档、简短成果与演示可按实际证据复核 | README/导航/状态/架构/开发/测试/操作/证据索引 | 当前文档与三分钟脚本已更新；实模演示待运行 |

## 审查点与执行顺序

1. 契约草案：审查内容过窗的HTTP410、薄身份/外键边界、业务完整保留及停写恢复设计。
2. 免费实现审查：页面/观测/清理备份恢复、PG/Linux/race/浏览器与实际导出证据，提交实现PR及具体收费manifest。此处停止收费，等待规划会话独立结论。
3. 有限真实执行：当天官方价格/账号只读核验，source/build/profile/名单/评分/阈值/时间窗/批准规则冻结。开发回归先于正式未见集创建/打开。每批完成后停发并追加累计账本；异常不自动补样。
4. 最终审查：结果、原始输出与费用/hold、机器摘要、8项CI最新head及必要文档就绪；独立接受后才squash合并并同步干净main。

整个S5授权20,000,000 microyuan。维护者于2026-10-08取消费用之外的预算，原300 Runs / 1,600 chat不再作为阶段执行上限，不再设置阶段级Run/chat/token预算；数量照常记录，原单Run协议与防无界循环限制属于产品合同。每批持久费用cap不能超过 `20,000,000 − ΣS5批次known − ΣS5批次held`，历史S1–S4独立保留。原[收费manifest](s5-paid-plan.json)保留为历史计划；后续具体费用与执行顺序见[ADR-0030](../adr/0030-s5-policy-condition-candidate.md)，独立审查通过后按既有费用授权执行。

开发候选正确方案和完整案例证据均须达到36/40、硬失败为0，才可冻结并创建新的未见正式集。下一完整40案先检查前10案语义；已无法达到开发门槛或出现系统性格式失败时停止后续提交，全部失败和未执行仍在原40分母。

正式v3停止时累计389 Runs /1,234 chat，known ¥4.201411、held 0，unknown及在途均0。正式v1/v2/v3均已见，不可重新计为未见验收；后续候选仍须独立审查、开发回归、新冻结与新正式集，正式质量通过后才能执行恢复和演示。¥20总费用及每批≤¥4/≤6小时保持，原[240 Run计划](../evidence/agent-v3-s5-paid-plan-240-2026-10-07.json)及其他数量计划作为历史记录保留。
