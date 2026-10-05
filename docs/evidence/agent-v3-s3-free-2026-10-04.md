# S3 实现与故障检查

2026-10-04 的免费检查验证[ADR-0025](../adr/0025-confirmed-step-recovery.md)机制；供应商/embedding/业务响应为测试 fixture，不代替真实模型。[十一项真实验收](agent-v3-s3-cloud-2026-10-04.md)另列。

## 验证范围与结果

| 层次 | 实际验证 |
|---|---|
| 真实 PG/race/SDK HTTP | 九字段关闭证明、guard、旧 profile、连续序号、并发 Claim/Reserve/旧报告、三账户预算与 migration 锁超时/往返；通过 |
| Linux 正式进程 | step/guardian/Go 死亡、许可前/完整确认后、丢 Commit ACK/下一许可前、Wait/EOF/Join/组消失；定向通过 |
| 自然时间 | 30s lease、同 principal session、180s attempt、1/2/4s 退避及四次丢失/仅三次恢复；未缩短生产值 |
| 外部代理/Supervisor | F06/F07 实际信号、分离 Wait/组消失时点、ACK queued 且 Python 未消费；通过 |
| 协议/镜像 | schema 3 坏帧、普通/计量片段、stderr 上限、协调器与生产不含测试模块/gold；通过 |
| Python/工具 | Windows 全套 1780 passed/12 skipped，skip 不计 Linux；最终工具固定 Linux 定向 63 passed，Linux mypy/Ruff 通过 |
| 全仓/最终 CI | Windows 全仓 race 通过；[最终 PR head 八项 CI](https://github.com/XJfyrh/JobForge/actions/runs/37196767188)包含完整 Linux 进程/PG/gRPC/SDK/race，通过 |

F01–F17 的逐项映射、实际命令/耗时、查询计划与锁等待证据见[原提交检查记录](https://github.com/XJfyrh/JobForge/blob/ef0cab8523470f3c9ced77f683a8b14ac3778ab3/docs/evidence/agent-v3-s3-free-2026-10-04.md)，原日志位于仓库外 `E:/JobForge-notes/2026-10-04-agent-v3-s3`。小 fixture 的 EXPLAIN 不代表生产吞吐或锁等待 SLO。

## 失败与修复

初次检查包括缺 hatchling、fault 目录不可写、屏障位置/SQL 字段/启动参数错误、inspector migration 名缺失、格式导入和 CI 源码路径错误；均保留原日志并定向修复。

初次 Linux 进程全套有两项满队列时限失败，隔离负载后相关用例通过；Python 全套 1365 passed/2 failed，相关 IPC tail 四项重跑通过。初次联合层因未复制新 SDK HTTP 脚本整轮失败，补路径后实际安装 SDK/PG HTTP 与外部 F06/F07 通过。**这些不是首次全套通过**；最终完整 CI 结果与初次失败分开记录，未放宽 TTL 或断言。

测试命令与层次边界统一见[测试指南](../tests.md)，生产恢复规则见[恢复指南](../agent-v3/recovery.md)。
