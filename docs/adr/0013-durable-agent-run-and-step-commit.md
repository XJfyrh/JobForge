# ADR-0013：单一 Run 与持久步骤提交

- 状态：Accepted；随 [PR #35](https://github.com/XJfyrh/JobForge/pull/35) 于2026-09-16合并接受。设计接受不代表生产Run或步骤恢复已实现。
- 日期：2026-09-16。
- 关联：[PRD v0.7](../product/JobForge_PRD_v0.7.md)、[路线 v3](../plans/agent-execution-roadmap-v3.md)。
- 拟取代范围：新版本中 ADR-0011 的从头重执行限定、ADR-0001 的基于领取 attempt 耗尽故障预算；旧版本历史记录保持不变。

## 上下文

审批释放执行资源后再次领取，是正常继续而非一次失败。当前代码将 Claim attempt 与 max_attempts 关联，不能直接表达该行为。新业务也需要复用已经持久提交的工具/模型结果，而非每次故障都从输入重算。

维护者不要求旧接口、任务类型或数据迁移兼容。仍要求 PostgreSQL 唯一事实源、at-least-once、终态不可变、租户隔离和 fenced 写入。

## 决策

1. 对外只有 Run；内部步骤不是可独立领取的 job，不引入第二个 scheduler。实现可复用已有领域逻辑和短事务技术，不为复用旧 API 保留兼容层。
2. 分离 attempt_no 与 recovery_count。每次 Claim 增加 attempt_no 和 token；正常审批继续不消耗恢复预算。最多三次实际故障恢复，重放 Fail 或回收不重复计数。
3. 状态集合、转换、业务 outcome 按 PRD v0.7 §3。进入 awaiting_approval 原子提交方案/审批记录并释放 owner/lease/容量；批准原子变 ready，不产生嵌套任务。
4. 使用数据库时钟判断 lease_until>now；匹配 owner/token 但到期的执行器仍然 STALE_LEASE。取消先提交时新的步骤提交与派发授权均拒绝。
5. 步骤提交事务锁 Run，验证执行权、预期游标与输入指纹，保存首次 output_ref/commit_hash，更新下一游标并追加元数据事件。下一步骤由服务端验证的模型决定和注册规则产生，不由任意 Worker cursor 指定。
6. 重复步骤 ACK 先验证当前执行权；同内容重复不重复写入，不同内容 STEP_CONFLICT，陈旧 Worker 始终 STALE_LEASE。持久结果复用是新的有效 Worker 读取 checkpoint，不是给旧 Worker 特赦。
7. profile 固定模型、执行器、工具、提示词、资源版本。旧代码/模型不可得时明确 PROFILE_UNAVAILABLE，不静默升级恢复。未提交的调用允许重做；不承诺确定性重放或从函数内部续作。
8. 步骤状态包含恢复必要数据，属于受保护业务存储；普通 events/Trace 只存引用和元数据。事件 cursor 在 Run 内单调递增。
9. 运行中的取消、总期限和执行段期限统一进入 stopping，首次原因只作为不可改写的审计。后到取消仍记录cancel_requested；收敛时先检查取消，再检查当前Run总期限，最后才允许段超时的有界恢复。Heartbeat不延长stopping执行权；等待态期限可直接结束，无需伪造Worker停止。
10. 固定业务snapshot_id覆盖工单、订单、物流聚合及政策版本；所有工具读同一不可变快照，写入时由业务服务原子检查当前版本向量。
11. 释放lease的yield/终态提交若丢ACK，Worker授权查询已接受记录和完整commit_hash；不要求它再提交一次有效写入，不因此Fail/重试计数。中间步骤的重复ACK仍严格检查当前执行权。

## 数据与公开契约

通过新增 versioned migration 创建 runs、run_attempts、run_steps、run_events 表族，以及 ADR-0014/0015 的预算、审批和动作记录。新运行时只调度 runs，不在旧 jobs 表再创建影子任务；新版本不并行运行旧队列调度器。已有旧表属于历史 migration，不要求拷贝或处理其数据。

建议唯一约束：tenant+submit_key；run+attempt_no；run+step_sequence；run+step_id；run+event_sequence。tenant 外键/查询过滤在每条资源访问路径体现。原子 Claim 继续使用 SKIP LOCKED；取消、步骤提交、释放容量及回收采用一致锁顺序。

公开 API 改为 /v2/runs；旧兼容不作为要求。数据库只新增 versioned migration，不改历史 SQL。新版本使用可重建环境，旧数据迁移不是此决策的实现项。

## 替代方案

- 每次失败完整重执行：代码少，但无法提供恢复已完成步骤的能力和调用成本证据。
- 每个工具步骤单独入队：引入子任务状态/调度/取消联动，超出顺序单 Agent 的需要。
- LangGraph/Temporal 同时保存进度：必须重新定义执行权和恢复所有者，首轮不并列使用。
- 用 attempt_no 限制重试：会把正常审批继续当故障耗尽，拒绝。

## 验证与实施门禁

真实 PG 并发 Claim、lease 到期但未重领、陈旧 token、重复/冲突步骤提交、Cancel/提交竞态、yield/approve 多次领取、恢复额度耗尽、profile 不匹配。实际 Kill/Wait + 自然 lease 回收证明已提交步骤不重做；对未提交窗口如实记录重复调用。

本 S0 只冻结候选协议并运行可行性探针，不创建新 Run 表或更改现有状态机。正式实现时给出 SQL 原子性测试、race 和定向性能证据。
