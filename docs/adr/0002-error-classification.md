# ADR-0002：错误分类与 HTTP/gRPC 映射

- 状态：Accepted
- 日期：2026-07-29
- 决策者：XJfyrh
- 关联 Issue/PR：S0 评审
- 取代：无
- 被取代：无

## 上下文

JobForge 的领域错误需要在 HTTP API 和 gRPC Worker API 之间保持一致映射。PRD 10.1 节列出了统一错误码最低要求，但未固定完整的领域错误类型、可重试分类和传输层映射规则。Worker 和管理端需要稳定的错误码来判断是否重试、是否升级告警。

## 决策驱动因素

- 客户端（SDK、Worker）必须能根据错误码做出明确行为决策（重试 / 放弃 / 告警）。
- HTTP 和 gRPC 对同一领域错误必须返回语义一致的响应。
- 错误码集合应最小化，避免过度细分导致客户端无法穷举处理。
- 不泄露 SQL、堆栈、凭据等内部信息。

## 决策

### 领域错误类型

| 错误码 | 语义 | 可重试 |
|---|---|---|
| `INVALID_ARGUMENT` | 请求参数非法（type 未注册、payload 超限、字段缺失） | 否 |
| `UNAUTHORIZED` | API key 缺失或无效 | 否 |
| `FORBIDDEN` | 跨租户访问被拒绝 | 否 |
| `NOT_FOUND` | job_id 不存在或不属于当前租户 | 否 |
| `CONFLICT` | 幂等键冲突但参数不一致 | 否 |
| `ALREADY_TERMINAL` | 任务已处于终态，操作不适用 | 否 |
| `STALE_LEASE` | fencing token 或 owner 不匹配，写入被拒绝 | 否（Worker 应放弃） |
| `CANCEL_REQUESTED` | 任务已进入 cancelling，Complete 被拒绝 | 否 |
| `QUEUE_OVERLOADED` | 队列积压超过硬阈值，提交被拒绝 | 是（退避后重试） |
| `INTERNAL` | 未预期的服务端错误 | 是（可重试） |

### 传输层映射

| 领域错误 | HTTP Status | gRPC Code |
|---|---|---|
| `INVALID_ARGUMENT` | 400 Bad Request | `INVALID_ARGUMENT` |
| `UNAUTHORIZED` | 401 Unauthorized | `UNAUTHENTICATED` |
| `FORBIDDEN` | 403 Forbidden | `PERMISSION_DENIED` |
| `NOT_FOUND` | 404 Not Found | `NOT_FOUND` |
| `CONFLICT` | 409 Conflict | `ALREADY_EXISTS` |
| `ALREADY_TERMINAL` | 409 Conflict | `FAILED_PRECONDITION` |
| `STALE_LEASE` | 409 Conflict | `FAILED_PRECONDITION` |
| `CANCEL_REQUESTED` | 409 Conflict | `FAILED_PRECONDITION` |
| `QUEUE_OVERLOADED` | 429 Too Many Requests | `RESOURCE_EXHAUSTED` |
| `INTERNAL` | 500 Internal Server Error | `INTERNAL` |

### 错误响应格式

HTTP JSON：

```json
{
  "error": {
    "code": "STALE_LEASE",
    "message": "fencing token mismatch: job has been reclaimed by another worker"
  }
}
```

gRPC：使用 `google.rpc.Status` details 携带 `code` 字段；`message` 保持人类可读但不含敏感信息。

### 可重试分类规则

- Worker 执行层面的可重试由 Handler 返回的 `retryable` 标志决定（网络超时、临时依赖不可用）。
- 控制面错误中，仅 `QUEUE_OVERLOADED` 和 `INTERNAL` 对提交方可重试。
- `STALE_LEASE` 对 Worker 不可重试：Worker 必须放弃该任务，不得循环重试。

## 公开契约与数据影响

- HTTP API 所有错误响应使用统一 `{"error": {"code", "message"}}` 结构。
- gRPC 错误使用标准 status code + details。
- Python SDK 将错误码映射为稳定异常类型层级。
- 新增错误码必须向后兼容：客户端遇到未知 code 应按 `INTERNAL` 处理。

## 后果

### 正面

- 客户端行为决策清晰：看到 `STALE_LEASE` 就放弃，看到 `QUEUE_OVERLOADED` 就退避。
- HTTP 和 gRPC 语义一致，SDK 不需要区分传输层做不同处理。
- 错误码集合最小化，客户端可穷举。

### 负面与成本

- 409 复用了多个领域错误（ALREADY_TERMINAL / STALE_LEASE / CANCEL_REQUESTED），客户端需解析 body 中的 code 字段区分。
- 未来新增错误码需要同步更新 SDK 异常映射。

### 风险与缓解

- 客户端未解析 code 字段时可能误判 409 为幂等冲突：SDK 文档明确说明必须检查 error.code。

## 考虑过的替代方案

### 使用 HTTP 422 区分业务拒绝

422 语义为"格式正确但语义错误"，可区分 ALREADY_TERMINAL 等。但 422 在 API 生态中不如 409 通用，且 gRPC 无直接对应。不采用。

### 更细粒度错误码（如 LEASE_EXPIRED vs TOKEN_MISMATCH）

增加客户端处理负担，且行为决策相同（都是放弃）。合并为 STALE_LEASE。

## 验证与退出条件

- W1 HTTP API 集成测试验证所有错误码的 HTTP status 映射。
- W4 gRPC 契约测试验证 gRPC code 映射。
- 错误响应不包含 SQL 语句、堆栈或凭据。

## 后续工作

- [ ] W4：gRPC 错误 details 实现与契约测试
- [ ] W5：Python SDK 异常类型映射
