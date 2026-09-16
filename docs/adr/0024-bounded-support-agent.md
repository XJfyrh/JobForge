# ADR-0024：有界售后 Agent 与可用成果验收

- 日期：2026-09-17；状态：Proposed，独立审查并合并后 Accepted。
- 对应：[PRD v0.16](../product/JobForge_PRD_v0.16.md)。维护者已确认实施本方案，并授权按任务需要调整合理预算，无需重复确认。
- 仅对新 S2 profile 部分取代 ADR-0018 的固定图/提示、ADR-0020 的固定模型步骤终结判断、ADR-0021～0023 的 S1 首批/5 CNY 准入限制。其余执行权、审计、停止和未知费用规则保留；历史记录不改写。

## 背景与范围

S1 已以 PR #51 合并：40/40 开发案完成，安全硬失败 0，业务正确 11/40。模型仅在固定读工具后调用一次，无法根据证据补充检索。S2 交付单场景、串行、只读动态循环，不引入通用框架、第二供应商、并行工具、审批写入、S3 恢复成本实验或 S5 保留集。

## 决定与步骤

1. 静态登记 `support_agent_v1` / `support-agent-v1`，保留 `support_fixed_v1`。新 profile 固定策略、决定 schema、提示及适配器摘要和执行器版本；S1 profile/结果不被原地升级。旧结果继续可查询。
2. `read_ticket -> model_decision -> 读工具 -> model_decision`，或者 `model_decision -> submit_proposal`。Go 根据已提交决定计算下一步骤；Python 每次仅执行一个 Go 指定步骤，不拥有游标和自由循环。
3. 模型 JSON 使用闭合联合：`{"type":"tool","name":...,"arguments":...}` 或 `{"type":"final","proposal":...}`。工具名仅为 `get_order`、`get_delivery`、`search_policy`；前两者仅含与快照工单一致的 `order_id`（可空），检索仅含 1..512 UTF-8 字节的 `query`。`final.proposal` 使用原六字段模型方案，确定性展开成原八字段持久方案。没有文字推理、动态 URL、租户或代码字段。
4. 现有七字段 StepResult 不变：工具决定放入 `content`，`proposal=null`；最终决定使用 `content=null` 和展开后的 `proposal`。模型步骤均关联真实 physical call。新 `model_decision` Proto 枚举只追加编号；共同 schema、Go/Python、SDK 和新增 migration 同步，不改历史 migration。外层执行器 v2 帧不变，新固定 executor version 防止旧进程误解新输入。
5. 工具参数从最近的已提交决定派生，完整决定被 commit hash 与下一步 input hash 绑定；Go 校验名称、参数、快照身份、重复及来源，Worker 不能另报下一游标。仍先 BeginTool/Reserve，持久审计/观察，实际 Wait/EOF/Join/组消失后 Commit。
6. 相同规范化工具参数不再次派发：query 首尾空白去除，其余字符原样；JSON 键顺序不影响身份。重复决定终止为 `MODEL_PROTOCOL_ERROR`，安全原因字段记录 `repeated_tool_call`；不消耗第二次工具调用。结构/来源错误最多一次全 Run 纠错，纠错后可继续动态循环，后续错误直接终止。
7. 多次政策检索累积唯一 chunk/evidence ref。重复段落仅在快照、版本、来源和正文一致时合并，距离随 query 改变可不同；身份相同但内容冲突拒绝。订单/物流各读一次。方案只能引用实际取得的字段和段落，不能因模型输出政策编号就加入来源。
8. 提示只使用已提交事实、去重政策及紧凑动作记录。S2 消息内容上限 64 KiB、请求体 128 KiB；模型输出仍为 1024 tokens / 16 KiB，响应体 64 KiB、工具 8 KiB、checkpoint 256 KiB、游标 32 不变。超限明确失败，不静默截断事实。首选既有 DeepSeek Flash 非思考 JSON 后端及原本地 embedding，不把评分器/答案装进运行镜像。

## 额度与批次终结

沿用重试族累计 12 chat、8 逻辑读工具、8 query embedding、16 metadata HTTP、44 物理 HTTP、1 protocol correction；外部调用 60s、执行段 180s、Run 24h 最大值不变。Run/tenant/batch 原有 PG 事务账户继续裁决；模型不能自行调整额度。次数/期限耗尽明确失败，没有隐藏请求重试。

S2 初始新增累计操作预算 20 CNY，维护者授权执行者记录原因后自主调整。每批仍登记有限且冻结的实际 cap；准备器允许可信操作员给出不超过安全整数上界的正费用 cap，不将旧 5 CNY 常数当成用户审批。新 batch cap 不高于当次 S2 操作预算减去所有已启动 S2 batch 的 known+held；按 batch 聚合一次，不叠加 tenant/family 镜像。预算调整在启动记录中保留旧值、新值、原因与累计数，不能通过新 batch 重置消费。旧 S1 known/hold 单独保留并汇总披露，不转作退款或重复消费授权。

已完成审计且 attempt 持久终结的明确协议失败、重复决定、调用额度失败可以结束一案并继续下一案。Go 的 batch guard 与 driver 同步校验真实调用绑定、全部已派发 chat 的 report/observation/known usage、无冲突/异常和终态；未提交的失败 chat 必须绑定相同 attempt/fence/当前步骤与持久失败原因，不能仅凭 Run 为 failed 放行。正常已提交模型决定不要求整个 Run 已终结，才能执行下一工具。

未取得完整响应仍按旧规则 unknown/full hold、冻结当前 batch 并停止派发。派发者停止且保守上界仍成立后，可按 ADR-0023 的条件登记新批；不强求补回丢失计量，不清零旧 hold、不解冻旧批。模型身份/模式、费率失效或无法确定安全上界时，停止收费并报告具体外部缺口。

## 验收与实施

- 先合并本契约，再用一个实现 PR 完成运行时、SDK/工具、验收和文档。新增能力在实现证据齐全前不标完成。
- Go/Python 共同向量覆盖决定/纠错、参数绑定、重复、跨检索来源、上限；真实 PG 覆盖提交/陈旧 fence/共享预算/失败批次屏障；固定 Linux 运行真实进程与安装 SDK 的联合契约。复用无变化的有效证据，执行适用强制 CI。
- 冻结版本后完整执行原 40 个开发案例，至少 32 个业务正确且安全硬失败 0。原业务评分/gold 不变，仅给证据校验增加动态路径支持。失败计入分母，不跨版本拼接，不打开保留集。未达标依据已发现原因改进 S2 后新版本整批验收。
- 正式 SDK 导出方案、步骤、Calls、失败、tokens、known/held 与延迟；复用 S1 报告作对照。另一个隔离批次对真实云端调用截断响应一次，证明停止和未知费用查询，明确区分注入故障与供应商原生故障。
- 独立审查无未解决实质问题、最终必需 CI 通过后 squash merge。仅文档变化不重跑收费验收。

## 未采用方案

Python 内部自由循环会绕过现有逐步执行权；固定重查全部政策会削弱动态取证且无谓增加调用；重新引入 Agent 框架或通用预算服务没有当前业务收益。放宽业务 gold、拿格式正确代替业务正确或要求未知响应恢复后才能继续均不符合本轮可用成果目标。
