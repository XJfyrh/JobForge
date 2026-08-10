# ADR-0008：取消控制通道与 Heartbeat 参数（P0 5s Heartbeat SLO，P1 ControlStream 预留）

- 状态：Accepted
- 日期：2026-08-10
- 决策者：JobForge 维护者
- 关联 Issue/PR：PRD v0.3 §5.4（FR-730~734、NFR-306/307、AT-24/25、D3）
- 取代：无（ADR-0001 中 heartbeat=10s 的参数取值由本 ADR 修订为 5s；ADR-0003 的取消通道后续演进由本 ADR 承接）
- 被取代：无

## 上下文

当前取消信号只有一条路径：Worker 每 10s 发送 Heartbeat，Gateway 在 Heartbeat 响应中检查 `state='cancelling'` 并返回 CANCEL 信号。健康链路下取消感知的最坏等待约一个心跳周期（10s），再叠加 Handler 响应时间（GAP-706）。同时心跳配置不完全收敛（GAP-707）：`JOBFORGE_HEARTBEAT_INTERVAL` 可配置 Worker 侧间隔，但 Gateway `RegisterResponse.HeartbeatInterval` 硬编码 10s，Runtime 也不采用响应值——运维配置与协议建议值可能不一致。

PRD v0.3 已确认 D3：P0 采用 5s Heartbeat 交付 5 秒级信号 SLO；P1 ControlStream 为可裁剪扩展。本 ADR 固定配置来源、SLO 度量口径与降级语义。实现随 M4（P0）与 M5（P1，可裁剪）交付，本 ADR 先行评审以解锁 M0 退出条件。

## 决策驱动因素

- 取消信号 SLO 必须可度量、可验收，且不受容器/WSL2 时钟漂移污染；
- Heartbeat 是永不丢失的兜底：任何加速通道失效都不得丢取消；
- 5s 心跳使 job lease 续租写入约翻倍，必须先量化写放大且不回退 W4/W11 门禁（NFR-306）；
- 不改变状态机、错误码与“先提交者生效”的取消竞争语义。

## 决策

### 1. Heartbeat 默认值与单一配置来源（FR-730）

- 默认 `JOBFORGE_HEARTBEAT_INTERVAL` 由 10s 调整为 **5s**；Lease TTL 默认保持 30s，一个 lease 周期约 6 次心跳；
- 单一配置来源：Gateway 与 Worker 使用同一环境变量语义。`RegisterResponse.HeartbeatInterval` 返回 Gateway 侧配置值（不再硬编码）；Worker Runtime 在未显式配置本地间隔时采用注册响应建议值，显式配置时本地优先。二者同源时数值必然一致，发布说明需注明默认值变更；
- 该默认值变更属于行为兼容性变更（写放大、信号时延），列入发布说明与文档同步清单。

### 2. 信号 SLO 度量口径（FR-731/732、NFR-307）

- **P0 signal SLO**：`cancel_requested_at`（DB）→ Gateway 发出信号（Heartbeat 响应携带 CANCEL）p95 ≤ 6s（AT-24）；
- 时延**不得**用 Gateway 本地 `time.Now()` 减 PostgreSQL 的 `cancel_requested_at`。Heartbeat 回查由 PostgreSQL 在同一查询中基于 `clock_timestamp()` 返回 elapsed（例如 `extract(epoch from clock_timestamp() - cancel_requested_at)`），Gateway 以该 DB-clock elapsed 记录 `jobforge_cancel_signal_latency_seconds`（label `path=heartbeat|stream`），规避容器时钟漂移。`cancel_requested_at` 与 cancelling 同事务写入、早于或等于 commit，该口径是 commit→signal 的保守上界；
- **分段独立报告**：Worker 收到信号/取消 context → Handler return 由 `jobforge_cancel_handler_stop_latency_seconds`（label `type`）单独度量；受控测试另行测量“Cancel API 成功返回 → Worker context cancelled”端到端时延，只报告、不套用 signal SLO 阈值；
- `job_id`/`event_id`/`trace_id`/`worker_id` 不作为 metrics label。

### 3. Heartbeat 永久兜底（FR-732）

- 无论 P1 通道是否存在或健康，Heartbeat 响应始终检查 cancelling 并返回信号；
- 瞬时 Gateway 故障恢复后继续检查；控制流不可用时不丢取消，最迟由 Heartbeat/lease expiry 收敛为 cancelled；
- AT-05/06 的取消竞争语义（Cancel 先提交后进入 cancelling；Complete 返回 CANCEL_REQUESTED）不变。

### 4. P1 ControlStream（FR-733/734，可裁剪，M5）

- 新增**向后兼容**的 server-streaming Control RPC：Worker 注册后维持控制流，Gateway 按 worker/session 推送 cancel；unary Poll/Heartbeat 保留，proto 只新增不修改既有字段语义；
- 控制流健康时同口径 signal p95 ≤1s（AT-25）；断流自动重连并退回 Heartbeat；
- 多 Gateway 实例：PostgreSQL cancel hint（pg_notify）唤醒本地 session registry；通知只作 hint，收到后回查 job owner/token/state；NOTIFY 丢失或错误 Gateway 收到通知均不影响最终取消；陈旧 session 不得取消新 lease（推送必须包含或回查 fencing token）；
- 不采用“仅向活跃 Poll 注入取消”：Runtime 在 capacity 为 0 时不发 Poll，满载 Worker 恰是最需要取消的场景。

### 5. 写放大与存活参数护栏（NFR-306）

- job lease 续租**不节流**：5s 心跳全部续租，允许写放大约 2×，以 W4/W11 回归量化；
- `workers.last_heartbeat_at` 等附属存活写继续按 `LeaseTTL/3` 条件刷新，不得跳过或延迟 job lease 续租；
- 回归必须覆盖默认 LeaseTTL=30s 下 5s job heartbeat 与 10s 存活刷新阈值的组合，并覆盖非默认 TTL/interval 组合，确认 active worker 指标不抖动、附属写不意外翻倍。

## 公开契约与数据影响

- **HTTP API**：无变化；
- **gRPC v1**：P0 无变化（RegisterResponse 字段已存在，仅取值来源改变）；P1 仅新增 Control streaming RPC；
- **状态机/错误码**：无新状态、无新错误码；
- **数据库**：P0 无 schema 变化（Heartbeat 回查查询扩展返回 DB-clock elapsed）；
- **配置**：`JOBFORGE_HEARTBEAT_INTERVAL` 默认 10s→5s（发布说明）；
- **指标**：新增 `jobforge_cancel_signal_latency_seconds`（path）、`jobforge_cancel_handler_stop_latency_seconds`（type）；Trace 新增 `gateway.cancel_signal` span；span 与指标不含完整 payload 与凭据。

## 后果

### 正面

- 健康链路取消信号最坏等待从约 10s 收敛到 5s 级（p95 ≤6s 可验收）；
- Gateway/Worker 心跳配置单一来源，消除协议建议值与运维值漂移；
- 为 P1 低延迟通道（p95 ≤1s）预留清晰的兼容路径，且不牺牲 Heartbeat 兜底。

### 负面与成本

- job lease 续租写放大约 2×，PostgreSQL 写入压力上升；
- P1 ControlStream 引入 Gateway session 状态（重连、泄漏、多实例路由复杂度），需 race/泄漏测试与 session fencing。

### 风险与缓解

- 写放大回退 W4/W11 门禁：M4 交付前完成基准量化，不达标则回到 ADR 修订参数而非裁剪正确性；
- DB-clock 口径被本地时钟混用：度量实现只接受 Heartbeat/ControlStream 回查返回的 elapsed 值，代码评审以测试固化；
- 旧 token 误取消：推送携带/回查 fencing token；陈旧 session 拒绝（AT-25 覆盖）。

## 考虑过的替代方案

### 方案 A：P0 直接实现 ControlStream

未采用：P0 目标是最小闭环的 5 秒级 SLO；ControlStream 的 session/多实例复杂度放到可裁剪的 M5，避免阻塞核心版。

### 方案 B：仅向活跃 Poll 注入取消信号

未采用：满载 Worker（available capacity = 0）不发起 Poll，而满载恰是最需要取消正在执行任务的场景；方案不可靠，PRD §5.4 已明确排除。

## 验证与退出条件

- AT-24：默认 5s、健康 Gateway，随机相位提交 cancel；DB-clock signal elapsed p95 ≤6s；端到端 context 取消只报告不套阈值；AT-05/06 不变；
- NFR-306：lease 续租写放大量化归档；W4/W11 门禁不回退；存活刷新阈值组合回归通过；
- AT-25（仅交付 M5 时必需）：正常推送 p95 ≤1s；断流/错 Gateway/NOTIFY 丢失时 Heartbeat 兜底；旧 token 不误取消；
- 新增 goroutine（control stream）有 owner、重连退避、取消与 Wait 路径，`go test -race ./...` 通过。

## 后续工作

- [ ] M4：5s 默认收敛（config/Gateway/Runtime 同源）、DB-clock elapsed 回查与两个取消时延指标、AT-24 与写放大归档；
- [ ] M5（可裁剪）：Control streaming RPC、多 Gateway session 路由与 AT-25；
- [ ] 发布说明注明心跳默认值变更。
