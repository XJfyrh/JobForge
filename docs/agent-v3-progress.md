# Agent v3 实施记录

- 开始日期：2026-09-16。
- 维护者已确认[路线 v3](plans/agent-execution-roadmap-v3.md)，当前推进 S0。
- 起点：5c82834；分支 XJfyrh/agent-v3-s0。
- 开始时仅有此前规划的 README 与 docs/plans、docs/research 未提交改动，全部保留。

## 阶段状态

| 阶段 | 状态 | 当前证据 |
|---|---|---|
| S0 契约与关键试验 | 进行中 | PRD v0.7、ADR-0013～0015；执行器真进程/race已通过，本地主模型仍在选型 |
| S1 业务与基线 | 未开始 | 无真实新工单工具/pgvector验收 |
| S2 Agent 与预算 | 未开始 | 无生产 Agent Run/额度表验收 |
| S3 步骤恢复 | 未开始 | 无新 Run checkpoint 验收 |
| S4 审批与写入 | 未开始 | 无新审批/写入验收 |
| S5 评测与展示 | 未开始 | 无新保留集、页面或观测验收 |

## S0 发现与决定

- attempt_no 和 recovery_count 分离，避免正常审批继续耗尽故障次数。
- 有效 lease 以数据库时间严格判定；匹配 owner/token 但已到期仍拒绝新步骤。
- 接收端先查相同 operation_id 的已有回执，再验首次写入版本，避免成功重发被新版本误拒。
- 取消终态后只读核对回执，不重新派发业务动作。
- stopping保存首次停止原因及独立cancel_requested；后到取消/总期限仍阻止恢复，Heartbeat不能无限延长停止阶段。
- 工具统一读取业务snapshot_id，物流聚合版本覆盖新增事件；动作事务复核完整版本向量。
- Python 执行器以有界进程组监督固定 step；关键进程退出能力由 Linux Docker 实测决定。
- 起始容器只有 qwen2.5:0.5b 与 all-minilm:22m；新增免费 qwen3:4b 冷/暖两次首调用均60s超时，未放宽预算。最后一个本地候选 qwen3:1.7b 正在试验。小模型旧抽取结果不代表新 Agent 能力。

## 验证记录

| 检查 | 实际状态 | 范围/证据 |
|---|---|---|
| Windows Go build/vet/golangci-lint | 通过 | 全仓编译/静态检查，0 issues；不等于Linux进程race |
| Linux执行器真进程/race | 通过 | 固定镜像、1CPU/256MiB/64PID、9个真实场景；[命令与边界](../tools/executorprobe/README.md)、[记录](../tools/executorprobe/acceptance-2026-09-16.txt) |
| Python SDK与两个探针guardrails | 通过 | 合计71项，SDK51项、模型12项、执行器8项；不调用真实模型 |
| Ruff check/format、Mypy | 通过 | 全仓Ruff；SDK7文件和两个探针入口Linux平台类型检查 |
| SQLFluff | 通过 | 3条历史基线有效，migration lint通过；没有新增业务migration |
| Buf lint/breaking | 通过 | 对main检查；未修改Proto |
| Prometheus/Grafana配置 | 通过 | promtool配置和5条规则测试；仪表盘重新生成无内容diff |
| 文档/证据格式 | 通过 | 当前33文件、129个本地链接，UTF-8/JSON/常见秘密格式检查无错误 |
| qwen3:4b冷/暖真实请求 | 失败 | 各1次请求，均超过60s；[模型记录](evidence/agent-v3-model-probe-2026-09-16.md) |
| qwen3:1.7b | 进行中 | 尚未作为主模型接受 |
| 全仓Go集成/race、PR CI | 待执行 | S0新进程race已单独执行；全仓CI结果稍后记录 |
| 新业务/审批/步骤恢复/保留集 | 未运行 | S1～S5能力尚未实现，不能由S0探针替代 |

执行器首次把解释器启动混入500ms/3s超时导致失败；COPY后仍复现，不能归因于Windows挂载。现用有界总启动/执行deadline和started后的1s步骤计时，分别记录启动与清理耗时。探针仅证明固定受监管进程组在所测场景可行，不是完整生产执行器。

远程供应商和费用 B 未选择，没有收费调用。S0 的模型工具 fixture 只检查模型协议，不作为真实业务验收。全阶段完成前不声明目标完成。
