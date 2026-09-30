# JobForge Agent 索引

适用于全仓库。先读 [README](README.md)、[当前范围](docs/product/README.md)和[贡献指南](CONTRIBUTING.md)。不要默认加载历史 PRD、规划或评测记录。

## 按任务读取

| 修改范围 | 继续阅读 |
|---|---|
| Go / Python / SQL / Proto | [代码规范](docs/code-standards.md) |
| Run、执行器、Agent | [核心概念](docs/concepts.md)、[S2 合同](docs/product/JobForge_PRD_v0.16.md)、[运行时](docs/agent-v3-runtime.md) |
| 供应商、预算、审计 | [审计合同](docs/agent-v3-provider-audit.md)、[云端运行](docs/agent-v3-cloud-batch.md) |
| 原有 Jobs / 事件 / 产物 | [架构](docs/architecture.md)、[故障语义](docs/failure-semantics.md) |
| 测试、CI、验收 | [验证指南](docs/verification.md)、[Agent 验证专题](docs/agents/verification.md) |
| 可靠性或公开契约决策 | [ADR 索引](docs/adr/README.md)中的相关决策 |

## 不变量

- PostgreSQL 是唯一执行事实源；at-least-once，不承诺 exactly-once。外部副作用需要业务幂等。
- Claim、租约、attempt、fencing 与状态在事务中一致；陈旧 Worker 不得覆盖新状态。
- Go 拥有 Run 执行权；Python 只执行预注册单步。ACK、Wait、EOF、Join、进程组消失不能用替身成功信号代替。
- 多租户隔离、受控工具、累计预算、未知 usage 全额 hold 和停发不可削弱。历史预算授权不自动授权新调用，服从本次用户授权。
- 不记录密钥或完整敏感 payload；只用合成输入验收。生产 registry 不安装测试 adapter、测试 origin 或 gold。
- 状态转换归 domain/service；生成代码不手改，已应用迁移不改写。

## 交付

先读相关实现和测试，再做最小清晰变更。修改公开接口、状态、错误、指标或故障语义时同步契约与回归测试；重要决策新增 ADR，不静默改历史结论。

测试只能使用专用可重建资源；不把服务缺失、平台 skip 或历史报告当本次通过。并发修改运行 race，数据库语义使用真实 PostgreSQL。按[验证指南](docs/verification.md)选择检查并汇报通过、失败和未运行层。

提交前检查 staged diff、秘密、链接和迁移风险。每个完成阶段提交，遵循贡献指南；是否推送、建 PR 或部署由本次授权决定。
