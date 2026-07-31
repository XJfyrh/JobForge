# JobForge Benchmarks

本目录包含 JobForge 的性能基准测试，用于测量和冻结关键操作的性能基线。

## 目录结构

```
benchmarks/
├── README.md           # 本文件
├── micro/              # 微基准测试（testing.B）
│   ├── enqueue_test.go # Enqueue 操作基准
│   └── claim_test.go   # Claim 操作基准
├── e2e/                # 端到端基准
│   └── main.go         # 完整生命周期基准
└── scripts/
    └── run_bench.sh    # 一键运行脚本
```

## 环境要求

- Go 1.26+
- PostgreSQL 16（通过 Docker Compose）
- CGO_ENABLED=1（用于 -race 检测）

## 快速开始

### 1. 启动 PostgreSQL

```sh
docker compose -f deploy/compose.yaml up -d postgres
```

### 2. 运行微基准

```sh
cd benchmarks/micro
go test -bench=. -benchmem -benchtime=10s
```

### 3. 运行端到端基准

```sh
cd benchmarks/e2e
go run . -jobs=10000 -workers=4
```

### 4. 使用一键脚本

```sh
./benchmarks/scripts/run_bench.sh
```

## 基准说明

### 微基准（Micro Benchmarks）

使用 Go 标准 `testing.B` 框架，测量单个数据库操作的吞吐量和延迟。

| 基准 | 说明 |
|------|------|
| BenchmarkEnqueue | 单任务插入 |
| BenchmarkEnqueueParallel | 并发任务插入 |
| BenchmarkEnqueueBatch | 批量插入（10/50/100） |
| BenchmarkClaim | 单 Worker 领取 |
| BenchmarkClaimBatch | 批量领取（1/5/10/20） |
| BenchmarkClaimParallel | 多 Worker 并发领取 |
| BenchmarkClaimContention | 高竞争场景（16 Workers） |

### 端到端基准（E2E Benchmark）

测量完整任务生命周期：submit → claim → complete。

输出指标：
- 提交吞吐量（jobs/sec）
- 处理吞吐量（jobs/sec）
- 延迟百分位数（p50/p95/p99）
- Goroutine 稳态验证（NFR 11.2.5）

## 冻结基线

W4 阶段冻结的性能基线记录在 `docs/benchmark.md`。

后续版本的性能不得相对基线下降超过：
- 吞吐量：15%
- 控制面 p95：20%

## 环境变量

| 变量 | 说明 | 默认值 |
|------|------|--------|
| JOBFORGE_TEST_DSN | PostgreSQL 连接串 | postgres://jobforge:jobforge@localhost:5433/jobforge?sslmode=disable |

## 注意事项

1. 基准测试会向数据库写入大量数据，建议使用独立的测试数据库
2. 端到端基准的 goroutine 稳态检查需要等待 60 秒
3. Docker 环境下的性能数据可能与原生环境有差异，记录环境规格用于对比
