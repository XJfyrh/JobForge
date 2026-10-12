# ADR-0034：S5 终裁任务分离与条件枚举候选

- 状态：Accepted（维护者 2026-10-12 批准方案与 ¥28 阶段上限；独立设计审查结论 `accept_with_conditions`，4 项 BLOCKING 全部处置完毕并复核；随本 PR 合并生效。**本候选尚未运行任何收费批次，无真实模型质量结果**）
- 日期：2026-10-12
- 决策者：维护者（2026-10-12 批准本方案与 S5 阶段费用上限由 ¥20 提高至 ¥28）
- 关联：[PR #61](https://github.com/XJfyrh/JobForge/pull/61)、[ADR-0030](0030-s5-policy-condition-candidate.md)、[ADR-0031](0031-s5-controlled-decision-diagnosis.md)、[ADR-0033](0033-versioned-final-adjudication.md)、[终裁开发验证](../evidence/agent-v3-s5-final-adjudication-2026-10-11.md)
- 取代：无。增补 ADR-0033 的冻结条件与 S5 阶段费用上限，不改写其历史正文与既有结果。
- 被取代：无

## 上下文

ADR-0033 的独立终裁候选停在冻结之前：开发 40 案正确/完整证据 38/38（初稿 38/39，净正确数未增加），已见 v3 回归 20 案 17/17（初稿 11/12），004/018 未解决。按 registration.json 绑定的原始包（开发集为仓库 `examples/support-agent`，已见集为仓库外 `formal-package-v3-r1`，gold 与 anchors 摘要逐字节相符）重建全部 60 案终裁输入，`derive_facts` 推出的期望与冻结 gold **60/60 一致**，因此以下判定基于实际合同而非推断。

5 个失败可完整分解为三类互不重叠的缺陷：

| 类别 | 案 | 实测依据 |
|---|---|---|
| 检索闭合缺口：必需主张类别的定义段未被调查取到，终裁无工具可补 | DEV-020、S5V3-004、S5V3-018 | 按**定义闭合**（该类的定义段是否取得）统计：全 60 案中完整 57 案→55 正确；不完整 3 案→**3 案全错** |
| 时效分支条件未定义：定义段齐备仍选错分支 | S5V3-017 | P02.1/P02.2/P07.2 均已取得，仍输出 `outstanding_overdue_lt48` |
| 终裁指令冲突：初稿正确，终裁返回 tool | DEV-036 | 839 次物理调用中唯一一次拒绝；HTTP 200、`response_complete=true`、thinking、reasoning 已观测 |

三类缺陷各自的机制：

1. **调查是主张驱动的**。[CONDITION_REVIEW](../../python/jobforge_agent/support_agent_v3.py) 要求"断言冲突前先取定义段"，这是断言的前置条件而非检验的触发条件；模型不打算主张冲突就永远不会取 P05.1。同时 `policy_retrieval` 只判断"话题别名集有无任一已检索"，而 `structured_conflict` 的别名集 `(P05.1, P05.2, P10.1)` 与 `carrier_correction` 共享 P05.2，于是一次纠正检索把冲突话题标记为已覆盖——全 60 案中 **19 案**出现该形状，且全部集中在 `structured_conflict`（只有 P05.2 或 P10.1 而缺 P05.1）；其中唯一真正需要 conflict 主张的 S5V3-004 失败了。v2 的角色视图其实报告了 `conflict_definition missing=[P05.1]`，却被 `not a requirement to fetch every role` 消音。
2. **"已送达/悬置"没有可操作定义**。[predicates.py](../../tools/support_evaluation/predicates.py) 以"活动事件线中是否存在未纠正的 delivered 事件"分支，与聚合 `delivery.status` 无关；而 P02.1、P07.2、P06.1 与提示词全部使用未定义的"delivered order / outstanding / completed delivery"。017/018 的聚合状态是 `lost` / `returned_to_sender` 且各有一个活动 delivered 事件，两种读法都自洽，合同只认一种。018 还叠加了引用不可达：其已检索集合不含任何 P02.x，而 `claim_sources` 对 `delivered_not_late` 只接受 `{P02.1,P02.2}`，对两个悬置分支都接受 P07.1——**唯一可通过覆盖检查的时效主张恰好都是错的**。
3. **终裁未做任务分离**。终裁 system message 17,931 字节中 final-only 文本仅 748 字节（4.2%），同一消息内并存 `your next response must be a search_policy tool decision` 与 `Never return a tool decision`；载荷 **60/60** 带 `remaining_tools>0`，平均 **8.58/12** 个导航角色递 `suggested_query`。[step.py](../../python/jobforge_agent/step.py) 的 `not context.final_adjudication` 与 [support_agent.go](../../internal/run/support_agent.go) 对 `model_proposal` 的 `CorrectionRequired` 要求使一次 tool 返回即终态失败。

另有两项度量缺陷使上一轮结论被高估：`draft_to_final` 只比对案级 `correct`，因此 S5V3-017 的 `full_case_evidence` 由 True 变 False（初稿时效主张谓词为真且引用完整，终裁换成谓词为假的主张）被记为"不变"，批次汇总写出"0 回退"；且不报告不可恢复集，使"模型答错"与"模型不可能答对"无法区分。

**必须区分两种闭合，二者不可互相替代。**独立审查复算全部 60 案后确认：

- **可引用闭合**（`claim_sources` 接受的别名是否有任一被检索）回答"是否存在任何合法终裁输出能满足来源覆盖"。上一轮为 **2/60 不可达**（DEV-020 缺 P03.x、S5V3-018 缺 P02.x）。它对 S5V3-004 报 **可达**，因为 `claim_sources` 对 conflict 接受 `{P05.1,P05.2,P10.1}`，而 P05.2 已由一次无关的纠正检索取得——这正是本 ADR 第 1 条诊断的别名共享病理，所以该指标**看不见** 004 形状。`adjudication_recoverable=true` 也**不是**通过的保证：它只检查政策别名，不检查 `claim_sources` 的事实指针分组。
- **定义闭合**（该类的定义段是否被检索，即 v5 的 `DEFINITION_ALIASES`）回答"调查是否真的读过定义该类的那一段"。上一轮为 **3/60 不完整**（DEV-020、S5V3-004、S5V3-018），且 57 完整→55 正确、3 不完整→0 正确，即本 ADR 上表引用的分解。

因此停止规则必须同时看两者：只用可引用闭合，会对三个已知案守卫之一（S5V3-004）的复发失明。

增加推理额度不是本轮的杠杆：终裁正确的 55 案 reasoning 中位数 930 token，错误的 5 案中位数 **9,772**；全语料最深的三次调用（14,590 / 12,161 / 9,772）**全部失败**，其中 DEV-020 用掉 16,384 completion 的 89% 与 69.9 秒，得出业务判断完全正确、却因 P03 未检索而无法计分的答案。反之 DEV-036 只用 1,167 token 即失败，败于指令冲突而非深度。

> **该相关性的效力限定（独立审查要求）**：这是**描述性**而非因果结论。错误组 n=5，其中 4 案各有独立的结构性成因（缺政策、缺定义、指令冲突），中位数不能支撑"推理越深越差"的因果读法。而且 [ADR-0031](0031-s5-controlled-decision-diagnosis.md) 的 B 组（thinking/high）对 A 组是 49/60 对 31/60 的**大幅改善**——思考模式本身确有效。本 ADR 据此只主张：**在已经开启 thinking/high 的前提下，再提高推理额度不能解决这四类结构缺陷**，不主张思考无用。

## 决策驱动因素

- 三类缺陷需要三种不同修法，任何单一手段只覆盖一部分。
- 强制检索决策优先级段 P10.1 不可行：它只在 6/60 案中被取到，该门会在 **90%** 的 Run 上触发。
- 无条件补齐整包政策已被 [ADR-0031](0031-s5-controlled-decision-diagnosis.md) 的 C 组证伪：正确 33/60（对 A 的 31/60 仅 +2），完整证据反而由 42/60 降到 39/60。
- 在运行时复制评分器的引用表会把评分合同搬进生产，违反 ADR-0030 的"Python 不执行政策谓词"。
- 结构性损耗（2/60 不可恢复 + 1/60 工具返回 = 5%）在正式 20 案门槛下期望损失约 1.0 案：对允许 4 个正确失败的门槛是 **25%** 余量，对只允许 2 个证据失败的门槛是 **50%** 余量（工具返回同样损失证据完整性，故两个比例同基准）。且 DEV-020 在 S1、S2、S5-v3 开发集与本轮初稿+终裁中全部失败，说明该损耗可以是确定性的。

## 决策

新增 Agent schema **13** / `support-agent-prompt-v5` / `linux-v2-adjudication-runtime-2`，保留全部旧定义与 hash。生产 registry ID 不增加。模型配置、许可、审计、租约、取消、恢复与费用边界全部沿用 ADR-0033，本 ADR 只改两个阶段各自看到的内容与离线度量。

**A. 终裁 final-only 投影。** 终裁不再由"继承调查提示词后追加限制"构造，改为独立构造：system 为自包含的 `FINAL_SYSTEM`（复用 `SYSTEM_INSTRUCTIONS` 六字段合同与数据/指令隔离守卫，加 Agent 字段命名、最小主张与优先级、引用清单、条件定义与终裁复核），不含任何工具 JSON 格式、检索指引或 NEXT-DECISION CHECK；user 载荷在既有证据投影（已剔除初稿）基础上移除 `previous_tools`、`remaining_tools`、`completed_requests`、`policy_navigation`、`policy_retrieval`，仅新增 `final_stage`。实现为 `support_agent_v5.py`，并有测试断言终裁 system 与 body 不含任何工具诱导字段、body 键集是调查 body 键集的子集。v4 的"终裁载荷与调查载荷逐字节相同"不变量继续只约束 v4。

**B. 条件枚举（通用政策条件澄清）。** 调查在返回闭合标记前，必须按结构化矛盾、有效送达争议、活动严重异常、必需事实缺失、时效的固定顺序，仅凭已捕获字段把每类判为 ESTABLISHED / EXCLUDED / UNDETERMINED；排除必须引用具体事实，沉默不构成排除；成立或无法判定且定义段不在 `available_refs` 时，下一次响应必须是对该定义的聚焦检索；已排除的类别不检索，也不得仅因导航列出某类就检索。枚举静默进行，输出仍是原 tool/final JSON，**不改标记形状、不改 Go**。

**C. 时效分支的可操作定义（通用政策条件澄清）。** 明确"完成/悬置"仅由活动事件线决定而非聚合 `delivery.status`；存在未纠正 delivered 事件即为完成，取最早该事件的 `event_minus_promise_seconds`，悬置超时阈值不适用，即使聚合状态为 lost/damaged/returned_to_sender；仅当不存在活动 delivered 事件时才为悬置。并明确活动严重异常支配时，结论仅在适用时效测试为两个悬置超时分支之一时为 `delayed`，否则为 `insufficient`，时效主张仍报告完成送达的实际结果。结论映射是 P06.1 原文"delayed if a known promise has already been missed **with delivery outstanding**; otherwise insufficient"配合上述定义的直接推论，与 [predicates.py](../../tools/support_evaluation/predicates.py) 的 critical 分支逐字一致。

> **诚实限定（独立审查要求）**：对"先有效送达、其后出现未被纠正的严重异常"这一顺序，语料本身是**沉默**的——P06.2 最接近的子句只覆盖相反顺序（异常之后的有效送达），而冲突定义也不把不同时刻的 delivered→lost 列为矛盾。因此本条在该顺序上是**填补语料沉默**，不是从既有正文逐步推出；说它"不新立规则"是过头的。它仍落在 ADR-0033 允许的"通用政策条件澄清"范围内：不含案号、不含答案、对所有案一致适用，且未修改任何政策正文。若审查认定这越界，替代路径是把该定义写入合同解释并同步语料与评分器说明，改动面更大。

**D. 导航信号去误导。** 调查载荷的 `policy_retrieval[topic]` **新增** `definition_retrieved`，按 v5 自有固定表计算（`structured_conflict` 的定义段为 P05.1，P05.2/P10.1 降为 supporting；其余话题沿用其角色别名集）。该表键集等于 `POLICY_TOPICS` 键集、值逐条为其子集，且 `structured_conflict:("P05.1",)` 与冻结的 v2 `conflict_definition` 角色逐字相同，属导航元数据而非政策判定，并由测试按此不变量钉住以防漂移。消音表述的改写**只作用于断言**：定义段缺失时不得断言该类主张、也不得把"同话题有兄弟段落"当作已覆盖；它**不**要求排除也必须先取定义——凭具体捕获事实作出的排除无需检索。若判断某类是否适用本身需要读未取得的规则，则诚实状态是 UNDETERMINED 而非 EXCLUDED，由 UNDETERMINED 触发检索。

> 独立审查曾在此处发现一个由本候选**自己引入**的矛盾（BLOCKING-3）：初版措辞写成"缺失定义段同时阻止断言与排除"，配合枚举条款会被读成收尾前必须取齐全部五类定义段。该状态在冻结语料的 **0/60** 次运行中出现，逐类强制门命中率为 conflict 75.0% / dispute 86.7% / critical 75.0% / missing 80.0% / timing 26.7%，最坏情形吃满 8/8 逻辑工具与 11/12 chat——正是本 ADR 第 35 行否决的 90% 命中门与被 ADR-0031 C 组证伪的无条件补齐。现措辞已收窄为上述形式。这是**提示级引导而非硬门**，其有效性正是批次 2 要测量的对象，停止规则由定义闭合指标给出。

**E. 离线度量升级。** 加法、仅离线，不改变 `correct` / `full_case_evidence` / `thresholds_met` 任何验收口径：

- `claim_level_delta`：`{draft_claims, final_claims, added, removed, flipped[{kind, from, to}], evidence_regressed, evidence_improved}`。按 `coverage_key` 分组，因此 `missing:<field>` 与 `ticket_status:<mode>` 不互相掩盖；终裁一侧的 coverage key 由 `validate_run_sources` 返回的已接受 proposal 计算，**不改既有 `claim_checks` 字段形状**，历史证据保持可比。`evidence_regressed` 独立于案级 `correct`，正是上一轮被"0 回退"掩盖的那类变化。
- **两种闭合，分别报告，不得混用**（见上文"必须区分两种闭合"）。`retrieval_closure` 为**可引用闭合**：`available` 只取 marker 之前已提交 `search_policy` 返回的段落 `chunk_id`，事实指针不计；`adjudication_recoverable=false` 表示不存在任何合法终裁输出能满足来源覆盖。`adjudication_recoverable=true` **不是**通过的保证，因为该指标只核对政策别名、不核对 `claim_sources` 的事实指针分组。`definition_closure` 为**定义闭合**，与之并列且从不合并：逐必需类别给出 `{required, topic, definition_aliases, definition_present, definition_satisfied}`，顶层给出 `definition_unsatisfied` 与 `investigation_definition_complete`。两者共用同一个 `_marker_context`，因此读到的已检索集合完全相同；类别到话题的映射由 `definition_topic` 给出，未映射或未绑定的类别抛 `DEFINITION_TOPIC_UNMAPPED` / `DEFINITION_TOPIC_UNBOUND` 而**不静默通过**。定义别名表直接 import v5 的 `DEFINITION_ALIASES`（不复制第二份），并有漂移测试钉住：该表键集等于**八个** `POLICY_TOPICS` 话题（七个被类别映射使用，外加无类别映射的 `extended_delay`），且 `structured_conflict` 仍为 `("P05.1",)`——两个方向的漂移都会失败。`accepted_aliases` 镜像 `predicates.claim_sources`（含 `outstanding_overdue_ge48` 只接受 P07.1、`missing:delivery.delivered_event` 追加 P02.2、两个 `ticket_status` 模式、`facts.timing` 不可导出时接受集为空），`claim_sources` 仍是唯一验收谓词。两种闭合均以 marker 为锚，Fixed 无 marker 故为 null。
  > 独立审查曾指出初版"由测试**双向**校验一致"是过头的：当时夹具案的 `facts.required` 只有 `['timing']`，实际只交叉核对了一个组合。**该缺口已补齐**：镜像测试现按全部 **17 个 (类别, 时效) 分支 × 语料全部 20 个别名**双向参数化，每个分支配一份 `claim_truth` 会接受的合成 `Facts`，负向探针由 8 个偏 `.1` 的别名扩到全部 20 个。
- `reasoning_budget`：`{reasoning_tokens, completion_capacity, ratio, wall_seconds, deadline_seconds, deadline_headroom_seconds, truncation_risk}`；比值 >0.9 或墙钟 >96 秒（120 秒截止的 80%）标记风险。它是观测标记而非评分失败，无已观测终裁调用时为 null。
- **跨案汇总进入仓库受审代码**：`score.py` 输出 `adjudication_measurement`，摘要 `schema_version` 由 1 升为 2（v2 从未发布过：两份冻结 `score.json` 都是 v1 且其 `adjudication_stages` 只含 `{investigation, final}`，所以 2 就是本候选首次引入的度量版本，无需再升 3）。理由是上一轮的盲点恰恰来自汇总逻辑位于仓库外未受审的批次脚本——它只比对案级 `correct`，于是把主张级回退报成"不变"。字段为 `evidence_regressed(_cases)`、`evidence_improved(_cases)`、`retrieval_closure_measured`、`adjudication_recoverable_false(_cases)`、`definition_closure_measured`、`investigation_definition_incomplete(_cases)`、`truncation_risk(_cases)`。未测得的案被跳过而**不计为通过**，两种闭合各带显式分母。已核实 `freeze.py` / `package.py` 均不校验摘要 `schema_version`（冻结按 sha256/kind/denominator/proposal_audit_complete 绑定，已见诊断门只看阈值），故历史 schema-1 摘要与冻结回执保持字节有效。

计算使用 `derive_facts`，只存在于 `tools/support_s5`，不进生产运行时。

**已知案守卫（增补 ADR-0033 的冻结条件）。** 冻结前的已知案由 004/018 扩展为 **004/018/DEV-020**。这三案是回退守卫，不是泛化证据；针对已知案调整提示词后，已见集彻底失去预测力，唯一有效门槛仍是冻结后新建的未见 20 案，报告不得把已见集改善称为泛化收益。

**S5 阶段费用上限由 ¥20 提高至 ¥28**（维护者于 2026-10-12 批准）。当前累计 known ¥9.275641、held 0，提高后余量 ¥18.724359。每批上限仍为 ¥4、6 小时、串行，逐调用保守预留与账户协议期限不变；费用按冻结费率与 observed usage 记为保守账本值，不称已结算账单，unknown/full hold 不记成零费。

**本轮明确不做**（留待后续候选，各自需要新的独立审查）：

- 协议级 discharge 列表（扩展 `investigation_complete` 标记，由 Python 只校验枚举完整性与别名存在）。属公开契约变更，须同时改 Python 与 Go 的标记校验；先看 B 的提示词版能否闭合 004/DEV-020。
- 终裁阶段一次协议纠错。它会破坏 ADR-0033 的"每案恰好一次终裁"审计不变量；先用 A 把触发概率压到接近零，由 E 持续记录事件率，正式轮出现 ≥1 次再提修订。
- 把 C 的澄清同步给 Fixed。`SYSTEM_INSTRUCTIONS` 为 v1–v4 与 Fixed 共用，改它会破坏全部旧版本字节兼容；要同步须另开 `support-fixed-prompt-v2`。正式门槛只卡 Agent，Fixed 为对照，**但报告必须显式写明这一不对称，不得把对照结果说成同等条件下的比较**。

## 公开契约与数据影响

- 新增部署定义 schema 13 与 `api/support/s5-agent-v5/profile-schema.json`；`prompt_sha256` 绑定 `python/jobforge_agent/support_agent_v5.py`。
- **冻结与新颖性工具必须同步扩展，否则批次 5–7 不可达**（独立审查 BLOCKING-1/2，本 ADR 初版遗漏）：[freeze.py](../../tools/support_s5/freeze.py) 的 (agent, fixed) schema 配对白名单原为 `{(6,5),(7,8),(9,10)}`，**(13,12) 会触发 `COMPARISON_PROFILE_MISMATCH`**；其 seen-formal 溯源只对 agent schema 7/9 绑定；其源绑定清单含 `s5-agent-v1..v4` 而**缺 `s5-agent-v5`**。[package.py](../../tools/support_s5/package.py) 无 v5 未见集的数据 ID，且 `prior_families` 对未知 dataset 返回空集，使**唯一承载验收权重的家族新颖性守卫形同虚设**——未知 dataset 必须**失败关闭**而非默认放行。该缺口同样阻塞 (11,12)，自 ADR-0033 起即存在，只因 v4 候选停在冻结之前而从未触发。
- 新增 Go 常量 `SupportAgentPromptV5`、`SupportAdjudicationV2ExecutorVersion`，`FinalAdjudicationEnabled` 同时接受 v1/v2 executor；Python 与工具的 executor 白名单同步加入 `linux-v2-adjudication-runtime-2`。
- HTTP、gRPC、SDK、Proto、数据库 schema、migration、状态机、错误码、指标与面板：**无变化**。终裁仍只允许 `model_proposal` 使用 thinking，审计仍为 schema 2。
- Fixed 保持 schema 12 / `support-fixed-prompt-v1` / `linux-v2-fixed-adjudication-runtime-1` 不变；schema 1–12 与 prompt v1–v4 的全部常量与 hash 字节冻结，并有测试断言。
- 政策 Markdown、fixture、gold、manifest、semantic anchors 与原始证据**不修改**。

## 后果

### 正面

- 三类缺陷各自被针对性修复，且 A 与 D 是纯代码改动，不含任何政策判断。
- 终裁 system message 由 17,931 降到 **13,619** 字节且工具诱导文本为零（12 个禁用 token 全部不出现），DEV-036 类失败的触发条件被移除而非被劝止。
- 度量升级把"模型答错"与"模型不可能答对"分开，使下一轮的花费决策可由报告直接给出。

### 负面与成本

- B 可能增加检索次数与费用，并引入干扰段落；调查单价按上浮约 30% 估算。
- 新增一套提示词常量与一个部署 schema 的维护面；`DEFINITION_ALIASES` 是新的硬编码导航元数据，语料增删段落时需同步。
- C 把政策术语的定义写进提示词，增加了"澄清"与"改政策"之间的解释负担。

### 风险与缓解

- **B 引入回退**：批次必须同时报告 `search_policy` 次数分布与完整证据数；若开发集完整证据相对本轮 38/40 下降即视为回退并停止后续提交。
- **C 越界**：其结论仍由 P06.1 原文导出；若审查认定越界，替代路径是把定义写入合同解释并同步语料与评分器说明，改动面更大。
- **对已见集过拟合**：无法避免，ADR-0033 本身要求解决已知案；唯一对冲是未见集新颖性审查必须真独立。
- **截断余量**：DEV-020 已用 89% completion 与 69.9 秒，诊断轮在同预算下出现过 3 次 `finish_reason=length`；E 的 `truncation_risk` 持续观测，比值 >0.9 或墙钟 >96 秒即标记。**已知最坏案不会触发它**（DEV-020 比值 0.8905、墙钟 69.90 秒，两项均在阈值之下；两批复算 `truncation_risk` 均为 0），所以它是一个预警旗标而非既成事实的记录，不能因为"上一轮为 0"就认为余量充足。
- **Fixed schema 12 从未运行**，其开发回归估算不确定性最大。

## 考虑过的替代方案

- **提高 reasoning_effort 或 completion 容量**：未采用。在已开启 thinking/high 的前提下，最深的三次终裁调用全部失败，四类结构缺陷均不由推理深度决定（效力限定见上文"上下文"末段）。
- **强制检索 P10.1**：未采用。命中率 90%，成本与回退风险不可接受。
- **无条件补齐整包政策**：未采用。C 组已实测证伪，完整证据反而下降。
- **在运行时复制 `claim_sources` 引用表**：未采用。等于把评分合同搬进生产并违反 ADR-0030；它能消除 UNSUPPORTED_CLAIM，但对"必需主张被整体省略"（DEV-020、S5V3-004）无效。
  > **散文与程序的区别（独立审查要求明确）**：`FINAL_SYSTEM` 确实含一份散文式引用清单，其内容与 `claim_sources` 的政策集合相对应。但它**逐字继承自冻结的 v1 `AGENT_INSTRUCTIONS`**（`support_agent.py` 的 CITATION CHECKLIST 段），不是本候选新写的第二个评分器。区别在于：散文清单是给模型的**指引**，模型可以读错、忽略或误用，其结果仍由 `claim_sources` 独立判定；而把该表实现为运行时代码会让执行器**替模型决定**哪条主张可被引用，并在不满足时拒绝输出——那才是 ADR-0030 禁止的"Python 执行政策谓词"。本候选不引入任何新的程序化引用表，`DEFINITION_ALIASES` 只用于导航展示与离线度量，从不参与 proposal 校验。
- **终裁允许一次检索**：未采用。与 ADR-0033 的"终裁无工具"核心设计直接冲突。
- **允许终裁返回有界的"证据不足"处置**：未采用为通过手段。它不改变评分结果（仍不达 gold），只把失败模式从"错误引用"改为"诚实失败"，可作为后续安全性改进单独提出。

## 验证与退出条件

免费层，按**实际被测试断言的内容**列（独立审查要求不得声称未测的检查）：

- v1–v4 提示词字节冻结：由 `python/tests/test_support_agent_v5.py` 对 `SYSTEM_INSTRUCTIONS`、`AGENT_INSTRUCTIONS`、`VERSIONED_GUIDANCE`、`CONDITION_REVIEW`、`FINAL_INSTRUCTIONS` 五个常量断言 SHA-256。**注意**：仓库中并不存在 schema 9–12 的 golden profile-hash 测试（只有 schema 1 有），所以"profile hash 不变"不是由 golden 测试保证的，而是由"所有 schema 9–12 分支逐字未改 + `SupportProfileHash` 输入未变"保证，并由既有 Go 测试与审查者的 `git diff --quiet HEAD` 交叉核对。
- v5 终裁 system 不含任何工具诱导文本、body 移除全部工具额度与导航键、body 键集为调查 body 键集的子集、初稿不泄漏且不可审批：由 `test_support_agent_v5.py` 断言。**body 侧只断言键，不断言工具名子串**——真实开发集中 DEV-037 的承运商 note 就含字面 `get_order`（注入文本），那是不可信证据而非工具诱导。
- `definition_retrieved` 在"仅有 P05.2 无 P05.1"时为 false 而 `retrieved` 仍含 P05.2；`DEFINITION_ALIASES` 键集等于 `POLICY_TOPICS` 键集且逐条为其子集（防止新增话题在投影处抛出未测的 KeyError）。
- `_bounded` 超限必须 `stop=True` 失败关闭、终裁步拒绝 `correction=True`。
- schema 13 在**真实 PostgreSQL** 下继承冻结的终裁契约：`TestRunAdjudicationV5InheritsFrozenFinalContract` 断言 v2 executor 绑定、审批与恢复能力保留、thinking 仍只绑定 `model_proposal`、16,384/120s 许可、audit schema 2、`KnownTokens` 16,484 与 schema 11 逐项相同、恰好一次终裁调用，并覆盖 `tool_rejected`（DEV-036 形状）终态失败且不重发。原有 `TestRunAdjudication*` 三组保持 schema 11/12 不变。
- 固定 Linux 镜像三层（process/python/integration）、全仓 `go test -race`、Python 三组套件、Ruff/mypy 六域、SQLFluff 基线、Buf lint 与本地链接检查通过；最新 head 的 8 项 CI 仍须在合并前通过。机制层使用合成模型，不替代真实质量结果。

收费层按序执行，每批前形成具体有限 manifest 并取得独立放行，每批后停发并追加累计账本：

| 序 | 批次 | 估算 | 上限 |
|---|---|---:|---:|
| 1 | 终裁投影固定输入重放（已见 20）：验证重放模块与真实终裁 HTTP 路径可用 | ¥0.83 | ¥1.5 |
| 2 | Agent 开发回归 40（v5） | ¥1.20 | ¥2.5 |
| 3 | 已见 v3 回归 20（守卫 004/018/020） | ¥1.20 | ¥2.0 |
| 4 | Fixed 开发回归 40（schema 12，历史分数不可复用） | ¥0.80 | ¥2.0 |
| 5 | 冻结 + 新建未见 20（独立新颖性审查） | ¥0 | — |
| 6 | 正式 Agent 20 | ¥1.20 | ¥2.5 |
| 7 | 同包正式 Fixed 20 | ¥0.50 | ¥1.5 |
| 8 | 恢复对照 11 + 实模演示 1 | ¥0.67 | ¥2.5 |

估算合计约 **¥6.40**，上限合计 **¥14.5**，均小于提高后的余量 ¥18.724359；每批上限亦不超过 ¥4。单价依据上一轮实测：开发集调查 ¥0.0112332/案、终裁 ¥0.0152953/案；已见集调查 ¥0.0143793/案、终裁 ¥0.0411672/案。**凡含调查阶段的批次（2、3、6）一律按 B 可能增加检索对调查单价上浮 30%**，终裁单价不上浮；批次 8 按已见 Agent 全案单价 ¥0.0555465 × 12 次运行计。批次 1 的 ¥0.83 即上一轮已见终裁阶段实测 ¥0.823344；批次 4/7 的 Fixed 无调查阶段，其终裁单价从未实测，故不确定性最大。固定输入重放只能验证终裁侧（A 与 C 的终裁部分），**验不了 B 与 D**，因为它们改变检索行为而重放的定义就是冻结已检索证据。

**批次 1 不能证明 tool 返回已归零**（独立审查 NB-10）：对一个 1/839 的缺陷做 20 次重放，即使零命中，其 95% 置信上界仍约 14%。批次 1 的实际目的是验证**重放模块本身**与**真实终裁 HTTP/审计/计量路径**在 v5 投影下可用，并给出终裁耗时与 reasoning 余量的实测分布。"工具诱导已被移除"的证据是**确定性**的，不靠抽样：`test_support_agent_v5.py` 对 system 与 body 键集的断言，加上对上一轮 60/60 次终裁载荷的离线重放测量（v4 的 system 含 10 类工具诱导文本、载荷 60/60 带 `remaining_tools>0`；v5 两者均为 0）。

**批次 5 的前置**：上文"公开契约与数据影响"所列 `freeze.py` / `package.py` 扩展已落地并有测试（(13,12) 可冻结、绑定 v1+v2+v3 三个已见正式包、`s5-agent-v5` 与 v5 提示源码均入 `source_hashes()`、未知 dataset 失败关闭）。但仍有两项必须在授权未见集**之前**完成：

1. **绑定 `DATASET_V4` 是必要但不充分的**。`package.py` 的 `DATASET_V4` 现为 `None` 占位（未绑定时不承认任何未见集），授权时同一个 dataset id 还必须同时加入：`tools/support_evaluation/evidence.py` 的 S5 dataset 白名单（否则 `DATA_VERSION`）、`internal/run/support_profile.go` 的 dataset 常量、`api/support/s5-agent-v5` 与 `api/support/s5-fixed-v4` 两份 profile-schema 的 `resources.dataset_id` enum，以及 `tools/support_evaluation/test_registration_routing.py`。漏一处就会在登记或评分处以 `DATA_VERSION` / `PROFILE_UNAVAILABLE` 失败。
2. **schema 13 的冻结必须在本轮改动提交之后创建**。`freeze.create` 要求 `git status --porcelain` 干净，且 `source_hashes()` 绑定 `tools/support_s5` 自身，本轮改动后它与任何既有回执都不同。因此批次 5 的前置是先提交，再冻结。

**v1 历史重评路径必须保持可用**：`prior_families` 改为失败关闭后曾连带拒绝 v1（首个未见集，本无前置正式包），而重评已见 v1 正式集是 README 明确支持的 `--seen-diagnostic` 路径。现已加显式 v1 分支返回空集并由测试钉住，未知 dataset 仍然抛 `UNKNOWN_DATASET_PRIOR_FAMILIES`。

批次 1 需要新增一个重放准备模块，不能直接复用 [tools/support_diagnosis](../../tools/support_diagnosis/README.md)：该工具绑定的是 v3 时期 `formal-agent-v3-20261008` 的证据与它自己的 `FINAL_ONLY` 常量，而不是 v4/v5 的终裁投影。新模块须从 `adjudication-20261011` 的逐案导出重建 marker 之前的 checkpoint（复用 `tools/support_evaluation.evidence.validate_run_sources`，`tools/support_s5/adjudication.py` 已用同一路径），再调用 v5 adapter 的终裁投影生成请求，并按原有谓词与 `claim_sources` 评分。其"准备"半边完全离线可测，只有"发送"半边进入批次 1 的 manifest 与费用。

门槛沿用不放宽：开发 40 正确与完整证据各 ≥36/40、四类硬安全失败 0，且 004/018/DEV-020 全部解决后才冻结；冻结后新建未见 20 案，正式 Agent 正确 ≥16/20、完整证据 ≥18/20、硬失败 0，通过后才启动恢复对照与演示。开发集前 10 案先审语义；已不可能达门槛或出现系统性格式失败即停止后续提交，全部失败与未执行仍留在原分母。任一门槛失败即记录失败并停止后继，不改本轮冻结候选、不追加样本、不选择性补跑；后续修订另经诊断、独立审查与新冻结，费用仍累计在原上限内。

**停止规则同时读两种闭合**（独立审查 BLOCKING-4）：`adjudication_recoverable=false` 比例仍大于 0，说明终裁面临不可引用的必需政策；`investigation_definition_complete=false` 比例仍大于 0，说明调查仍未读到定义所需规则——**后者才覆盖 S5V3-004 形状**，因为 `claim_sources` 对 conflict 接受 P05.2，仅看前者会对三个已知案守卫之一的复发失明。任一比例大于 0 都是启动本轮明确不做的第 1、2 项的信号，须由新 ADR 承接。

**费用授权的证据状态（诚实记录）**：¥20→¥28 的上限提高来自维护者于 2026-10-12 在本工作会话中的明确批准。按 [ADR-0030](0030-s5-policy-condition-candidate.md) 第 7 行引用仓库外记录及其 SHA-256 的先例，该批准已转录为仓库外记录 `E:/JobForgeEvidence/s5-2026-10-07/budget-authorisation-20261012/authorisation.json`，SHA-256 `16a54ef623b373f04d17c0188bc5d5d475ba925e90fc39ce7efca5f4977fe852`，内含逐批上限、批准原文、账本上下文与该批准**不包含**的事项。**它不是独立签名的外部授权收据**：既有各批收据记录的都是 `20000000`，本记录只转录会话内批准，且会话批准无机器时间戳，故只断言日期、不编造时刻。费用仍按冻结费率与 observed usage 记为保守账本值，不称已结算账单；unknown/full hold 不记成零费。每批仍需自己的有限 manifest 与独立放行，本次批准不构成逐批放行。

## 独立设计审查

按 ADR-0030/0033 的先例，本候选在实施后、转 Accepted 前经过一次**全新上下文、对仓库只读**的独立设计审查。审查记录位于仓库外 `E:/JobForgeEvidence/s5-2026-10-07/v5-design-review-20261012/`：`design-review.md`（SHA-256 `63e0ca4e6642ec49c7f8b3478e44fbfb8da1fcedd76c752912452a8dc973551e`）与 `design-review.json`（SHA-256 `256e6d4400d21f789c4271f7a57e4aae44e56e0b6e20a7b493b2a2578a6debde`），另含 21 份支撑脚本与机器可读测量。

审查结论为 `accept_with_conditions`，四项 BLOCKING 全部为可编辑修复、无一要求重新设计：(1) `freeze.py`/`package.py` 未扩展导致批次 5–7 不可达且新颖性守卫空转；(2) 冻结源绑定清单缺 `s5-agent-v5`；(3) 本候选**自己引入**的提示词矛盾会把排除也变成强制检索（实测逐类命中率 75%–87%，最坏吃满 8/8 工具与 11/12 chat）；(4) 出货的可引用闭合指标无法复现本 ADR 的三案分解，对 S5V3-004 报"可达"。四项均已在本 ADR 与实现中处置。

审查独立复算并**逐字确认**了本 ADR 引用的经验数据（v4 终裁 17,931/748 字节、v5 审查时 13,428 字节、`remaining_tools>0` 60/60、`suggested_query` 均值 8.5833/12、839 次物理调用中唯一一次 tool 拒绝、正确/错误终裁 reasoning 中位数 930/9,772、最深三次 14,590/12,161/9,772 全败、19/60 的 `structured_conflict` 假覆盖且仅 S5V3-004 需要它、`derive_facts` 对冻结 gold 60/60、全部费用数字精确到 micro-yuan、已见集绑定的是 `formal-package-v3-r1` 而非 `formal-package-v3`），并证明度量改动对已发布验收字段**零扰动**（逐字段重算比对，0 处差异）。审查后为处置 NB-5（覆盖冻结共享常量里残留的"within 1024 output tokens"）与 NB-7（补五类到 topic 键的显式映射），v5 终裁 system message 增至 **13,619** 字节，禁用 token 仍为零命中。审查要求修改的两处过头表述（C 条"不新立规则"、E 条"双向校验"）均已照改，其中 E 条的全分支双向测试已实现，故该限定改为记录"曾过头、已补齐"。

## 后续工作

实施状态、已验证范围、剩余步骤与已知陷阱汇总在[S5 候选 v5 交接说明](../agent-v3/s5-v5-candidate-handover.md)。

- [x] 维护者批准本 ADR 与 ¥28 阶段上限（2026-10-12，会话内批准；授权转录记录见上，非独立签名收据）
- [x] 完成独立设计审查（`accept_with_conditions`，4 项 BLOCKING，记录与哈希见上）
- [x] 实现 v5 适配器、schema 13 注册面与离线度量升级
- [x] 处置审查的 4 项 BLOCKING：(3) 提示词矛盾已修并补 UNDETERMINED 定义；(4) 已实现 `definition_closure` 双口径，60 案复算为可引用 2/60、定义 3/60（57 完整→55 正确、3 不完整→0 正确），镜像测试扩到全 17 分支 × 20 别名双向；(1)(2) `freeze.py` 的 (13,12) 冻结路径、v1+v2+v3 三已见包绑定、`s5-agent-v5` 与 v5 提示源码入 `source_hashes()`，`package.py` 未知 dataset 失败关闭
- [x] 修复失败关闭引入的 v1 历史重评回归（`prior_families` 加显式 v1 分支，未知 dataset 仍抛错），并补测试
- [x] 补齐 ¥28 上限的维护者授权记录并在本 ADR 引用其 SHA-256（非独立签名收据，已如实标注）
- [x] 全部 BLOCKING 处置并重跑免费验证后，本 ADR 转 Accepted
- [ ] 实现批次 1 所需的终裁重放准备模块，并离线验证其请求构造与评分
- [ ] 按序执行批次 1–8，逐批 manifest、独立放行与账本追加
- [ ] 依实际结果更新[当前状态](../status.md)与 [S5 验收矩阵](../agent-v3/s5-acceptance.md)，并新增证据文档与机器摘要
- [ ] 若任一闭合指标仍报缺口，提出协议级 discharge 列表或终裁协议纠错的后继 ADR
