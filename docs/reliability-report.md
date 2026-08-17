# JobForge 可靠性报告（Scale 套件与真实进程崩溃）

> 对应需求：PRD v0.4 FR-801～807、AT-02R/13R/14R；PRD v0.2 FR-601/602/603（AT-13/AT-14）；v0.1 NFR-001/NFR-002。
> 首次报告：2026-08-04（W11）；本次更新：2026-08-17（v0.4）
> 套件位置：`tests/scale/`（`//go:build scale` 隔离，FR-603：默认 CI 不执行）

## 结论

| 验收项 | 规模（字面值） | 结果 |
|---|---|---|
| AT-02R / NFR-401 | 效果提交后真实 Kill/Wait Worker A，Worker B 重领 | **PASS**：succeeded；token 1→2；持久效果 1 行；applied=1、deduplicated=1；连续 10 次与 race 均通过 |
| AT-13 / NFR-001 | 100 轮真实 Worker OS 进程 kill（每轮 10 任务） | **PASS**：每轮数据库屏障后 Kill + Wait；非终态任务零静默丢失 |
| AT-14 / NFR-002 | 10,000 个含重复投递的持久幂等任务 | **PASS**：效果表 10,000 行、dedup 10,000、重复业务效果 0 |
| NFR-204 / NFR-003 | 每轮恢复时间 ≤ lease_ttl + scan_interval + 2s | **PASS**：完整套件 max 22.9993ms ≪ 35s 上界 |
| NFR-202 | scale 套件 race 抽样 | **PASS**：AT-13 全量真实 kill + AT-14（1,000 任务）`-race` 无数据竞争（38.179s） |

## 环境

| 项 | 值 |
|---|---|
| 操作系统 | Windows（Docker Desktop / WSL2） |
| PostgreSQL | 16.14（`postgres:16-alpine`，localhost:5433） |
| Redis | 7.4.10，AOF `appendonly yes` / `appendfsync everysec`（完整 scale 的 NFR-302） |
| Go | 1.26.5 windows/amd64，`-tags scale` |
| 隔离 | build tag `scale`；默认 `go test ./...` 与 CI 均不执行本套件 |

运行前置：停止 api/scheduler/gateway/publisher/worker 等应用服务，保留测试 PostgreSQL；运行 NFR-302 时同时启动 durable-events Redis。v0.4 helper 直接重启当前 test binary，除数据库/Gateway 参数外不接受任务 payload 或任意代码；helper 只存在于 `_test.go`。

## AT-13：循环故障注入（`TestScaleAT13WorkerKillRounds`）

每轮流程：入队 10 个带 60s post-effect delay 的任务 → 启动真实 Worker A 子进程 → 等待 10 行持久效果且 jobs 均为 running/owner=A 的数据库屏障 → 调用 `Process.Kill` 并 `Wait` 回收进程 → 以 PostgreSQL 时钟强制 lease 过期 → Scheduler `RecoverExpiredLeases` 回收（记录耗时）→ Worker B 重领并全部 Complete → 断言全部 succeeded。测试不通过 shell 启动进程，也不给生产二进制添加 crash 开关。

结果（100 轮 × 10 任务）：

| 指标 | 值 |
|---|---|
| 静默丢失任务数 | **0** |
| 恢复耗时 min / p50 / p95 / max | 13.7569ms / 16.2563ms / 20.5892ms / 22.9993ms |
| NFR-003 上界（30s lease_ttl + 3s scan_interval + 2s） | 35s（全部满足） |
| AT-13 用例耗时 | 24.14s |

注：测试通过强制过期 + 直接触发回收测量恢复耗时，等价于 scan_interval 趋近 0 的最坏注入场景；生产语义上界仍为 lease_ttl + scan_interval + 2s（默认 33s）。

## AT-14：万级幂等（`TestScaleAT14IdempotentTenThousand`）

流程（每批 1,000，共 10 批，8 并发 delivery goroutine）：入队 → 通过生产 `IdempotentEffectHandler` 与 PostgreSQL EffectStore 首次执行 → 故意省略 Complete 形成可恢复重复投递（真实 OS kill 由 AT-02/13 单独证明）→ 强制过期并批量回收 → 重领重投，Handler 按 job ID 读取既有 `result_ref` → Complete。

结果（10,000 任务字面规模）：

| 指标 | 值 |
|---|---|
| 重复投递命中（dedupHits） | 10,000（每个任务均被重复投递一次） |
| `demo_idempotent_effects` 行数 | **10,000** |
| 重复业务副作用计数 | **0** |
| 未达 succeeded 的任务数 | 0 |
| 完整 scale 中 AT-14 用例耗时 | 106.92s |

## v0.4 完整回归与性能证据（2026-08-17）

完整 `go test -tags scale -count=1 -v -timeout 60m ./tests/scale/` **PASS（342.406s）**；唯一 skip 是直接执行套件时预期跳过的子进程 helper 入口。关键结果：

| 项 | 实际结果 | 门禁 |
|---|---|---|
| AT-13 | 100×10 真实进程 kill，0 丢失；recovery p95/max 20.5892/22.9993ms | ≤35s，PASS |
| AT-14 | 10,000 行效果、10,000 dedup、0 重复；全部 succeeded | 零重复，PASS |
| NFR-302 | 2,196 events/sec；publish lag p95 156.87ms | floor 1,800、lag≤2s，PASS |
| 20,000-job Claim | p50/p95 95.7573/115.1454ms | 相对 M4 88.716/102.177ms 为 +7.9%/+12.7%，均 <15%，PASS |
| NFR-306 | 10s/5s cadence 300/600 lease UPDATE，2.00× | 只作写放大证据，PASS |
| 多租户公平性 | B/C/D p95 23.486/22.8131/24.294ms，max≤87.4394ms | p95≤1s、max≤2s，PASS |

完整 `go test -race -count=1 -timeout 30m ./...` **PASS**，integration 包 130.891s，覆盖真实 PostgreSQL、AOF Redis、AT-02 race-instrumented helper 与既有 AT-01～24。Scale race 按 AT-13 100 轮、AT-14 1,000 job 抽样 **PASS（38.179s）**，无数据竞争。

机械门禁全部 PASS：`go build ./...`、`go vet ./...`、golangci-lint（0 issues）、Ruff check/format、mypy、SQLFluff 历史 baseline + 全 migrations、Buf lint 与 Buf breaking。0018 的独立 schema 测试完成 0017→0018→down→0018，并验证无 jobs 外键。

性能收官：Claim 五轮中位数从实现前 7.233324ms/op 降至 6.679887ms/op（改善 7.65%）；同参数 e2e 为 Submit 313.36 jobs/sec、Process 354.49 jobs/sec、p95 13.4332ms、goroutine 2→2，相对 M4 同机结果分别 -8.4%、-6.9%、+4.6%，均在 15% 当前回归门槛内。历史 W4 单操作 Claim 4.171091ms/op 的绝对门禁仍未通过，不以 v0.4 当前基线覆盖，详见 [benchmark.md](benchmark.md)。

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

SQLFluff 历史基线门禁追加验收（2026-08-11）同样保留 M3 当时的非 PASS 记录。冻结 allowlist 后，baseline validator **PASS**（有效条目严格为 0006 up/down、0013 up），全量 `sqlfluff lint migrations` **PASS**。负向探针 `0018_sqlfluff_negative_probe.up.sql` 以确定性 LT01 被 SQLFluff 拒绝；临时把该路径加入 ignore 后，baseline validator 亦按预期失败。探针与临时条目删除后再次全量 lint 通过，且 `git diff -- migrations` 为空。配套 Ruff check/format、mypy、go vet、golangci-lint（0 issues）、Buf lint 均通过；默认全仓 race 回归 **PASS**（130.4s，其中 `tests/integration` 122.800s）。

## M4 取消 SLO 与收官（2026-08-12）

M4 未新增 migration 或 Proto 字段；默认 heartbeat 由 10s 收敛为 5s，Lease TTL 保持 30s。Gateway 注册响应来自配置；Worker 未显式覆盖时采用建议值、显式本地值优先。Heartbeat 使用一个 PostgreSQL `clock_timestamp()` 样本完成续租、cancelling 检测和 elapsed 返回，lease/fencing、八状态状态机、错误码与 at-least-once 语义不变。

真实 PostgreSQL + HTTP Cancel + gRPC Gateway + Worker Runtime 的 `-race` 定向验收：

| 场景 | 实际结果 |
|---|---|
| AT-24，默认 5s、20 个确定性随机相位 | **PASS**：DB `cancel_requested_at`→Gateway signal p50/p95/max = 1.835/4.281/4.590s，p95≤6s |
| 分段报告（不套 signal 阈值） | Cancel API return→context cancelled p50/p95/max = 1.982/4.602/4.933s；context cancelled→Handler return p50/p95/max = 2.122/2.492/2.646ms |
| 指标与 Trace | 两个 histogram 各 20 样本；标签严格为 `path=heartbeat` / 预注册 `type`（另有 exporter scope 标签），无 job/worker/trace ID 或 payload；20 个 `gateway.cancel_signal` span 全部续接原 traceparent且只含 path/elapsed |
| 默认/非默认参数 | config 30s/5s 默认与 17s/2s 显式组合通过；Gateway 5s/2s/fallback 注册建议通过；7s TTL 的 DB lease 与约 2s DB-clock cancel elapsed 通过 |
| worker liveness | 默认 30s lease 保持 10s（TTL/3）条件刷新；非默认 6s lease + 500ms job heartbeat 下，job lease 每次续租但 liveness 只在 2s 后刷新 |
| Heartbeat 瞬时故障 | `TestFaultGatewayBlipWithinTTLNoRedelivery` **PASS（6.22s）**：TTL 内恢复后继续 heartbeat，无重投、无丢取消语义 |
| 取消竞争 | `TestCompleteCancelRace` **PASS（0.30s）**；AT-05/06 的先提交者生效、`CANCEL_REQUESTED`/`ALREADY_TERMINAL` 语义不变 |

NFR-306 完整 scale 套件 **PASS（240.528s）**。100 个 active jobs 的逻辑 30s 窗口中，10s cadence 真实 lease UPDATE=300，5s=600（**2.00×**）；p50/p95 分别为 1.4410/2.4402ms 与 1.4558/2.4283ms，只作容量观测。AT-14 10,000 job 零重复副作用（48.02s）；AT-13 100轮×10 job 零静默丢失、恢复 max 22.7972ms；NFR-302 2,316 events/sec、publish lag p95 129.464ms；20,000-job Claim p50/p95 88.716/102.177ms；多租户 B/C/D ready→claim p95 均≤23.164ms、max≤30.169ms。

NFR-306 定向 scale race 亦 **PASS（package 5.709s）**：300/600 次真实更新不变，10s p50/p95=1.5089/2.1913ms，5s p50/p95=1.5036/2.2572ms。最终代码树的完整 `go test -race -count=1 ./...` **PASS**，其中真实 PostgreSQL/Redis 集成包 128.671s；未发现数据竞争或 goroutine 生命周期回退。

W4/W11 同参数端到端复测 **PASS**：100 jobs/4 workers，Submit 342.21 jobs/sec、Process 380.95 jobs/sec、p95 12.8372ms、goroutine 2→2；均满足相对 W4 的 -15%/+20%/±5 门槛。历史单操作 Claim 微基准绝对值在重启并重置测试库后仍为 5.900438ms/op（W4 为 4.171091ms/op），因此该单项明确记为 **非 PASS**；M4 未修改 Claim 路径，且当前基线的 20,000-job scale 仅 +6.0%，但在冻结环境复测/调查前不宣称全部历史微基准已收官。详见 [benchmark.md](benchmark.md) 的 M4 节。

AT-25 仍为可裁剪 P1/M5 skeleton，未在 M4 中实现，也不计作 M4 skip。M4 不宣称 ControlStream 的 ≤1s SLO；Heartbeat 永久保留为取消兜底。

## Race 抽样（NFR-202）

v0.4 执行 `go test -tags scale -race`：AT-13 以 100 轮真实 Worker 进程 kill 字面规模运行，AT-14 以 `JOBFORGE_SCALE_IDEMPOTENT_JOBS=1000` 抽样运行。结果：`ok`，无数据竞争（38.179s）；AT-13 recovery p95/max 22.3441/24.7763ms，AT-14 1,000/1,000 dedup、零重复效果。

## 复现命令

```powershell
# 1. 启动 PostgreSQL；完整 scale 的 NFR-302 同时需要 AOF Redis
docker compose -f deploy/compose.yaml --profile durable-events up -d postgres redis

# 2. 字面规模运行（默认 100 轮 / 10,000 任务）
$env:JOBFORGE_TEST_DSN = "postgres://jobforge:jobforge@localhost:5433/jobforge?sslmode=disable"
$env:JOBFORGE_TEST_REDIS_URL = "redis://localhost:6379/0"
go test -tags scale -count=1 -v -timeout 60m ./tests/scale/

# 3. race 抽样（AT-14 降采样到 1,000 任务）
$env:JOBFORGE_SCALE_IDEMPOTENT_JOBS = "1000"
go test -tags scale -race -count=1 -timeout 60m `
  -run '^(TestScaleAT13WorkerKillRounds|TestScaleAT14IdempotentTenThousand)$' `
  ./tests/scale/
```

规模参数（均可通过环境变量降采样，见 `docs/development.md`）：

| 环境变量 | 默认值 | 说明 |
|---|---|---|
| `JOBFORGE_SCALE_KILL_ROUNDS` | 100 | AT-13 轮数 |
| `JOBFORGE_SCALE_KILL_JOBS_PER_ROUND` | 10 | AT-13 每轮任务数 |
| `JOBFORGE_SCALE_IDEMPOTENT_JOBS` | 10000 | AT-14 任务总数 |
| `JOBFORGE_SCALE_WORKERS` | 8 | AT-14 并发 Worker 数 |
