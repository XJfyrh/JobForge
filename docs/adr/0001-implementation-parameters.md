# ADR-0001：实现参数与通信模式

- 状态：Accepted
- 日期：2026-07-29
- 决策者：XJfyrh
- 关联 Issue/PR：S0 评审
- 取代：无
- 被取代：无

## 上下文

PRD 第 20 节要求 S0 结束前固定五项实现参数：lease TTL、heartbeat 间隔、Scheduler scan interval、Worker Poll 通信模式和 dead 任务人工重试策略。这些参数直接影响队列内核事务设计、Worker 协议复杂度和状态机不变量。

## 决策驱动因素

- 可靠性优先：参数选择必须保证 at-least-once 语义和故障恢复上界可计算。
- 实现复杂度：MVP 阶段优先选择简单、可测试的方案，降低 W1-W3 风险。
- 可演进性：选择不应阻塞 P1 升级路径。

## 决策

| 参数 | 值 | 说明 |
|---|---|---|
| Lease TTL | 30 秒 | Worker 崩溃后任务恢复上界 = 30s + scan_interval + 2s |
| Heartbeat 间隔 | 10 秒 | 约 3 次 heartbeat 覆盖一个 TTL 周期 |
| Scheduler scan interval | 1 秒 | scheduled/retry_wait 推进延迟上界 1s |
| Worker Poll 模式 | Unary long-poll | 服务端在 deadline 内等待可用任务后返回 |
| 人工重试策略 | 克隆新 job_id | 新 job 记录 `retry_of_job_id`，原任务终态不可变 |
| API key 存储 | 静态配置映射 | P0 使用 env/config 文件；P1 可升级数据库表 |

### Worker Poll 补充说明

- Worker 发起 `Poll` unary RPC，携带 worker_id、max_jobs 和 gRPC deadline（建议 30s）。
- 服务端查询数据库；有任务立即返回，无任务则等待（pg_notify 或短轮询）直到有任务或 deadline 到期。
- 连接断开等价于一次 Poll 失败，Worker 重新发起即可。
- 服务端无需维护 Worker 会话状态。
- P1 可选增加 server streaming 模式作为低延迟升级路径，不影响 unary 契约。

### 人工重试补充说明

- dead/cancelled 任务保持终态不变。
- 人工重试创建新 job，`retry_of_job_id` 指向原任务，attempt 从 1 开始。
- 审计链通过 `retry_of_job_id` 字段追溯，不需要状态回退。
- succeeded 任务不允许重试。

## 公开契约与数据影响

- `jobs` 表新增 `retry_of_job_id UUID NULL` 字段（nullable FK 自引用）。
- gRPC Worker API 使用 unary RPC：Register、Poll、Heartbeat、Complete、Fail。
- HTTP API 人工重试端点 `POST /v1/jobs/{job_id}:retry` 返回新 job_id。

## 后果

### 正面

- 参数固定后 W1 可直接实现 claim/heartbeat 事务，无需等待后续决策。
- Unary long-poll 实现简单，故障测试场景少，与 gRPC deadline 天然配合。
- 克隆 job_id 保持终态不可变不变量，状态机无回退转换。

### 负面与成本

- 30s lease TTL 意味着 Worker 崩溃后最长 ~33s 才恢复；对延迟极敏感场景可能偏长。
- 人工重试产生新 job_id，查询历史需要跟随 `retry_of_job_id` 链。

### 风险与缓解

- 如果 30s 恢复时间不满足演示需求，可通过配置缩短 TTL（测试环境已允许）。
- 如果后续需要 server streaming，作为 P1 新增 RPC 不破坏现有 unary 契约。

## 考虑过的替代方案

### Server Streaming Poll

服务端主动推送任务，延迟更低。但需要维护流状态、处理断开重连和背压，Gateway 复杂度增加约 30-50%，故障测试场景翻倍。MVP 阶段不采用。

### 复用原 job_id 新增 attempt

ID 不变、查询简单，但打破终态不可变不变量，状态机需增加 dead→ready 转换，并发安全更复杂。不采用。

### 数据库表存储 API key

支持动态增删，更贴近生产。但 P0 阶段静态配置足够，避免额外 migration 和缓存一致性问题。

## 验证与退出条件

- W1 集成测试验证 lease 30s + heartbeat 10s 参数下 claim/expire 行为正确。
- 人工重试在 W3 实现时验证原 job 终态不被修改。
- `go test -race ./...` 通过。

## 后续工作

- [ ] W4：基于 unary long-poll 实现 Worker Gateway gRPC 服务
- [ ] W5：Python SDK 封装 Poll 超时与重连
- [ ] P1：评估 server streaming 升级需求
