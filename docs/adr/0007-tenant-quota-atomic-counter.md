# ADR-0007：租户配额原子计数（派生计数表 + 事务内原子预留）

- 状态：Accepted
- 日期：2026-08-10
- 决策者：JobForge 维护者
- 关联 Issue/PR：PRD v0.3 §5.3（FR-720~726、NFR-304/305、AT-21~23、D4）
- 取代：无（ADR-0001 的配额参数语义保持不变，本 ADR 只改变其执行机制）
- 被取代：无

## 上下文

v0.2 的租户 inflight 上限（FR-302）实现为：Claim 事务内先 `FOR UPDATE SKIP LOCKED` 锁定候选任务，再对每个候选执行 `select count(*) from jobs where tenant_id = $1 and state = 'running'`（migration 0012 的部分索引降低扫描成本）。PRD v0.3 确认该实现存在三个缺口：

- **GAP-703**：配额检查位于行锁事务热区，批量 Claim 逐候选 count 增加 round-trip 与锁持有时间；
- **GAP-704**：count 是快照读，并发事务可能同时观察到旧 count，硬配额存在超配风险（AT-10 只覆盖串行场景）；
- **GAP-705**：count 只统计 `state='running'`，`cancelling` 任务提前释放配额。

此外，满额租户的候选会占满 `LIMIT` 候选窗口（`order by priority desc, created_at asc` 对跨租户候选一视同仁），导致其他租户的 ready 任务被饿死（FR-725 / AT-22 的对照场景）。

PRD v0.3 §5.3 要求编码前以 ADR 固定五项实现决策：全局锁顺序、预留策略、候选选择与回填、预筛陈旧窗口、migration 0012 索引处置。

## 决策驱动因素

- 硬上限正确性不可用性能裁剪换取（NFR-305）：并发 Claim 峰值永不超过 limit；
- PostgreSQL 是唯一事实源：派生计数只能在 PostgreSQL 内，禁止进程内计数、Redis counter 或仅周期聚合作为硬配额依据；
- Claim 单事务原子性不变量（owner、lease、attempt、fencing token、state 同事务）不得破坏；
- 满额租户不得阻塞其他租户（FR-725），且 Claim 吞吐不得相对修改前基线恶化 ≥15%（NFR-304 双口径协议）。

## 决策

### 1. inflight 定义与计数表（FR-720）

inflight = `running + cancelling`（FR-723：running→cancelling 不释放 slot；cancelling→cancelled、Complete、Fail、lease recover 释放）。

新增 versioned migration 创建派生计数表：

```sql
create table tenant_quota_counters (
    tenant_id  text primary key,
    inflight   integer not null check (inflight >= 0),
    updated_at timestamptz not null default now()
);
```

- 行由首次预留（upsert）或迁移 backfill 惰性创建；
- 表是 PostgreSQL 内的派生一致性数据：任何时刻可从
  `select tenant_id, count(*) from jobs where state in ('running','cancelling') group by tenant_id`
  完整重建；
- 该表不得用于任务查询、恢复或审计，jobs 仍是唯一事实源（§7.2）。

### 2. 预留策略：按租户批量预留 + 快照定批 + 竞争回滚重试（FR-721）

采用**按租户批量预留**（PRD §5.3 第 2 项允许的两形态之一），且预留是 Claim 事务的**最后一步**（乐观预留）。Claim 对每个租户的候选组只执行**一条**带硬上限条件的原子 upsert：

```sql
insert into tenant_quota_counters (tenant_id, inflight, updated_at)
values ($1, $n, now())
on conflict (tenant_id) do update
set inflight = tenant_quota_counters.inflight + $n,
    updated_at = now()
where tenant_quota_counters.inflight + $n <= $limit
returning inflight
```

事务内流程：

1. 候选选择（含预筛）并锁定 jobs 行；
2. 不加锁读取候选租户的 counter 快照，按 `limit - inflight` 决定每租户领取数 k（快照只定批量大小，不是硬限制依据）；
3. 对计划候选执行 lease 更新 + attempt 写入；
4. **最后**按 tenant_id 升序执行条件批量预留。条件 `inflight + N <= limit` 使整批在越限时原子失败（返回空行、counter 不递增，满足“整批不满足时不得递增”）；
5. 预留失败 = 快照后并发事务刚填满了 slot：**回滚整个事务并带新快照重试**（有界重试；重试等价于向逐 slot 粒度收敛），预筛使这种竞争只在窗口期内发生。

- **回补/回滚路径**：每个成功预留都对应同一事务内一个实际的 job Claim，commit 时不存在“未用 slot”，无需运行时回补；事务回滚时预留与 Claim 同时消失，即回滚就是全部回补；
- **预留置后的性能理由**（NFR-304 实测依据，见 benchmark.md M1 对比章节）：counter 行锁持有到事务提交；若在事务开头预留，锁会横跨全部 lease 更新，单租户批量领取下 8 并发 claim p95 从 M0 的 236ms 恶化到 1.7s 级；置后使 counter 行锁只持有 commit 前最后一个往返；
- 新行插入 `inflight=N` 只在 N ≤ limit 时发生（快照定批保证），配额关闭（limit ≤ 0）时不进入该路径，始终合法；
- `returning inflight` 供测试在事务内断言“预留后 counter ≤ limit”（AT-21）。

逐 slot 条件预留（`inflight + 1 <= limit`）保留为该机制的极限形态：重试在可用 slot 收敛时与其等价；正确性始终由条件更新在 counter 行锁内的串行判定保证，不依赖快照新鲜度。

### 3. 全局锁顺序（FR-722）

所有同时触碰 jobs 行与 tenant_quota_counters 行的事务遵守同一顺序：

1. **先锁 jobs 行**：Claim 用 `FOR UPDATE SKIP LOCKED`（非阻塞）；Complete/Fail/Cancel 按 owner+token 条件更新单行；Scheduler recover 用 CTE `FOR UPDATE SKIP LOCKED`；
2. **后锁 counter 行**，且同一事务内多个 counter 行按 **tenant_id 升序**加锁。

Claim 因此分两步：候选选择并锁定后执行 lease 更新与 attempt 写入，最后按 tenant_id 升序批量预留（竞争失败则回滚重试）——jobs 行先锁、counter 行后锁的顺序不变。

- SKIP LOCKED 使 jobs 行永不互相阻塞；
- counter 行按全局一致的 tenant_id 顺序加锁，跨事务不会形成 counter 死锁环；
- jobs→counter 的单向顺序对 Claim、Complete、Fail、Cancel、recover 全部路径一致，不存在反向边。

同租户 Claim 在 counter 行上短暂串行化是可接受的（PRD §7.2.5），禁止为吞吐放松硬上限。

### 4. 候选预筛与回填（FR-721/725/726）

Claim 候选选择在行锁**之前**用计数表排除满额租户，谓词内联在候选查询中：

```sql
and not exists (
    select 1 from tenant_quota_counters c
    where c.tenant_id = jobs.tenant_id
      and c.inflight >= $limit
)
```

- 满额租户的行在 `LIMIT` 之前被排除，其他租户候选自然回填候选窗口（FR-725）：租户 A 满额积压 10,000 ready 时，B 的单个 ready 任务可立即进入窗口并被领取（AT-22）；
- 预筛读发生在 READ COMMITTED 快照上，**不加锁**；允许陈旧窗口等于任意并发事务的未提交时长。陈旧只造成两类误差，均不影响正确性：
  - 预筛通过但预留失败（counter 刚被并发事务填满）→ 候选跳过，浪费一次行锁，无超配；
  - 预筛排除但实际有空位（counter 刚被并发事务释放）→ 下一轮 Claim 立即可见（≤1 个 Claim round-trip），无饿死；
- 硬上限始终由第 2 节的事务内原子预留保证，预筛误差只影响性能（FR-726）；
- 预筛可由配置 `JOBFORGE_TENANT_QUOTA_PREFILTER`（默认 true）关闭；关闭后仍执行原子预留，正确性不降级，仅损失公平性性能。关闭路径必须保留并由测试覆盖。

### 5. migration 0012 索引处置（FR-721 / PRD §5.3 第 5 项）

- **新增 migration**：`idx_jobs_tenant_inflight on jobs (tenant_id) where state in ('running','cancelling')`。服务对象：reconcile 的按租户聚合、AT-21 采样断言查询、计数重建；
- **删除 migration**（单独一个 versioned migration，便于回滚粒度）：`drop index idx_jobs_tenant_running`。其唯一消费者 `claimTenantRunningCount` 随本 ADR 实现移除，保留只会徒增写放大。EXPLAIN 证据归档于 benchmark.md 的 M0/M1 基线章节；
- migration 0001~0012 不改写；锁风险说明沿用 0012 的口径（CREATE INDEX 非 CONCURRENTLY 取 SHARE 锁，MVP 规模可接受）。

### 6. 释放路径（FR-722/723）

所有离开 inflight 的状态转换在同一事务扣减计数：

| 转换 | 扣减 |
|---|---|
| running → succeeded（Complete） | -1 |
| running → retry_wait（Fail 可重试） | -1 |
| running → dead（Fail 不可重试） | -1 |
| cancelling → cancelled（Fail 于 cancelling） | -1 |
| running → ready（Scheduler recover 过期 lease） | -1（每租户聚合为一条 -N） |
| cancelling → cancelled（Scheduler recover 过期 lease） | -1（每租户聚合为一条 -N） |
| running → cancelling（Cancel） | **0**（FR-723 保留占用） |
| scheduled/ready/retry_wait → cancelled（Cancel） | 0（本就不占 slot） |

扣减语句带 `inflight > 0`/`greatest(inflight - n, 0)` 保护，防止负数；任何漂移由 reconcile 以 jobs 聚合为准修复（第 7 节）。

### 7. 漂移核对与修复（FR-724）

- **命令**：`jobforge ctl quota-reconcile` 只读比对 counter 与 jobs 聚合，逐租户输出差异；`--repair` 以 jobs 聚合为唯一事实覆盖写入 counter（含将无 inflight 任务的租户计数归零）；
- **周期任务**：Scheduler leader 每 `JOBFORGE_QUOTA_RECONCILE_INTERVAL`（默认 5 分钟）执行一次核对，差异绝对值之和写入 `jobforge_quota_counter_drift` Gauge；发现差异时自动修复并记录结构化日志（含 tenant、counter 值、实际值），日志即审计记录；
- 核对/修复只读写派生数据，不触碰 jobs 状态；修复语句使用与状态转换相同的 jobs-first 锁序（只读 jobs 聚合不加行锁，随后更新 counter 行）。

## 公开契约与数据影响

- **HTTP API**：无变化；
- **gRPC v1**：无变化（配额是 Claim 内部机制）；
- **错误码/状态机**：无新状态、无新错误码；
- **数据库 schema**：新增 `tenant_quota_counters` 表与 `idx_jobs_tenant_inflight` 索引；删除 `idx_jobs_tenant_running` 索引。全部为只增 versioned migration，0001~0012 不改写；
- **配置**：新增 `JOBFORGE_TENANT_QUOTA_PREFILTER`（默认 true）、`JOBFORGE_QUOTA_RECONCILE_INTERVAL`（默认 5m）；`JOBFORGE_TENANT_MAX_INFLIGHT` 语义不变；
- **指标**：新增 `jobforge_quota_reservation_conflicts_total`（无标签 Counter）、`jobforge_quota_counter_drift`（无标签 Gauge），遵守 PRD §8 标签约束；
- **兼容性**：配额关闭（limit ≤ 0）时 Claim 走原有查询路径，行为与 v0.2 一致。释放路径（Complete/Fail/lease 回收）的 counter 扣减与 Scheduler 周期 reconcile 始终运行，与配额开关无关（无配额时通常为 0 行空更新），使配额可运行时开关而无需重新 backfill。

## 后果

### 正面

- 并发 Claim 下硬上限结构性成立：条件更新 `inflight + 1 <= limit` 在 counter 行锁内串行判定，64 并发不超配（AT-21）；
- 满额租户不再占据候选窗口，跨租户公平性可验收（AT-22）；
- cancelling 占用配额，取消风暴不再提前释放 slot（AT-23）；
- Claim 事务内逐候选 count 消失，锁内工作与 round-trip 减少：每租户每次 Claim 仅一条 counter upsert（批量形态），预筛命中时不再逐候选往返。

### 负面与成本

- 每个成功 Claim 多一次 counter 行写；同租户 Claim 在 counter 行上短暂串行；
- 新增派生数据的一致性维护面：所有状态转换路径必须同事务扣减，遗漏即漂移（由 reconcile 兜底但需持续对账测试）；
- 释放路径需要返回/携带 tenant_id，改动 Complete/Fail/recover 的 SQL 返回子句。

### 风险与缓解

- **counter 热点**：单租户高并发 Claim 串行于 counter 行。缓解：事务保持短（预留即条件更新，无额外往返）；以 NFR-304 双口径 p95 量化，恶化 ≥15% 时按 PRD 允许在后续版本评估批量预留；
- **释放遗漏导致 slot 泄漏**：AT-21 终态核对（jobs 聚合 = counter = 0）+ Scheduler 周期 reconcile + drift Gauge 告警面；
- **漂移修复与并发预留竞争**：修复更新 counter 行取行锁，与预留串行；以 jobs 聚合为准意味着修复时刻之后的并发 Claim 不受影响（修复后预留在新值上递增）。

## 考虑过的替代方案

### 方案 A：保留逐候选 count，仅补 cancelling 口径与并发测试

未采用：没有消除 GAP-703 的锁内重复计数，也无法在快照读上证明不超配（GAP-704），且无法实现候选窗口回填（FR-725）。

### 方案 B（早期形态）：事务开头预留（逐候选或批量）

M1 首轮实现即此形态，NFR-304 Phase B 实测暴露两次问题：逐候选 50 次 round-trip 时 claim p95 恶化到 1.7s 级；改批量后 counter 行锁横跨全部 lease 更新仍达 1.0~1.2s 级（M0 为 236ms）。根因是 counter 行锁持有到提交。改为“预留置后 + 快照定批 + 竞争回滚重试”后，counter 行锁只持有 commit 前最后一个往返；“预留数 = 实际 Claim 数”的不变量保持成立（commit 时无未用 slot）。

### 方案 C：进程内计数 / Redis counter / 周期聚合作为硬配额

未采用：违反 PRD §5.3 事务约束与 PostgreSQL 唯一事实源边界。

## 验证与退出条件

- AT-21：64 并发 Claim、limit=10，事务内断言每次预留后 counter ≤ 10，采样断言 running+cancelling ≤ 10，终态聚合一致且无 slot 泄漏；
- AT-22：A 满额 10,000 ready 时 B 单任务 ≤1s 被领取；持续流量变体 p95 ≤1s、max ≤2s；
- AT-23：取消风暴期间 cancelling 占用 slot，未终止前该租户不再被 Claim；
- 关闭预筛（`JOBFORGE_TENANT_QUOTA_PREFILTER=false`）后 AT-21 仍通过（正确性不依赖预筛）；
- NFR-304 双口径：M0 五轮旧 Phase B 归档先行，M1 重写为“预留激活但配额不阻塞”五轮对比，p95 中位数恶化 <15%，计划断言指向 counter 主键与 `idx_jobs_tenant_inflight`；
- 注入漂移后 `ctl quota-reconcile` 可检测，`--repair`/Scheduler 周期修复后 drift Gauge 归零；
- `go test -race ./...` 覆盖并发配额路径。

## 后续工作

- [x] migration 0013（计数表 + backfill）/0014（inflight 索引）/0015（删除 0012 索引）；
- [x] Claim 预筛 + 批量原子预留/逐 slot 降级与释放路径同事务扣减；
- [x] AT-21/22/23 集成测试与 Phase B 重写；
- [x] reconcile 命令、Scheduler 周期核对与两个新指标；
- [ ] 若实测 counter 热点仍显著（多租户更大规模），评估分片计数或近似预筛缓存的后续 ADR。
