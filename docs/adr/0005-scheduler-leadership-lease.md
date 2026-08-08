# ADR-0005：Scheduler 领导权租约（advisory lock + epoch fencing）

- 状态：Accepted
- 日期：2026-08-08
- 决策者：XJfyrh
- 关联 Issue/PR：Scheduler 领导权卡死修复
- 取代：无（补充 PRD v0.1 决策 #5 的 advisory-lock 单活方案）
- 被取代：无

## 上下文

Scheduler 单活选举原先仅依赖会话级 `pg_try_advisory_lock`：锁只在进程退出（连接断开）或 ctx 取消后的显式 `ReleaseLock` 时释放。当 leader 的扫描循环卡死（死循环、阻塞在失联连接上）而 lock 连接仍存活时，锁永不释放，standby 的 `pg_try_advisory_lock` 永远失败——FR-204"主实例终止后备用实例自动接管"退化为"主实例必须干净终止"，单活变成零活。

PRD v0.1 决策 #5 固定了"不自研共识协议，Scheduler 使用 PostgreSQL advisory lock 实现单活与切换"。本决策在不引入外部组件、不改变该边界的前提下，叠加一个 PostgreSQL 表级租约作为领导权的事实源。

## 决策驱动因素

- 卡死但存活的 leader 必须可被接管（advisory lock 无法表达"持有者已不工作"）。
- 进程终止的快速接管路径（NFR-004：`2 × lock_retry_interval + 2s`）不得回归。
- 复用任务层已验证的"租约 + fencing token"模式，保持最小机制面。
- 双扫描窗口必须无害：promote/recover 既有幂等性是兜底。

## 决策

### 机制

- 新增单例行 `scheduler_leadership(id=1, leader_id, epoch, last_seen)`（migration 0010）；`leader_id IS NULL` 表示无主。
- advisory lock 保留为互斥快路径，租约行是领导权事实源。
- **心跳在扫描循环内**：每次 `scanCycle` 先执行 `UPDATE ... SET last_seen=now() WHERE leader_id=$me AND epoch=$epoch`；0 行 = 已被接管，立即让位。扫描循环卡死 = 心跳停止，这正是本决策要检测的故障。
- **接管双路径**（`TryBecomeLeader`）：
  1. advisory try 成功 → 无条件强制领取（epoch+1）。会话级锁随连接消失，前一持有者必然已死，等待租约过期会拖慢 NFR-004 快速路径；
  2. advisory 被他人持有（卡死场景）→ 仅当租约无主或过期（`last_seen < now() - leadership_timeout`，默认 10s）时条件领取。
- **优雅让位**（`ReleaseLeadership`）：置 `leader_id=NULL`（leader_id+epoch guard）+ 释放 advisory lock，standby 无需等待租约过期即可接管。
- leader 在心跳时机会性补获 advisory lock（接管卡死 leader 后，旧锁被释放时收编），缩短无锁窗口。

### 阈值

- 心跳节律 = 扫描节律（默认 1s）；`JOBFORGE_SCHEDULER_LEADERSHIP_TIMEOUT` 默认 10s。
- 接管上界：终止路径不变（NFR-004 公式）；卡死路径 = `leadership_timeout + lock_retry_interval`。

### 正确性论证

- **脑裂窗口**：旧 leader 复活至其下次心跳自检之间可能短暂双扫描。promote 有 `state IN ('scheduled','retry_wait')` 过滤（重复扫描为 no-op，state_version 只递增一次），recover 有 `lease_until < now()` 过滤与 `ON CONFLICT` 审计，均无副作用放大。AT-09 的"无重复推进"由状态过滤保证，测试以 state_version 断言。
- **epoch 单调**：每次接管递增，陈旧 leader 的心跳/释放被 guard 拒绝。

## 替代方案

- **独立心跳 goroutine**（每 5s 刷新）：无法检测扫描循环卡死（心跳照常更新），核心故障原样保留。拒绝。
- **事务级 advisory lock + 心跳事务**：持锁事务生命周期难控，复杂度高。拒绝。
- **仅修正注释、接受卡死不可接管**：不满足 FR-204 语义。拒绝。

## 后果与兼容性

- 新增一张表与一次迁移；无 proto/错误码/指标变更。
- 单实例部署行为不变（无竞争者时每次强制领取，epoch 递增无害）。
- 卡死接管是新增故障路径，文档（failure-semantics.md）同步。

## 验证方式

- `TestSchedulerStuckLeaderTakeover`：卡死 leader（停止心跳、连接存活）→ standby 租约过期接管、epoch 递增、旧 leader 复活被 epoch fencing、无重复 promote（state_version 断言）。
- `TestSchedulerGracefulReleaseImmediateTakeover`：优雅让位后首次尝试即接管。
- `TestSchedulerFailover`（AT-09）/ `TestSchedulerAdvisoryLock` 适配新 API 后回归，NFR-004 计时断言不变。
