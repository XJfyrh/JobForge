# JobForge 可靠性报告（Scale 套件）

> 对应需求：PRD v0.2 FR-601/602/603（AT-13/AT-14），闭环 v0.1 NFR-001/NFR-002 字面规模证据缺口（GAP-1）。
> 报告日期：2026-08-04（W11）
> 套件位置：`tests/scale/`（`//go:build scale` 隔离，FR-603：默认 CI 不执行）

## 结论

| 验收项 | 规模（字面值） | 结果 |
|---|---|---|
| AT-13 / NFR-001 | 100 轮 Worker kill（每轮 10 任务） | **PASS**：非终态任务零静默丢失（totalLost = 0） |
| AT-14 / NFR-002 | 10,000 个含重复投递的幂等任务 | **PASS**：重复业务副作用计数 = 0 |
| NFR-204 / NFR-003 | 每轮恢复时间 ≤ lease_ttl + scan_interval + 2s | **PASS**：最大恢复耗时 29.72ms ≪ 35s 上界 |
| NFR-202 | scale 套件 race 抽样 | **PASS**：AT-13 全量 + AT-14（1,000 任务）`-race` 无数据竞争 |

## 环境

| 项 | 值 |
|---|---|
| 操作系统 | Windows（Docker Desktop / WSL2） |
| PostgreSQL | 16（`docker compose -f deploy/compose.yaml up -d postgres`，localhost:5433） |
| Go | go.mod 指定版本，`-tags scale` |
| 隔离 | build tag `scale`；默认 `go test ./...` 与 CI 均不执行本套件 |

运行前置：仅保留 postgres 服务（停止 api/scheduler/gateway/publisher/worker 等应用服务，避免运行中实例与测试争用数据与 advisory lock）。

## AT-13：循环故障注入（`TestScaleAT13WorkerKillRounds`）

每轮流程：入队 10 任务 → Worker A 全部领取后模拟 kill（不发 Complete）→ 以 PostgreSQL 时钟强制 lease 过期（规避 WSL2 时钟漂移）→ Scheduler `RecoverExpiredLeases` 回收（记录耗时）→ Worker B 重领并全部 Complete → 断言全部 succeeded。

结果（100 轮 × 10 任务）：

| 指标 | 值 |
|---|---|
| 静默丢失任务数 | **0** |
| 恢复耗时 min / p50 / p95 / max | 17.29ms / 19.62ms / 22.72ms / 29.72ms |
| NFR-003 上界（30s lease_ttl + 3s scan_interval + 2s） | 35s（全部满足） |
| 套件耗时 | 11.63s |

注：测试通过强制过期 + 直接触发回收测量恢复耗时，等价于 scan_interval 趋近 0 的最坏注入场景；生产语义上界仍为 lease_ttl + scan_interval + 2s（默认 33s）。

## AT-14：万级幂等（`TestScaleAT14IdempotentTenThousand`）

流程（每批 1,000，共 10 批，8 并发 Worker）：入队 → 并发领取并执行 `demo.idempotent_effect`（副作用首次触发）→ 全部 Worker 模拟 ACK 前崩溃 → 强制过期并批量回收 → 重领重投（Handler 按 job_id 去重，返回 `deduplicated`）→ Complete。

结果（10,000 任务字面规模）：

| 指标 | 值 |
|---|---|
| 重复投递命中（dedupHits） | 10,000（每个任务均被重复投递一次） |
| 重复业务副作用计数 | **0** |
| 未达 succeeded 的任务数 | 0 |
| 套件耗时 | 45.71s |

## 耐久事件恢复（PRD v0.3 M2，ADR-0006）

Redis Streams transport 的故障路径以真实 Redis（AOF `appendonly yes`/`appendfsync everysec` + 命名 volume）验证，数据详见 [benchmark.md](benchmark.md) “M2 耐久事件”章节：

| 场景 | 结果 |
|---|---|
| AT-17：Redis stop/start（保留 volume）重启恢复 | 停机前 Stream 记录保留；停机期 outbox 未发布行保留；恢复后全部补入、积压归零 |
| AT-18：XADD 成功后标记前崩溃 | 同 event_id 多 entry 被允许；outbox 最终标记成功；任务状态不变 |
| AT-20：双 consumer group | 每组完整消费，组内实例分摊，无跨组游标干扰 |
| NFR-303：Redis 暂停 60s 恢复 | 停机期任务状态事务不被 publisher 阻塞；恢复后积压归零、零静默丢失 |
| NFR-302 smoke | 约 7,640 events/sec（floor 1,800）；健康态 publish lag p95 146.66ms ≤ 2s |

notify 默认下 AT-15/16 不回退，启动日志/指标明确 `durable=false`。

## 事务性事件消费（PRD v0.3 M3，2026-08-11）

在 Windows + PostgreSQL 16 + Redis 7 AOF Compose 环境中，以 `-race -count=1` 独立运行 M3 消费闭环及 AT-20 回归：

| 场景 | 实际结果 |
|---|---|
| AT-19：同 event_id 两个 entry；Consumer A commit 后、ACK 前退出；Consumer B XAUTOCLAIM | **PASS（0.97s）**：两源 entry 最终 ACK、PEL=0、inbox=1、无 event_id 唯一约束的 demo effects=1 |
| 指标与 traceparent | **PASS**：redelivery/duplicate 两个 Counter 均增加；`event.consume`/`event.process` 延续 envelope trace_id，span 无完整 payload |
| pending 游标继续扫描 | **PASS（0.19s）**：25 条 PEL 前缀保持 fresh、尾部变 stale；`XAUTOCLAIM` 空页返回的非零 cursor 被保留，尾部事件最终恢复 |
| inbox group binding 与元数据完整性 | **PASS（0.04s）**：同组多实例可复用；不同 group 共享 schema、相同 event_id 元数据不一致均 fail closed，且不 ACK、不 poison |
| Redis read、数据库处理与 ACK 瞬时故障 | **PASS（0.67s）**：三类故障依次注入并恢复；经 PEL/XAUTOCLAIM 收敛至 PEL=0、inbox=1、effect=1、poison=0 |
| deleted pending payload | **PASS（0.03s）**：pending entry 被 `XDEL` 后，Consumer 返回 fatal integrity error，`pending_payload_deleted` 增加且不执行业务效果 |
| poison 与前进性 | **PASS（3.20s）**：unsupported schema 第 5 次永久投递进入 poison（生产默认上限=5），原 entry ACK；后续合法事件完成；poison 无 payload |
| 取消与生命周期 | **PASS（0.07s）**：运行中 Handler 已写 effect 后收到取消，inbox/effect 一起回滚，entry 保持 pending；Close/Wait 与单元 race 通过 |
| migration 0017：0016 基线前滚与 down | **PASS（0.37s）**：独立临时 PostgreSQL 数据库登记 0001～0016，通过正式 Migrator 前滚至 0017；随后按 effect → inbox → binding 顺序 down 并销毁临时库 |
| AT-20 晚建 group + 双 group/双实例 | **PASS（0.22s）**：事件先发布、group 后从 0-0 创建；每组 40/40，组内各实例 20/20 |
| Compose `--scale consumer=2` | **PASS**：两个副本持续运行；Redis 注册名为 `9ace7e34d049-1`/`43b16dda1807-1`，loopback 随机端口为 `127.0.0.1:17706`/`:17707`，无名称或端口冲突 |

复现命令：

```powershell
$env:JOBFORGE_TEST_DSN = "postgres://jobforge:jobforge@localhost:5433/jobforge?sslmode=disable"
$env:JOBFORGE_TEST_REDIS_URL = "redis://localhost:6379/0"
go test -race -count=1 -v ./internal/eventconsumer ./tests/integration `
  -run 'TestConsumer|TestDurableAT19TransactionalConsumer|TestEventConsumerPendingCursorSkipsFreshPrefix|TestConsumerInboxBindingAndDuplicateMetadata|TestEventConsumerTransientFailuresRecoverWithoutPoison|TestEventConsumerDeletedPendingFailsClosed|TestEventConsumerPoisonIsolationAndForwardProgress|TestMigration0017FromClean0016|TestEventConsumerCancellationRollsBackInFlight|TestDurableAT20GroupIsolation'
```

这些结果证明的是 at-least-once + PostgreSQL 同事务 inbox 幂等，不是跨系统 exactly-once。poison 测试与运行配置均使用默认的 5 次投递上限。

同一环境随后执行完整 `go test -race -count=1 ./...`：**PASS**，其中 `tests/integration` 用时 124.212s，覆盖 Redis stop/start、60s broker pause、AT-19/20 与 AT-21～23；无数据竞争。

本次新增的 0017 up/down 文件单独执行 SQLFluff：**PASS**。全 `migrations/` 目录执行结果仍为非 PASS，仅报告已应用且未在本增量改写的 0006（indent/long line）和 0013（spacing/select layout/alias）既有格式债务；未将该工具缺口误报为通过。

M3 后 scale 回归（2026-08-11）实际结果：AT-13 100 轮 × 10 任务零静默丢失（12.98s，恢复 max 26.235ms）；AT-14 10,000 任务、10,000 次重复命中、零重复副作用（46.89s）；NFR-302 10,000 事件 2,239 events/sec、publish lag p95 164.867ms；多租户公平性 15s 套件通过。完整 scale 命令还运行了 `TestScalePerfPromoteClaimLatency`，该用例因 PostgreSQL 在空 inflight 集合上选择同样覆盖谓词的 `idx_jobs_lease_expiry`、而测试硬要求 `idx_jobs_tenant_inflight` 而失败；这是执行计划断言的既有可移植性问题，M3 相关路径与上述独立 scale 回归均通过，不能把完整 scale 命令记为 PASS。

Quality Gate Debt Cleanup 后的追加验收（2026-08-11）保留上述历史失败、不回写原结论。索引门禁已拆为 catalog 结构契约与自然执行计划性能性质：连续五轮定向测试全部通过（340.142s），每轮均在事务内删除 `idx_jobs_tenant_inflight` 并确定得到 missing-index 错误，回滚后结构检查恢复；`-race` 定向测试通过（60.826s），无数据竞争。完整 scale 套件随后 **PASS**（245.283s，无非预期 skip）：AT-14 10,000 任务零重复副作用（45.67s）；AT-13 100 轮零静默丢失、恢复 max 23.1424ms（12.48s）；NFR-302 为 2,275 events/sec、publish lag p95 120.931ms（4.64s）；索引性能测试与多租户公平性测试均通过。默认全仓 `go test -race -count=1 ./...` 亦 **PASS**（131.5s，其中 `tests/integration` 123.266s）。

## Race 抽样（NFR-202）

`go test -tags scale -race`：AT-13 以 100 轮字面规模运行，AT-14 以 `JOBFORGE_SCALE_IDEMPOTENT_JOBS=1000` 抽样运行。结果：`ok`，无数据竞争（19.5s）。

## 复现命令

```sh
# 1. 仅启动 PostgreSQL（Windows；Linux CI 由 testcontainers 自动提供）
docker compose -f deploy/compose.yaml up -d postgres

# 2. 字面规模运行（默认 100 轮 / 10,000 任务）
set JOBFORGE_TEST_DSN=postgres://jobforge:jobforge@localhost:5433/jobforge?sslmode=disable
go test -tags scale -count=1 -v -timeout 60m ./tests/scale/

# 3. race 抽样（AT-14 降采样到 1,000 任务）
set JOBFORGE_SCALE_IDEMPOTENT_JOBS=1000
go test -tags scale -race -count=1 -timeout 60m ./tests/scale/
```

规模参数（均可通过环境变量降采样，见 `docs/development.md`）：

| 环境变量 | 默认值 | 说明 |
|---|---|---|
| `JOBFORGE_SCALE_KILL_ROUNDS` | 100 | AT-13 轮数 |
| `JOBFORGE_SCALE_KILL_JOBS_PER_ROUND` | 10 | AT-13 每轮任务数 |
| `JOBFORGE_SCALE_IDEMPOTENT_JOBS` | 10000 | AT-14 任务总数 |
| `JOBFORGE_SCALE_WORKERS` | 8 | AT-14 并发 Worker 数 |
