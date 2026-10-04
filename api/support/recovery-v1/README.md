# support recovery profile v1

本目录定义 [ADR-0025](../../../docs/adr/0025-confirmed-step-recovery.md) 的 schema 3 profile，固定 `recovery_policy=confirmed_uncommitted_v1` 和 `executor_version=linux-v2-recovery-runtime-1`，只用于动态 `support-agent-v1`。原 [profile-v1](../profile-v1/) 与 [agent-v1](../agent-v1/) 的 schema、hash 和共同向量不改写。

[profile-schema.json](profile-schema.json)校验闭集结构；Go domain 继续校验日期、定价、资源组合与整个 profile hash。新 source 示例在[独立准备文件](../../../deploy/support-recovery.source.example.json)，来源审查回执仍为空，不能直接启用收费。

[fixtures.json](fixtures.json)由 `internal/run/recovery_test.go` 生成，Go/Python 共用 definition、profile、原执行身份、完整九字段待执行 step 和 execution binding hash。通常测试只校验一致性；确需更新源合同后，使用 `UPDATE_RECOVERY_FIXTURE=1 go test ./internal/run -run TestRecovery -count=1` 再审查差异。旧向量不随此命令改写。

attempt 的 `recovery_step/recovery_ordinal` 来自原关闭事务，不接受当前 cursor 推测或历史回填。migration 0026 写入第一份证明后禁止 down 丢失证据，应以前滚 migration 修复；运行、复现与层次边界见[恢复指南](../../../docs/agent-v3-recovery.md)。
