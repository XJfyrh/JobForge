# 核心概念

JobForge 当前以售后 Agent 的 Run 为产品主线。旧 Jobs 与 Run 是独立控制路径，不在同一次执行中叠加两套调度器。

| 概念 | 责任与边界 |
|---|---|
| Run | 一次持久业务处理，拥有状态、deadline、attempt 与当前游标 |
| Step | 已提交的执行进度，没有独立队列或租约；恢复从持久游标继续 |
| Attempt / fencing | Worker 的当前执行权；过期或被替换的持有者不能提交 |
| Profile | 冻结模型、费率、prompt、schema、执行器与业务快照，不能在运行中改写 |
| Physical call | 每次实际 HTTP 请求的账本身份；授权先持久化，不能隐式重试 |
| Known / held cost | 已知计量费用与未确定调用的保守预留；observed usage 不是已结算账单 |
| Batch | 有限预算与停发边界；新建批次不能重置用户累计授权 |
| Proposal | 结构化方案及真实取得的来源；当前到 `awaiting_approval`，没有业务写入能力 |

## 正常路径

读取工单 → 模型决定 → 已授权只读工具 → 模型再次决定 → 持久化可审阅方案。模型只能选择 `get_order`、`get_delivery`、`search_policy` 或最终方案。每个决定先由 Go 校验并提交，再派发下一步。

Go Worker 持有唯一租约，监管 Python 进程。Python 不直接写控制库，也不自主循环。普通观察得到持久确认、报告汇合、进程真正退出并清理后才能提交步骤。

## 故障路径

- Worker 崩溃：租约过期后可恢复；已提交步骤不会由新 Worker 当成未完成步骤执行。
- 已发请求但响应不完整：不能假设免费；保留 full hold，冻结当前批次。
- 陈旧结果：由 owner/fence/状态条件拒绝，不能覆盖新执行权。
- 重复工具决定：明确失败，不再次扣工具额度或请求工具。
- 方案来源无效：最多一次全 Run 纠错；不能将模型猜测加入事实。

这些语义保证 at-least-once 执行，不保证模型计算只发生一次。审批后业务写入和恢复成本实验仍是未来阶段；不要把状态枚举或规划图当成已实现能力。

协议详情按需阅读：[Run](agent-v3-runs.md)、[运行时](agent-v3-runtime.md)、[审计](agent-v3-provider-audit.md)、[S2](agent-v3-support-agent.md)。
