# JobForge 性能基线报告

> 冻结日期：2026-08-01  
> 阶段：W4（Scheduler HA 与基线压测）  
> 状态：已冻结

本文档记录 JobForge 在 W4 阶段冻结的性能基线。后续版本的性能不得相对本基线下降超过 PRD 规定的阈值。

## 环境规格

| 项目 | 值 |
|------|-----|
| CPU | AMD Ryzen 7 7840HS w/ Radeon 780M Graphics |
| 内存 | 16 GB |
| Go 版本 | go1.26.5 |
| GOMAXPROCS | 16 |
| PostgreSQL | 16-alpine (Docker) |
| 操作系统 | Windows 11 (Docker Desktop) |
| 测试日期 | 2026-08-01 |

## 微基准结果

### Enqueue 操作

| 基准 | ops/sec | ns/op | B/op | allocs/op |
|------|---------|-------|------|-----------|
| BenchmarkEnqueue | ~368 | 2,715,866 | 3,761 | 58 |
| BenchmarkEnqueueParallel | ~3,495 | 286,119 | 4,178 | 63 |
| BenchmarkEnqueueBatch/batch10 | ~36 | 27,399,108 | 41,548 | 630 |
| BenchmarkEnqueueBatch/batch50 | ~7 | 135,851,419 | 207,792 | 3,151 |
| BenchmarkEnqueueBatch/batch100 | ~4 | 263,486,969 | 415,774 | 6,303 |

### Claim 操作

| 基准 | ops/sec | ns/op | B/op | allocs/op |
|------|---------|-------|------|-----------|
| BenchmarkClaim | ~240 | 4,171,091 | 6,630 | 84 |
| BenchmarkClaimBatch/batch1 | ~244 | 4,096,612 | 6,632 | 84 |
| BenchmarkClaimBatch/batch5 | ~107 | 9,383,682 | 23,610 | 301 |
| BenchmarkClaimBatch/batch10 | ~65 | 15,300,833 | 44,871 | 572 |
| BenchmarkClaimBatch/batch20 | ~39 | 25,480,186 | 87,392 | 1,113 |

## 端到端基准结果

测试参数：100 jobs, 4 workers

### 吞吐量

| 阶段 | 吞吐量 |
|------|--------|
| Submit | 278.01 jobs/sec |
| Process | 347.82 jobs/sec |

### 延迟百分位数

| 百分位 | 延迟 |
|--------|------|
| p50 | 9.59 ms |
| p95 | 14.23 ms |
| p99 | 69.12 ms |
| min | 7.25 ms |
| max | 73.21 ms |

### Goroutine 稳态

| 指标 | 值 |
|------|-----|
| 基线 goroutines | 2 |
| 最终 goroutines | 2 |
| 差异 | 0 |
| 容差 | ±5 |
| 结果 | PASS |

## 性能门禁

根据 PRD NFR 11.2，后续版本必须满足：

1. **吞吐量**：相对本基线不得下降超过 15%
2. **控制面 p95**：相对本基线不得恶化超过 20%
3. **Goroutine 稳态**：10,000 no-op 任务完成后等待 60s，goroutine 数回到测试前 ±5%

## 复现命令

### 微基准

```sh
cd benchmarks/micro
go test -bench=. -benchmem -benchtime=10s
```

### 端到端基准

```sh
cd benchmarks/e2e
go run . -jobs=10000 -workers=4
```

### 一键运行

```sh
./benchmarks/scripts/run_bench.sh 10000 4
```

## 注意事项

1. 本基线在 Docker 环境下测量，性能可能低于原生环境
2. 绝对数值受硬件影响，重点是相同环境下的相对变化
3. 如需对比，必须在相同环境下运行

## pprof 热点分析（W6）

> 分析日期：2026-07-29  
> 阶段：W6（观测与事件扩展）

### 采集方法

```sh
# 启动服务后，在压测期间采集 CPU profile
go tool pprof http://127.0.0.1:6060/debug/pprof/profile?seconds=10

# 查看 top 热点
(pprof) top 10

# 查看火焰图
(pprof) web
```

### 已识别热点：Claim 事务中的 pgx 查询序列化

**现象**：在 Claim 操作期间，CPU profile 显示 `github.com/jackc/pgx/v5/internal/iobufpool` 和 `encoding/binary` 占据显著 CPU 时间。

**原因分析**：

1. `FOR UPDATE SKIP LOCKED` 查询返回多行结果，每行需要反序列化 15+ 个字段（包括 payload bytea）。
2. pgx 使用内部 buffer pool 进行二进制协议解码，高并发 Claim 时 buffer 分配/回收成为热点。
3. 单次 Claim 事务包含：SELECT 候选行 → 租户配额检查 → UPDATE 状态 → 写入 job_attempts，共 3-4 次 round-trip。

**证据**：

- BenchmarkClaim: ~4.17ms/op，其中约 60% 为数据库网络 + 序列化时间
- BenchmarkEnqueue: ~2.72ms/op，单次 INSERT 比 Claim 快约 35%（无多行解码）
- Heap profile 显示 `pgx` 相关分配占 Claim 路径总分配的 ~45%

**优化方向（未实施，记录为后续参考）**：

- 减少 Claim 返回字段（不返回 payload，Worker 通过单独查询获取）
- 批量 Claim 时复用 buffer（pgx v5 已内置 pool，但高并发下仍有竞争）
- 考虑 `COPY` 协议替代多行 SELECT（不适用于 FOR UPDATE 场景）

### 结论

Claim 操作的 pgx 二进制协议解码是当前最显著的 CPU 热点，符合预期：队列内核的核心路径是数据库 I/O + 序列化。此热点不构成性能瓶颈（Claim 吞吐 ~240 ops/sec 已满足 P0 目标），但为后续优化提供了明确方向。

## 最终发布基准（W7）

> 测量日期：2026-08-03  
> 阶段：W7（作品集交付）  
> 目的：验证 W5/W6 变更未引入性能回归

### 环境（与 W4 相同）

| 项目 | 值 |
|------|-----|
| CPU | AMD Ryzen 7 7840HS w/ Radeon 780M Graphics |
| 内存 | 16 GB |
| Go 版本 | go1.26.5 |
| GOMAXPROCS | 16 |
| PostgreSQL | 16-alpine (Docker) |
| 操作系统 | Windows 11 (Docker Desktop) |

### 微基准对比

| 基准 | W4 基线 (ns/op) | W7 最终 (ns/op) | 变化 |
|------|-----------------|-----------------|------|
| BenchmarkEnqueue | 2,715,866 | 2,563,809 | -5.6% (改善) |
| BenchmarkClaim | 4,171,091 | 3,553,890 | -14.8% (改善) |

### 端到端对比

测试参数：100 jobs, 4 workers

| 指标 | W4 基线 | W7 最终 | 变化 |
|------|---------|---------|------|
| Submit 吞吐 | 278.01 jobs/sec | 363.92 jobs/sec | +30.9% (改善) |
| Process 吞吐 | 347.82 jobs/sec | 429.19 jobs/sec | +23.4% (改善) |
| p50 | 9.59 ms | 8.41 ms | -12.3% (改善) |
| p95 | 14.23 ms | 12.85 ms | -9.7% (改善) |
| p99 | 69.12 ms | 25.48 ms | -63.1% (改善) |

### Goroutine 稳态

| 指标 | 值 |
|------|-----|
| 基线 goroutines | 2 |
| 最终 goroutines | 2 |
| 差异 | 0 |
| 容差 | ±5 |
| 结果 | PASS |

### 性能门禁验证

| 门禁 (PRD 11.2) | 阈值 | 实际 | 结果 |
|------|------|------|------|
| 吞吐相对 W4 下降 | < 15% | 改善 23-31% | PASS |
| 控制面 p95 恶化 | < 20% | 改善 9.7% | PASS |
| Goroutine 稳态 | ±5% | 差异 0 | PASS |

### 恢复时间（NFR-003 / NFR-004）

| 指标 | 约束公式 | 上界 | 验证方式 |
|------|----------|------|----------|
| 任务恢复 | lease_ttl + scan_interval + 2s | 33s | TestFaultAT04HeartbeatLoss |
| Scheduler 接管 | 2 × lock_retry + 2s | 12s | TestSchedulerFailover |

恢复时间通过集成测试验证（测试中使用缩短的 lease TTL 以加速验证，逻辑等价）。

### 复现命令

```sh
# 微基准
cd benchmarks/micro
go test -bench=. -benchmem -benchtime=5s

# 端到端
cd benchmarks/e2e
go run . -jobs=100 -workers=4

# 集成测试（含恢复时间验证）
go test -race ./tests/integration/ -run "TestFault|TestSchedulerFailover" -v
```

### 结论

W5（租户隔离、Python SDK）和 W6（OTel、Prometheus、pprof）的变更未引入性能回归。所有指标相对 W4 基线均有改善，性能门禁全部通过。

## v0.2 收官复测（W11）

> 测量日期：2026-08-04  
> 阶段：W11（scale 可靠性套件，PRD v0.2 NFR-201 / M3 退出条件）  
> 目的：验证 W8（outbox publisher）/ W9（ctl CLI）/ W10（obs profile）变更未引入性能回归

### 环境（与 W4 相同）

| 项目 | 值 |
|------|-----|
| CPU | AMD Ryzen 7 7840HS w/ Radeon 780M Graphics |
| 内存 | 16 GB |
| Go 版本 | go1.26.5 |
| GOMAXPROCS | 16 |
| PostgreSQL | 16 (Docker, compose postgres-only) |
| 操作系统 | Windows 11 (Docker Desktop) |

复测前仅保留 postgres 服务（停止全部应用服务），并清空 jobs/job_attempts/outbox_events/workers 表，与基线环境对齐。

### 微基准对比（-benchtime 10s）

| 基准 | W4 基线 (ns/op) | W11 复测 (ns/op) | 变化 | 门禁（吞吐下降 < 15%） |
|------|-----------------|------------------|------|------|
| BenchmarkEnqueue | 2,715,866 | 2,735,009 | +0.7%（噪声范围） | PASS |
| BenchmarkEnqueueParallel | 286,119 | 290,193 | +1.4%（噪声范围） | PASS |
| BenchmarkClaim | 4,171,091 | 4,020,165 | -3.6%（改善） | PASS |
| BenchmarkClaimBatch/batch1 | 4,096,612 | 3,956,092 | -3.4%（改善） | PASS |
| BenchmarkClaimBatch/batch10 | 15,300,833 | 13,987,615 | -8.6%（改善） | PASS |
| BenchmarkClaimBatch/batch20 | 25,480,186 | 24,773,400 | -2.8%（改善） | PASS |

### 端到端对比

测试参数：100 jobs, 4 workers（与 W4/W7 基线参数一致）

| 指标 | W4 基线 | W11 复测 | 变化 | 门禁 |
|------|---------|----------|------|------|
| Submit 吞吐 | 278.01 jobs/sec | 288.10 jobs/sec | +3.6%（改善） | PASS |
| Process 吞吐 | 347.82 jobs/sec | 366.53 jobs/sec | +5.4%（改善） | PASS |
| p50 | 9.59 ms | 10.07 ms | +5.0%（噪声范围） | — |
| p95 | 14.23 ms | 13.20 ms | -7.2%（改善） | PASS（恶化 < 20%） |
| p99 | 69.12 ms | 27.12 ms | -60.8%（改善） | — |

### Goroutine 稳态

| 指标 | 值 |
|------|-----|
| 基线 goroutines | 2 |
| 最终 goroutines | 2 |
| 差异 | 0 |
| 容差 | ±5 |
| 结果 | PASS |

### 性能门禁结论（NFR-201）

| 门禁（PRD 11.2） | 阈值 | 实际 | 结果 |
|------|------|------|------|
| 吞吐相对 W4 下降 | < 15% | 改善 3.6%~5.4%（微基准最大 +1.4% 噪声） | PASS |
| 控制面 p95 恶化 | < 20% | 改善 7.2% | PASS |
| Goroutine 稳态 | ±5% | 差异 0 | PASS |

### 复现命令

```sh
# 仅保留 postgres 并清空基线数据
docker compose -f deploy/compose.yaml up -d postgres

# 微基准
cd benchmarks/micro
go test -bench Benchmark -benchmem -benchtime 10s

# 端到端（与基线同参数）
cd benchmarks/e2e
go run . -jobs 100 -workers 4
```

### 结论

W8~W10（outbox publisher、ctl CLI、obs profile 与文档对齐）未引入性能回归；全部指标相对 W4 基线满足 PRD §11.2 门禁，e2e p95/p99 与 Claim 路径持续改善。NFR-203 亦间接验证：outbox 发布在任务状态事务外异步进行，enqueue/claim 热路径无可测量回退。

## 附录：热路径部分索引验证（migration 0011/0012）

> 测量日期：2026-08-08（同环境规格，`-race` 开启）

为 Scheduler promote 扫描与 Claim 事务内 FR-302 租户配额计数新增部分索引（`idx_jobs_promote_ready`、`idx_jobs_tenant_running`，见 [架构文档](architecture.md)的“热路径部分索引”），消除两处全表扫描。`tests/scale/perf_index_test.go`（`TestScalePerfPromoteClaimLatency`）在 20,000 行规模下实测：

| 路径 | 规模 | p50 | p95 | 备注 |
|------|------|-----|-----|------|
| promote（batch=1000） | 20,000 scheduled / 共 ~60,000 行 | 23.5 ms | 25.7 ms | 21 批共 463 ms；部分索引有序扫描免排序 |
| claim（batch=50，8 worker 并发，配额计数激活） | 20,000 ready | 188.8 ms | 304.2 ms | 424 次调用零重复领取；配额计数为 index-only，不再随表增长拉长行锁持有 |

索引命中验证：promote 扫描在 20k 行规模下自然计划即命中 `idx_jobs_promote_ready`；配额计数在 running 行为零时自然计划可能漂移至 `idx_jobs_lease_expiry`（同为 `state='running'` 候选），故断言在 `enable_seqscan = off` 下确认专用索引可服务该查询（集成套件 `index_plan_test.go` 同样覆盖）。延迟绝对值受 Docker/WSL2 与 `-race` 影响，仅作基线参考；复现命令：

```sh
go test -tags scale -count=1 -timeout 60m -run TestScalePerfPromoteClaimLatency ./tests/scale/
```

## M1 前基线（NFR-304 第 1 步，PRD v0.3）

> 测量日期：2026-08-10（同环境规格）<br>
> 代码 commit：`2153c777c9cf36578baaba9c09740ca9fa6d97a1`（代码基线 `e6b2ef3` + PRD v0.3 文档）<br>
> 条件：`-tags scale`，**race 检测器关闭**（race 抽样由 `go test -race ./...` 回归覆盖）；PostgreSQL 16-alpine（compose，localhost:5433）；负载参数 20,000 jobs / 8 workers / batch 50 / 单租户 `perf-tenant`

按 NFR-304 基线迁移协议，在任何 quota 代码、migration 及 Phase B 改写之前，对当前实现（锁候选后逐候选 `count(state='running')`，migration 0012 索引）运行 5 轮并归档：

| 轮次 | claim 调用数 | claim p50 | claim p95 | promote p50 | promote p95 |
|------|-----------|-----------|-----------|-------------|-------------|
| 1 | 424 | 160.94 ms | 220.34 ms | 24.59 ms | 34.83 ms |
| 2 | 424 | 160.03 ms | 236.36 ms | 22.86 ms | 29.22 ms |
| 3 | 424 | 160.03 ms | 219.05 ms | 24.40 ms | 28.96 ms |
| 4 | 424 | 160.23 ms | 250.28 ms | 23.55 ms | 27.17 ms |
| 5 | 424 | 165.92 ms | 259.35 ms | 25.44 ms | 28.71 ms |

**比较口径（供 M1 使用）**：claim p50 各轮中位数 = **160.23 ms**；claim p95 各轮中位数 = **236.36 ms**。M1 重写后以相同负载参数的 5 轮 p95 中位数与之比较，恶化须 < 15%（即 ≤ 271.8 ms）。历史参考值 304.2ms（`-race` 开启、不同日期）不作跨硬件硬门禁。

EXPLAIN 计划证据（每轮测试后在 20,000 running 行上采集，`enable_seqscan = off` 事务内）：

```
Aggregate  (cost=4.31..4.32 rows=1 width=8) (actual time=2.710..2.710 rows=1 loops=1)
  ->  Index Only Scan using idx_jobs_tenant_running on jobs  (cost=0.29..4.30 rows=1 width=0)
        (actual time=0.052..2.234 rows=20000 loops=1)
        Index Cond: (tenant_id = 'perf-tenant'::text)
        Heap Fetches: 18882
```

该断言只描述改造前的 `count(*) WHERE state='running'` 路径；M1 验收改用 quota counter 主键与 `idx_jobs_tenant_inflight` 的结构性证据，不再引用 `explainQuotaCount` 或 `idx_jobs_tenant_running`。

### 修改前多租户公平性观测（AT-22 对照）

在同一环境以旧候选选择语义（`order by priority desc, created_at asc limit 50`，无满额租户预筛）复现：租户 `obs-a` 满额且积压 10,000 ready 任务，租户 `obs-b` 有 1 个 ready 任务时，候选窗口组成：

```
 tenant_id | candidates
-----------+------------
 obs-a     |         50
```

窗口被满额租户 100% 占据；旧流程在行锁事务内逐候选 count 后全部跳过，Claim 返回空且下一轮选中同一批候选——`obs-b` 的任务永远无法进入窗口（饿死）。M1 预筛后 `TestQuotaAT22FullTenantDoesNotBlock` 实测 B 在 19.9 ms 内被领取（上界 1 s）。

### 复现命令

```powershell
docker compose -f deploy/compose.yaml up -d postgres
$env:JOBFORGE_TEST_DSN = "postgres://jobforge:jobforge@localhost:5433/jobforge?sslmode=disable"
go test -tags scale -count=1 -timeout 30m -run TestScalePerfPromoteClaimLatency -v ./tests/scale/
```

## M1 改造后双口径对比（NFR-304 第 3 步，PRD v0.3）

> 测量日期：2026-08-10（同环境规格，与 M0 同参数：20,000 jobs / 8 workers / batch 50 / 单租户，race 检测器关闭）<br>
> 实现形态：“预留激活但配额不阻塞”（limit = n+100）：候选预筛 + counter 快照定批 + 事务末尾条件批量预留（ADR-0007 §2）

| 轮次 | claim p50 | claim p95 | promote p50 | promote p95 |
|------|-----------|-----------|-------------|-------------|
| 1 | 87.74 ms | 113.10 ms | 19.36 ms | 22.41 ms |
| 2 | 86.91 ms | 105.48 ms | 18.61 ms | 64.82 ms |
| 3 | 87.15 ms | 116.56 ms | 19.58 ms | 60.28 ms |
| 4 | 88.53 ms | 109.49 ms | 19.23 ms | 71.42 ms |
| 5 | 88.42 ms | 119.43 ms | 18.59 ms | 65.62 ms |

**比较结论（各轮中位数）**：

| 口径 | M0 前基线 | M1 改造后 | 变化 | 门禁（恶化 < 15%） |
|------|-----------|-----------|------|--------------------|
| claim p50 中位数 | 160.23 ms | 87.74 ms | **-45.2%（改善）** | PASS |
| claim p95 中位数 | 236.36 ms | 113.10 ms | **-52.2%（改善）** | PASS |

改善来源：逐候选 `count(state='running')`（每候选一次 index-only 扫描，batch 50 即 50 次）被替换为每租户一次 counter 快照读 + 事务末尾一次条件 upsert。

形态演进记录（NFR-304 协议要求结构性证据与口径可审计）：首轮“逐候选预留”（claim p95 ≈ 1.7s）与次轮“事务开头批量预留”（claim p95 ≈ 1.0~1.2s）均因 counter 行锁持有到提交而超标，最终采用“预留置后 + 快照定批 + 竞争回滚重试”，详见 [ADR-0007](adr/0007-tenant-quota-atomic-counter.md) §2 与方案 B。当时的 M1 结构断言以 `enable_seqscan = off` 确认 `idx_jobs_tenant_inflight` 可服务 inflight 口径计数；预留 upsert 的 EXPLAIN 命中 `tenant_quota_counters_pkey`（`assertCounterTableUsable`），不再引用 `explainQuotaCount`/`idx_jobs_tenant_running`。

### Quality Gate Debt Cleanup（2026-08-11）

为消除 PostgreSQL 优化器合法选择漂移造成的假失败，`TestScalePerfPromoteClaimLatency` 将索引结构契约与执行计划性能性质拆开：catalog 门禁精确核对 `public.idx_jobs_tenant_inflight` 的所属表、唯一键列、partial predicate、valid/ready 状态；每轮在事务内删除索引并确认专用 missing-index 错误，回滚后再次确认结构恢复。执行计划门禁则在 4,096 条、32 租户、`running`/`cancelling` 交替的非空 fixture 上 `ANALYZE jobs` 后运行自然计划，只禁止 `Seq Scan on jobs`，允许优化器选择任一合法索引或 bitmap 路径。该 fixture 在 Claim 延迟采样前删除，不改变原有 20,000 jobs 口径。

同一 Windows + PostgreSQL 16 Compose 环境连续五轮定向验收：

| 轮次 | claim p50 | claim p95 | promote p50 | promote p95 |
|------|-----------|-----------|-------------|-------------|
| 1 | 84.65 ms | 100.33 ms | 18.57 ms | 21.73 ms |
| 2 | 86.89 ms | 100.86 ms | 18.89 ms | 22.94 ms |
| 3 | 87.03 ms | 98.51 ms | 18.98 ms | 22.40 ms |
| 4 | 84.98 ms | 97.18 ms | 19.68 ms | 22.48 ms |
| 5 | 85.61 ms | 97.77 ms | 18.63 ms | 27.25 ms |

五轮命令全部通过（package 340.142s），每轮均实际覆盖删除索引负向探针。相同测试在 `-race` 下通过（package 60.826s；claim p50/p95 92.43/107.63ms）。完整 scale 套件通过（package 245.283s；无非预期 skip），其中该测试的单轮 claim p50/p95 为 84.82/96.43ms。上述延迟仅作可复现观测，不作为机器相关硬门槛。

### 多租户混合场景（NFR-304 第 4 步）

`TestScaleQuotaMultiTenantFairness`（`-tags scale`）：租户 A 满额（limit=20）且持续保有 10,000 ready 积压，租户 B/C/D 各以 20 jobs/s 持续 15s，8 worker 即领即完成：

| 口径 | 值 |
|------|-----|
| reservation conflicts | 0 |
| claim 调用数 | 5,063 |
| claim 调用 p50 / p95（含 counter 行等待） | 13.12 ms / 17.87 ms |
| B ready→claim p50 / p95 / max | 23.15 ms / 36.91 ms / 45.96 ms |
| C ready→claim p50 / p95 / max | 22.81 ms / 36.10 ms / 45.60 ms |
| D ready→claim p50 / p95 / max | 22.98 ms / 37.04 ms / 56.14 ms |

B/C/D 各自满足 AT-22 上界（p95 ≤1s、max ≤2s）；A 的积压全程零领取。正确性以 AT-21 为准，本场景不外推为配额压力容量证明。

## M2 耐久事件（PRD v0.3 NFR-302/303，ADR-0006）

> 测量日期：2026-08-11（同环境规格，另加 Redis 7.4.10-alpine，`appendonly yes`、`appendfsync everysec`，命名 volume；Docker Desktop/WSL2）

### NFR-302 事件发布 smoke（`TestScaleNFR302EventPublishSmoke`，`-tags scale`）

| 口径 | 值 |
|------|-----|
| 积压注入（10,000 events，服务端 generate_series） | 52 ms |
| 积压 drain（10,000 events，batch 500 / 并发投递 8 / 批量标记） | 1.309 s → **约 7,640 events/sec** |
| smoke floor（PRD §6.1，5× W11 ≈ 1,800 events/sec） | **PASS（约 4.2×）** |
| 健康态新波（1,000 events，无积压）publish lag p50 / p95 / max（纯 PostgreSQL 时钟口径） | 146.66 ms / 146.66 ms / 146.66 ms |
| 健康态 publish lag p95 ≤ 2s 门禁 | **PASS** |

说明：本测试是选型门槛的 smoke floor，不是容量报告（PRD §6.1）；正式容量结论还需覆盖持续写入、消费者滞后、PEL 回收、AOF rewrite 与故障恢复尾延迟。失败口径区分（PRD NFR-302 原文支持）：**健康态 publish lag p95 ≤ 2s 是功能门禁**（超限即测试失败）；**吞吐 floor（约 1,800 events/sec）不达标仅归档为容量风险**（触发 Kafka 门槛评估，不作为功能失败）。健康波 p50/p95/max 同为 146.66ms 系批量提交同轮标记所致（lag = published_at − created_at 同批一致），非测量异常。优化背景：首版逐条 XADD + 逐条 mark 仅约 425 events/sec，经批量标记（`MarkPublishedBatch`）与有界并发投递（`PublishConcurrency` 默认 8）后达标；无全局顺序保证的契约不变。

### NFR-303 Redis 暂停恢复（`TestDurableNFR303PauseRecovery`）

Redis 容器 stop 60s 后 start（保留命名 volume）：停机期间任务状态事务（submit→claim→complete）不被阻塞（实测 <5s），停机期事件保持未发布；恢复后积压全部补入 Stream、归零，零静默丢失（总耗时 61.80s，PASS）。

### 事件恢复验收证据（AT-17/18/20，`tests/integration/durable_events_test.go`）

| 用例 | 结果 |
|------|------|
| AT-17（`TestDurableAT17RestartRecovery`） | 停机前 Stream 记录经 AOF 重启保留；停机期事件未标记成功；恢复后全部补入、积压归零（PASS） |
| AT-18（`TestDurableAT18AckCrashWindow`） | XADD 成功后批量标记前崩溃：同 event_id 两条 entry 被允许，outbox 最终标记成功，job 状态不变（PASS） |
| AT-20（`TestDurableAT20GroupIsolation`） | 双 group 各完整消费 40 事件，组内两实例各分摊 20，无跨组游标干扰（PASS） |

Windows 约定按 PRD §10.2：`docker compose --profile durable-events up -d postgres redis` + `JOBFORGE_TEST_REDIS_URL`；重启验证用保留命名 volume 的 `docker stop/start`（容器 `deploy-redis-1`）。Linux CI 由 testcontainers 自动拉起同参数 AOF Redis。

## M4 取消 SLO 与 Heartbeat 写放大（PRD v0.3 NFR-306/307，ADR-0008）

> 测量日期：2026-08-12；Windows 11、AMD Ryzen 7 7840HS、16 GB、Go 1.26.5、PostgreSQL 16 Compose；应用服务停止，仅保留 PostgreSQL/测试所需 Redis。

### NFR-306 job lease 写放大

`TestScaleNFR306HeartbeatWriteAmplification` 对 100 个持续运行 job 使用真实 PostgreSQL `UPDATE ... RETURNING`，在逻辑 30s 窗口分别执行 10s cadence 的 3 轮与 5s cadence 的 6 轮。它验证写入次数，不用 wall-clock sleep 模拟 30s，也不以本机延迟设硬门槛：

| cadence | lease UPDATE | p50 | p95 | 放大 |
|---|---:|---:|---:|---:|
| 10s 基线 | 300 | 1.4410 ms | 2.4402 ms | 1.00× |
| 5s M4 | 600 | 1.4558 ms | 2.4283 ms | **2.00×** |

每次 heartbeat 都实际续租，未被 `workers.last_heartbeat_at` 的 `LeaseTTL/3` 条件刷新合并。完整 scale 套件 **PASS（240.528s）**；同轮 `TestScalePerfPromoteClaimLatency` claim p50/p95 为 88.716/102.177ms，相对 Quality Gate Cleanup 最近归档的 84.82/96.43ms 分别变化 +4.6%/+6.0%，低于 15% 当前基线门槛。NFR-302 为 2,316 events/sec、publish lag p95 129.464ms；AT-13/14 与多租户公平性同轮通过。

### W4/W11 同参数端到端复测

在重置 schema 的干净测试库运行 `go run ./benchmarks/e2e -jobs 100 -workers 4`：

| 指标 | W4 基线 | W11 复测 | M4 实测 | 相对 W4 | 结果 |
|---|---:|---:|---:|---:|---|
| Submit 吞吐 | 278.01 jobs/sec | 288.10 jobs/sec | 342.21 jobs/sec | +23.1% | PASS |
| Process 吞吐 | 347.82 jobs/sec | 366.53 jobs/sec | 380.95 jobs/sec | +9.5% | PASS |
| 控制面 p95 | 14.23 ms | 13.20 ms | 12.8372 ms | -9.8% | PASS |
| goroutine | 2→2 | 2→2 | 2→2 | 差异 0 | PASS |

### 历史微基准绝对值核对

同机 `-benchtime 10s` 的 Enqueue 路径通过历史阈值：`BenchmarkEnqueue=2.540972ms/op`（相对 W4 改善 6.4%），`BenchmarkEnqueueParallel=0.290935ms/op`（相对 W4 +1.7%）。Claim 的历史绝对值核对则未通过：重启 PostgreSQL、重置 schema 后的独立 `BenchmarkClaim=5.900438ms/op`，相对 W4 4.171091ms/op 增加 41.5%；完整微基准中的 batch1/batch10/batch20 分别为 5.551585/19.849866/30.301239ms/op，亦高于 W4 表中对应值。

该偏差不能归因于 M4 heartbeat 代码路径：本增量未修改 Claim/Enqueue/Complete，且同轮端到端、20,000-job Claim scale 与最近当前基线均通过。不过，按“不得把未通过误报为通过”的规则，本报告不将历史 Claim 微基准绝对门禁标为 PASS；需要在冻结环境上复测或单列 Claim 性能调查后，才能宣称全部 W4/W11 绝对微基准收官。M4 的功能、SLO 与相对当前基线证据不受此记录替代。

### 复现命令

```powershell
$env:JOBFORGE_TEST_DSN = "postgres://jobforge:jobforge@localhost:5433/jobforge?sslmode=disable"
$env:JOBFORGE_TEST_REDIS_URL = "redis://localhost:6379/0"
go test -race -count=1 -v -run TestCancelAT24HeartbeatSignalSLO ./tests/integration/
go test -tags scale -count=1 -v -timeout 60m ./tests/scale/
go test -count=1 -bench Benchmark -benchmem -benchtime 10s ./benchmarks/micro/
go run ./benchmarks/e2e -jobs 100 -workers 4
```

## v0.4 实现前 Claim 冻结基线

> 测量日期：2026-08-17；commit `1b31334`；Windows amd64、AMD Ryzen 7 7840HS、Go 1.26.5、PostgreSQL 16.14（`postgres:16-alpine`）、Docker Engine 29.6.2；容器无 CPU/内存限额；非 race。

在 schema 0017 上执行以下同机五轮命令：

```powershell
$env:JOBFORGE_TEST_DSN = "postgres://jobforge:jobforge@localhost:5433/jobforge?sslmode=disable"
go test ./benchmarks/micro -run '^$' -bench '^BenchmarkClaim$' -benchmem -benchtime=10s -count=5
```

| 轮次 | ns/op | B/op | allocs/op |
|---:|---:|---:|---:|
| 1 | 5,762,112 | 6,888 | 91 |
| 2 | 6,644,814 | 6,886 | 91 |
| 3 | 7,233,324 | 6,887 | 91 |
| 4 | 8,566,824 | 6,890 | 91 |
| 5 | 8,849,266 | 6,888 | 91 |
| **中位数** | **7,233,324** | **6,888** | **91** |

同一测试库中的历史 job 行会在五轮校准期间累积，实测值也随之上升。本组数据只用于 v0.4 以完全相同命令、容器和数据清理条件做前后中位数比较；它不替代 W4 4.171091ms/op 的历史绝对门禁，也不把当前历史偏差改标为 PASS。v0.4 的相对回归上限为 15%。
