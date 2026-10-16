# 当前状态与下一步

更新：2026-10-16。Agent v3 已完成 S5 工程与正式模型质量验收，公开预览版 [v0.1.0-preview.1](https://github.com/XJfyrh/JobForge/releases/tag/v0.1.0-preview.1) 已发布。

首次使用从[免费 Run](quickstart.md)开始；了解客服用途看[业务说明](support-overview.md)，评估上线条件看[部署参考](deployment.md)。

## 已交付

| 能力 | 结果与证据 |
|---|---|
| 售后方案与来源 | V9 冻结候选在未见正式集上正确 **18/20**、完整证据 **18/20**，达到原 16/18 门槛，四类硬失败为 0；Fixed 各 11/20，配对胜 8 / 平 11 / 负 1。[逐案报告](evidence/agent-v3-s5-v9-formal-v4-2026-10-16.md)保留两案错误及对照条件 |
| 步骤恢复 | 原 11 项对照中，5 项接管机制通过，8 个完整方案中 7 个正确，3 个 H0 按计划取消。两组可比案例的续跑费用高于中断前缀加从头执行，另一组因错答不可比。[恢复报告](evidence/agent-v3-s5-v9-recovery-2026-10-16.md) |
| 人工审批与工单更新 | 独立审批身份批准原方案；实际业务提交后丢失响应，经自然租约接管查询到唯一回执，工单版本只增加一次，模型步骤未重放。[实际故障演示](evidence/agent-v3-s5-closeout-2026-10-16.md) |
| 任务工作台 | 提交、方案与来源查看、审批及实际结果核对；页面采用新版浅色工作台。[操作与配置](agent-v3/operations.md) |
| 观测与数据操作 | 有限 OTLP 关联、指标/面板、7 日终态内容清理和停写双库恢复演练。[验收矩阵](agent-v3/s5-acceptance.md) |
| 公开体验与接入 | 新版三分钟视频、免费隔离 Run、SDK/curl 示例、英文摘要及部署矩阵。[Preview 交付记录](evidence/public-readiness-2026-10-16.md) |

正式验收的 Agent 为 schema 18 / prompt-v9，Fixed 为 schema 17；版本与执行器对应关系见[运行时](agent-v3/runtime.md)。默认开发 Compose 提供空控制面，免费体验使用独立的 demo Compose；真实模型执行通过受审 profile、预算和专用凭据启用。

S5 已闭账：50 库累计 727 Runs / 2,924 chat，按 usage 与冻结费率估算 ¥23.325083；held、unknown chat、在途调用与未结束 attempt 均为 0。该金额是用量估算，供应商结算以账单为准。逐批来源、历史停止及动作 POST 的 unknown 状态见[闭账报告](evidence/agent-v3-s5-closeout-2026-10-16.md)。

## 下一步

近期工作见[路线图](../ROADMAP.md)：完善接入示例、英文部署入口与可复核演示。S5 本轮已结束，没有待执行的收费实验；新实验另行确定范围和预算。改动检查由[CI](https://github.com/XJfyrh/JobForge/actions/workflows/ci.yml)按最新提交运行，版本交付记录见 [PR #61](https://github.com/XJfyrh/JobForge/pull/61) 和 [PR #68](https://github.com/XJfyrh/JobForge/pull/68)。

## 限制与未结事项

| 范围 | 当前情况与处理入口 |
|---|---|
| 生产运维 | v3 已验证开发/有限评测环境与停写恢复；容量治理、HA/PITR、长期 Trace 和外部告警闭环仍待建立。适用性、资源与服务等级集中见[部署矩阵](deployment.md#适用性矩阵) |
| 旧 Job Claim 性能 | W4 超过历史 15% 回退门槛，待同环境复验或基线决策。[原结果](archive/benchmark-history.md#历史微基准绝对值核对)、[审查记录](archive/agent-rag-review.md) |
| 旧 Job 取消控制流 | AT-25 可选 ControlStream 未实现；现有 heartbeat 取消信号合同为 p95≤6s。[ADR-0008](adr/0008-cancel-control-channel-heartbeat.md)、[历史可靠性](archive/reliability-history.md) |
| 旧远程 Ollama | 自配远程服务的连通、鉴权和完整任务路径待验收。[旧模型接入](legacy/real-tasks.md) |

S0–S5 的阶段结果、错答和停止记录从[证据索引](evidence/README.md)进入；实施过程与旧候选的当时计划保存在[历史记录](archive/agent-v3-progress.md)。
