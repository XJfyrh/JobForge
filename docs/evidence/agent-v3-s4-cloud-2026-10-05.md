# S4 人工审批与业务回执：有限真实验收

2026-10-05，固定 **10 个源样本及 2 个预定回执后继**全部执行并获独立接受：**10/10 个原始方案业务质量合格，10/10 个机制项通过**。实际新增 12 个 Run，42 次真实 chat；记录 7 条唯一业务结论，审批后没有模型重规划或写入重发。[PR #60](https://github.com/XJfyrh/JobForge/pull/60)交付该能力。

## 范围与结果

使用独立 schema 4 profile/batch、正式 Go/Python、安装 SDK、真实 DeepSeek、MiniLM/pgvector 和业务 HTTP/PostgreSQL。名单、原评分器、gold、身份、账户和 6 小时窗口在执行前冻结；累计上限 5 CNY、12 个新 Run。只执行 DEV-006/007 各一次原定后继，没有补样或替换。

| 样本 | 验收行为与实际结果 | 同一 operation 的 POST / GET |
|---|---|---:|
| DEV-001 | 普通结论记录，succeeded/applied | 1 / 1 |
| DEV-016 | 待补充信息标记，succeeded/applied | 1 / 1 |
| DEV-011 | 升级人工标记，succeeded/applied | 1 / 1 |
| DEV-002 | 拒绝有效方案，succeeded/rejected，effect=none，无写入 | 0 / 0 |
| DEV-003 | 真实 loader 将工单 revision 增加 1；failed/ACTION_CONFLICT，无业务回执 | 1 / 1 |
| DEV-004 | 真实提交后丢响应并 SIGKILL/Wait；自然 lease 接管，succeeded/applied | 1 / 2 |
| DEV-005 | 提交后取消；原 cancelled/unknown 保持，reconcile 后 effect=applied | 1 / 2 |
| DEV-006 | 提交后取消；原终态不变，回执后继 succeeded/applied，0 模型调用 | 1 / 2 |
| DEV-007 | 授权保存后、首次 POST 前取消；后继 failed/ACTION_OUTCOME_UNKNOWN，0 重发 | 0 / 1 |
| DEV-035 | 原始方案业务校验通过，批准后 succeeded/applied | 1 / 1 |

12 个 Run 中 7 succeeded、2 failed、3 cancelled；负向用例按原定行为计入完整分母。首次终态 Run/result/disposition 逐字段保持，effect 独立推进。DEV-004 recovery_count=1，旧 attempt 自然关闭不早于观测 lease，实际 Go 进程返回 -9，动作边界无 Python 子组。

9 个签名授权、8 次动作 POST、12 次回执 GET 与 7 条唯一回执全部绑定原审批和业务请求。两个后继没有模型调用、新签名或 POST。applied 证明结论记录或工单标记已提交，不表示客户问题已解决；本次没有自动关单、付款、退款或邮件操作。

## 用量与保留

| 项目 | 实测 |
|---|---:|
| 真实 chat | 42 |
| 模型/只读工具相关物理 HTTP | 113，另计上述动作 POST/GET |
| known tokens | 158,852 |
| known 账本估算 | 102,188 microyuan（**0.102188 CNY**） |
| held tokens / cost、未知 chat | 0 / 0 / 0 |
| 执行容器 | 退出 0、PID 0、无 OOM、清理错误为空 |

金额是 observed usage 与冻结声明费率的保守账本估算，不能称供应商已结算费用。模型/动作账本与业务效果全批审计通过；新增执行、控制和业务服务已停止，数据库、卷、原失败容器及证据保留。共享历史和 Ollama 环境未停止。

## 证据

[机器摘要](agent-v3-s4-cloud-2026-10-05.json)保留逐项质量/终态/后继/物理计数、源码和镜像绑定、原报告及证据索引哈希。真实执行源码为 `0360139e111bb606c89f3c839e5906682cb81738`，其[现有 8 项 CI](https://github.com/XJfyrh/JobForge/actions/runs/37329991721)全部通过；交付文档与该执行源码分开记录。

原 SDK、模型响应、PG、动作边界与 outbound 位于仓库外私有目录。规划侧直接复核原 pending/评分输入、已接受步骤、模型账本、自然 lease、终态与业务回执，并独立接受。

[免费工程检查](agent-v3-s4-free-2026-10-05.md)与[审批指南](../agent-v3/approval.md)分别提供工程分层和操作合同；当前阶段与下一步见[状态页](../status.md)。
