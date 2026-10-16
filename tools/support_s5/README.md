# S5 公平对照与冻结

新开发批次使用[配置驱动的共享工作流](host/README.md)和 [ADR-0039](../../docs/adr/0039-s5-startup-review-and-automatic-health.md)：一次启动审查，逐案自动健康检查，第 10 案自动继续；质量失败与安全停发分开。下文旧版本的执行顺序与暂停条件保留作历史背景，新批次以 ADR-0039 为准。当前 v8 已实现，直接准备完整开发回归，不补诊断。本次工作流改造不授权收费。

当前候选按 [ADR-0038](../../docs/adr/0038-policy-bundle-before-adjudication.md) 实现完整政策前置读取：Agent schema16 / prompt-v8 / executor5，Fixed schema17 / prompt-v2 / fixed-executor2。Go 在任何模型调用前执行 `read_policy_bundle`，一次业务 HTTP 取得授权快照索引全部 20 段，无 embedding；工具上限 32 KiB，原搜索仍 top-3/8 KiB。调查保留事实读取选择，final 使用同一提示、事实与完整政策，不带初稿、工具额度或补证分支；只接受嵌套 final envelope，不做格式重试。每 Run 仍为 8 工具/12 chat/44 物理调用，两策略均为一次 16,384 completion / 120 秒终裁，family token 12,610,560。完整政策可读不代表业务质量通过。

新冻结只接受 `(16,17)`，绑定三个已见正式包。build receipt 必须使用 `source_hashes(16)`，在旧集合外纳入 `internal/runprotocol/v2`、`internal/runinput`、executor v2 schema 与生成的 Agent RPC 代码；正式冻结仍要求干净提交。新未见集尚未创建，`DATASET_V4` 尚未绑定；届时同步 `s5-agent-v8` / `s5-fixed-v5` 的登记路由与 dataset 枚举。

历史 v7 候选为 [ADR-0037](../../docs/adr/0037-status-policy-read-prerequisite.md) 的 schema 15 / `support-agent-prompt-v7` / `linux-v2-adjudication-runtime-4`。调查沿用 v5；marker 后 Go 核验权威 `informational_only` 所需 P08.1 或 `escalated` 所需 P08.2，缺段落时执行原状态查询，实际取得后才允许首次 thinking。读取不决定行动，已有段落不重读；相同查询已执行而仍缺段落则失败关闭。前置读取不占用 [ADR-0036](../../docs/adr/0036-s5-controlled-policy-supplement.md) 的一次模型补证机会，但共用 8 工具/12 chat；自动读取后不能再次请求状态查询。无格式重试，已停止的 v5/v6 批次不续跑。两次 thinking 各按原 16,384 completion 上限计量，family token 为 12,625,920；Fixed schema 12 不变，能力及最大 completion 容量不对称须在配对报告中保留。

schema 15 冻结使用 `(15,12)`，与 schema 13/14 一样绑定三个已见正式包；新颖性、包加载与直接正式评分均拒绝把旧集重标为未见集。新未见集尚未创建，`DATASET_V4` 仍未绑定；创建时同步当前 `s5-agent-v7` 与 `s5-fixed-v4` 的登记路由和枚举，不改历史候选定义。现有 `support_recovery` 计划入口不接受 schema 14/15，后续恢复验收须单独扩展其版本与证据校验，不能直接复用旧计划。

独立终裁候选见 [ADR-0033](../../docs/adr/0033-versioned-final-adjudication.md)：schema 11 Agent / 12 Fixed，调查维持原参数，`model_proposal` 使用 thinking/high、16,384 completion token、120s 上限；family token 合同为 12,610,560，其余次数合同不变。注册绑定 `final_model`，两策略配对必须完全相同。新版评分按原调用步骤验证预算与 audit schema；每案 `adjudication_stages` 分开列初稿/终裁质量及 chat 计量，验收仍只看最终方案。原 v3 正式集已见，只可作为诊断，不能再次充当未见验收。

此目录是外部验收工具，不进入生产 Worker。历史40案、schema 1–8 与原评分结果保留。第二次正式失败后的候选使用 schema 9 Agent（`support-agent-prompt-v3`）及行为相同的 schema 10 Fixed，版本选择见 [ADR-0030](../../docs/adr/0030-s5-policy-condition-candidate.md)。原候选v2见 [ADR-0028](../../docs/adr/0028-versioned-support-evidence-navigation.md)。schema 13 Agent（`support-agent-prompt-v5` / `linux-v2-adjudication-runtime-2`）为终裁任务分离与条件枚举候选，见 [ADR-0034](../../docs/adr/0034-s5-final-stage-separation-and-condition-enumeration.md)（Accepted）；实际回放与开发结果见[v5验证记录](../../docs/evidence/agent-v3-s5-v5-validation-2026-10-13.md)。Fixed 仍为 schema 12，未获得 v5 澄清，配对对照存在这一已知不对称。

旧版两种策略都使用 `deepseek-flash` / DeepSeek-V4.1-Flash、thinking disabled、temperature 0、1024输出token、json_object、消息64KiB/请求128KiB/响应64KiB/内容16KiB。每个family上限12 chat、8逻辑工具、8 query embedding、16 metadata HTTP、8业务HTTP、44物理HTTP、1纠错、12,595,200 token、5,000,000 microyuan；批次持久额度另限制，不能通过新Run绕过。旧固定图生成1次chat及至多1次纠错；新版终裁不纠错，额度不要求把次数耗尽。

## 开发回归与正式集顺序

[ADR-0035](../../docs/adr/0035-s5-batch-reservation-and-v5-replay.md)将 ADR-0034 各收费批次上限统一为 ¥4，S5 累计仍为 ¥28，逐调用预留不减。schema 13 正式入口拒绝三个已见 dataset，诊断重评分仍支持。批次 1 使用 `python -m tools.support_s5.replay prepare --root ROOT --output NEW_EXPERIMENT --runner RUNNER --preflight PREFLIGHT --authorisation APPROVAL --database-name DATABASE --database-id UUID`；它只读验证原 v4 的 118 次请求并生成 20 次 v5 终裁，不调用模型。实际发送复用隔离 diagnosis runner 的 schema 3，具体 manifest 独立放行后才启动。事后用同模块 `score --root ROOT --experiment EXPERIMENT --ledger PG_EXPORT --output NEW_SCORE` 评分。原输入的双闭合缺口单列，不冒充 v5 调查复发，也不计正式验收。

1. 免费实现/机制与具体收费manifest审查通过后才允许第一笔收费。开发构建可绑定不可变的工作树源码快照，不将它称为干净提交或正式冻结。先保存当天官方价格与账号只读状态，记录实际源码、构建和profile摘要。
2. 两策略先在已见40案完整回归；失败和未尝试保留在分母，原模型输出不改。允许开发调优，改变候选后重新形成适用开发证据。
3. 独立接受开发证据后，以干净提交、源码摘要、实际镜像ID及两个prepared profile冻结候选、基线、评分和阈值。命令不读取或创建正式案例：

```powershell
$env:PYTHONPATH='python;sdk/python'
.venv/Scripts/python.exe -m tools.support_s5.freeze --agent-config <Agent control.disabled.json> --fixed-config <固定流程 control.disabled.json> --development-review <独立开发审查JSON> --build-receipt <构建JSON> --out <仓库外新freeze.json>
```

4. 此后才建立并独立审查20案的新场景/模板家族。仅更名、改数字或翻译旧8家族不能成为未见集。新包 `manifest.json` 必须绑定freeze摘要及晚于冻结的创建时间，逐文件hash覆盖runtime、case-map、gold、语义锚与family-review；正式case-map包含template_family。独立family-review明确确认场景/模板新颖性。机器hash/集合校验不能代替该语义审查。
5. `prepare-support` 新source schema 2绑定独立 `case_map` 与 `runtime_data.manifest/seed` 的路径/hash，资源仍在仓库外；source v1保留原40案路径和摘要。分别创建新批次/tenant账户及profile，不覆盖历史数据或账户。

`freeze.validate` 实际用于正式包读取、注册及评分；注册/评分还核对strategy blueprint，仅允许dataset/seed/runtime manifest/index身份按独立资源审查绑定。提示词、代码、模型参数、价格、额度或政策变化会拒绝，需新候选冻结。运行容器按冻结镜像ID核对；源码和gold只供host验收，不进入生产镜像。

## 登记、执行与评分

候选v2冻结使用 `--seen-formal-manifest <首轮 manifest.json>`。候选v3同时传原v1的 `--seen-formal-manifest` 与原v2的 `--seen-formal-v2-manifest`，新数据 ID 为 `support-s5-2026-10-08-v3`；family-review 的 `seen_formal_manifest_sha256_by_dataset` 绑定两个摘要，`novelty_against_seen_formals_confirmed=true`确认对两套场景/模板的新颖性。两个已见20案包只可用 `assemble/score --seen-diagnostic` 读取原包和原 freeze；报告为 `s5-seen-diagnostic`，`thresholds_met` 始终为 false。ADR-0034 候选（Agent schema 13 / Fixed schema 12）冻结时三个已见正式包全部已见，必须同时传 `--seen-formal-manifest`、`--seen-formal-v2-manifest` 与 `--seen-formal-v3-manifest`，回执写出三条 `seen_formal_packages`；缺任一为 `SEEN_FORMAL_PROVENANCE_REQUIRED`，schema 9 及更早传 v3 为 `SEEN_FORMAL_PROVENANCE_SCOPE`。其未见集数据 ID 尚未产生，须在 `package.py` 的 `DATASET_V4` 显式绑定后才被接纳（同时还需登记面 `tools/support_evaluation/evidence.py`、`internal/run/support_profile.go` 与 `api/support/s5-agent-v5`+`s5-fixed-v4` 的 `dataset_id` 枚举），未绑定时任何未知 dataset 都以 `UNKNOWN_DATASET_PRIOR_FAMILIES` 拒绝，家族新颖性门不会退化为空集通过。本轮不另设阶段Run/chat/token预算，prepared批次计数沿用登记数量乘以family合同；费用累计不重置。

host在启动前用 `python -m tools.support_s5.assemble register --config <prepared目录> --out <新registration.json>` 登记开发回归；正式集额外传 `--package <新包根> --freeze <freeze.json>`。Go的prepared配置默认disabled。

官方登记对正式v1/v2/v3均要求20案，开发集仍要求40案；数量和未知dataset负例由[`test_registration_routing.py`](../support_evaluation/test_registration_routing.py)覆盖。[ADR-0029](../../docs/adr/0029-s5-registration-routing-amendment.md)记录原v2登记路由的操作修订，保留原冻结时间与后续实际修订时间；ADR-0030新增v3与两套已见诊断的显式版本范围。

固定Linux外部operator沿用原安装SDK单次Submit、原稳定键/窗口、launcher进程回收及停批规则；S5入口仅显式支持20/40名单。构建先使用冻结production Worker构建 `deploy/Dockerfile.support-approval`，再以其不可变image ID为 `OPERATOR_IMAGE` 构建 `deploy/Dockerfile.support-s5`。`prepare-state` 后才可启动；完整命令/秘密挂载规则复用[批次指南](../../docs/agent-v3/cloud-batch.md)。

S5 launcher 向 SDK 和 Worker 传递三个追踪配置变量，并向 Worker 单独传递可选 metrics 地址；Worker 凭据不传给 SDK。外部 SDK 在 OTLP opt-in 时安装有界 provider，队列256、批次64、HTTP超时2秒，结束时最多等待2秒导出。固定模型密钥仍由原 executor 凭据文件提供。

收集原SDK archive、全部outbound、独立业务reader的前后audit，以及receipt reader执行[原始回执查询](../support_approval/actions_audit.sql)的前后文件。`assemble` 模式要求 `--registration/--archive/--metadata/--before/--after/--receipts-before/--receipts-after/--out`，正式集同样传package/freeze。再运行：

```powershell
.venv/Scripts/python.exe -m tools.support_s5.score --registration <registration.json> --evidence <evidence.json> --package <正式包> --freeze <freeze.json>
```

正式20案的方案正确≥16，完整案例证据≥18；每条主张真值/来源覆盖和必要主张齐全缺一不可。越权、未经批准写入、重复效果、额度突破为0。所有失败仍在20分母；批准不修复原方案分数。paired输出同一完整分母的胜/平/负及相同model/resources/family限额校验，不要求Agent胜过基线。

费用不完整时 `usage_total=null`，仅提供 `verified_usage_subtotal`；阶段账本缺失或损坏时 `adjudication_stages.*.usage=null`，完整 unknown 保留 full hold。`no_reasoning_signal_calls` 表示正文和 usage 均无推理信号，不代表精确空正文数量。持久预算权威始终是所有S5批次known+held（含在途），配对费用保持同样口径。恢复/从头对照另记录冻结故障时点、所有实际调用、known/held与活跃时间；检索20查询另列为本地MiniLM诊断。

## 逐案诊断与阶段汇总（ADR-0034）

schema16/17 均输出摘要 schema3 与实际 `final_input_closure`；Fixed 保持无 marker、无 draft 比较，单次终裁保留原 `reasoning_budget` 结构。bundle 实际来源计入双闭合，未提交 final 不计闭合通过。

schema 14 增加 `final_input_closure`：以唯一已提交 final 的输入为锚，包含 `step_index`、`retrieval_closure` 与 `definition_closure.definition_complete`。补证 intent 不是 final；未到达或未提交 final 返回 null，不计通过。原 marker 指标原样保留，不能用补证后的完整输入掩盖原调查缺口。新批逐案守卫必须检查最终输入的两种闭合；缺口停止当前批次剩余提交和后继批次，不能继续使用只读 marker 的 v5 守卫。

[guard.py](guard.py) 实现 v6 开发 40 案宿主停发规则：最终双闭合、已验证来源与计量、硬失败、累计费用及剩余案例可达门槛。成功态缺闭合度量停发；未到 final 的失败保留在原分母。宿主在每案导出后评分，只向 driver 返回 continue/stop；第 10 案另须绑定实际 score、ready 与 manifest 的独立审查。守卫和 gold 不进入 Worker/operator 镜像，受审 manifest 绑定宿主源码。

schema 14 的 `reasoning_budget` 改为 `calls` 数组、全阶段 `usage` 和 `truncation_risk`：每项独立报告物理调用 ID、reasoning/completion、实际预留容量、裁剪后的期限及余量，缺失值保留 null。风险按单次 completion 超过容量 90% 或墙钟超过实际期限 80% 标记，不把两次用量相加后除以一次容量。摘要升为 schema 3，新增 `final_input_closure_measured`、`final_input_unrecoverable`、`final_input_definition_incomplete` 及对应案例列表；旧版本仍使用下述 schema 2 结构。

以下字段均为**加法、仅离线**，不进入生产运行时，也不改变 `correct` / `full_case_evidence` / `thresholds_met` 任何验收口径。汇总放在仓库内受审代码里，正是因为上一轮的盲点来自仓库外未受审的批次脚本：它只比对案级 `correct`，于是在终裁把一条成立且引用完整的主张换成不成立主张时仍报"0 回退"。

逐案（`adjudication_stages` 内）：

- `claim_level_delta`：`{draft_claims, final_claims, added, removed, flipped[{kind, from, to}], evidence_regressed, evidence_improved}`。按 `coverage_key` 而非裸 kind 分组，因此 `missing:<field>` 与 `ticket_status:<mode>` 不会互相掩盖；"成立"= 谓词真且来源覆盖，与两个评分器已有的覆盖语义一致。`draft_claims`/`final_claims` 为逐主张 `{index, kind, predicate, source_coverage}`，终裁一侧直接透传已接受的 `claim_checks`，不二次计算。仅在实际评出初稿、且两侧主张与其 coverage key 数量一致时非 null；`evidence_regressed`/`evidence_improved` 独立于案级 `correct`。
- `retrieval_closure`：`{available, required[{required, accepted_aliases, present_aliases, satisfied}], unsatisfied, adjudication_recoverable}`。`available` 只取 marker 之前已提交 `search_policy` 或新 `read_policy_bundle` 步骤返回的段落 `chunk_id`，事实指针不计入。`adjudication_recoverable=false` 表示**不存在任何合法终裁输出能满足来源覆盖**——终裁无工具，缺的不是推理而是政策，必须与"答错"分开统计。**但 `adjudication_recoverable=true` 不是通过保证**：该口径只镜像 `claim_sources` 的政策别名集，既忽略同一谓词里的事实指针组（`groups`），也不区分"定义该类别的段落"与"同话题的兄弟段落"。它回答的是"是否可能存在一个被完整取证的合法终裁方案"，是必要条件而非充分条件。`accepted_aliases` 镜像 `predicates.claim_sources`（含 `outstanding_overdue_ge48` 只接受 P07.1、`facts.timing` 不可导出时接受集为空），并由测试对全部 17 个（类别, timing）分支在完整 20 个别名上**双向**校验一致；`claim_sources` 仍是唯一验收谓词。以 marker 为锚，Fixed 无 marker 故为 null。
- `definition_closure`：`{required[{required, topic, definition_aliases, definition_present, definition_satisfied}], definition_unsatisfied, investigation_definition_complete}`。**另一种、且更严的闭合口径**，回答"调查是否真的取到了定义该类别的那一段"。类别→话题映射为 conflict→structured_conflict、dispute→delivery_dispute、critical→critical_exception、timing→timing、correction→carrier_correction、`missing:*`→missing_facts、`ticket_status:*`→ticket_status；定义段表**直接 import** v5 的 `DEFINITION_ALIASES`（`python/jobforge_agent/support_agent_v5.py`），不在此处复制，故提示词侧漂移会被测试立即打断。`definition_present` 是该话题定义段中实际已检索到的子集，`definition_satisfied` 为其非空。两个口径**并列输出、绝不合并，且互不蕴含**：S5V3-004 因一次纠正检索取到 P05.2 而 `adjudication_recoverable=true`，但定义五类冲突的 P05.1 从未被读到，故 `investigation_definition_complete=false`；反向同样可能（`outstanding_overdue_ge48` 只接受 P07.1，只取到 P02.1 时定义闭合却不可达）。复现已知案分解的是**定义闭合**：对 `adjudication-20261011` 全部 60 案实测，定义完整 57 案→55 正确，不完整 3 案（DEV-020、S5V3-004、S5V3-018）→0 正确；同一批证据上 `adjudication_recoverable=false` 只有 2 案（DEV-020、S5V3-018）。因此"下一轮是否仍需协议级 discharge"这类判断要看定义闭合，不能只看可达率。同样以 marker 为锚，Fixed 无 marker 故为 null。
- `reasoning_budget`：`{reasoning_tokens, completion_capacity, ratio, wall_seconds, deadline_seconds, deadline_headroom_seconds, truncation_risk}`。`truncation_risk` 在 reasoning 超过 16,384 的 90%，或墙钟超过 96 秒（120 秒截止的 80%）时为真。它是观测标记，不是评分失败；没有已观测的终裁调用时为 null。

阶段汇总（`score.py` 输出的 `adjudication_measurement`，摘要 `schema_version` 由 1 升为 2）：`{evidence_regressed, evidence_regressed_cases, evidence_improved, evidence_improved_cases, retrieval_closure_measured, adjudication_recoverable_false, adjudication_recoverable_false_cases, definition_closure_measured, investigation_definition_incomplete, investigation_definition_incomplete_cases, truncation_risk, truncation_risk_cases}`。未测得的案被跳过而**不计为通过**，因此两个闭合口径各自带显式分母 `retrieval_closure_measured` / `definition_closure_measured`，两者可以不同。定义闭合字段仍属同一版本 2 的加法内容（版本 2 尚未由任何已接受批次发布，已发布摘要均为版本 1），故不再升版本。已发布摘要与冻结回执按 sha256/kind/denominator 绑定，`freeze.py`/`package.py` 均不校验摘要 `schema_version`，历史 schema-1 摘要保持字节有效。
