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
| BenchmarkGatewayPollClaim | Register→Poll 完整入口；可注入 owner inflight 脏库夹具 |

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
| JOBFORGE_BENCH_GATEWAY_DIRTY_INFLIGHT | Gateway Poll 计时前注入的无关 inflight jobs 数；仅用于 benchmark | 0 |

### Gateway Poll clean/dirty 对照

正式脏库口径固定为 20,000 条 `running/cancelling` jobs，平均分布到 8 个非目标 owner。夹具通过服务端 `generate_series` 在计时前写入并执行 `ANALYZE jobs`；它不改变生产配置或 Poll 语义。每轮必须重建 schema，不能用同一数据库上的 `-count=5` 代替五个独立轮次：

```powershell
$env:JOBFORGE_TEST_DSN = "postgres://jobforge:jobforge@localhost:5433/jobforge?sslmode=disable"

# clean：将下一行改为 20000 即为 dirty；两组分别运行五轮
$env:JOBFORGE_BENCH_GATEWAY_DIRTY_INFLIGHT = "0"
1..5 | ForEach-Object {
  go test -count=1 -run '^$' ./tests/integration
  go test ./benchmarks/micro -run '^$' `
    -bench '^BenchmarkGatewayPollClaim$' -benchmem -benchtime=10s -count=1
}
```

## 注意事项

1. 基准测试会向数据库写入大量数据，必须使用可重建的独立测试数据库
2. 端到端基准的 goroutine 稳态检查需要等待 60 秒
3. Docker 环境下的性能数据可能与原生环境有差异，记录环境规格用于对比

## Worker Runtime 容量复用基准

`BenchmarkWorkerRuntimeThroughput` 经过真实 Runtime、loopback gRPC Gateway 和 PostgreSQL，测量 1ms 短任务在 capacity=1/4 时的吞吐。它覆盖 Worker 满载后的补位等待；上面的存储层 E2E 基准不经过 Runtime。

```powershell
docker compose -f deploy/compose.yaml up -d postgres
$env:JOBFORGE_TEST_DSN = 'postgres://jobforge:jobforge@localhost:5433/jobforge?sslmode=disable'
go test ./tests/integration -run '^$' `
  -bench '^BenchmarkWorkerRuntimeThroughput$' -benchtime=32x -count=5 -benchmem
```

该命令由集成测试 TestMain 重建测试库 schema。fixture 创建不计时，注册及完整处理计时；每轮核对全部任务成功且 attempt 总数正确。五轮前后对照、分配开销和测试边界见 [Worker 容量释放唤醒报告](../docs/worker-capacity-performance.md)，[原始输出](results/worker-capacity-2026-09-12.txt) 已归档。
