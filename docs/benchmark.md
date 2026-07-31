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
