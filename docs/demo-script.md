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

提交一个 PageWise 索引重建任务（Agent 真实负载演示）：

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

## 步骤 3：Kill Worker 演示崩溃恢复

提交一个长时间运行的任务：

```sh
curl -s -X POST http://localhost:8080/v1/jobs \
  -H "Content-Type: application/json" \
  -H "X-API-Key: dev-api-key" \
  -d '{"queue":"default","type":"demo.sleep","payload":{"duration_ms":30000},"timeout_seconds":60}'
```

在任务运行中 kill worker-1：

```sh
docker compose -f deploy/compose.yaml kill worker-1
```

## 步骤 4：观察 Lease 过期与重新领取

等待 lease 过期（默认 30s）+ Scheduler 扫描（1s）：

```sh
# 等待约 35 秒后查询任务状态
sleep 35
curl -s http://localhost:8080/v1/jobs/{job_id} \
  -H "X-API-Key: dev-api-key" | python -m json.tool
```

预期：任务被 worker-2 重新领取（`lease_owner` 变为 worker-2，`attempt` 递增）。

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

## 步骤 6：幂等性演示

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

通过故障测试验证执行幂等（崩溃重投后副作用仅一次）：

```sh
go test -run TestFaultAT02CrashBeforeACK ./tests/integration/ -v -count=1
```

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

关键数据：
- Enqueue: ~364 jobs/sec
- Claim: ~429 jobs/sec
- p50: 8.4ms / p95: 12.9ms / p99: 25.5ms
- 热点：pgx 二进制协议解码（Claim 路径 ~45% CPU）

## 清理

```sh
docker compose -f deploy/compose.yaml down -v
```

## 相关文档

- [系统架构](architecture.md)
- [故障语义](failure-semantics.md)
- [性能基线](benchmark.md)
- [可观测性](observability.md)
