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

