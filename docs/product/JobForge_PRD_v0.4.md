# JobForge：持久业务幂等与真实 Worker 崩溃证据 PRD（v0.4 定稿）

> 文档版本：v0.4<br>
> 文档状态：定稿（2026-08-17）<br>
> 创建/确认日期：2026-08-17<br>
> 基线版本：[PRD v0.3](JobForge_PRD_v0.3.md)<br>
> 代码基线：`1b31334`（main，migrations 0001～0017）<br>
> 前置决策：[ADR-0009](../adr/0009-demo-persistent-effects-and-real-crash-evidence.md)<br>
> 相关文档：[PRD v0.1](JobForge_PRD_v0.1.md)、[PRD v0.2](JobForge_PRD_v0.2.md)、[架构](../architecture.md)、[故障语义](../failure-semantics.md)、[可靠性报告](../reliability-report.md)、[性能基线](../benchmark.md)

本文档定义 v0.3 之后的最小可靠性证据增量。v0.1～v0.3 已固定的 at-least-once、PostgreSQL 唯一任务事实源、lease/fencing、终态不可变、取消竞争与安全边界继续有效。本增量只把 Demo 业务幂等从进程内状态升级为持久原子效果，并把 AT-02/13/14 从状态级崩溃模拟升级为真实 Worker OS 进程终止证据。

---

## 1. 基线差距核对

| 范围 | 结论 |
|---|---|
| v0.1 控制面 | FR-002～005 已实现；FR-001 部分完成：幂等、延迟和 payload 上限存在，但 API 只校验 type 非空，不拒绝未注册类型。 |
| Worker/调度 | lease、fencing、Heartbeat、Complete/Fail、重试/DLQ、Scheduler HA、取消竞争基本完成；AT-01、AT-03～12 有自动化证据。 |
| 业务幂等与崩溃 | **未完整闭环**：`demo.idempotent_effect` 使用进程内 map；AT-02/14 复用同一进程状态；AT-13 未实际 kill Worker。 |
| Worker 能力契约 | 部分完成：Runtime 按本地注册类型 Poll，但 Gateway 未严格核验 Poll 类型/队列/容量属于已登记能力。 |
| Agent/RAG Demo | `pagewise.reindex` 是 sleep + callback 模拟，不是真实 PageWise/RAG 集成；FR-403 仅为演示骨架。 |
| gRPC 错误契约 | 未携带 ADR-0002 的稳定领域码 details；`CANCEL_REQUESTED` 映射与 ADR 不一致。 |
| 安全与观测 | API key、payload 限制、pprof/metrics 边界已实现；Worker 独立凭据未实现，部分 Complete/Fail 指标缺 queue/type 标签。 |
| v0.2/v0.3 | Publisher、CLI、obs、scale、耐久事件、配额和取消 SLO 已交付；AT-13/14 的真实 kill/持久效果口径仍需升级；ControlStream FR-733/734、AT-25 未实现。 |

## 2. 目标与选择理由

### 2.1 Increment 名称

**JobForge v0.4 MVP Increment：持久业务幂等与真实 Worker 崩溃证据闭环**

### 2.2 目标

1. 同一 job 的 Demo 业务效果跨 Worker 进程持久且原子去重；
2. 在效果提交后、Complete 前真实终止 Worker，证明租约恢复、fencing 与持久幂等形成闭环；
3. 以 100 轮 kill 与 10,000 重投刷新 NFR-001/002 的可复现证据；
4. 保持任务状态机、公开 API、核心 Runtime 依赖和既有性能不回退。

选择本增量是因为它直接修复 G-02、AT-02、NFR-001/002 的 P0 可信度缺口，且可以在不改变公开协议或任务状态机的前提下交付。ControlStream 属于已明确可裁剪的 P1，不应先于尚未闭环的 P0 可靠性声明。

### 2.3 候选对比

| 候选 | 结论 | 理由 |
|---|---|---|
| 持久幂等 + 真实进程崩溃 | **选择** | 直接补齐核心可靠性证据，范围集中。 |
| M5 ControlStream | 推迟 | P1 可裁剪；不影响当前 Heartbeat 兜底正确性。 |
| 未注册 type/能力校验/gRPC error details | 下一候选 | 重要 P0 契约债务，但审查面不同，混入会扩大增量。 |
| Claim 性能专项 | 前置/持续证据 | 需要同机复测与披露，但本身不是用户能力。 |
| cron、暂停、mTLS、UI/MCP、Kafka | 排除 | 需要新产品/接口/部署决策，超过最小增量。 |

## 3. 功能需求

| ID | 需求 |
|---|---|
| FR-801 | 新增 0018 `demo_idempotent_effects`，以 `job_id uuid primary key` 持久保存确定性 `result_ref` 与 `applied_at`，不设置 jobs 外键。 |
| FR-802 | `demo.idempotent_effect` 以单条 `INSERT ... ON CONFLICT DO NOTHING` 产生效果；冲突返回已有 result；数据库错误可重试；不得回退内存 map。 |
| FR-803 | 支持首次效果提交后的 `post_effect_delay_ms`，默认 0、最大 60s、响应 context；重复投递不等待。 |
| FR-804 | 默认 Demo Worker 注入小容量 PostgreSQL pool；核心 Runtime、自定义 Handler API 与 Gateway-only 模型不引入数据库依赖。 |
| FR-805 | AT-02 使用真实 Worker helper 进程，在数据库屏障后 kill/Wait，由另一 Worker 重领并完成。 |
| FR-806 | AT-13 默认 100 轮实际终止 Worker 进程；AT-14 对 10,000 job 使用持久效果存储验证重复投递。 |
| FR-807 | 记录固定 outcome 指标和不含 payload/凭据的结构化日志。 |

## 4. 非功能需求

| ID | 要求 |
|---|---|
| NFR-401 | AT-02 最终 succeeded、fencing token 递增、效果表每 job 恰一行，applied=1、deduplicated≥1。 |
| NFR-402 | AT-13 默认 100 轮真实 kill，非终态任务静默丢失数为 0，恢复不超过既有 NFR-003 上界。 |
| NFR-403 | AT-14 默认 10,000 job、每 job 至少一次重复投递，效果表恰 10,000 行，重复业务效果为 0。 |
| NFR-404 | 子进程测试以数据库状态为屏障；所有进程有超时、Kill、Wait、cleanup；不使用生产 crash 后门。 |
| NFR-405 | `go test -race ./...`、integration、scale、Redis 套件及 AT-01～24 不回退。 |
| NFR-406 | 同环境 Claim 微基准、20k-job Claim 与 e2e 相对实现前中位数恶化均 <15%；历史绝对门禁独立披露。 |
| NFR-407 | 指标 outcome 只允许 applied/deduplicated/failed；日志不含完整 payload、DSN、Authorization 或秘密。 |

## 5. 数据、接口与兼容性

```sql
create table demo_idempotent_effects (
    job_id uuid primary key,
    result_ref text not null,
    applied_at timestamptz not null default now()
);
```

- HTTP、Python SDK、gRPC Proto、状态机、错误码不变；
- 只新增 0018 up/down，不修改 0001～0017，不新增第三方依赖；
- 幂等范围是同一 job ID 的重复投递；人工 retry 克隆的新 job ID 不共享效果；
- 该表不是 jobs/outbox 的替代事实源；跨系统效果仍需目标系统持久化 idempotency key。

## 6. 可观测性

新增 `jobforge_demo_idempotent_effects_total{outcome}` Counter；outcome 枚举为 `applied`、`deduplicated`、`failed`。Handler 日志记录 `job_id`、`effect_outcome`、`duration`，不把 job ID 作为 metric label，不记录完整 payload 或数据库连接信息。

## 7. 验收矩阵

| ID | 场景 | 操作 | 预期 |
|---|---|---|---|
| AT-26 | 0018 前滚/回滚 | 从 0017 up→down→up | schema 可逆；历史 migration 未改；SQLFluff 通过。 |
| AT-27 | 原子持久效果 | 并发对同一 job 调用 Handler | 一次 applied，其余 deduplicated；result_ref 一致；表一行。 |
| AT-02R | Complete 前真实崩溃 | Worker A 提交效果并进入 delay；父进程观察 DB 行后 kill/Wait；租约恢复；Worker B 执行 | succeeded；token 增长；效果一行；applied=1、deduplicated≥1。 |
| AT-13R | 100 轮真实 kill | 每轮子进程领取后 kill，强制租约过期并恢复完成 | 零静默丢失；恢复时间符合 NFR-003。 |
| AT-14R | 10,000 持久幂等 | 每个 job 执行、失 ACK、恢复并重投 | 表恰 10,000 行；重复效果 0；全部 succeeded。 |

AT-02R/13R/14R 是对既有 AT-02/13/14 证据口径的加强，不新建公开产品行为。

## 8. 最小可验收标准

- 0018 可前滚/回滚，SQLFluff 不新增 ignore；
- 默认 Handler 无内存去重路径，瞬时 PostgreSQL 错误为 retryable；
- AT-02R、默认 100 轮 AT-13R、默认 10,000 AT-14R 达标；
- helper 仅在 `_test.go`，数据库行作为 kill 屏障，进程全部被 Wait；
- 默认 race、integration、scale、Redis、lint、Buf、Ruff、mypy 门禁不回退；
- 实现前后同环境性能回退 <15%，旧 Claim 绝对门禁继续诚实披露；
- 所有文档只使用“at-least-once + 业务幂等”。

## 9. 非目标

- 不实现 FR-733/734 ControlStream 或 AT-25；
- 不处理未知 type 拒绝、Poll 能力校验、gRPC error details；
- 不把 `pagewise.reindex` 升级为真实集成；
- 不实现跨 job ID 去重、跨系统原子性或 exactly-once；
- 不改变 jobs/outbox、lease、fencing、重试、取消和配额语义；
- 不加入 Web UI、MCP、cron、队列暂停、mTLS、Kafka或新消息中间件；
- 不为 Demo 效果表设计自动 retention。

## 10. 里程碑与交付顺序

| 阶段 | 交付物 | 退出条件 |
|---|---|---|
| M0 文档与基线 | 本 PRD、ADR-0009、五轮 Claim 实现前中位数 | ADR Accepted，证据含环境与原始值。 |
| M1 持久效果 | 0018、PostgreSQL store、Handler、Worker wiring、指标 | 原子/错误/delay/cancel/敏感日志测试通过。 |
| M2 真实崩溃 | AT-02R、AT-13R、AT-14R | 单轮默认测试与 scale 字面规模达标。 |
| M3 证据收官 | race/integration/scale/benchmark/lint；文档同步 | 回归门禁达标，历史失败无误报。 |

## 11. 风险与依赖

| 风险 | 缓解与验证 |
|---|---|
| Demo 被误解为 exactly-once | ADR、日志、README 与报告明确同库业务幂等边界。 |
| Worker 新增 DB 连接 | 仅 Demo wiring，小 pool、Ping/Close；核心 Runtime 无 pgx 依赖。 |
| kill 窗口不稳定 | 效果行 + running 状态双屏障；首次效果后有界 delay。 |
| Windows 子进程泄漏 | 直接启动当前测试二进制；统一 deadline、Process.Kill、Wait，不通过 shell。 |
| 100 次启动拖慢默认 CI | AT-13R 继续由 `scale` build tag 隔离。 |
| 效果表增长 | 本版仅 Demo/测试保留审计行；产品化 retention 另行决策。 |
| Claim 历史绝对值未通过 | 同机五轮前后中位数判断本增量；旧门禁单列，不重置历史。 |

## 12. PRD 追溯

| v0.4 | 上游映射 |
|---|---|
| FR-801～804、AT-27 | v0.1 G-02、§8.2、FR-402 |
| FR-805、AT-02R | v0.1 AT-02、W3 |
| FR-806、NFR-402、AT-13R | v0.1 NFR-001；v0.2 FR-601、AT-13、NFR-204 |
| FR-806、NFR-403、AT-14R | v0.1 NFR-002；v0.2 FR-602、AT-14 |
| scale 隔离 | v0.2 FR-603、NFR-202 |
| race、报告与作品集证据 | v0.1 G-04/G-05、W7、Definition of Done |
| 性能不回退 | v0.1 §11.2；v0.2 NFR-201；v0.3 NFR-301 |

## 13. 实现前性能基线

2026-08-17 在 commit `1b31334`、Windows amd64、AMD Ryzen 7 7840HS、Go 1.26.5、PostgreSQL 16.14（`postgres:16-alpine`，Docker Engine 29.6.2，无 CPU/内存限额）、非 race 下执行：

```powershell
$env:JOBFORGE_TEST_DSN = "postgres://jobforge:jobforge@localhost:5433/jobforge?sslmode=disable"
go test ./benchmarks/micro -run '^$' -bench '^BenchmarkClaim$' -benchmem -benchtime=10s -count=5
```

五轮为 5.762112、6.644814、7.233324、8.566824、8.849266 ms/op；中位数 **7.233324 ms/op**，6886～6890 B/op，91 allocs/op。该组值随同一数据库内基准累积行数上升，只作为本增量前后用完全相同命令和环境比较的当前基线；它不覆盖或重置 W4 历史绝对值门禁。
