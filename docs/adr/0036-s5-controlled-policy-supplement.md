# ADR-0036：S5 裁决阶段受控补证

- 日期：2026-10-13；状态：Accepted（维护者批准受控补证方案；独立复审与[免费机制验证](../evidence/agent-v3-s5-v6-mechanism-2026-10-13.md)通过；后续[真实开发](../evidence/agent-v3-s5-v6-development-2026-10-13.md)在DEV-033缺口后停发，未冻结）。
- 增补 [ADR-0033](0033-versioned-final-adjudication.md)、[ADR-0034](0034-s5-final-stage-separation-and-condition-enumeration.md)。旧版本、旧失败和原始证据不变；[ADR-0035](0035-s5-batch-reservation-and-v5-replay.md) 的每批 ¥4、S5 累计 ¥28 不变。

## 问题与选择

[v5 验证](../evidence/agent-v3-s5-v5-validation-2026-10-13.md)在 DEV-020 停止：描述是非空但含糊的 `Something is wrong. Please help.`，调查只检索时效政策，初稿回答准时；终裁识别应补充问题描述，却没有 P03 可引用。终裁没有截断，增加思考容量不能补回政策。已见 018 同样是终裁判断改善、时效政策缺失；004 还存在未识别冲突的风险。

本次离线重建 v4 开发 40、v4 已见 20、v5 已执行 20，共 80 次运行的 435 个实际 chat 请求，parameter hash 全部一致。要求闭合前无条件取得 P03.1/P03.2，会影响其中 38/40、20/20、17/20；这是覆盖范围测量，不是额外调用数预测。原始材料在 `E:/JobForgeEvidence/s5-2026-10-07/v5-20261013/closure-analysis/report.json`，SHA-256 `8592e43e79214ece4fb19dcee321f1e91d5c9a04fc2c4cd64a373598034b7ed3`。

不采用五类 `discharge` 作为闭合证明：模型仍可错误填 `EXCLUDED` 并引用真实描述，枚举完整与引用存在不能证明排除正确。不强制所有工单检索 P03，也不把 gold、关键词锚点或业务评分谓词搬进生产。

选择受控补证，让已经识别到政策缺口的 thinking 裁决有一次取得定义的机会。独立审查由 `/root/s5_design_review` 只读完成，支持该方向；其明确指出该机制不能自动发现遗漏类别，004 仍须真实验证。

## 版本化合同

新增 Agent schema **14**、`support-agent-prompt-v6`、`linux-v2-adjudication-runtime-3`、`support-agent-decision-v2`；生产 registry 仍只有既有两个 adapter。Fixed schema 12 不变，报告继续说明两策略任务与能力不对称。

调查沿用 v5，marker 仍为 `{type,draft}`。只有新版本的第一个 `model_proposal` 可以返回 final 或以下严格意图：

```json
{"type":"policy_supplement","topic":"missing_facts","required_aliases":["P03.2"]}
```

topic 的允许别名取冻结的 `DEFINITION_ALIASES[topic]`，固定查询取 `POLICY_TOPICS[topic]`，不使用导航中的宽泛关联别名。别名是非空、排序、去重的该 topic 定义子集，且每个别名此前都未取得。意图不带初稿、结论、自由查询、工具名、URL 或动作；不是 Proposal。补证只允许一次 `search_policy`，不是任意工具循环。

状态路径为：调查 marker → thinking 裁决 → final，或者调查 marker → thinking 补证意图 → Go 调度政策检索 → thinking final。Go 保持唯一状态转换与发送许可权，Python 只校验并执行所登记的单步。

- 补证意图以 `content` 持久化，`proposal=null`。Go 验证新版本、marker 前驱、无历史补证、指定定义缺失、工具未重复及原额度。不得把非法 final 自动改写成补证。
- 检索返回必须通过现有快照、租户、政策版本和来源验证，并取得全部指定别名；空结果、无关结果或缺任一指定段落失败关闭，不发第二次裁决。检索“发生过”不代表补证成功。
- 补证后只允许 final，不能再次补证。第二次裁决仍独立，只接收累计已提交事实与政策，不接收初稿、前次候选结论、补证理由或推理正文。
- 首次裁决提示词明确两个分支，不能同时保留 v5 的“没有检索能力”措辞。补证后使用 final-only 任务。模型未识别缺口或直接输出不充分 final，仍按原验证与离线验收判失败，不隐式重试。
- 此方案验证的是补证授权及指定定义真实取得，不声称保证所有必要政策均已发现。事实判定与最终正确性仍由真实回归验收。

## 计量与恢复

两个 thinking 调用各自使用 16,384 completion、120 秒请求上限、独立物理调用 ID、预留、报告和结算；都受原 attempt/Run/账户剩余期限裁剪。未知、矛盾计量仍保留 full hold 并停发。格式错误、截断和最终拒绝不触发纠错或补答。

保持每 Run 的 12 chat、8 逻辑工具及物理调用限制，补证消耗剩余额度，不重置 attempt/lease/费用。schema 14 的 family token 上限按两次 thinking 与其余 nonthinking 的原 12 次混合上界调整为 **12,625,920**；旧 schema 仍为原值。这是单 Run 许可合同，不新增阶段 Run/chat/token/耗时预算。

补证 intent、检索结果及最终 Proposal 都复用现有持久步骤与 JSONB。已提交 intent 不重发模型；已提交检索不重跑工具；已提交 final 不重发裁决。未提交但已确认调用的恢复继续按 ADR-0025 处理，unknown 不恢复发送。审批、动作许可和幂等回执不变，不新增 migration。

## 度量与验收

保留 marker 时刻的原两种闭合指标，另报告真正 final 之前的补证后闭合；不把原调查缺口改写为完整。新候选停止规则检查最终输入的可引用闭合与定义闭合，任一缺口停止当前批次剩余提交及后继批次；补证失败或未到 final 单列，不计闭合通过。v5 原规则及本次 DEV-020 停止不追溯改变。

每次 thinking 调用分别报告 reasoning/completion/期限余量，另汇总整个裁决阶段成本。不能把两次 reasoning 相加后除以一次容量，也不能只计最后一次费用。原评分谓词及 `correct` / `full_case_evidence` 不变。

免费验证覆盖：新旧版本隔离；严格意图及篡改拒绝；指定政策缺失/存在；空、无关、跨快照检索；重复查询/第二次补证；初稿隔离；补证各边界恢复、取消、失租；两次计量与 unknown hold；原请求哈希及历史评分保持一致。共享状态与并发修改须通过真实 PG、全仓 race 与固定 Linux 执行器测试。

免费机制通过并独立审查后，使用新身份、新有限 manifest 执行开发 40 和已见 20，不续跑 v5 的剩余 20，也不单独重跑失败题。前 10 案独立语义检查、原分母保留和逐调用收费守卫不变。开发正确至少 36/40、完整证据不得低于原 38/40，四类硬失败零，004/018/DEV-020 全部解决后才冻结。正式新未见集须在干净提交冻结后创建，原已见集不得充当正式验收；登记与新颖性入口须随新 schema 同步。后继正式、Fixed、恢复与演示仍按原门槛依次执行。

本次维护者批准的是该契约实施，不是任意收费 manifest 放行。当前累计 observed-usage 估算 ¥10.591762，held 为零；没有新增收费。
