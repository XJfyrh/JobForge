# 性能基准与结果入口

当前基准代码/运行说明见 [benchmarks](../benchmarks/README.md)，从仓库根目录执行。对比必须固定机器、数据库规模、并发度、构建与 Claim 口径；小样本或不同条件不能解释为生产 SLO。

| 要核对什么 | 证据 |
|---|---|
| 旧 Job 微基准、E2E 与历次迁移比较 | [完整历史基准](archive/benchmark-history.md) |
| W4 门槛及未关闭回退 | [原门禁结果](archive/benchmark-history.md#历史微基准绝对值核对)、[当前限制](status.md#限制与未结事项) |
| Worker 满容量释放唤醒 | [2026-09-12 调优报告](archive/worker-capacity-performance.md)、[原始结果](../benchmarks/results/worker-capacity-2026-09-12.txt) |
| Run/步骤/审计/恢复的机制检查 | [测试指南](tests.md)、[S3 免费证据](evidence/agent-v3-s3-free-2026-10-04.md) |

历史报告记录当时结果，不作为新功能已通过或新环境性能承诺。新增比较保存原始输出与适用范围，不覆盖旧失败。
