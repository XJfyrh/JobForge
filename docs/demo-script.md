# 三分钟演示脚本

本文档提供 JobForge 的可复现演示步骤，对应 PRD 第 18 节。全部命令可在 Docker Compose 环境中直接执行。

## 前置条件

- Docker Desktop 已安装并运行
- curl 可用（Windows 11 内置）
- Go 1.26+ 已安装（用于运行测试命令）

## 步骤 1：启动全栈

```sh
docker compose -f deploy/compose.yaml up -d --build
```

等待所有服务就绪：

```sh
docker compose -f deploy/compose.yaml ps
```

预期输出包含：postgres (healthy)、api、scheduler、gateway、worker-1、worker-2。

## 步骤 2：提交任务

提交一个通用 echo 任务：

```sh
curl -s -X POST http://localhost:8080/v1/jobs \
  -H "Content-Type: application/json" \
  -H "X-API-Key: dev-api-key" \
  -d '{"queue":"default","type":"demo.echo","payload":{"message":"hello jobforge"}}' | python -m json.tool
```

提交一个 PageWise 索引重建任务（当前为模拟 Agent Handler 骨架，不连接真实 PageWise 服务）：

```sh
curl -s -X POST http://localhost:8080/v1/jobs \
  -H "Content-Type: application/json" \
  -H "X-API-Key: dev-api-key" \
  -d '{"queue":"default","type":"pagewise.reindex","payload":{"document_id":"doc-001","pages":5},"timeout_seconds":60}' | python -m json.tool
```

记录返回的 `job_id`，查询状态：

```sh
curl -s http://localhost:8080/v1/jobs/{job_id} \
  -H "X-API-Key: dev-api-key" | python -m json.tool
```

验证部署任务类型目录：Worker 是否在线不影响合法 type 入队；未知 type 必须在 job/outbox 写入前拒绝。

```sh
curl -s -X POST http://localhost:8080/v1/jobs \
  -H "Content-Type: application/json" \
  -H "X-API-Key: dev-api-key" \
  -d '{"queue":"default","type":"demo.unknown","payload":{}}' | python -m json.tool
```

预期 HTTP 400：`{"error":{"code":"INVALID_ARGUMENT","message":"task type is not registered"}}`。API 与 Gateway 启动日志中的 `catalog_size=6` 和 `catalog_sha256` 应一致；Compose 已为二者显式设置相同 `JOBFORGE_TASK_TYPES`。

## 步骤 3：持久效果提交后 Kill Worker

先停止 worker-2，确保待故障任务由 worker-1 领取；随后提交一个在**首次持久效果提交后**等待 60 秒的任务：

```sh
docker compose -f deploy/compose.yaml stop worker-2

curl -s -X POST http://localhost:8080/v1/jobs \
  -H "Content-Type: application/json" \
  -H "X-API-Key: dev-api-key" \
  -d '{"queue":"default","type":"demo.idempotent_effect","payload":{"post_effect_delay_ms":60000},"timeout_seconds":120}'
```

记录返回的 `{job_id}`。用效果表作为同步屏障，确认业务效果已经提交：

```sh
docker compose -f deploy/compose.yaml exec -T postgres \
  psql -U jobforge -d jobforge -c \
  "select job_id, result_ref, applied_at from demo_idempotent_effects where job_id = '{job_id}'"
```

查询返回一行后，在 Complete 前实际 kill worker-1：

```sh
docker compose -f deploy/compose.yaml kill worker-1
```

## 步骤 4：观察 Lease 过期与重新领取

启动 worker-2，等待 lease 过期（默认 30s）+ Scheduler 扫描（1s）：

```sh
docker compose -f deploy/compose.yaml start worker-2

# 等待约 35 秒后查询任务状态
sleep 35
curl -s http://localhost:8080/v1/jobs/{job_id} \
  -H "X-API-Key: dev-api-key" | python -m json.tool
```

预期：任务被 worker-2 重新领取并进入 `succeeded`，`lease_owner` 变为 worker-2，`attempt` 与 fencing token 递增。重复执行命中既有 `result_ref`，不会再次等待 60 秒。

再次查询效果表，仍必须恰好一行：

```sh
docker compose -f deploy/compose.yaml exec -T postgres \
  psql -U jobforge -d jobforge -c \
  "select count(*) as persistent_effects from demo_idempotent_effects where job_id = '{job_id}'"
```

重启 worker-1：

```sh
docker compose -f deploy/compose.yaml start worker-1
```

## 步骤 5：STALE_LEASE 演示

通过集成测试验证旧 Worker 晚到 Complete 被拒绝：

```sh
go test -run TestFaultAT03StaleWorkerLateComplete ./tests/integration/ -v -count=1
```

预期输出包含：`STALE_LEASE` 错误被正确返回，新状态不被覆盖。

v0.5 还要求 gRPC status 带稳定领域 detail，且取消先提交时 `CANCEL_REQUESTED` 映射为 `FAILED_PRECONDITION`。真实 PostgreSQL 契约验证：

```powershell
$env:JOBFORGE_TEST_DSN = "postgres://jobforge:jobforge@localhost:5433/jobforge?sslmode=disable"
go test -run TestAT31LeaseAndCancelDomainDetails ./tests/integration/ -v -count=1
```

## 步骤 6：提交幂等与执行幂等

提交带幂等键的任务（重复提交不会创建新任务）：

```sh
# 第一次提交
curl -s -X POST http://localhost:8080/v1/jobs \
  -H "Content-Type: application/json" \
  -H "X-API-Key: dev-api-key" \
  -d '{"queue":"default","type":"demo.idempotent_effect","payload":{"key":"effect-001"},"idempotency_key":"unique-key-001"}'

# 第二次提交（相同 idempotency_key）
curl -s -X POST http://localhost:8080/v1/jobs \
  -H "Content-Type: application/json" \
  -H "X-API-Key: dev-api-key" \
  -d '{"queue":"default","type":"demo.idempotent_effect","payload":{"key":"effect-001"},"idempotency_key":"unique-key-001"}'
```

预期：第二次返回 `"deduplicated": true`，job_id 相同。

上述 `idempotency_key` 是提交 API 去重；执行幂等是另一层契约，范围为同一 job ID 的重复投递。通过故障测试验证真实 Worker 进程退出后持久效果仍仅一次：

```powershell
$env:JOBFORGE_TEST_DSN = "postgres://jobforge:jobforge@localhost:5433/jobforge?sslmode=disable"
go test -run TestFaultAT02CrashBeforeACK ./tests/integration/ -v -count=1
```

预期包含 `real Worker kill`、fencing token 递增、`applied=1 deduplicated=1`。该 Demo 只证明同一 PostgreSQL 原子效果，不表示跨系统 exactly-once；人工 retry 克隆的新 job ID 也不会自动共享效果。

## 步骤 7：DLQ 与人工重试

提交一个必定失败的任务（3 次 attempt 后进入 dead）：

```sh
curl -s -X POST http://localhost:8080/v1/jobs \
  -H "Content-Type: application/json" \
  -H "X-API-Key: dev-api-key" \
  -d '{"queue":"default","type":"demo.fail","payload":{"error_code":"TRANSIENT_FAILURE"},"max_attempts":3}'
```

等待重试完成（退避 1s + 2s + 4s ≈ 7s），查询状态为 `dead`：

```sh
sleep 10
curl -s http://localhost:8080/v1/jobs/{job_id} \
  -H "X-API-Key: dev-api-key" | python -m json.tool
```

执行人工重试：

```sh
curl -s -X POST http://localhost:8080/v1/jobs/{job_id}:retry \
  -H "X-API-Key: dev-api-key" | python -m json.tool
```

预期：返回新的 job_id（克隆），原任务终态不变。

## 步骤 8：Benchmark 与 pprof

查看 Prometheus 指标：

```sh
curl -s http://localhost:6060/metrics | grep jobforge_
```

采集 CPU profile（需要服务运行中）：

```sh
go tool pprof http://localhost:6060/debug/pprof/profile?seconds=5
```

查看性能基线报告：

```sh
cat docs/benchmark.md
```

关键数据（v0.5 收官）：

- Submit: 307.94 jobs/sec（100 jobs / 4 workers）
- Process: 395.09 jobs/sec
- p50: 8.84ms / p95: 14.44ms / p99: 31.65ms
- Claim 五轮中位数：7.071ms/op，相对 v0.5 实施前改善 8.1%
- Gateway Register→Poll 五轮中位数：9.458ms/op，相对实施前 +13.3%，低于 15% 门禁；144 allocs/op 作为后续优化观察项
- 历史 W4 Claim 4.171ms/op 绝对门禁仍保留未通过披露

## 清理

```sh
docker compose -f deploy/compose.yaml down -v
```

## 相关文档

- [系统架构](architecture.md)
- [故障语义](failure-semantics.md)
- [性能基线](benchmark.md)
- [可观测性](observability.md)
