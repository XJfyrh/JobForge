# JobForge PRD v0.12：S1 provider 持久审计与批次停发

- 日期：2026-09-16；状态：**Proposed，尚未接受、实现或验收**。
- 基线：[PRD v0.10](JobForge_PRD_v0.10.md)、[v0.11](JobForge_PRD_v0.11.md)、ADR-0017～0019；对应建议决策为 [ADR-0020](../adr/0020-provider-audit-and-batch-stop.md)。
- 对应代码核对：C3b 已通过 PR #46 合并，起点 `0829a4c`。本稿仅供下一合同 PR 审阅，不修改已合并实现。
- 本增量补 C-01/C-03/C-07/C-08 的持久审计和首批停发条件。support_fixed_v1、六字段模型输出、持久方案/来源/模板和离线评分已由 ADR-0018 决定，不在此重新决策。

## 1. 用户可见行为

每次真实 DeepSeek HTTP 仍先取得 Go 控制面的持久许可。固定 Python adapter 在一次有界读取后，将响应身份、完整响应摘要、可观察的 reasoning 数量状态和计量事实作为同一不可变报告，沿已有计量管道交 Go 保存。没有完整响应时保留“未观察到完整响应”；usage 不完整时保留 unknown/full hold。HTTP、供应商正文或模型输出都不构成新执行权限。

调用原租户的 reader/operator 可通过只读调用查询看到：事前许可、参数/profile/price hash、响应审计、已知费用、仍占用的额度和固定停发原因。查询不返回完整 prompt、模型输出、reasoning_content、供应商错误正文、业务全文、凭据、控制 token 或可执行命令。原始敏感正文不因“审计”要求被新增保存到数据库、日志或 Trace。

“计数结构完整”与“可按当前 profile 定价”分开。返回 response.model 与冻结 profile 不兼容时，即使 token 计数内部合法，也只记录观测计量和审计，维持 unknown/full hold 并停批，不能按原价结算或释放差额。身份兼容、计量合同完整时，业务 JSON/schema 错误不阻止正常计量结算；这些错误仍按原一次纠正规则处理。

本稿建议为**新首批审计 profile**增加一条明确的保守运行政策：任一 chat 的计量最终为 unknown、供应商身份/非思考模式不兼容、报告冲突或持久确认不确定，均停止本批次后续收费调用。unknown 仍是合法的账本事实，不被篡改成零费用或 measurement anomaly。该政策比旧 profile 的“保留 hold 后在额度允许时可继续”更窄；不追溯改变旧 profile 的晚到计量权限。

## 2. 最小实现边界

复用现有 metering FD、SettleUsage RPC、物理调用记录、三层账户与 batch 的 frozen 标志；不增 FD、provider 调度器、通用事件总线、审计后台或第二套执行权状态机。新报告 hash 绑定原执行/步骤/物理调用/参数以及 usage 和 audit；普通 observation 的 hash 再绑定同一 audit hash。只有报告事实与普通 ObserveCall 均按原顺序持久确认，才能继续正常流程。

审计独立于计量的缺失状态，但不是独立的发送权限。审计-only 确认只表示记录已保存，不表示 usage_known、已结算、已退款或允许下一次调用。停止、失权和原 deadline 不因审计等待或晚到确认而被恢复。

## 3. 验收映射

| ID | 要求 | 必须实际验证的事实 |
|---|---|---|
| A-01 | 有界 typed provider 审计 | 区分完整/无完整响应、兼容/不兼容/非法身份，reasoning 缺省/显式零/正数/非法；严格字段、null、safe integer、重复键、非法 UTF-8、大小边界；无正文或秘密泄漏 |
| A-02 | 定价资格 | 兼容身份的完整计量即使方案无效也结算；不兼容 model 的内部完整计数保存但不计原 profile 价格、full hold 不释放；结构完整计数超原 token 上限仍独立触发三层 measurement anomaly；非法/缺失计量仍 unknown；合法正数 reasoning 不重复计费且停批 |
| A-03 | 跨 FD 确认 | 报告/观察任意合法先后顺序、错 audit/report/hash、遗漏报告、重复和迟到 ACK；数据库未提交、ACK 丢失或冲突时下一 HTTP 均为 0；普通 ACK 不能被报告 ACK 替代 |
| A-04 | 同事务和幂等 | 实际 PG 中，首次 coherent report 的审计、符合定价资格的 usage 结算以及必要批次冻结同事务；同报告重放不重复扣费；不同报告不覆盖首次事实，冲突有持久停批且不能 ACK 成功 |
| A-05 | 原身份晚到窄权限 | 原 principal/session/attempt/fence/call 与预留时绑定逐项一致；30 日窗口内可过期后确认原报告，不要求旧 profile 仍可执行；新 session/另一 tenant/另一 call 拒绝；不改 Run/active_call/lease/游标 |
| A-06 | 停批职责 | Python 停止当前执行；Go 锁存停止 Claim/许可/纠正并清理；控制面持久冻结对应 batch，并以已预留 chat 的报告/普通观察/已提交步骤事实阻断未确认时的后续许可；SDK 驱动停止提交下一例。重启或另一 driver 不能绕过持久 guard；计量异常三层冻结与审计原因仅 batch 冻结明确区分；未确认不能宣称已冻结 |
| A-07 | 有权限的只读查询 | 新 `/v2/runs/{run_id}/calls` 仅原 tenant reader/operator；跨租户 404、缺/错凭据拒绝；按现有硬上限一次返回≤44项、总响应≤256KiB，无分页或收费副作用；缺报告/历史未采集/unknown 明确区分，不从 hash 还原正文 |
| A-08 | 可复现且不扩大授权 | 新旧 executor/profile 混用拒绝；旧 profile late usage 回归；固定 Linux 真实进程、真实 PG/gRPC/HTTP、Go/Python fixture、适用 race/SQL/Buf/SDK 与独立审查；收费验证仍只有既有批准批次 |

## 4. 兼容与范围

建议 ADR-0020 明确取代 ADR-0018 §3 中“metering_report 必有可结算 usage”的窄帧形状，以及 ADR-0019 §1 的 observation hash/汇合字段，限新 executor/profile；原许可、BOOTTIME、普通 ACK、退出码、Kill/Wait/EOF/Join、2 秒总收尾、30 日结算权限、unknown hold、measurement anomaly 三层冻结、at-least-once 均保留。

内部 `api/executor/v2` 保持 version=2，但只由新的固定 `executor_version=linux-v2-audit-runtime-1` 使用本次不兼容细化。旧 v2 runtime 的 schema/fixture 由 Git 历史保留，不引入双 codec、历史目录运行包或兼容执行路由。Proto 只追加有明确 presence 的 typed 字段和枚举，不重用既有字段号；对升级前已预留原调用的 usage-only late RPC，仅保留原身份/原窗口的窄计量权限，不允许旧 Worker 新 Claim/Reserve。

HTTP/SDK 新增只读 calls 查询；既有 Run/Budget 响应形状和错误码不因本稿静默改变。持久 schema 只新增 migration，不能修改已应用 0023。

## 5. 预算和证据边界

既有首批共享 5 CNY、6 小时、capacity=1 和 40 案分母不变；不新增免费或租约外探测、自动充值、换供应商或换 batch 绕过 unknown。模型别名、价格快照与 system_fingerprint 不提供不可变模型/价格锁，执行当天仍须核对官方合同。无法确认即停止，不能把 metadata endpoint 返回或鉴权成功当推理验收。

持久审计中可观察的错误只证明收到的有限元数据；不能检测同一别名、相同 fingerprint 下供应商未披露的模型/价格变化。已发生外部请求的取消不保证供应商停止或退款。

完整跨队列 Trace、生产长期留存/S5、S2 动态 Agent、S3 恢复对比、S4 批准写入仍另行实现和验收。本稿只要求原调用确认窗口所需审计不被提前删除，并提供可核对的批次事实。40 案因本稿的新停发条件未能全部运行时，应报告未尝试行，S1 仍未完成；不调整分母或自行增加额度。

本合同未实现，文档验证不构成模型或运行时验收成功证据。
