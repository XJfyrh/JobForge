# Agent v3 S1-B：Run Claim 与调用账本定向基线

日期：2026-09-16。对应 [PRD v0.9 B-10](../product/JobForge_PRD_v0.9.md) 与 [ADR-0017](../adr/0017-run-admission-and-call-ledger.md)。

本次完成真实 PostgreSQL 上的新 Run 查询计划与定向测量。**这是新路径自身的 candidate 基线，不是性能门禁通过，也不是相对旧 Run 的提升结论。此前没有同契约的 Run 基线；旧 W4 性能门禁失败、AT-25 跳过、真实模型与生产留存未验收继续保留。**

## 1. 实现与测量边界

测试为 [TestRunHotPathPlansAndBaseline](../../tests/integration/run_performance_test.go)，默认 16 个 Run，`JOBFORGE_RUN_PERF_RUNS` 可设为 16～256。默认测试实际执行计划与事实断言，不以环境变量缺失为由跳过；本次 candidate 使用 128。

所有 Run 经生产 `Service.Submit` 接纳，两个并发 Worker 经生产 Register/Claim、BeginTool、ReserveCall、ObserveCall、CommitStep、GetCheckpoint，最后提交 `no_action`，关闭 attempt、释放容量。没有直接 SQL 插入 Run、步骤、调用或账本。Capture、工具结果与模型输出都是明确标注的确定性 fixture；没有实际业务 HTTP、模型 HTTP 或收费调用。因此本报告测量控制库路径，不测跨库快照 HTTP、网络/模型延迟、供应商计费、真实业务质量或受监管执行器生命周期。

128 个 Run 对应 128 次成功 Claim、768 次持久步骤、896 个物理调用记录、384 个逻辑工具记录；数据库核对 128 个 `succeeded/no_action`、全部容量槽 `used` 之和为零，并断言无重复 Claim ID。每个 Run 的 7 个调用包括读取订单、物流、政策检索的四个子请求和 chat；只对 **chat Reserve** 与 **带 usage 的 chat Observe** 单独记录延迟。其余真实账本事务仍包含在整体工作阶段耗时中。

Submit 串行播种；执行阶段有 2 个 goroutine Worker、2 个租户、1 个 profile，worker/tenant/profile 容量均为 2。每轮 Claim 前经生产 HeartbeatSession 保持空闲会话存活。延迟取客户端调用开始到返回的单调时钟时长，包含数据库网络往返及事务提交；分位采用 nearest rank，包括首次调用，不裁掉慢样本。空 Claim 单独报告。CommitStep 分位混合各步骤与终态提交；GetCheckpoint 不包含终态后的无效授权查询。

EXPLAIN 使用 Go AST 从生产源文件读取实际 SQL 表达式，包括 Claim 的完整 `runColumns`、容量预筛、`FOR UPDATE SKIP LOCKED`，不会维护另一份缩减 SQL。对数据执行 ANALYZE 后运行 `EXPLAIN (ANALYZE, BUFFERS)`；锁查询在短事务内执行后 rollback。保留 `enable_seqscan=on`、`plan_cache_mode=auto`，不固定索引名称断言或禁用顺序扫描。计划是单次带参数自然计划诊断，不代表所有 pgx 预备语句在稳态都会得到相同计划。

除全部 ready 的初始分布外，测量结束后额外通过生产 Submit 创建 1 个只用于计划的 ready Run，与已完成历史一起检查稀疏 ready 分布。此 Run 不计入吞吐或 latency 样本；计划阶段总数为 129。全部数据位于测试创建并在结束后删除的独立数据库。

## 2. 固定环境

| 项目 | 实测值 |
|---|---|
| 主机 | Windows 11 家庭版中文版，10.0.26200 / build 26200，amd64 |
| CPU / 内存 | AMD Ryzen 7 7840HS，8 核 / 16 逻辑处理器，约 31.28 GiB 可见内存 |
| Go | go1.26.5 windows/amd64；GOMAXPROCS=16；pgx pool max=16 |
| PostgreSQL | 16.14，x86_64-pc-linux-musl，Alpine gcc 15.2.0，64-bit |
| PostgreSQL 容器 | `postgres:16-alpine`，本地 image ID `sha256:57c72fd2a128e416c7fcc499958864df5301e940bca0a56f58fddf30ffc07777` |
| Docker | Server 29.6.2，Linux x86_64，16 CPU，16,385,245,184 bytes 内存 |
| 连接 | Windows 主机到本地 Compose PostgreSQL，loopback:5433；独立测试数据库 |
| 优化器 | enable_seqscan=on，plan_cache_mode=auto；未更改 planner 设置 |
| 代码 | `XJfyrh/s1-run-ledger` 工作树；基于 `696404e8976c41692d9568e07bed7a92d0572799`，S1-B 实现尚未提交 |
| 测试源码 SHA-256 | `7cf875c7fc7949943abeb2631d7b42d11f1a64d76338320925b3c3fe1bd6533a` |

运行时串行占用 integration DSN；未同时运行其他 integration suite、模型请求或容器构建。为减少定时扫描干扰，暂时停止 `compose.agent.yaml` 的 `control` 服务，完成后已恢复；保留 PostgreSQL 及其他业务容器。此环境仍是共享开发主机，不是隔离性能实验室。未采集 CPU/heap profile、磁盘或长期稳态压力，不能推导这些指标已达标。

## 3. 实际结果

最终源码的 16 Run race 测试 **PASS**，测试体 4.87s、包耗时 6.718s；112 调用、48 工具、96 步骤，无跳过。128 Run 非 race candidate **PASS**，测试体 28.33s、包耗时 29.107s；Submit 播种 2.7926564s，执行阶段 24.7725351s，即 5.167 Run/s。测试 PASS 仅指事实一致性和计划成功执行；代码没有延迟/吞吐阈值。

最终 candidate 客户端延迟如下，单位均为 ms：

| 路径 | 样本数 | p50 | p95 | p99 | max |
|---|---:|---:|---:|---:|---:|
| Submit（串行，fixture capture） | 128 | 21.0342 | 29.1253 | 39.1076 | 42.7885 |
| Claim（成功领取） | 128 | 24.7971 | 37.4088 | 43.2218 | 61.1175 |
| Claim（空结果，单列） | 2 | 1.3168 | 1.7126 | 1.7126 | 1.7126 |
| CommitStep（含终态提交） | 768 | 13.3421 | 18.2274 | 24.2701 | 37.3005 |
| GetCheckpoint | 640 | 7.7651 | 10.0867 | 11.6828 | 24.1466 |
| ReserveCall（chat） | 128 | 12.8215 | 16.2426 | 20.8826 | 22.5988 |
| ObserveCall（chat + 合法 usage） | 128 | 10.6720 | 14.4857 | 16.3253 | 20.4939 |

同环境此前一轮 128 Run（尚未添加测量结束后的稀疏 ready 计划）也 PASS：执行阶段 23.6232842s、5.418 Run/s，Claim p50/p95/p99 为 24.2259/29.9245/41.0914ms。两轮测量部分相同；新增稀疏计划位于测量之后。本报告采用最终源码的后一轮作为完整归档，不取更快一轮覆盖它，也不把两轮的小幅差异解释为回归或改进。

## 4. 自然查询计划

以下为最终 128 Run candidate 的观察；执行时间是 PostgreSQL EXPLAIN 的单次时间，不能与上表包含整个事务的客户端延迟等同。buffer hit 是对应计划顶层执行节点的 shared hit，不含单列的 Planning buffers。

| 查询 / 生产来源 | 主要访问方式 | Execution Time | shared hit |
|---|---|---:|---:|
| Claim，128/128 ready；`lifecycle.go:Claim` | runs Seq Scan + Sort + LockRows；容量表为空 | 1.095ms | 33 |
| Claim，1/129 ready；`lifecycle.go:Claim` | `runs_ready_claim_idx` Index Scan + Sort + LockRows；容量表 5 行 | 0.271ms | 20 |
| 调用身份锁；`ledger.go:readCall` | `physical_calls_tenant_id_run_id_physical_call_id_key` Index Scan | 0.085ms | 4 |
| 物理调用 ordinal；`ledger.go:ReserveCall` | `physical_calls_run_idx` Bitmap Index/Heap Scan，读取目标 Run 的 7 行 | 0.076ms | 4 |
| 工具子请求顺序；`ledger.go:checkSubcall` | `physical_calls_run_idx` Bitmap Index/Heap Scan，再按工具过滤 | 0.112ms | 4 |
| Run usage 聚合；`query.go:fillBudget` | `physical_calls_run_idx` Bitmap Index/Heap Scan | 0.087ms | 4 |
| Run 工具次数；`query.go:fillBudget` | `tool_invocations_tenant_id_run_id_invocation_id_key` Bitmap Index/Heap Scan | 0.056ms | 3 |
| 工具身份锁；`ledger.go:readTool` | `tool_invocations_pkey` Index Scan | 0.052ms | 4 |

小表且全部 ready 时 planner 自然选择顺序扫描；这不是被测试隐藏的失败。完成历史占多数时同一原始 Claim SQL 使用 ready 部分索引。896 行账本上的目标 Run 查询自然使用索引；默认 16 Run、112 行账本上部分查询选择顺序扫描，日志保留该选择。这个规模只建立有界定向证据，不验证万级 backlog、多 profile、大租户基数、故障期间锁竞争或长期膨胀的性能，也不替代这些未来测量。

## 5. 复现与验证状态

Windows 启动真实 PostgreSQL，然后串行运行；`tests/integration` 的 TestMain 会清理 DSN 数据库，不能与其他测试套件同时使用同一 DSN。

```powershell
docker compose -f deploy/compose.yaml up -d postgres
$env:JOBFORGE_TEST_DSN = 'postgres://jobforge:jobforge@localhost:5433/jobforge?sslmode=disable'
Remove-Item Env:JOBFORGE_RUN_PERF_RUNS -ErrorAction SilentlyContinue
go test -race -count=1 -v -run '^TestRunHotPathPlansAndBaseline$' ./tests/integration/
$env:JOBFORGE_RUN_PERF_RUNS = '128'
go test -count=1 -v -run '^TestRunHotPathPlansAndBaseline$' ./tests/integration/
Remove-Item Env:JOBFORGE_RUN_PERF_RUNS
.tools/bin/golangci-lint.exe run ./tests/integration/...
```

- 已执行：gofmt、`go test -c` 编译、最终 16 Run 定向 race、最终 128 Run 定向基线、integration golangci-lint（0 issues），均通过。
- 本子任务没有修改生产 SQL、migration、SDK、模型配置或预算价格，不涉及 SQLFluff/Buf 生成变更。
- 本报告只覆盖 B-10 中的查询计划和定向基线；全仓 race、跨语言、协议、其他 B-01～09 证据由整体实施记录分别报告。
- 测试结束删除所属独立数据库，恢复之前停止的 control 服务。无模型或收费调用。

## 6. 最终 candidate 原始输出

以下包含所有测量值与完整 EXPLAIN，UUID 均为本次独立测试随机 fixture 身份。

```text
=== RUN   TestRunHotPathPlansAndBaseline
    run_performance_test.go:41: RUN-PERF environment: os=windows arch=amd64 go=go1.26.5 cpus=16 gomaxprocs=16 postgres=PostgreSQL 16.14 on x86_64-pc-linux-musl, compiled by gcc (Alpine 15.2.0) 15.2.0, 64-bit pool_max=16 enable_seqscan=on plan_cache_mode=auto
    run_performance_test.go:43: RUN-PERF scope: runs=128 tenants=2 workers=2 worker_capacity=2 tenant_capacity=2 profile_capacity=2; synthetic capture/tool/model results, no external HTTP; no direct SQL inserts
    run_performance_test.go:51: RUN-PERF seed: runs=128 elapsed=2.7926564s
    run_performance_test.go:53: RUN-PLAN claim-ready (lifecycle.go:Claim):
        Limit  (cost=38.53..38.54 rows=1 width=1394) (actual time=0.851..0.853 rows=1 loops=1)
          Buffers: shared hit=33
          InitPlan 1 (returns $0)
            ->  Seq Scan on execution_slots execution_slots_2  (cost=0.00..0.00 rows=1 width=0) (actual time=0.006..0.006 rows=0 loops=1)
                  Filter: ((used >= capacity) AND (resource_kind = 'worker'::text) AND (resource_id = 'contract-worker'::text))
          ->  LockRows  (cost=38.53..40.10 rows=126 width=1394) (actual time=0.850..0.851 rows=1 loops=1)
                Buffers: shared hit=33
                ->  Sort  (cost=38.53..38.84 rows=126 width=1394) (actual time=0.837..0.838 rows=1 loops=1)
                      Sort Key: runs.created_at, runs.run_id
                      Sort Method: quicksort  Memory: 167kB
                      Buffers: shared hit=32
                      ->  Result  (cost=0.00..37.90 rows=126 width=1394) (actual time=0.024..0.504 rows=128 loops=1)
                            One-Time Filter: (NOT $0)
                            Buffers: shared hit=32
                            ->  Nested Loop Anti Join  (cost=0.00..37.27 rows=126 width=1378) (actual time=0.015..0.453 rows=128 loops=1)
                                  Join Filter: (execution_slots_1.resource_id = runs.profile_id)
                                  Buffers: shared hit=32
                                  ->  Nested Loop Anti Join  (cost=0.00..35.68 rows=127 width=1372) (actual time=0.014..0.370 rows=128 loops=1)
                                        Join Filter: (execution_slots.resource_id = runs.tenant_id)
                                        Buffers: shared hit=32
                                        ->  Seq Scan on runs  (cost=0.00..34.08 rows=128 width=1366) (actual time=0.012..0.246 rows=128 loops=1)
                                              Filter: ((profile_id = ANY ('{contract-profile-v1}'::text[])) AND (tenant_id = ANY ('{tenant-a,tenant-b}'::text[])) AND (state = 'ready'::text))
                                              Buffers: shared hit=32
                                        ->  Seq Scan on execution_slots  (cost=0.00..0.00 rows=1 width=38) (actual time=0.000..0.000 rows=0 loops=128)
                                              Filter: ((used >= capacity) AND (resource_kind = 'tenant'::text))
                                  ->  Seq Scan on execution_slots execution_slots_1  (cost=0.00..0.00 rows=1 width=38) (actual time=0.000..0.000 rows=0 loops=128)
                                        Filter: ((used >= capacity) AND (resource_kind = 'profile'::text))
        Planning:
          Buffers: shared hit=262 read=1
        Planning Time: 6.670 ms
        Execution Time: 1.095 ms
    run_performance_test.go:119: RUN-PERF complete: runs=128 calls=896 tools=384 steps=768 elapsed=24.7725351s runs_per_second=5.167
    run_performance_test.go:120: RUN-PERF claim: count=128 sum=3.193765s p50=24.7971ms p95=37.4088ms p99=43.2218ms max=61.1175ms
    run_performance_test.go:120: RUN-PERF claim-empty: count=2 sum=3.0294ms p50=1.3168ms p95=1.7126ms p99=1.7126ms max=1.7126ms
    run_performance_test.go:120: RUN-PERF commit-step: count=768 sum=10.7705519s p50=13.3421ms p95=18.2274ms p99=24.2701ms max=37.3005ms
    run_performance_test.go:120: RUN-PERF get-checkpoint: count=640 sum=5.0968127s p50=7.7651ms p95=10.0867ms p99=11.6828ms max=24.1466ms
    run_performance_test.go:120: RUN-PERF observe-chat-with-usage: count=128 sum=1.4193187s p50=10.672ms p95=14.4857ms p99=16.3253ms max=20.4939ms
    run_performance_test.go:120: RUN-PERF reserve-chat: count=128 sum=1.6977708s p50=12.8215ms p95=16.2426ms p99=20.8826ms max=22.5988ms
    run_performance_test.go:120: RUN-PERF submit: count=128 sum=2.7876264s p50=21.0342ms p95=29.1253ms p99=39.1076ms max=42.7885ms
    run_performance_test.go:127: RUN-PLAN call-identity-lock (ledger.go:readCall):
        LockRows  (cost=0.28..8.31 rows=1 width=847) (actual time=0.025..0.026 rows=1 loops=1)
          Buffers: shared hit=4
          ->  Index Scan using physical_calls_tenant_id_run_id_physical_call_id_key on physical_calls  (cost=0.28..8.30 rows=1 width=847) (actual time=0.020..0.021 rows=1 loops=1)
                Index Cond: ((tenant_id = 'tenant-a'::text) AND (run_id = 'a2888f8f-8367-4136-9130-f18f0d55b94c'::uuid) AND (physical_call_id = '575660f2-8e9c-4d8e-8832-c5fd48351432'::uuid))
                Buffers: shared hit=3
        Planning:
          Buffers: shared hit=120
        Planning Time: 0.982 ms
        Execution Time: 0.085 ms
    run_performance_test.go:128: RUN-PLAN call-ordinal (ledger.go:ReserveCall):
        Aggregate  (cost=17.67..17.68 rows=1 width=4) (actual time=0.026..0.026 rows=1 loops=1)
          Buffers: shared hit=4
          ->  Bitmap Heap Scan on physical_calls  (cost=4.32..17.66 rows=4 width=4) (actual time=0.018..0.020 rows=7 loops=1)
                Recheck Cond: ((tenant_id = 'tenant-a'::text) AND (run_id = 'a2888f8f-8367-4136-9130-f18f0d55b94c'::uuid))
                Heap Blocks: exact=2
                Buffers: shared hit=4
                ->  Bitmap Index Scan on physical_calls_run_idx  (cost=0.00..4.32 rows=4 width=0) (actual time=0.014..0.014 rows=7 loops=1)
                      Index Cond: ((tenant_id = 'tenant-a'::text) AND (run_id = 'a2888f8f-8367-4136-9130-f18f0d55b94c'::uuid))
                      Buffers: shared hit=2
        Planning Time: 0.205 ms
        Execution Time: 0.076 ms
    run_performance_test.go:129: RUN-PLAN tool-subcall-sequence (ledger.go:checkSubcall):
        Sort  (cost=17.68..17.68 rows=1 width=99) (actual time=0.040..0.041 rows=1 loops=1)
          Sort Key: ordinal
          Sort Method: quicksort  Memory: 25kB
          Buffers: shared hit=4
          ->  Bitmap Heap Scan on physical_calls  (cost=4.32..17.67 rows=1 width=99) (actual time=0.022..0.025 rows=1 loops=1)
                Recheck Cond: ((tenant_id = 'tenant-a'::text) AND (run_id = 'a2888f8f-8367-4136-9130-f18f0d55b94c'::uuid))
                Filter: (tool_invocation_id = '4baac4a0-a289-4f42-9ac0-45446645bc16'::uuid)
                Rows Removed by Filter: 6
                Heap Blocks: exact=2
                Buffers: shared hit=4
                ->  Bitmap Index Scan on physical_calls_run_idx  (cost=0.00..4.32 rows=4 width=0) (actual time=0.015..0.015 rows=7 loops=1)
                      Index Cond: ((tenant_id = 'tenant-a'::text) AND (run_id = 'a2888f8f-8367-4136-9130-f18f0d55b94c'::uuid))
                      Buffers: shared hit=2
        Planning Time: 0.087 ms
        Execution Time: 0.112 ms
    run_performance_test.go:130: RUN-PLAN run-usage (query.go:fillBudget):
        Aggregate  (cost=17.81..17.83 rows=1 width=64) (actual time=0.025..0.026 rows=1 loops=1)
          Buffers: shared hit=4
          ->  Bitmap Heap Scan on physical_calls  (cost=4.32..17.66 rows=4 width=69) (actual time=0.012..0.014 rows=7 loops=1)
                Recheck Cond: ((tenant_id = 'tenant-a'::text) AND (run_id = 'a2888f8f-8367-4136-9130-f18f0d55b94c'::uuid))
                Heap Blocks: exact=2
                Buffers: shared hit=4
                ->  Bitmap Index Scan on physical_calls_run_idx  (cost=0.00..4.32 rows=4 width=0) (actual time=0.008..0.008 rows=7 loops=1)
                      Index Cond: ((tenant_id = 'tenant-a'::text) AND (run_id = 'a2888f8f-8367-4136-9130-f18f0d55b94c'::uuid))
                      Buffers: shared hit=2
        Planning:
          Buffers: shared hit=10
        Planning Time: 0.215 ms
        Execution Time: 0.087 ms
    run_performance_test.go:131: RUN-PLAN run-tool-count (query.go:fillBudget):
        Aggregate  (cost=9.77..9.78 rows=1 width=8) (actual time=0.012..0.012 rows=1 loops=1)
          Buffers: shared hit=3
          ->  Bitmap Heap Scan on tool_invocations  (cost=4.29..9.76 rows=2 width=0) (actual time=0.009..0.009 rows=3 loops=1)
                Recheck Cond: ((tenant_id = 'tenant-a'::text) AND (run_id = 'a2888f8f-8367-4136-9130-f18f0d55b94c'::uuid))
                Heap Blocks: exact=1
                Buffers: shared hit=3
                ->  Bitmap Index Scan on tool_invocations_tenant_id_run_id_invocation_id_key  (cost=0.00..4.29 rows=2 width=0) (actual time=0.005..0.005 rows=3 loops=1)
                      Index Cond: ((tenant_id = 'tenant-a'::text) AND (run_id = 'a2888f8f-8367-4136-9130-f18f0d55b94c'::uuid))
                      Buffers: shared hit=2
        Planning:
          Buffers: shared hit=28
        Planning Time: 0.305 ms
        Execution Time: 0.056 ms
    run_performance_test.go:132: RUN-PLAN tool-identity-lock (ledger.go:readTool):
        LockRows  (cost=0.27..8.30 rows=1 width=156) (actual time=0.023..0.024 rows=1 loops=1)
          Buffers: shared hit=4
          ->  Index Scan using tool_invocations_pkey on tool_invocations  (cost=0.27..8.29 rows=1 width=156) (actual time=0.017..0.017 rows=1 loops=1)
                Index Cond: (invocation_id = '4baac4a0-a289-4f42-9ac0-45446645bc16'::uuid)
                Buffers: shared hit=3
        Planning:
          Buffers: shared hit=27
        Planning Time: 0.308 ms
        Execution Time: 0.052 ms
    run_performance_test.go:138: RUN-PLAN sparse fixture: ready=1 succeeded=128 total_runs=129
    run_performance_test.go:139: RUN-PLAN claim-sparse-ready (lifecycle.go:Claim):
        Limit  (cost=11.42..11.44 rows=1 width=1378) (actual time=0.114..0.116 rows=1 loops=1)
          Buffers: shared hit=20
          InitPlan 1 (returns $0)
            ->  Seq Scan on execution_slots execution_slots_2  (cost=0.00..1.09 rows=1 width=0) (actual time=0.006..0.006 rows=0 loops=1)
                  Filter: ((used >= capacity) AND (resource_kind = 'worker'::text) AND (resource_id = 'contract-worker'::text))
                  Rows Removed by Filter: 5
                  Buffers: shared hit=1
          ->  LockRows  (cost=10.34..10.35 rows=1 width=1378) (actual time=0.114..0.114 rows=1 loops=1)
                Buffers: shared hit=20
                ->  Sort  (cost=10.34..10.34 rows=1 width=1378) (actual time=0.108..0.109 rows=1 loops=1)
                      Sort Key: runs.created_at, runs.run_id
                      Sort Method: quicksort  Memory: 26kB
                      Buffers: shared hit=19
                      ->  Result  (cost=0.12..10.33 rows=1 width=1378) (actual time=0.082..0.085 rows=1 loops=1)
                            One-Time Filter: (NOT $0)
                            Buffers: shared hit=19
                            ->  Nested Loop Anti Join  (cost=0.12..10.32 rows=1 width=1362) (actual time=0.063..0.065 rows=1 loops=1)
                                  Join Filter: (execution_slots_1.resource_id = runs.profile_id)
                                  Buffers: shared hit=18
                                  ->  Nested Loop Anti Join  (cost=0.12..9.23 rows=1 width=1356) (actual time=0.033..0.035 rows=1 loops=1)
                                        Join Filter: (execution_slots.resource_id = runs.tenant_id)
                                        Buffers: shared hit=17
                                        ->  Index Scan using runs_ready_claim_idx on runs  (cost=0.12..8.15 rows=1 width=1350) (actual time=0.026..0.028 rows=1 loops=1)
                                              Index Cond: (profile_id = ANY ('{contract-profile-v1}'::text[]))
                                              Filter: ((tenant_id = ANY ('{tenant-a,tenant-b}'::text[])) AND (state = 'ready'::text))
                                              Buffers: shared hit=16
                                        ->  Seq Scan on execution_slots  (cost=0.00..1.07 rows=1 width=20) (actual time=0.005..0.005 rows=0 loops=1)
                                              Filter: ((used >= capacity) AND (resource_kind = 'tenant'::text))
                                              Rows Removed by Filter: 5
                                              Buffers: shared hit=1
                                  ->  Seq Scan on execution_slots execution_slots_1  (cost=0.00..1.07 rows=1 width=20) (actual time=0.007..0.007 rows=0 loops=1)
                                        Filter: ((used >= capacity) AND (resource_kind = 'profile'::text))
                                        Rows Removed by Filter: 5
                                        Buffers: shared hit=1
        Planning:
          Buffers: shared hit=242
        Planning Time: 2.164 ms
        Execution Time: 0.271 ms
--- PASS: TestRunHotPathPlansAndBaseline (28.33s)
PASS
ok  	github.com/xjfyrh/jobforge/tests/integration	29.107s

```
