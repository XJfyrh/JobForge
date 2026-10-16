# ADR-0040：S5 证据绑定与验收一致性

- 状态：Accepted（随本 PR 合并生效）
- 日期：2026-10-16

## 背景

正式部署只比较 build/config 镜像，未检查 freeze 中记录的镜像；门槛与费用上限存在独立副本。运行时测试分片遗漏新增家族，OpenAPI 仍拒绝已实现的 audit schema 2/thinking。指标抓取会重复聚合全量保留数据。

## 决策

正式新批次在任何执行前逐项比较冻结、build 与 config 的四个不可变镜像 ID；变化返回 `FROZEN_IMAGES_CHANGED`，须重新冻结、审查新批次。seen 诊断沿用历史来源校验，不把旧镜像变成当前候选。历史回执、manifest、停止批次与冻结 schema 字节不重写。

S5 的开发/正式门槛、逐批/累计金额上限以 `tools/support_evaluation/s5_policy.py` 为执行单源，并由原 source hashes 覆盖。旧 guard 的闭合规则继续保留，新批次仍按 ADR-0039 自动健康检查；此次不改变任何数值或授权范围。

Linux 自动分片收集新增常规家族，与 go-test 排除模式共用定义，禁止选中验收 skip。两个历史恢复用例按明确名单留在人工恢复验收，不能由常规 CI 推断数据所有权。

公开 ProviderAudit schema 允许版本 1/2 和 thinking；thinking 限 schema 2，与 ADR-0033 的既有实现一致。RunStepDuration 的 kind 标签采用协议 snake_case。`jobforge_run_recoveries` 仍为保留数据 gauge，告警使用 delta，删除保留数据不视为计数器重置。

PG 指标完整快照最多每 15 秒刷新一次，成功与失败都缓存；失败时不输出部分数据或旧成功数据，输出 snapshot_success=0。查询仍有 2 秒超时，刷新由 collector 锁合并。列表预算在同一 repeatable-read 事务按页聚合，保持原三层账户与每 Run 用量语义。

## 影响与验证

镜像变化会阻止此前未检查到的候选漂移；新增冻结错误码不重开旧批次。schema 1 客户端继续有效；schema 2 客户端应使用补齐后的 OpenAPI。指标在两次刷新间保持同一快照；依赖旧 STEP_KIND_* 标签的查询应改用 snake_case。未增索引或迁移。

诊断预检的首次元数据采集需显式传 `--inventory`，不再内置历史容器名；磁盘字段从 `c_free_bytes` 改为 `evidence_volume`/`evidence_free_bytes`，表示输出目录所在卷。旧回执仍保留原字节，新的采集命令见诊断 README。

使用四镜像变更拒绝测试、门槛来源测试、16份 schema 字节及 Go/Python向量检查、SDK audit 1/2与非法thinking组合测试、真实 PG 列表一致性、全仓 race、Linux 禁skip分片和 promtool 告警回归验证。
