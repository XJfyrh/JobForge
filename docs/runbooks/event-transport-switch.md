# 外部事件 transport 受控切换运行手册

本手册用于将 Outbox Publisher 从 `notify` 切换到 `redis_streams`，或执行反向回退。切换不是双写或零停机流程；PostgreSQL `outbox_events` 是事实源，但单一 `published_at` 只能表示“已交付给切换时启用的 transport”，无法证明历史事件已进入新的 transport。

## 不变量与责任人决策

- 任务处理继续保持 at-least-once；不得把切换描述为 exactly-once。
- 切换不改写 jobs、八状态状态机、lease/fencing 或错误码。
- Redis 只能接收 PostgreSQL outbox 发布的数据，不能成为任务状态事实源。
- 变更负责人必须在操作单中书面选择历史边界：
  - **截断**：新 transport 仅承诺高水位之后的事件；或
  - **受控回灌**：按批准的 event_id 范围重置/重放，接受重复投递。
- 没有该书面决定时停止切换。禁止凭单一 `published_at` 猜测历史事件已耐久交付。

## 切换前检查

1. 确认 migration 0016、0017 已应用；记录当前应用版本与数据库备份/恢复点。
2. 确认 Redis 7 使用 AOF `appendonly yes`、`appendfsync everysec` 与持久 volume；执行 `PING`、`INFO persistence`，检查磁盘容量。
3. 确认固定 stream、consumer group、poison stream 名称；poison 与源 stream 必须不同。查询 `consumer_inbox_binding`，确认该 reference inbox 只绑定当前逻辑 group。
4. 确认 Consumer 配置满足 `PENDING_MIN_IDLE > PROCESS_TIMEOUT`，并已验证 `/metrics` 与 trace 输出不包含凭据/payload。
5. 记录以下基线：outbox 未发布数、最大 event_id、当前 publisher 实例、Redis stream 长度、group lag/PEL、poison 长度及 `JOBFORGE_REDIS_STREAM_MAXLEN`。默认 0 不裁剪；非零时必须有基于 outbox 高水位的人工恢复方案。
6. 暂停 retention/cleaner 与所有 Publisher。保持 API、Gateway、Scheduler、Worker 运行不会调用 broker，也不会影响任务状态事务。

建议核对 SQL：

```sql
SELECT count(*) AS unpublished, max(event_id) AS high_watermark
FROM outbox_events
WHERE published_at IS NULL;

SELECT max(event_id) AS all_events_high_watermark
FROM outbox_events;
```

## notify → redis_streams

1. 停止全部 Publisher，确认没有实例继续更新 `published_at`。
2. 记录切换高水位与时间，完成“截断或回灌”审批：
   - 截断：保留历史 `published_at`，明确耐久承诺从高水位之后开始。
   - 回灌：在维护窗口内仅对批准范围重置发布标记；该操作会重复发布，必须确认所有消费者按 `event_id` 幂等。JobForge 不提供自动回灌命令。
3. 启动 Redis，验证 AOF 与 volume；先启动 `jobforge consumer`。新 group 从 `0-0` 创建，已有 group 不得删除/重建。
4. 以 `JOBFORGE_OUTBOX_TRANSPORT=redis_streams` 启动**一个** Publisher；确认启动日志标记 transport durable，且没有 Redis URL/凭据。
5. 观察至少一个完整业务事件：outbox 从 NULL 变为已发布、Stream 出现 envelope、Consumer inbox/effect 提交、group lag 与 PEL 收敛。
6. 逐步恢复其他 Publisher 实例；确认 outbox backlog 与 publish lag 收敛、transport failures 不持续增长。
7. 恢复 retention。归档高水位、决策、配置、指标截图/查询与验证结果。

Compose 演示环境：

```powershell
$env:JOBFORGE_OUTBOX_TRANSPORT = "redis_streams"
docker compose -f deploy/compose.yaml --profile durable-events up -d --build
docker compose -f deploy/compose.yaml port --index 1 consumer 6060
```

## 验证门槛

- Task readiness 与任务状态事务在 Redis 故障时保持可用；Redis 不得写回 jobs。
- `jobforge_outbox_pending` 最终归零；`jobforge_event_publish_lag_seconds` 回到健康范围。
- Consumer group lag 与 PEL 最终归零；commit 后 ACK 前退出能由 `XAUTOCLAIM` 恢复。
- `jobforge_event_redeliveries_total` 与 `jobforge_consumer_inbox_duplicates_total` 在故障注入后增加，demo effects 对同一 event_id 仍只有一行。
- malformed/unsupported envelope 达上限进入 poison，后续合法事件继续处理；poison 不含 payload。
- 将一个 pending payload 以 XDEL/测试裁剪删除时，Consumer 必须以 `pending_payload_deleted` 退出且 transport failure 增加，不得把 PEL 清空解释为业务成功。
- AT-17/18/19/20 与 NFR-303 集成测试通过。缺少 PostgreSQL/Redis 时必须报告 skip/无法运行，不能当作通过。

## 回退到 notify

回退同样是非零停机受控变更：停止 Publisher/retention，记录 Redis 高水位、stream/group lag/PEL 与未发布 outbox，书面决定尚未消费事件的处置，然后以 `notify` 启动一个 Publisher 验证。不要删除 Redis stream、PEL、poison 或 inbox；它们是审计和后续恢复证据。notify 非耐久，回退后不得宣称 broker 重启可恢复。

## 事故处理

- Redis/网络故障：保持 Publisher/Consumer 退避；不要将失败事件标记成功，不要降级双写 notify。
- 数据库故障：Consumer 不 ACK，事务回滚；恢复后由 PEL 重投。
- poison 增长：按 `source_stream + source_entry_id` 聚合，检查 reason 与对应部署版本；不要直接复制/打印原 payload，不自动重放。
- PEL 长期不降：确认 Consumer 存活、group/name 配置一致、min-idle 合理；禁止用删除 group 清空故障证据。
- `pending_payload_deleted`：立即停止“积压已收敛”声明，保留 Stream/AOF/outbox/inbox 证据，按切换高水位书面决定回灌或截断；本版本不自动回灌。
