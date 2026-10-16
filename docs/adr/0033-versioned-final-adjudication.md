# ADR-0033：版本化独立终裁

- 日期：2026-10-11；状态：Accepted（维护者批准方案，独立设计审查通过）。
- 依据：[裁决诊断](../evidence/agent-v3-s5-decision-diagnosis-2026-10-11.md)。B 改善已见样本的单次裁决，不代表完整工作流或正式验收通过。

## 决策

新增 Agent schema 11、`support-agent-prompt-v4`、`linux-v2-adjudication-runtime-1`，保留所有旧定义和 hash。原 nonthinking 调查、工具、事实及 v3 业务提示词不变。调查返回闭合的 final 对象后，将原始六字段初稿保存为 `content.type=investigation_complete` 的 `draft`，`proposal=null`；初稿不是待审批结果，其事实或引用错误不阻止独立终裁。Go 只允许此标记推进到已有 `model_proposal`，不允许 Python 选择下一步。

终裁只接收已提交的票据、订单、轨迹、检索政策及其来源，不接收初稿或其结论。它只能返回 final JSON，经过既有来源与 proposal 校验后，才进入 `submit_proposal` 和原审批流程。生产 registry ID 不增加。Fixed schema 12 使用 `linux-v2-fixed-adjudication-runtime-1`，保留固定检索图和固定提示词，采用相同终裁模型配置。

新定义增加独立 `final_model`：deepseek-flash、thinking enabled/high、16,384 completion token、JSON object、非流式；调查仍 disabled/1,024。思考模式忽略 temperature，不声称温度为零。响应体上限仅终裁增至 1 MiB，final 内容仍为 16 KiB；不把推理正文传入 IPC、checkpoint 或业务日志。completion 已含 reasoning，不重复计费。

## 许可、审计和恢复

预留、许可、历史预算恢复及晚到计量均由冻结 profile 和原始 step kind 派生，不能依据当前 cursor 或 Python 参数放宽。终裁 call deadline 取 120 秒、attempt 剩余、Run 剩余及账户有效期的最小值；不扩大 dispatch/lease，不重置 180 秒 attempt。调查仍为 60 秒。每个 family 原有 12 chat、工具及物理调用限制保持，token 上限增加一个终裁相对调查的 15,360 token；费用上限不变。

新审计政策 `deepseek-audit-v2` 仅绑定新 executor。终裁使用 audit schema 2，可记录 `thinking`；非空 reasoning 与显式零 reasoning token 的矛盾仍拒绝，空推理仅为观测。旧 schema/hash 不变，调查及旧 profile 仍拒绝 thinking。Go 必须依据原始 profile/step 检查 mode，不能全局放开。完整有效计量按原费率记录；未知或矛盾保留 full hold 并停发。

截断或不合法终裁结束该 Run，不安排纠错、补答或降级；合法终态拒绝须闭合 batch audit guard，不能误报审计缺口。既有已确认计量但尚未提交的崩溃恢复保留，与质量重试分开：提交调查标记后不重新调查，提交终裁后不重发；unknown 不恢复发送。

无需新增数据库字段，原 JSONB、step kind 和审计存储容量足够；只增加 Proto mode 枚举并重新生成绑定。旧 profile、审计和恢复证据保持可重放。

## 验证与费用

先免费验证版本隔离、初稿不可审批及不入终裁输入、长调用取消/失租、终裁拒绝、未知 hold、两阶段边界恢复和不重发。再运行开发集 40 条及已见 v3 的 20 条回归，分别报告初稿/终裁质量、耗时和费用。开发正确与证据完整各至少 36/40、硬安全失败为零，且已知 004/018 问题解决后才冻结。若仍失败，只能以通用政策条件澄清创建新开发提示词版本，不改政策、gold 或加入特定题答案。

冻结后才创建新的未见 20 条，Agent 与 Fixed 使用同一终裁模型配置，旧 Fixed 分数不能复用。正式 Agent 正确至少 16/20、证据完整至少 18/20、硬安全失败零；通过后才进行恢复比较与演示。失败不增加样本或选择性重跑凑通过。

S5 唯一阶段预算为累计 ¥20，覆盖所有旧库、诊断库和新库的 known+held；批准时累计估算 ¥7.103572、held 零。旧 ADR 的阶段 Run/chat/token/耗时额度不再约束本轮，包括 ADR-0030 的批次六小时阶段额度；请求、attempt、Run、账户协议期限及逐调用保守预留仍生效。已授权范围内不逐批请示，费用不足、安全问题或实质方案变化再交维护者决策。不恢复或提交已删除的交接文档。
