# Agent 验证专题

命令与 CI 对照统一维护在[验证指南](../verification.md)。本页只说明如何选择层次、识别假通过和汇报边界，不复制检查清单。

## 按修改选择证据

| 修改 | 必须覆盖的风险 | 领域入口 |
|---|---|---|
| Jobs / Run 事务 | 真实 PG、陈旧 fence、取消竞争、幂等、恢复；并发修改加 race | [故障语义](../failure-semantics.md)、[Run](../agent-v3-runs.md) |
| SDK / 公共接口 | 正常与拒绝路径、已安装当前 SDK 的真实 HTTP 契约；不能只跑源码 fixture | [SDK](../../sdk/python/README.md) |
| 执行器 / Worker | 真实 Linux 进程、FD、ACK、Wait/EOF/Join、旧进程组消失与失权停止 | [运行时](../agent-v3-runtime.md) |
| 模型 / 审计 / 预算 | 共同 audit/report/observation 向量，真实 PG 首报告/冲突/冻结/晚到与批次屏障 | [审计](../agent-v3-provider-audit.md) |
| S2 决定 / 来源 | Go/Python 决定与方案 schema、跨检索来源合并、重复工具零二次派发、纠错上限 | [S2](../agent-v3-support-agent.md) |
| 业务 / 检索 | 独立 pgvector 库、角色权限、租户与快照绑定；检索质量另跑真实模型 | [业务](../agent-v3-business.md) |
| 观测 | promtool 配置/规则测试、仪表盘生成一致性；遥测失败不改变任务状态 | [观测](../observability.md) |

## 不算通过的情况

- 缺少 DSN、已安装 SDK、专用进程开关或模型后端导致的 skip。
- Windows 不支持分支、纯协议 fixture、合成 HTTP/embedding/model 响应，代替正式 Linux 进程或真实模型质量。
- profile 已登记、合同已接受、字段或枚举已存在，代替实现和验收。
- 只看到 report recorded、管道 write 成功或进程退出码 0，代替完整持久确认和进程清理屏障。
- observed usage、known cost 或缺失 usage，被当作供应商实际账单或零费用。
- 历史冻结报告，被当作当前源码重新验收。

## 资源与安全

仅新建可重建测试资源。同一控制库不并行运行清表套件；按本次创建的 ID 清理，不全局 prune 服务或卷。迁移只新增，历史 SQLFluff 基线不能扩充以绕过检查。

生产 registry 保留 `support-fixed-v1` 与 `support-agent-v1`；测试 adapter、固定回环供应商 origin 和 gold 只能进专用测试构建。所有 profile、manifest、两端运行时须匹配固定 executor version。不得把真实凭据、敏感 payload、gold 或保留集装进 Worker。

收费调用需要本次明确授权与可核验的保守计费上界；历史授权不沿用。未知 usage 按全额 hold、停止当前批次，不能用新批重置累计预算。具体流程按云端运行指南。

## 汇报

记录实际命令、源码版本、环境、服务隔离方式、通过/失败/skip、替身边界及日志位置。先诊断失败，不删除有效回归保障。只有新的修改、失败或未解决风险才扩大或重复检查。提交前检查 staged diff、敏感信息、文档链接与迁移风险。
