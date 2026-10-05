# S4 免费工程检查

本层验证工程和机制，不替代[真实模型与业务验收](agent-v3-s4-cloud-2026-10-05.md)。执行源码 `0360139e111bb606c89f3c839e5906682cb81738` 的[现有 8 项 CI](https://github.com/XJfyrh/JobForge/actions/runs/37329991721)全部通过。

| 检查 | 实际结果 |
|---|---|
| 全仓 Go race、真实 PostgreSQL | 当前 head 的 Linux CI 通过 |
| Go lint/build/vet、Buf lint/breaking、迁移 SQLFluff | 通过 |
| Python、SDK、协议与工具 | Linux CI 共 1896 项通过：SDK 180、S0 工具 43、adapter 1369、evaluation 202、recovery 63、approval 39；Ruff/mypy 通过 |
| 业务 PG/HTTP | 原子结论与回执、幂等/冲突、低权限、严格 JSON、过期/回滚、缺失来源与 loader 并发通过 |
| 控制 PG/SDK | 独立 actor、租户权限、审批重放/冲突、4+4 动作额度、锁序、租户 gate、终态/取消/过期、旧 profile 只读通过 |
| 正式 Linux Go/Python | 普通写入和提交后丢响应/杀 Worker/30s 自然 lease 恢复通过；本地两项合计 52.46s，当前 CI 亦实际执行通过 |
| 生产边界 | registry 仅 support-fixed-v1/support-agent-v1，无故障代理、测试 adapter、gold |
| 完整来源与启动校验 | 218 个冻结来源、23 个实际安装工具文件匹配；无网络、无启动审查夹具直接执行完整正式 verify_release 通过，正式启动未使用夹具 |
| 实际验收镜像文件 | 新增 CI 从源码构建外部镜像并逐字节核对 23 个冻结工具文件，通过 |

免费联合测试使用合成模型/向量响应；真实 DeepSeek 和 MiniLM 结果只计入真实验收报告。

原本地日志入口为 `E:\JobForge-notes\2026-10-05-agent-v3-s4\free-checks`；当前 CI 原日志由上述运行进入。可靠性和公开合同见 [PRD v0.18](../product/JobForge_PRD_v0.18.md)、[ADR-0026](../adr/0026-approval-actions-and-receipt-recovery.md)。
