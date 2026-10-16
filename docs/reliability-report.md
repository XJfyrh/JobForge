# 可靠性验收入口

本页汇总 Run 与旧 Job 的可靠性结果，便于从历史合同或审查记录追到相应证据。

| 范围 | 结果与深入材料 |
|---|---|
| Agent v3 当前实现 | [状态页](status.md)、[S1–S5 阶段证据](evidence/README.md) |
| 既有 Job：kill/接管、万次幂等、耐久事件和 heartbeat 取消 | [历史可靠性报告](archive/reliability-history.md)；验收按对应 PRD/ADR，Job 的恢复时间口径不套用到 Run |
| 本次改动需要的检查 | [测试指南](tests.md)、[Windows 手册](runbooks/windows-acceptance.md) |

两条路径都使用 PostgreSQL、fencing 与 at-least-once 交付。当前运维适用性见[部署参考](deployment.md)，旧 Job 未结事项见[状态页](status.md#限制与未结事项)。
