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
