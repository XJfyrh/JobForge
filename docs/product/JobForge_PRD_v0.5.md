# JobForge：任务类型目录与 Worker 执行契约硬化 PRD（v0.5 定稿）

> 文档版本：v0.5<br>
> 文档状态：定稿（2026-08-18）<br>
> 实施状态：待实现<br>
> 创建/确认日期：2026-08-18<br>
> 基线版本：[PRD v0.4](JobForge_PRD_v0.4.md)<br>
> 代码基线：`881ddad`（main，migrations 0001～0018）<br>
> 前置决策：[ADR-0010](../adr/0010-task-type-catalog-and-worker-capability-binding.md)<br>
> 继承决策：[ADR-0002](../adr/0002-error-classification.md)<br>

本文档定义 v0.4 之后的最小 P0 契约增量。v0.1～v0.4 的 at-least-once、PostgreSQL 任务事实源、lease/fencing、终态不可变、业务幂等与取消竞争继续有效。本增量只闭合合法任务类型、Worker 登记能力与稳定 gRPC 领域错误三个相邻契约，不引入 Worker 认证、状态机或新数据库表。

## 1. 基线差距核对

| 范围 | 结论 |
|---|---|
| 控制面 | FR-002～005 已完成；FR-001 仍只校验 type 非空，未知 type 可入库。 |
| Worker 可靠执行 | FR-102～108、AT-01～11 的 lease/fencing/重试/取消/恢复已完成。 |
| Worker 能力 | FR-101 部分完成：Register 能力已持久化，Poll 未绑定登记 queue/type/capacity。 |
| gRPC 错误 | 未携带 ADR-0002 稳定领域码 details；`CANCEL_REQUESTED` 错映射为 `ABORTED`。 |
| v0.2～v0.4 | 耐久事件、配额、取消 SLO、持久效果与真实进程崩溃证据已完成。 |
| 仍为空白 | Worker 独立凭据、真实 PageWise 集成、ControlStream、Complete/Fail 完整指标标签及其他 P1。 |

## 2. 目标与选择理由

**Increment 名称：JobForge v0.5 MVP Increment：任务类型目录与 Worker 执行契约硬化。**

目标：

1. 部署静态目录定义合法 type，未知 type 在写 job/outbox 前拒绝；
2. Register 只能声明目录内类型，Poll 只能使用登记能力；
3. 并发 Poll 下单 Worker 的 `running + cancelling` 不超过登记容量；
4. Worker RPC 错误携带稳定领域码并修复 `CANCEL_REQUESTED` 映射；
5. 不改变状态机、公开成功响应、at-least-once、lease/fencing 或 Python SDK。

这是 v0.4 明确的下一候选，直接闭合 FR-001、FR-101 与 ADR-0002。ControlStream 是可裁剪 P1；Worker 认证需要凭据分发、轮换和发布兼容的独立安全决策；真实 PageWise 集成依赖外部系统，均不先于本增量。

## 3. 功能需求

| ID | 需求 |
|---|---|
| FR-901 | `JOBFORGE_TASK_TYPES` 提供部署静态类型目录；默认包含五个通用 Demo 与 `pagewise.reindex`；配置非法时启动失败。 |
| FR-902 | API 在持久化前验证 type；未知 type 返回 HTTP 400 + `INVALID_ARGUMENT`，job/attempt/outbox 零写入。 |
| FR-903 | Register 校验 worker ID、正容量、非空且无重复的 queues/types，以及 types 为部署目录子集。 |
| FR-904 | Poll 只接受 active 已登记 Worker，queues/types 必须是登记能力子集。 |
| FR-905 | Poll 能力验证、owner inflight 统计与 Claim 在同一事务；同 Worker 并发 Poll 不得超过登记 capacity。 |
| FR-906 | Worker Proto v1 兼容新增 `DomainErrorDetail{code,retryable}`；全部非成功 RPC 携带 detail，`CANCEL_REQUESTED` 映射为 `FAILED_PRECONDITION`。 |
| FR-907 | 契约拒绝使用固定 surface/reason 指标；API/Gateway 记录目录 size/hash，不记录请求 payload 或凭据。 |

## 4. 非功能需求

| ID | 要求 |
|---|---|
| NFR-501 | 未知 type、非法 Register、越权 Poll 均不得产生 job/worker/claim 状态副作用。 |
| NFR-502 | capacity=2、至少 8 路并发 Poll 时，该 worker 的 running+cancelling 最大值不超过 2；释放后可继续领取。 |
| NFR-503 | Proto 只做兼容新增，Buf lint/breaking 通过；旧客户端可忽略 detail。 |
| NFR-504 | `go test -race ./...`、AT-01～24、AT-02R/13R/14R、Redis 与 Python SDK 不回退。 |
| NFR-505 | Claim 五轮、20k Claim 与同参数 e2e 相对实施前同机基线恶化均小于 15%；W4 历史绝对失败继续披露。 |
| NFR-506 | 拒绝指标只有固定枚举标签；日志/trace/metrics 不记录完整 payload、DSN、Authorization 或秘密。 |

## 5. 配置、接口与兼容性

`JOBFORGE_TASK_TYPES` 是逗号分隔 allowlist；默认：

```text
demo.echo,demo.sleep,demo.fail,demo.idempotent_effect,demo.http,pagewise.reindex
```

类型名必须匹配 `^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`。空目录、空项、重复项或非法名称使配置加载失败。API 与 Gateway 使用同一语义；合法性不依赖 Worker 在线状态。

Proto 兼容新增：

```proto
message DomainErrorDetail {
  string code = 1;
  bool retryable = 2;
}
```

HTTP/SDK 成功响应不变；Register/Poll 的既有字段语义按 Proto 注释严格执行。不新增 migration 0019，不使用 `workers.inflight` 作为正确性来源，不新增第三方依赖。

## 6. Register/Poll 原子语义

Register 在 upsert 前完成全部字段和目录子集校验。Poll 每次实际 Claim 尝试在一个短事务内：

1. `SELECT workers ... FOR UPDATE`；
2. 验证 Worker active、queue/type 子集和 `max_jobs`/`available_capacity` 范围；
3. 统计该 owner 的 `running + cancelling`，计算服务端剩余 slot；
4. 以请求上限、客户端容量和服务端 slot 的最小值执行现有 Claim；
5. 提交后才进入通知等待；每次重试重新验证。

无服务端 slot 时立即返回空响应。该行锁序列化同 Worker 的并发 Poll 与重新注册，避免能力读取和 Claim 之间的 TOCTOU。

## 7. 可观测性

新增 `jobforge_contract_rejections_total{surface,reason}`：

- surface：`submit`、`register`、`poll`、`grpc_interceptor`；
- reason：`unknown_type`、`malformed_capability`、`capability_mismatch`、`unregistered_worker`、`capacity_exceeded`、`missing_deadline`。

实际 type、queue、worker ID、job ID 不进入该指标。API/Gateway 启动日志只记录排序目录的数量和 SHA-256 指纹。

## 8. 验收矩阵

| ID | 场景 | 预期 |
|---|---|---|
| AT-28 | Worker 离线时提交默认合法 type；再提交未知 type | 合法任务入库；未知 type 400/INVALID_ARGUMENT 且 job/outbox 零写入。 |
| AT-29 | 空/重复/目录外 Register；对已有 Worker 尝试非法覆盖 | RPC 拒绝；workers 行不存在或保持原值。 |
| AT-30 | 未注册 Poll、queue/type 越权、8 路并发 capacity=2、Poll 与重新注册竞争 | 越权零领取；最大 inflight≤2；重新登记后只按新能力领取。 |
| AT-31 | deadline、validation、stale lease、cancel requested 与 internal 错误矩阵 | status code 与 ADR-0002 一致；每个错误恰有稳定 DomainErrorDetail。 |

## 9. 最小可验收标准

- 默认六种类型可提交，未知 type 在持久化前失败；
- 目录配置错误 fail fast；本地 Registry 拒绝空类型、nil Handler 与重复注册，Types 有确定顺序；
- Register/Poll 不能扩大目录或登记能力；并发 Poll 容量硬上限成立；
- `CANCEL_REQUESTED` 为 `FAILED_PRECONDITION`，全部 Worker RPC 错误携带 detail；
- 拒绝指标标签只有本文枚举；
- migrations 仍止于 0018，历史 SQL 无修改；
- 默认 race、完整 integration/scale 与机械门禁通过；
- 当前性能回退小于 15%，历史 W4 绝对门禁不改写。

## 10. 非目标

- 不实现 Worker 凭据、mTLS、密钥轮换或声称完整信任边界；Gateway 仍需可信网络；
- 不实现 ControlStream/AT-25、动态 type API/数据库目录/热更新；
- 不把 `pagewise.reindex` 升级为真实 PageWise 集成；
- 不修复 Complete/Fail 空 queue/type 指标；
- 不改变状态机、lease/fencing、重试、DLQ、取消、配额、业务幂等或投递保证；
- 不实现跨 job ID 去重、exactly-once、Web UI、MCP、cron、暂停队列、Kafka。

## 11. 里程碑与交付顺序

| 阶段 | 交付物 | 退出条件 |
|---|---|---|
| M0 文档与基线 | 本 PRD、ADR-0010、Gateway benchmark harness、实施前 Claim/20k/e2e 数据 | ADR Accepted；无未决核心语义。 |
| M1 类型目录 | Catalog、配置、API/Registry、Compose、AT-28 | 未知 type 零持久化；配置 fail fast。 |
| M2 Worker 能力 | Register/Poll 原子能力和容量、AT-29/30 | 子集和并发容量全部通过。 |
| M3 错误契约 | Proto detail、统一映射、Runtime 兼容、AT-31 | Buf lint/breaking 与错误矩阵通过。 |
| M4 证据收官 | race/integration/scale/benchmark/lint、文档 | NFR-501～506 达标；历史失败无误报。 |

## 12. 风险与依赖

| 风险 | 缓解与验证 |
|---|---|
| API/Gateway 目录漂移 | Compose 显式同值；启动日志比较 size/hash。 |
| 移除类型遗留非终态任务 | 先停提交并 drain，再移除 Worker/Gateway/API 配置。 |
| 并发 Poll/重新注册 TOCTOU | workers 行锁、能力核对、容量计算与 Claim 同事务。 |
| owner inflight 统计拖慢 Claim | 实施前后同机基准；超过 15% 停止交付并单独评审索引/migration。 |
| 严格字段校验影响旧自定义客户端 | stock Runtime 已发送字段；发布说明与 preflight 明确正值/子集要求。 |
| 指标被任意 type 放大 | 不使用实际输入作为 label。 |
| Worker RPC 未认证 | 本增量保持可信网络边界；独立安全增量处理凭据与 mTLS。 |

## 13. 需求追溯

| v0.5 | 上游映射 |
|---|---|
| FR-901/902、AT-28 | v0.1 FR-001、§8.3、ADR-0002 |
| FR-903～905、AT-29/30 | v0.1 FR-101、W2/W4 |
| FR-906、AT-31 | v0.1 §10.1/10.2、ADR-0002、W4 |
| FR-907、NFR-506 | v0.1 G-04/G-05、§11.4/§12、Definition of Done |
| NFR-504 | v0.1 NFR-001～006；v0.2 NFR-202；v0.4 NFR-405 |
| NFR-505 | v0.1 §11.2；v0.2 NFR-201；v0.3 NFR-301；v0.4 NFR-406 |

## 14. 实施前性能基线

2026-08-18 在 commit `881ddad`、Windows amd64、AMD Ryzen 7 7840HS、Go 1.26.5、PostgreSQL 16（`postgres:16-alpine`）、Docker Desktop、非 race 下运行：

- `BenchmarkClaim` 五轮：6.156913、8.966757、7.492314、7.694718、9.038335 ms/op；中位数 **7.694718 ms/op**，6886～6889 B/op，91 allocs/op；
- `BenchmarkGatewayPollClaim` 五轮：6.713446、7.035100、8.348057、8.792114、9.571607 ms/op；中位数 **8.348057 ms/op**，7970～7971 B/op，112 allocs/op；
- 20,000-job Claim：p50 **103.7507ms**、p95 **120.8718ms**；Promote p50/p95 20.8472/24.6098ms；
- e2e 100 jobs/4 workers：Submit **301.50 jobs/sec**、Process **336.40 jobs/sec**、p50/p95/p99 **10.8192/15.1353/31.9063ms**、goroutine 2→2。

五轮使用同一数据库，结果随历史行累积波动，只用于本增量用相同命令和清理条件做前后比较。历史 W4 `4.171091ms/op` 绝对门禁继续标记未通过。

## 15. 实施与验收结果

待 M4 收官后填写。未运行或缺少依赖的检查不得标为 PASS。
