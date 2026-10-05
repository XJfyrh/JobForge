# 可靠性验收入口

可靠性按合同与测试层次核对，不能用合成响应替代实际进程/数据库事实。

| 范围 | 结果与深入材料 |
|---|---|
| Agent v3 当前实现 | [状态页](status.md)、[S1/S2/S3 最终证据](evidence/README.md) |
| 既有 Job：kill/接管、万次幂等、耐久事件和 heartbeat 取消 | [历史可靠性报告](archive/reliability-history.md)；验收按对应 PRD/ADR，Job 的恢复时间口径不套用到 Run |
| 本次改动需要的检查 | [测试指南](tests.md)、[Windows 手册](runbooks/windows-acceptance.md) |

真实 PostgreSQL、fencing 和 at-least-once 是共同边界。skip 不计通过；历史未结事项见[限制清单](status.md#限制与未结事项)。
