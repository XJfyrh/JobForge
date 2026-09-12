# Worker 容量释放唤醒：性能迭代报告

日期：2026-09-12。分支：`XJfyrh/perf-worker-capacity-wakeup`。

## 选择依据

阅读 PRD v0.1/v0.5、ADR 索引及 ADR-0001/0002/0008/0010、编码规范、贡献指南和开发指南，并检查 Worker Runtime、Gateway Poll、现有 Worker 测试与 benchmark 后，本轮选择消除 **Worker 满载后的固定 500ms 等待**。

旧 Runtime 在 `inflight == capacity` 时执行 `time.After(500ms)`。任务及结果上报提前结束，虽然已经释放了本地并发槽位，领取循环仍要等定时器触发。短任务连续积压时，这段等待限制了吞吐。现有 `benchmarks/e2e` 直接调用存储层 Claim/Complete，不经过 Runtime，因此不能观察这个问题。

该方向可以在仓库内闭环：只修改本地执行槽位的等待方式，保持 unary Poll、服务端容量校验以及既有任务事务契约。它不涉及新的状态、错误码、公开配置、Proto、数据库迁移或关键依赖，不需要新增架构决策。

## 实现与并发边界

生产改动集中在 [runtime.go](../internal/worker/runtime.go)：

1. 每个 Runtime 增加一个容量为 1 的通知 channel，合并多个任务同时结束的唤醒。
2. 每个任务在执行及结果上报结束后，先在锁内递减 `inflight`，再非阻塞地通知领取循环，最后调用 `WaitGroup.Done()`。上报期间继续占用槽位。
3. 满载时等待通知或 context 取消；每次醒来重新读取实际可用容量。通知只是提示，不代表额外许可，陈旧通知不能导致超量领取。
4. 通知在检查容量与开始等待之间到达时，缓冲区保留提示，防止丢失唤醒；多个完成合并时，重新读取计数保留全部可用槽位。
5. channel 不关闭：即使执行晚于退出宽限期结束，也不会向已关闭 channel 发送。没有新增后台 goroutine。

数据库仍决定 Claim、lease、fencing、attempt 和最终状态；交付保证继续是 at-least-once，外部副作用仍需业务幂等。

## 测量方法

新增 [BenchmarkWorkerRuntimeThroughput](../tests/integration/worker_capacity_test.go)，运行真实 `Runtime → loopback gRPC Gateway → PostgreSQL` 链路。固定 32 个任务、一个 Worker，分别设置 capacity=1 和 4；每个 Handler 进行 1ms 可取消等待，然后报告成功。

- fixture 创建和启动 Gateway 不计时；从启动 Runtime（包含注册及首次领取）计时，到全部任务完成数据库事务时停止。
- 每轮验证 32 个任务均为 succeeded，attempt 总数为 32。没有通过少处理任务提高数字。
- before/after 各五轮，下面使用中位数。`ms/job` 是批次总耗时除以任务数，**不是单任务 p50/p95 或排队时延**。
- 优化前先编译并保存独立测试程序，优化后使用相同 benchmark。两次程序启动均由测试 TestMain 重建 schema；单次五轮在同一 schema 内使用独立随机 queue/worker ID。
- 代码基线为 `172e863` 加任务开始时已有的未提交改动，包含 migration 0019 与 Gateway owner inflight 索引。两侧均包含这些已有改动，它们不计为本轮成果。
- 本测试不是 W4 存储层冻结基准，不能用这些数字覆盖既有 W4 门禁结论。

环境：Windows 11 `10.0.26200`；AMD Ryzen 7 7840HS（16 逻辑 CPU，GOMAXPROCS=16）；物理内存约 31.28 GiB；Go `1.26.5 windows/amd64`；Docker PostgreSQL `16.14`（Alpine），宿主端口 5433；`shared_buffers=128MB`、`synchronous_commit=on`、`fsync=on`、`max_connections=100`。测量时仅运行测试 PostgreSQL，没有 Compose 应用服务。

| Worker capacity | 优化前 jobs/s | 优化后 jobs/s | 吞吐倍率 | 优化前 ms/job | 优化后 ms/job |
|---|---:|---:|---:|---:|---:|
| 1 | 2.034 | 86.22 | 42.4× | 491.605 | 11.599 |
| 4 | 8.899 | 222.8 | 25.0× | 112.377 | 4.488 |

原始五轮输出见 [worker-capacity-2026-09-12.txt](../benchmarks/results/worker-capacity-2026-09-12.txt)。这是同机短任务、持续积压、32 个任务一轮的受控结果；没有测量长任务、跨主机网络、大 payload 或生产容量上限，不外推为所有负载都有相同提升。

### 分配开销

| Worker capacity | 优化前 B/op | 优化后 B/op | 优化前 allocs/op | 优化后 allocs/op |
|---|---:|---:|---:|---:|
| 1 | 51,186 | 53,125 | 686 | 683 |
| 4 | 44,474 | 50,750 | 460 | 578 |

capacity=4 的每任务分配次数中位数增加约 25.7%，分配字节数增加约 14.1%。从实现推断，即时补位更容易产生较小的 Poll/Claim 批次；本轮没有单独统计 RPC 次数来量化这个原因。该变化用更多即时处理换取更高槽位利用率，不构成内存用量下降或数据库负载下降的承诺。

## 验证证据

- [runtime_capacity_test.go](../internal/worker/runtime_capacity_test.go)：满载无 RPC、完成后无额外定时等待、陈旧提示、等待前多个并发完成、取消与完成同时就绪、deadline、停止领取、Poll 失败后保留容量。使用 Go 标准库 [testing/synctest](https://pkg.go.dev/testing/synctest) 的虚拟时钟和阻塞同步，避免依赖机器快慢的墙钟阈值。
- `go test -race -count=20 -run '^TestPoll' ./internal/worker`：通过。
- 通过临时 Go overlay 仅还原旧 Poll 实现，`TestPollWakesOnCapacityRelease` 的两个子场景均按预期失败：`capacity release did not wake poll`。工作区保留修复版本。
- [worker_capacity_test.go](../tests/integration/worker_capacity_test.go)：capacity=1/4 各处理 24 个任务，验证 16 succeeded、8 dead、24 条已结束 attempt，峰值 Handler 并发等于配置容量；另验证满载取消后 Runtime 退出，未领取任务仍可由其他 Worker 领取。
- `go test -count=1 -run '^TestWorkerRuntime' -v ./tests/integration`：通过。
- `go build ./...`、`go vet ./...`、golangci-lint：通过，lint 为 0 issues。
- Ruff check/format、mypy、SQLFluff 历史基线与全量 migration lint、buf lint：通过。
- 全仓 `go test -race -count=1 -json ./...`：通过，11 个测试包、179 个顶层测试通过（含子测试共 309 项），0 失败；真实 PostgreSQL + Redis 集成包耗时 152.304s，新增 Runtime 测试也在该 race 运行内通过。两个跳过项为尚未实现的 P1 ControlStream AT-25，以及只能由父测试作为子进程启动的 Worker helper 入口；不把跳过算作通过。

本轮没有修改 Proto，因此 breaking 检查不适用；没有修改迁移或 Claim/Heartbeat 算法，因此未另跑 `-tags scale` 字面规模套件，也未宣称本轮重新认证 W4 或 scale 门禁。

## 复现

基准会重建测试库 schema，只针对可重建的开发测试数据库运行：

```powershell
docker compose -f deploy/compose.yaml up -d postgres
$env:JOBFORGE_TEST_DSN = 'postgres://jobforge:jobforge@localhost:5433/jobforge?sslmode=disable'
go test ./tests/integration -run '^$' `
  -bench '^BenchmarkWorkerRuntimeThroughput$' -benchtime=32x -count=5 -benchmem
```

完整默认门禁：

```powershell
docker compose -f deploy/compose.yaml --profile durable-events up -d postgres redis
$env:JOBFORGE_TEST_DSN = 'postgres://jobforge:jobforge@localhost:5433/jobforge?sslmode=disable'
$env:JOBFORGE_TEST_REDIS_URL = 'redis://localhost:6379/0'
$env:JOBFORGE_TEST_REDIS_CONTAINER = 'deploy-redis-1'
go test -race -count=1 ./...
```

Worker 容量唤醒的代码、测试和报告作为一个独立提交交付；Gateway owner inflight 索引优化及其文档由另一个提交维护。`benchmarks/README.md` 在 Worker 提交中只追加本报告与新基准的入口。
