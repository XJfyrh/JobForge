# 通用任务扩展契约

当前范围由 [PRD v0.6](product/JobForge_PRD_v0.6.md) 定义，设计取舍见 [ADR-0011](adr/0011-general-task-results-and-model-adapters.md)。PostgreSQL 是任务事实源，业务包只实现已注册的能力；新增业务类型不改核心状态机或存储。

## 1. 输入与注册

类型名遵循 ADR-0010，API/Gateway `JOBFORGE_TASK_TYPES` 同值，Worker Registry 注册相应 Handler。两种真实类型是 `rag.index`、`agent.extract`；`demo.*` 保留为快速确定性测试能力。

业务 payload 由 Handler 严格校验，最少包含 `version` 和 `business_key`，其余为业务注册的资源 ID / 版本。队列内核不知道文档、模型、索引或抽取 Schema。业务 payload 不能选择 shell、代码、执行路径、网络 endpoint、模型工具或租户。

```go
registry.Register("example.task", worker.HandlerFunc(func(ctx context.Context, job *worker.ClaimedJob) (string, error) {
    // Validate a versioned input, call one preconfigured capability, and
    // publish using a durable business idempotency key before returning.
    return "artifact:opaque-reference", nil
}))
```

`Execute` 并发安全，必须尊重 context，所有网络调用有截止时间，关闭响应体、等待自建 goroutine。不得把后台工作留给下一 attempt 管理。Handler 能力只从部署配置注册。

## 2. 幂等、deadline 与错误

- HTTP `idempotency_key` 只在 tenant 范围去重提交；同键异参数为 CONFLICT。
- 业务 `business_key` 在 tenant + type 范围去重产物发布。输入内容和算法/模型版本指纹相同才可复用；同键异内容明确失败。同 job 重投、人工 retry 克隆共享键。真正的新效果使用新业务键或版本。
- `timeout_seconds`：0/省略使用 300，显式有效值 1..86400 秒；每 attempt 独立计时。业务 HTTP 超时取任务剩余时间与业务上限的较小值。
- 普通 Handler 错误不可重试；`worker.NewRetryableError` 表示临时依赖错误；context deadline 为 TIMEOUT、可重试；context cancelled 停止执行。Gateway 根据真实状态决定 retry_wait/dead/cancelled，Handler 不直接调度。
- 临时模型不可用可重试；非法输入、未知版本、输出超限、Schema 修正耗尽、业务键冲突不可重试。日志只记固定错误类别，不拼接文档、模型输出、凭据或请求体。

## 3. 产物引用

`result_ref` 是可选不透明 UTF-8 元数据。空字符串在数据库为 NULL、HTTP 为 null；非空最多 2048 字节，禁止 C0/DEL 控制字符。旧响应没有此字段时 SDK 得到 None。URI 仅为推荐格式，历史非 URI 引用仍接受；禁止文档正文、完整模型结果、访问令牌或签名 URL。

产物先发布到业务存储，Complete 再把引用和成功、attempt、quota/outbox 在同一事务提交。首次成功后重复同 lease Complete 仅 ACK，不替换引用/耗时或重复计数。不同 owner/token、已恢复的旧 attempt 返回 STALE_LEASE。Cancel 先赢则 Complete 返回 CANCEL_REQUESTED，结果不入 jobs。

引用的任务查询必须通过 tenant 鉴权；拥有引用不等同于拥有产物读取权限。SDK 不自动访问引用，业务存储须独立检查租户并限制结果大小。

## 4. 恢复边界

Go Runtime 是唯一 lease 持有者。业务 HTTP 服务/模型后端不领取 job、不续租、不决定任务状态。Worker 崩溃后 Scheduler 按现有租约语义重新投递；没有持久断点的工作从头执行。效果已发布、Complete 前崩溃时，下一次执行先查业务幂等键并复用结果。

取消关闭正在进行的 HTTP 请求并阻止后续处理/发布；不承诺撤回后端已开始计算或已经提交的外部效果。取消与业务发布之间仍有竞争窗口，业务幂等是重复执行的保护，不是跨系统原子撤销。人工 retry 可以取回已发布效果。

## 5. 验证分层

快速测试验证版本、输入边界、超时/取消传播、输出校验、持久幂等与核心故障恢复；固定替身只能证明这些协议。真实模型验收必须检查真实向量、检索命中、实际模型抽取字段、产物读取，以及两个任务的 OS Worker kill / 租约恢复。结果引用、日志/trace 和业务存储形成可核对的证据链。

## 6. 当前适配器输入与产物 API

实现位于 `internal/tasks`，固定文档和 Schema 位于其 `fixtures`。`rag.index` 输入：

```json
{"version":1,"business_key":"handbook-run-1","corpus_version":"handbook-v1"}
```

索引版本 `paragraph-runes480-overlap80-cosine-v1`：去标题、按段落解析，长段落每 480 Unicode 字符切分，重叠 80 字符；语料产生 6 个分块，每个向量 384 维。产物保存文本、来源、向量、模型 digest 与真实检索断言。

`agent.extract` 输入：

```json
{"version":1,"business_key":"order-run-1","document_version":"purchase-order-v1","schema_version":"purchase-order-v1"}
```

严格验证 Schema、必填字段、正数、USD、日期、来源引文以及订单号/供应商/日期的来源依据；最多修正一次。修正提示不拼接错误响应，不宣称对未知文档的准确率。

`jobforge artifacts` 提供 `GET /v1/artifacts/{hex_id}` 与 `POST /v1/artifacts/{hex_id}/search`。后者接收 `{"query":"...","k":1}`（query≤2048 字节、k=1..10、四路并发、60s 上限），对持久向量执行真实查询向量检索。两者按 Bearer key 鉴权，其他租户返回 404。引用格式 `jobforge-artifact:<hex_id>` 不含凭据。

稳定业务错误码见 `internal/tasks/contracts.go` 和适配器：输入/版本/Schema/检索验证/业务键冲突不可重试；`MODEL_UNAVAILABLE`、`ARTIFACT_UNAVAILABLE` 可重试。Handler 可实现 `ErrorCode() string` 返回固定机器码，错误文本必须脱敏。业务键、query、文档和产物不进入日志/Trace。
