# JobForge PRD v0.7：可恢复业务 Agent

- 日期：2026-09-16。
- 状态：本详细契约随 [PR #35](https://github.com/XJfyrh/JobForge/pull/35) 于2026-09-16通过S0评审；[路线 v3](../plans/agent-execution-roadmap-v3.md)的S1～S5功能尚未实现或验收。
- 决策记录：[ADR-0013](../adr/0013-durable-agent-run-and-step-commit.md)、[ADR-0014](../adr/0014-supervised-python-executor-and-call-budget.md)、[ADR-0015](../adr/0015-approved-business-actions-and-receipts.md)。
- 实施与验证：[S0 记录](../agent-v3-progress.md)。旧版本验收保持历史事实，不构成本版通过证据。
- 2026-09-16 模型主线修订：按维护者反馈采用云端主chat，本地chat为可选扩展；该调整发生于S0评审期间，不改写此前已接受ADR或本地试验历史。

## 1. 产品范围

实现订单交付异常处理 Agent：提交工单 → 模型选择订单/物流/政策查证工具 → 生成有证据的方案 → 人工批准 → 幂等更新业务工单 → 查询步骤与回执。

三个预注册只读工具为 get_order、get_delivery、search_policy。唯一写动作为 apply_ticket_resolution，只能由执行控制面根据批准记录派发。首轮可记录结论、标记待补充或升级人工跟进，不自动关闭工单，不接真实客户、付款、退款或邮件。

固定合成数据允许用于业务验收，但模型推理、embedding、向量检索、HTTP 工具、数据库更新及故障过程必须真实。模型协议探针中的工具 fixture 不构成业务验收。

新版本以 Run 为唯一可调度实体，不在 job 之上再建另一套 Agent 调度器。旧任务类型、接口、数据迁移和双版本运行不属于需求。v0.6 的独立 rag.index/agent.extract 产品验收、纯 Go 模型接入及不支持步骤恢复的限定，在新版本由本 PRD 的场景取代；不更改旧报告或未经实施宣称已替代默认服务。

## 2. 不变量

1. at-least-once；外部调用可能重复，业务接收端负责幂等。
2. PostgreSQL 是 Run、步骤、审批、预算和动作授权的唯一事实源。业务服务独立管理业务对象与接收回执。
3. Claim 原子更新 owner、lease、attempt_no、fencing_token 与状态；同一 Run 只有一个当前执行权。
4. 接受步骤、派发授权及正常执行写入必须验证状态、owner、token、有效 lease 和预期游标。lease 以数据库时间判定，过期即拒绝，不等待再次 Claim 才失效。
5. 终态不回退。人工 retry 创建新 Run。审批继续、故障重投使用原 Run。
6. 状态转换集中于 domain/service；外部网络不在数据库事务内。
7. 输入、模型和工具内容不授予权限，不决定任意代码、URL、tenant、数据库或动态能力。

## 3. Run 与 attempt 状态契约

Run 状态为 ready、running、retry_wait、awaiting_approval、stopping、succeeded、failed、cancelled。stopping 保存首次停止原因 cancel/run_deadline/attempt_timeout，统一表达停止执行但尚未释放进程资源的阶段。首轮不暴露延时任务、cron、优先级或通用 DAG。

| 事件 | 前态 → 后态 | 原子内容与限制 |
|---|---|---|
| 提交 | 无 → ready | 参数/profile/资源归属校验；请求幂等键及 hash |
| Claim | ready → running | 有界容量、owner/lease、attempt_no+1、token+1 |
| 提交中间步骤 | running → running | 首次结果、游标版本、下一步骤及事件一起提交 |
| 提交待审批方案 | running → awaiting_approval | 方案/审批绑定、关闭当前 attempt、释放租约与容量 |
| 批准 | awaiting_approval → ready | 审批决定与下一游标原子保存；重复相同批准不重复入队 |
| 拒绝 | awaiting_approval → succeeded | outcome=rejected、结果引用；无写入 |
| 无动作方案 | running → succeeded | outcome=no_action、结果引用 |
| 写入回执完成 | running → succeeded | outcome=applied、首次回执引用 |
| 暂时故障/失联 | running → retry_wait 或 failed | 原游标保留，结算 attempt，按 recovery_count 判定 |
| 退避到期 | retry_wait → ready | 不修改已完成步骤 |
| 永久故障 | running → failed | 稳定错误分类、关闭 attempt、释放容量 |
| 非运行态取消 | ready/retry_wait/awaiting_approval → cancelled | 等待审批立即失效；无新执行 |
| 运行态停止 | running → stopping | 原因是取消、Run期限或attempt期限；阻止新调用/步骤/动作授权并发停止信号 |
| 停止期间取消 | stopping → stopping | 记录cancel_requested事实，不覆盖首次停止原因；禁止后续恢复 |
| 停止确认/租约到期 | stopping → 对应状态 | 已接受取消→cancelled；否则总期限已到→failed；否则attempt_timeout按恢复余额进入retry_wait或failed |
| 等待期间期限到期 | ready/retry_wait/awaiting_approval → failed | RUN_DEADLINE_EXCEEDED 或 APPROVAL_EXPIRED，不归咎于Worker失联 |

attempt_no 每次 Claim 递增，用于审计；不直接决定故障预算。recovery_count 只在暂时故障/失联后实际安排自动恢复时递增。默认 max_recoveries=3，即首次执行外最多三次故障恢复机会；下一次需要恢复而无额度时 failed。正常审批继续不消耗 recovery_count。重复 Fail/回收不增加计数。

attempt outcome 至少有 yielded_approval、succeeded、failed_retry、failed_terminal、lease_expired_retry、lease_expired_terminal、cancelled。时长只统计该次活跃持有执行权的时间；审批等待单列。

首次停止原因由同一 Run 锁串行固定，供审计；后到取消仍可记录独立cancel_requested事实，后到总期限也必须阻止恢复。收敛时依次检查已接受取消、当前Run总期限，再决定是否因attempt_timeout消耗恢复次数，不能因为第一次停止是段超时而忽略后到取消。默认 lease TTL=30s、heartbeat=5s、恢复扫描=1s；Heartbeat 不能续租超过 attempt_deadline 或 run_deadline，在 stopping 只传达停止/确认，不延长执行权。到期比较使用取得行锁后的数据库时钟，等号视为到期。批准也在事务内检查期限，不依赖扫描器及时运行。

业务 outcome 和执行状态分开：rejected/no_action/applied 均可表示正常完成；基础设施、预算或协议错误为 failed。动作回执 unknown 不能伪装为 applied。取消终态可另显示实际效果 applied/unknown，不能声称取消撤销了效果。

## 4. 步骤和恢复

每个 Run 使用不可变 profile、版本化业务引用和有界 checkpoint。步骤由 step_id、sequence、kind、input_hash、output_ref、commit_hash、cursor_version 标识。步骤无独立 lease 或后台调度。

业务服务在提交准备阶段生成不可变 snapshot_id，绑定工单、订单、物流集合 aggregate_revision 和当前适用政策 revision；所有只读工具读取同一快照。新增物流事件也必须递增聚合版本。动作事务对比当前权威版本向量，避免多轮读取混合时点或只检查已读物流行遗漏新事实。

服务端根据已提交模型响应校验工具选择/最终方案后生成下一步骤；Worker 提出的游标不作为事实。重复提交先验证当前执行权，再识别完全相同的已接受提交；陈旧 owner/token 不因步骤已完成而得到成功 ACK。首轮仅顺序步骤，同一时刻最多一个模型/工具请求。

上述重复 ACK 适用于仍持有有效 lease 的中间步骤。yield/终态提交会释放 lease，若响应丢失，Worker 改为授权查询已接受步骤及其完整 commit_hash，不再把重报提交视为有效写入，也不因此报 Fail 或消耗恢复次数。只读确认不向旧 Worker 授予新执行权。

模型响应尚未落库的步骤可以重执行并再次计费；已提交步骤必须复用。profile 不匹配或资源版本丢失时明确失败，不静默替换模型/规则。SDK 查询、恢复读取及已有回执复用先于新增模型额度检查。

checkpoint 可保存后续执行必要的有界业务内容，按租户隔离并由授权接口查询；普通事件、日志和 Trace 只保存元数据/引用，不保存完整输入输出、秘密或模型内部推理。

## 5. 审批、业务授权和回执

审批绑定 tenant、run_id、方案 hash、操作者权限、有效期及全部决策依赖版本，至少包括工单、订单、物流和政策。首轮一个审批点，仅批准/拒绝；不在线编辑方案，不自动重新审批。

审批/许可截止时间为方案提交后1小时与 Run deadline 的较早者，批准、排队和恢复均不延长。动作授权截止不超过这一许可时间和 Run deadline；到期后只能查询既有回执，不再首次写入，以 ACTION_AUTHORIZATION_EXPIRED 结束。接收端仍先返回相同已应用操作的回执，再检查首次执行许可。

批准/拒绝请求先做当前鉴权，再查其操作幂等键：相同已接受决定返回原结果，不要求 Run 仍处于 awaiting_approval；新决定才验证状态、方案版本和期限。同键异内容 CONFLICT，不能以网络重试再次入队。

通过审批并不直接写入。Worker 在有效 lease 下请求动作派发授权；服务端与取消锁同一 Run 行，固定 operation_id、精确参数 hash、批准引用和前置版本。参数在首次授权后不可改写。

业务接收端在一个事务内先查 tenant+operation_id：同内容已接受则返回首次回执，异内容冲突；尚未接受才校验批准证据、期限、对象关系和全部前置版本，并原子写入工单及回执。先判断版本再查回执会误拒已经成功的重发，禁止采用。

所有前置版本由本地业务服务维护可原子核验的权威版本/指纹。规则索引引用是不可变版本；写入时业务服务验证当前适用规则版本，客户端先查后写不足以构成保护。

ACTION_CONFLICT 终止旧方案，重新核查需新 Run 和新批准。重新核查若仍属于同一业务请求且已有授权操作，则先查询原操作：已应用复用回执；尚未确定不能改写动作内容。明确的新业务请求才建立新操作身份。

派发授权先于取消提交时，动作可能完成；取消不撤销在途调用。取消终态后控制服务只允许有界只读查询既有 operation_id，补齐效果视图，不重开 Run、不主动重发写入。无回执则 unknown。正常故障恢复可按同一 ID 查询/重发相同已授权内容。

人工 retry 只允许 failed/cancelled，创建新 Run 并记录 retry_of；默认不导入旧游标。新 Run 继承业务请求身份、已授权动作身份和费用批次，先查可复用回执；不存在可确认回执才按契约继续，不能重置预算或制造第二效果。

人工新 Run 若发现旧 Run 已有派发授权但回执未知，则以 ACTION_OUTCOME_UNKNOWN 结束，不重发旧授权、不续签、不重新生成方案。只有原 Run 的正常非终态恢复可在原授权有效期内重发相同动作。首次授权前的失败可人工 retry 重新核查；已应用动作直接复用。此范围避免新 Run 借旧审批改变授权对象或延长不确定写入窗口。

## 6. 调用、安全和资源预算

| 范围 | 初始约束 |
|---|---|
| 主后端 | 一个云端主chat用于开发、正式评测与演示；embedding独立选择一个固定真实profile；本地chat可选 |
| 模型/工具总量 | 每 Run 最多 12 次 chat、8 次只读工具及最多 8 次 query embedding；重发均计数 |
| 协议 | 一次一个工具或最终方案；整个 Run 最多一次协议纠正 |
| 时间 | 单外部调用≤60s，单次活跃 attempt≤180s，Run 总期限≤24h（含审批） |
| 输出 | chat≤1024 token 且≤16KiB；单工具≤8KiB；持久 checkpoint≤256KiB；报告≤16KiB |
| 输入 | 业务输入≤64KiB；模型输入 token/字节上限在具体 profile 中冻结，包含工具定义和协议开销 |
| 调用并发 | 一个 Run 一个在途步骤；租户/Worker/profile 上限在配置中明确且服务端强制 |
| 回执核对 | 单次核对≤10s、每次请求最多一次业务查询，按租户限频；自动恢复最多三次 |

发请求前在有效租约下原子预留调用身份、类别、最大 token/费用与批次余额。reserved/unknown 都占用额度，只有有凭据的 known usage 结算可减少保守预留。网络无响应不当作零用量；不启用模型 SDK/HTTP 客户端隐藏重试。只有授权管理员可创建/增加费用批次。

已授权的物理请求次数不随 usage 结算退回；结算只调整 token/费用。physical_call_id 穿透 Go/Python 消息，同一执行器重复帧不能再次发请求；无法确认命令已执行时保留 unknown，重发需新调用 ID 和新额度。“一个在途步骤”限定当前有效本地执行器，故障后远端旧计算可能与新 attempt 重叠，不能解释为供应商全局并发保证。

云端主线已确定，首选候选与费用方案见[路线v3](../plans/agent-execution-roadmap-v3.md#5-模型安全和费用从第一条链路开始具备)，具体可用账号、凭据和预算B仍待落实。在任何收费请求（包括探测）前必须确认B、计价版本和可保守上界的模式；本轮方向调整不等于费用授权。上述缺口只阻挡实际收费调用，不阻挡云端适配、持久调用账本、真实业务工具和确定性测试的实现。

profile固定供应商/部署地域、服务端配置的HTTPS endpoint、模型快照或可取得的版本标识、参数、执行器及工具Schema；不要求云端供应商提供权重digest。客户端payload或模型不能决定endpoint和凭据，不自动跨供应商切换。embedding独立固定模型身份、维度与索引版本；无论本地或云端均需真实检索验收，收费embedding也纳入额度。

模型提出未知工具、跨租户引用、任意 URL/命令、超长参数或多个并行调用时不执行。工具内容和检索文本仅为数据。持续重复动作或预算耗尽以稳定错误终止，不通过静默删证据或自动无限重试换取成功。

## 7. 新 HTTP/SDK 与错误契约

候选命名空间为 /v2/runs：POST 提交，GET 列表/详情，GET /{id}/steps、/{id}/events、/{id}/result，POST /{id}/cancel、/{id}/retry、/{id}/approval、/{id}/reconcile。事件按 Run 内单调序号游标分页；页面先采用轮询，无常驻运行 span 或强制 SSE。

tenant 和操作者权限来自鉴权上下文，payload 不接受它们作授权。跨租户资源按不存在处理，缺少本租户审批角色返回 FORBIDDEN。审批/取消/人工 retry 均使用独立操作幂等键；同键异请求返回 CONFLICT。

错误仍使用 {"error":{"code":"...","message":"..."}}，并保持脱敏稳定异常类型。HTTP 400 用于 INVALID_ARGUMENT，401 UNAUTHORIZED，403 FORBIDDEN，404 NOT_FOUND，409 用于 CONFLICT/ALREADY_TERMINAL/INVALID_TRANSITION/STALE_LEASE/CANCEL_REQUESTED/STOP_REQUESTED/STEP_CONFLICT/APPROVAL_CONFLICT/ACTION_CONFLICT，429 用于 QUEUE_OVERLOADED/RATE_LIMITED，500 INTERNAL，503 DEPENDENCY_UNAVAILABLE。

模型/执行失败码出现在成功查询返回的 Run 错误字段，不把 GET failed Run 变成 HTTP 失败：MODEL_PROTOCOL_ERROR、MODEL_UNSUPPORTED、BUDGET_EXHAUSTED、TIMEOUT、RUN_DEADLINE_EXCEEDED、APPROVAL_EXPIRED、ACTION_AUTHORIZATION_EXPIRED、ACTION_OUTCOME_UNKNOWN、PROFILE_UNAVAILABLE 等。执行错误是否可恢复由稳定分类决定，不能仅按 HTTP 429 或错误字符串猜测。

内部 Worker 契约与 Python 消息协议独立版本化；不保证旧 SDK 或 Worker 兼容。确切源 Schema 与生成方式在对应实现 PR 中交付，并执行跨语言契约检查。

## 8. 验收和数据生命周期

执行路线 V3-01～12 映射：真实模型/工具、有效租约与原子步骤、自然恢复、暂停审批、受控写入、取消、预算、租户隔离、公平评测、全链路观测和干净环境复现。

40 个开发案例、20 个保留案例，按模板/场景族切分，gold 与运行资源分离。最低候选门槛固定为方案正确率≥80%（至少16/20）、证据正确率≥90%，所有失败保留在分母；证据必须支撑具体结论。安全门禁为未经授权动作、未经批准写入、重复效果、额度突破均为0。计分细则与保留集 manifest 在打开保留集前冻结。

比较相同模型/工具/数据/预算下的合理固定流程；另测相同策略从头重执行与步骤恢复的成本。Agent 不必优于固定流程，但未满足最低可用性不能标完成。依据保留集修改策略后需另建未见集。

初版演示保留规则：终态 Run 和受保护 checkpoint 保存7日；人工 retry 仅在原业务请求创建后7日内；业务操作身份/回执至少保存30日且不短于请求有效期。活动/待审批 Run 不按终态 TTL 清理，总期限收敛后再计时。过窗操作拒绝，不能删除去重记录后重受理旧请求。备份恢复需覆盖控制和业务数据的同一可核对快照，执行前停写演练；不承诺在线跨库一致备份、PITR 或 HA。

Linux 容器是部署基线；Windows 使用 Docker Desktop/WSL2 启动同一版本。适用 Go/Python/SQL/协议/观测门禁均实际执行。真实模型、真实进程故障与替身分层报告；模型探针通过不能替代业务、恢复或质量验收。

## 9. S0 退出与后续阶段

S0交付本PRD、三项ADR、可复现执行器探针及结果、云端候选/profile要求/费用方案；保留已有本地模型试验。契约评审与进程可行性交付可独立完成，本地chat成功不再是退出或契约PR合并条件，云端未调用也不阻挡S1工程实现。云端真实接入验证和S2业务Agent验收单独记录，未实际运行不得报通过。S0不声称业务Run、checkpoint、审批、预算表或写工具已经实现。

后续按 S1 真实业务和基线、S2 Agent/调用治理、S3 持久恢复、S4 审批/写入、S5 评测/观测推进。每阶段保存验证证据，未满足依赖时推进不受影响部分；整体完成需所有主线验收通过。
