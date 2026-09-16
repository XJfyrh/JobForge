# Agent v3：供应商报告与批次停止

[ADR-0020](adr/0020-provider-audit-and-batch-stop.md)定义持久供应商报告、跨 FD 确认和首批停止规则。实现复用原 PostgreSQL 调用账本、SettleUsage RPC、计量管道和 batch frozen；没有第二调度器。当前默认部署仍未启用收费 profile，确定性机制测试中的供应商响应不能作为 DeepSeek 推理或 40 案业务验收。

## 数据与权限

新 profile 固定 `executor_version=linux-v2-audit-runtime-1`、`provider_audit_policy=deepseek-audit-v1` 和 `expected_response_model=deepseek-flash`。可信登记器将这些字段纳入不可变 profile definition/hash；manifest、Go Worker 和 Python 固定安装包必须匹配。旧 Worker 不能登记新 session 或取得新许可；升级前原调用的 usage-only 晚到结算仍遵守原身份和 30 日窗口。

每次 chat 消费许可后，固定 adapter 在业务输出校验前冻结一份报告。它仅含安全的响应标识、model/fingerprint、完整响应摘要、精确计数和闭集状态；不保存输入输出正文、reasoning 内容、工具内容、错误正文、headers 或凭据。无完整响应不能生成完整摘要；没有保存任何报告的调用显示 `missing`，已保存的 unavailable 报告显示 `recorded`。缺失 reasoning 与显式零分别为 `absent/null` 和 `observed/0`。

Reserve 在原事务保存 execution binding hash。报告 hash 绑定原执行身份、physical call、参数、usage 和 audit；服务器由持久原行重算。首次报告与符合资格的结算、必要冻结同事务提交。相同 hash 重放不重复计费；不同 hash 只保存首次冲突 hash 并冻结批次，第二份即使超限也不能覆盖首次 usage 或产生第二次异常计费。首次完整计数超预留仍按既有三层 measurement anomaly 处理。

| 报告事实 | 已知费用与 hold | 后续执行 |
| --- | --- | --- |
| 兼容 model、完整 usage、非思考模式 | 按冻结价格结算；reasoning 不重复加到 output | 仍须普通观察确认、原期限和步骤提交 |
| 上述计量合法、业务 JSON/schema/来源错误 | 正常结算 | 只允许合同内的一次纠正 |
| model 不兼容但计数完整 | 只保存 observed usage；不按原价结算，保留完整 hold | 停批 |
| 身份/计数无效、模式违规或 chat 无完整响应 | 保存可捕获事实；不可结算部分保留 hold | 停批 |
| 已预留 chat 尚缺报告、观察或步骤结束事实 | 不凭进程退出退款 | 持久 guard 拒绝新增 Claim/工具/调用 |

免费业务请求沿用 unknown + 普通 ACK。非 chat 的 query embedding 沿用原未知计量/full hold 语义：实际向量和业务校验成功、普通 ACK 与期限有效时可以继续检索；不会伪造 usage/report，也不会触发 chat 的 unknown 停批。若 embedding 取得完整计量，则必须先得到同份 usage-only report 的 settled 确认。

## 确认与恢复

计量 ACK 关联 `report_hash`，取值为 `settled/recorded/anomaly/conflict/unconfirmed`。只有 settled 且原报告一致、无停止事实时才可能继续；recorded 只证明保存，不是定价或执行许可。普通 `observation.v2` hash 另外绑定 audit hash。普通 FD 与计量 FD 可以反序到达，Go 仍在持久 SettleUsage 和 ObserveCall 成功后才发送普通 ACK。

批次 guard 在新 Claim、BeginTool、Reserve 前持批次账户锁，读取已有 chat 的原报告、观察和步骤结束事实。新 session 或进程重启不能绕过未完成的收费窗口。原步骤 Commit（含首次纠正标记）可解除待确认屏障；第二次纠正的明确终态 MODEL_PROTOCOL_ERROR 只有原 attempt/session/fence/step/输入/报告/观察全匹配才可解除。超时、取消、尺寸失败、失权不属于该例外。

因此，chat 已发送但未完成持久屏障后崩溃，本首批可能保守停止；不会重新收费来探测结果。非收费阶段仍按既有 checkpoint 恢复。管道、RPC 或 ACK 丢失仅表示本地未确认，不能声称数据库未提交、已冻结或已退款。已捕获报告只能使用原有有限收尾路径；晚到报告不改变 Run 终态或重新打开普通执行。供应商已接收的请求不能靠本地取消撤销。

## 只读查询

`GET /v2/runs/{run_id}/calls` 返回单次 repeatable-read 快照，最多 44 项且完整响应不超过 256 KiB，无分页或查询参数。reader/operator 都受原租户边界限制，跨租户与不存在均为 404。共享 batch 只返回冻结状态与固定原因，不暴露触发者的 Run、call 或 tenant。

```python
from jobforge import RunClient

with RunClient("http://localhost:8093", api_key=reader_key) as client:
    evidence = client.calls(run_id)
    for call in evidence.items:
        print(call.physical_call_id, call.audit_status, call.usage_known)
        print(call.known_cost_microyuan, call.held_cost_microyuan)
```

`observed_usage` 是报告中的结构化观测；只有 `settled_usage` 与 `usage_known` 表示按原价确认。`legacy_not_collected/not_applicable/missing/recorded` 区分旧行、非 chat、缺报告和已记录。SDK 不重试，也不把 null 转换为零。公开源合同见 [OpenAPI](../api/run/v2/openapi.yaml)，共同读取向量由 [Go HTTP 测试](../internal/run/httpapi/calls_test.go)生成。

## 验证与运维边界

Go/Python 共同 audit/report/observation 向量、真实 PG 首份/重放/冲突/冻结/晚到测试、安装 SDK 的真实 HTTP 查询，以及固定 Linux 进程/FD/PG/gRPC 故障分别验证不同层。Windows 必须先按 [开发指南](development.md)启动测试 PG 并设置 DSN；同一数据库不能并行运行会清理数据的测试进程。容器命令见 [运行时指南](agent-v3-runtime.md)。跳过不能记为通过，合成响应不能代替真实 DeepSeek。

报告至少覆盖原 30 日晚到窗口；没有自动清理、归档服务或生产长期留存验收。日志/Trace 不承载完整报告正文或模型输入输出。首批启动器、完整评分器和实际 40 案仍需分别验收。历史 W4 性能失败、AT-25 跳过、远程推理与生产留存未验收继续保留。
