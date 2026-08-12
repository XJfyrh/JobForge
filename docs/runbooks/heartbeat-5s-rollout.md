# 5s Heartbeat 发布与滚动升级说明

PRD v0.3 M4 / ADR-0008 将 `JOBFORGE_HEARTBEAT_INTERVAL` 默认值从 10s 调整为 5s，默认 Lease TTL 保持 30s。该变更不新增 migration，不改变 HTTP/gRPC v1 字段、八状态状态机、fencing、错误码或 at-least-once 语义。

## 配置优先级

1. Gateway 从自身 `JOBFORGE_HEARTBEAT_INTERVAL` 读取配置并在 `RegisterResponse.heartbeat_interval` 返回；默认 5s。
2. Worker 未显式设置本地 `JOBFORGE_HEARTBEAT_INTERVAL` 时采用注册建议值。
3. Worker 显式设置本地值时本地优先，并记录选择来源；这可用于受控回退，但该 Worker 可能不再满足默认取消 SLO。

Compose 仅在 Gateway 设置该变量，默认 Worker 不再硬编码 10s。

## 兼容滚动升级

建议先升级 Gateway，再升级 Worker：

1. 记录升级前 PostgreSQL heartbeat UPDATE 负载、Gateway 错误率、lease expiry 和取消延迟。
2. 升级 Gateway。旧 Worker 仍按其本地旧默认运行；新 Gateway 不改变 lease/fencing 语义。
3. 分批升级 Worker。新 Worker 未显式覆盖时从注册响应采用 5s；确认日志中的 `source=gateway` 与 `heartbeat_interval=5s`。
4. 确认所有 Worker 不再携带遗留显式 `10s`，再开始声明 M4 的 ≤6s signal p95。
5. 观察 `jobforge_cancel_signal_latency_seconds{path="heartbeat"}`、heartbeat RPC 错误、PostgreSQL 写入与 lease expiry。Handler 响应时间使用 `jobforge_cancel_handler_stop_latency_seconds{type}` 单独判断。

新 Worker 连接旧 Gateway 时会采用旧 Gateway 返回的 10s 建议，因此保持兼容但尚未获得 M4 SLO；只有 Gateway 与 Worker 都完成升级且没有本地 10s override 后，5s 默认才完整生效。

## 容量与回退

每个持续运行 job 在逻辑 30s 窗口内从约 3 次续租增加为约 6 次，真实 PostgreSQL 基准应呈 2.00× lease UPDATE。`workers.last_heartbeat_at` 仍按 `LeaseTTL/3` 条件刷新，不随 job heartbeat 翻倍。

若数据库容量不足，可临时在 Gateway 与目标 Worker 显式设置较长间隔并滚动重启；这不会改变正确性，Heartbeat/lease expiry 仍会最终收敛，但 5s 取消 signal SLO 不再成立。不得通过跳过 job lease 续租、放宽 fencing 或改为 ACK/状态的非事务路径降载。

## 验证

```powershell
$env:JOBFORGE_TEST_DSN = "postgres://jobforge:jobforge@localhost:5433/jobforge?sslmode=disable"
go test -race -count=1 -v -run 'TestCancelAT24HeartbeatSignalSLO|TestCompleteCancelRace|TestGatewayLivenessRefreshOnRPCs|TestGatewayNonDefaultLeaseLivenessThrottle' ./tests/integration/
go test -tags scale -count=1 -v -run TestScaleNFR306HeartbeatWriteAmplification ./tests/scale/
```

AT-25 / ControlStream 属于可裁剪 M5，不是本次升级依赖；Heartbeat 永久保留为取消兜底。
