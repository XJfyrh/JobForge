# ADR-0009：Demo 持久业务效果与真实进程崩溃证据边界

- 状态：Accepted
- 日期：2026-08-17
- 决策者：JobForge 维护者
- 关联 Issue/PR：PRD v0.4；PRD v0.1 G-02、§8.2、FR-402、AT-02、NFR-001/002；PRD v0.2 FR-601/602、AT-13/14
- 取代：无
- 被取代：无

## 上下文

JobForge 承诺 at-least-once 投递，要求 Handler 对外部副作用实施业务幂等。现有 `demo.idempotent_effect` 仅以 Worker 进程内 map 记录已处理的 job ID；Worker OS 进程退出后记录丢失，无法证明“副作用已提交、Complete 尚未发送”窗口中的重复投递仍不重复。AT-02、AT-13、AT-14 也只通过省略 Complete 和强制租约过期模拟崩溃，没有实际终止 Worker 子进程。

因此，代码已经证明任务状态机能从 lease expiry 恢复，却尚未按 PRD 最强字面口径证明：真实 Worker 进程在业务效果提交后退出时，另一进程重领同一 job 仍只产生一次持久业务效果。

## 决策驱动因素

- 以最小范围补齐 v0.1/v0.2 的 P0 可靠性证据，不改变公开 API 或任务状态机；
- 持久效果必须跨 Worker 进程存活，并以数据库状态作为可重复的测试同步屏障；
- Demo 依赖不得渗入核心 Worker Runtime 或把 PostgreSQL 效果表提升为任务事实源；
- 证据必须继续表达为 at-least-once + 业务幂等，不得暗示跨系统 exactly-once。

## 决策

### 1. 效果存储

- 新增 migration 0018：`demo_idempotent_effects(job_id uuid primary key, result_ref text not null, applied_at timestamptz not null default now())`；
- 不设置到 `jobs` 的外键。该表模拟独立业务系统的持久效果，不参与任务状态转换、lease、fencing、outbox 或调度事实；
- `job_id` 是本 Demo 唯一幂等键。首次执行用单条 `INSERT ... ON CONFLICT DO NOTHING RETURNING result_ref` 原子产生效果；冲突时读取并返回已提交的 `result_ref`；
- `result_ref` 由 job ID 确定性生成。人工 retry 克隆产生新 job ID，因而不自动共享效果；需要跨 job 去重的业务必须自行选择稳定业务键；
- PostgreSQL 错误由 Handler 归类为 retryable，不允许回退到进程内 map。

### 2. 依赖边界

- 只有默认 Demo Worker wiring 建立小容量 PostgreSQL 连接池并注入效果存储；
- `internal/worker` Runtime、Registry 与自定义 Handler API 继续只依赖 Gateway 协议，不导入 PostgreSQL；
- Worker 不负责运行 migration；部署必须先由 migrate/API/Gateway/Scheduler 路径将 schema 升级到 0018。

### 3. 可控崩溃窗口

- `demo.idempotent_effect` 接受可选 `post_effect_delay_ms`，仅在首次效果成功提交后等待；默认 0，范围 0～60,000ms；
- delay 只用于演示和可靠性测试，必须响应 context cancellation；无效或越界值为不可重试 Handler 错误；
- 真实崩溃测试以效果表行和 job running 状态作为屏障：父进程观察到效果已提交后才调用 OS process kill，并始终执行 Wait 清理；不得靠固定 sleep 猜测窗口；
- 测试 helper 只存在于 `_test.go`，生产二进制不增加 crash 后门。

### 4. 可观测性与安全

- 新增低基数 Counter `jobforge_demo_idempotent_effects_total{outcome}`，outcome 只允许 `applied`、`deduplicated`、`failed`；
- 结构化日志记录 `job_id`、`effect_outcome` 和 duration；不记录完整 payload、数据库 URL、凭据或秘密；
- `job_id` 可出现在日志和 trace 中，但不得成为 metric label。

### 5. 保证边界

本方案只证明同一 PostgreSQL 原子插入所代表的 Demo 业务效果，在同一 job ID 的重复投递下不重复。它不提供任务状态与任意外部系统之间的分布式原子提交，也不把 JobForge 的交付保证升级为 exactly-once。支付、邮件、对象存储等目标系统仍必须接受并持久化合适的 idempotency key，或采用自己的事务/outbox 协议。

## 公开契约与数据影响

- HTTP、Python SDK、gRPC Proto、状态机和领域错误码：无变化；
- PostgreSQL：只新增可回滚的 0018 migration；历史 migration 不修改；
- Worker Demo：启动时新增小容量 PostgreSQL 连接池；核心 Runtime 与自定义 Handler 契约不变；
- 指标：新增一个仅含固定 outcome 标签的 Demo Counter。

## 后果

### 正面

- 进程退出不再丢失 Demo 幂等状态，AT-02/13/14 可按真实 kill 与持久效果口径验收；
- 数据库行提供稳定同步屏障，降低进程故障测试的时序脆弱性；
- 改动隔离在 Demo wiring，不扩大核心调度与公开协议审查面。

### 负面与成本

- 默认 Demo Worker 需要可访问 PostgreSQL，并消耗一个小连接池；
- 效果表持续增长，本增量不自动清理，仅适用于 Demo/测试；
- scale AT-13 真实启动 100 个进程，运行时间和 CI 资源开销高于状态级模拟。

## 考虑过的替代方案

### 方案 A：继续使用进程内 map

未采用：进程退出即丢失，不能满足 AT-02“kill Worker 后重投、副作用仍一次”的字面语义。

### 方案 B：把效果字段写入 jobs

未采用：会把业务效果与任务事实耦合，并容易被误解为 JobForge 对所有外部副作用提供 exactly-once。

### 方案 C：引入独立外部数据库或消息系统

未采用：它更接近跨系统现实，但需要分布式失败矩阵与新部署依赖，超出最小证据闭环。当前 Demo 明确限定为同一 PostgreSQL 内原子效果。

### 方案 D：为生产 Worker 增加 crash RPC/flag

未采用：会制造可被误用的生产故障入口。测试使用当前测试二进制的 `_test.go` helper 子进程。

## 验证与退出条件

- 0018 可从 0017 前滚/回滚，SQLFluff 通过且不增加 ignore；
- 单元/集成测试覆盖首次插入、并发重复、数据库失败、delay 边界与 context cancellation；
- AT-02：效果提交后真实 kill Worker A，Worker B 重领并成功；token 递增，效果表恰一行，applied=1、deduplicated≥1；
- AT-13：默认 100 轮实际终止 Worker OS 进程，非终态静默丢失为 0；
- AT-14：10,000 job 每个至少重复投递一次，效果表恰 10,000 行，重复业务效果为 0；
- 子进程全部有超时、Kill、Wait 与 cleanup 路径；默认 race、integration、scale、Redis 套件不回退；
- 实现前后同环境 Claim、20k-job 与 e2e 中位数恶化均小于 15%；历史 Claim 绝对门禁若仍失败必须继续披露。
