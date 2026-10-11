# ADR-0032：空推理观测与原名单续跑

- 日期：2026-10-11；状态：Accepted（维护者明确批准修订并继续剩余 225 次；分类规则经独立审查）。
- 只修订 [ADR-0031](0031-s5-controlled-decision-diagnosis.md) 的空推理停发分类，不修改模型输入、政策、gold、正式门槛或费用上限。

## 决策

首段 15 次调用在 `S5V3-004-B-1` 停止：请求为 thinking enabled/high，返回完整 final、空 `reasoning_content` 和零 `reasoning_tokens`，usage 自洽。此前 `003-B-1` 用尽 16,384 输出 token，全部用于推理，最终内容为空。

官方响应 schema 允许 `reasoning_content` 为空，没有规定每条 thinking 请求的推理 token 必须为正。空推理不再单独触发停发；按请求分配 A/B/C/D 评价最终答案，另列推理可观测率。截断仍为失败，非空推理正文与明确零推理 token 的矛盾、未知费用、身份错误等原停发条件不变。thinking 模式忽略 `temperature=0`，不得声称四组实际采样温度均为零。[响应规范](https://api-docs.deepseek.com/api/create-chat-completion/)、[思考模式](https://api-docs.deepseek.com/guides/thinking_mode/)

## 保留与续跑

原 15 条响应、报告、评分、账户冻结和 experiment stop 全部保留。schema 2 manifest 绑定原 manifest hash、同一库名/UUID 及连续 15 条 known 前缀；完整 240 trial、顺序、请求字节、费率、历史费用基数不变。Go 逐项验证 PG 前缀后，只发送尚未执行的 225 条。新的 experiment 行记录本次修订，不清空旧账、不更换数据库、不重发既有 trial，也不引入自动恢复或重试引擎。

S5 累计费用继续为历史基数加同库所有 batch known+held。批准修订时为 ¥4.493878，held 为零，总额上限仍为 ¥20。新分析包含旧 15 条原始观察并明确标注修订口径，旧分析不覆盖；不能挑选重试或剔除截断案例。

免费验证覆盖空推理仍可评分、真实矛盾计量仍停发、已完成前缀不可重发、费用基数与旧 stop 保留、跨 experiment 的 PG 导出及评分绑定。生产代码和正式验收范围不变。
