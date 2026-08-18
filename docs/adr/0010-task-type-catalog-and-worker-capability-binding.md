# ADR-0010：部署任务类型目录与 Worker 能力原子绑定

- 状态：Accepted
- 日期：2026-08-18
- 决策者：JobForge 维护者
- 关联 Issue/PR：PRD v0.5 M0
- 取代：无
- 被取代：无

## 上下文

PRD v0.1 FR-001 要求 API 拒绝非法任务类型，FR-101 要求 Worker 只收到自己声明支持的 queue/type，并以登记容量限制领取。当前 API 只检查 `type` 非空；Gateway 虽然保存 Register 的能力，却在 Poll 时直接信任请求字段，也不从服务端核对该 Worker 已持有的运行中租约数。

API、Gateway 与 Worker 是独立进程。以当前在线 Worker 作为合法类型来源会让 Worker 暂时离线影响任务提交，也会让陈旧注册决定产品契约；把类型目录放入 PostgreSQL 则需要动态管理、缓存失效与额外 migration，超出当前 MVP 的变化频率和运维需求。

## 决策驱动因素

- 合法 type 是部署契约，不应随 Worker 在线状态波动；
- 未注册或越权 Worker 不得领取 payload；
- 并发 Poll 与重新注册不能绕过 queue/type/capacity；
- 不改变任务状态机、at-least-once、lease 或 fencing；
- 保持 Proto v1 向后兼容，并避免新增数据库事实源。

## 决策

### 1. 部署级静态任务类型目录

- `JOBFORGE_TASK_TYPES` 是 API 与 Gateway 共同采用的合法类型 allowlist，使用逗号分隔；
- 默认目录为 `demo.echo`、`demo.sleep`、`demo.fail`、`demo.idempotent_effect`、`demo.http`、`pagewise.reindex`；
- 类型名必须匹配 `^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`；空目录、空项、重复项或非法名称使进程启动失败；
- 目录不依赖在线 Worker。API 可在 Worker 离线时接受合法任务；Worker 注册的 supported types 必须是目录子集；
- API 与 Gateway 启动时记录排序目录的数量和 SHA-256 指纹，供部署比较配置漂移。

### 2. Register 能力校验

Gateway 在写 workers 行之前验证：worker ID 非空、capacity 为正、queues/types 非空且无空项或重复项、types 全部属于部署目录。失败返回 `INVALID_ARGUMENT`，不得部分更新已有登记。

### 3. Poll 与 Claim 原子绑定

每次实际 Claim 尝试在一个短事务中完成：

1. 以 worker ID 查询 workers 行并 `FOR UPDATE`；
2. 验证 Worker 存在、active，Poll queues/types 是登记能力子集；
3. 验证 `max_jobs` 与 `available_capacity` 在 `1..registered capacity`；
4. 从 jobs 统计该 owner 当前 `running + cancelling` 数量，得到服务端剩余 slot；
5. 以请求上限、客户端可用容量和服务端剩余 slot 的最小值执行现有 Claim。

同一 workers 行锁序列化并发 Poll 与重新注册，能力读取、容量计算和 jobs 领取在同一事务提交，避免 TOCTOU。长轮询等待前提交事务并释放锁；每次唤醒重新执行完整校验。服务端无 slot 时立即返回空结果。

`workers.inflight` 仍不作为正确性来源；jobs 的 `running + cancelling` 聚合是容量核对依据。现有 schema 足够，不新增 migration。

### 4. 目录发布规则

- 新增类型：先让 API/Gateway 使用包含新类型的目录，再上线对应 Worker；期间任务允许排队；
- 移除类型：先停止该类型提交并确认所有非终态任务已清空，再从 Worker/Gateway/API 目录移除；
- API 与 Gateway 目录指纹不同属于部署错误，运维必须在接流量前修正。

## 公开契约与数据影响

- HTTP：成功响应不变；未知 type 明确返回 HTTP 400 + `INVALID_ARGUMENT`，且不写 job/outbox；
- gRPC：Register/Poll 的既有字段语义收紧为 Proto 注释已经声明的子集/容量约束；不删除或改号字段；
- 配置：新增 `JOBFORGE_TASK_TYPES`；
- 数据库：无 schema 变化，无 migration 0019；
- 状态机、lease、fencing、重试、取消、幂等和 Python SDK：无变化；
- 安全：本 ADR 不引入 Worker 凭据，Gateway 仍必须位于可信网络；独立凭据另行决策。

## 后果

### 正面

- 未知 type 在进入可靠队列前失败，不再形成永久无人消费的积压；
- Worker 无法在 Poll 时扩大已登记 queue/type/capacity；
- 同一 Worker 的并发 Poll 也不能超过服务端登记容量；
- Worker 离线不影响合法任务提交。

### 负面与成本

- API 与 Gateway 必须保持目录配置一致；
- 每次 Claim 尝试增加一次 Worker 行锁与 owner inflight 统计；
- 自定义 Handler 上线前必须显式更新目录；严格校验会暴露旧客户端遗漏的 Poll 字段。

### 风险与缓解

- 配置漂移：Compose 显式同值，启动日志记录目录指纹；
- 类型移除遗留任务：按上述 drain 顺序发布，并在运行手册给出非终态查询；
- Claim 性能回退：同机五轮、20k Claim 与 e2e 均要求恶化小于 15%；不达标时单独评审索引或 schema，不降低正确性；
- 锁竞争：只在短 Claim 事务持有 Worker 行锁，长轮询等待不持锁。

## 考虑过的替代方案

### PostgreSQL `task_types` 表

能提供动态单一来源，但需要 migration、管理入口、缓存/查询策略与删除一致性。本阶段类型目录规模小、变化低频，不采用。

### 当前 Worker 注册记录作为合法类型来源

无需新配置，但首次 Worker 注册前无法提交，离线/陈旧/未认证注册会改变控制面契约，不采用。

### 只在 Worker Runtime 拒绝未知类型

会在执行时才把任务送入 dead，仍允许错误任务长期占用队列与 attempt，不满足 FR-001/101，不采用。

## 验证与退出条件

- AT-28：合法类型在 Worker 离线时可提交；未知类型 400 且 job/outbox 零写入；
- AT-29：非法 Register 不写或改 workers 行；
- AT-30：Poll 子集、重新注册竞争与至少 8 路并发容量测试通过；
- `go test -race ./...`、现有 AT-01～24 与 v0.4 可靠性套件不回退；
- 当前 Claim/e2e/20k Claim 相对实施前同机基线恶化均小于 15%。

## 后续工作

- [ ] 独立 ADR 设计 Worker 凭据、轮换与 mTLS 演进；
- [ ] 若 owner inflight 统计成为热点，基于实测另行评审索引/migration。
