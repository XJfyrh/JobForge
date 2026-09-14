# JobForge PRD v0.6：通用 Agent/RAG 真实执行闭环

- 日期：2026-09-14
- 状态：实现及本地/Linux CI 验收完成（见[实施记录](../agent-rag-progress.md)）；[PR #33](https://github.com/XJfyrh/JobForge/pull/33) 待评审，ADR-0011/0012 尚未接受
- 上游：本轮维护者明确授权的三个阶段；v0.1～v0.5 可靠性不变量继续有效

## 1. 范围与优先级

按接入契约、真实业务、可观测性的顺序交付。原有 PageWise 集成要求由 `rag.index` 与 `agent.extract` 两个独立真实任务验收替代。直接删除 PageWise Handler、注册、配置、示例及专属测试；无兼容别名、弃用期、旧任务迁移或存量处理。旧 PRD/ADR 作为历史保留，当前入口指向本增量。

PostgreSQL 仍是任务唯一事实源；at-least-once、原子 Claim、owner/token fencing、终态不可变、租户隔离不变。业务产物位于业务存储，核心只存有界引用。无工作流引擎、任意代码执行、动态工具选择或第二套调度。

## 2. 需求—验收映射

| 阶段 / ID | 需求与判定 | 自动化证据 / 交付 |
|---|---|---|
| M1 / AT-32 | SDK 正确解析嵌套错误；未知/畸形响应安全归类；安装可用 | Python 单测、干净 venv 安装、CI |
| M1 / AT-33 | 真实 HTTP + SDK submit/get/cancel/retry、attempt、提交幂等、租户隔离 | Go 启动真实 HTTP/Gateway/Worker + Python 契约程序，真实 PostgreSQL |
| M1 / AT-34 | result_ref 与成功同事务；首次结果不可覆盖；取消与旧 lease 拒绝 | 0020 up/down、并发/回滚/重复 Complete、HTTP/SDK 查询 |
| M1 / AT-35 | 预注册 Handler 的版本化输入、业务键、deadline、错误分类及引用契约 | ADR-0011、任务扩展指南；新增类型不改队列内核 |
| M2 / AT-36 | rag.index 读取固定语料、解析分块、真实 embedding、持久索引、检索命中 | 固定模型 digest、语料/索引版本、实际产物与预定义查询断言 |
| M2 / AT-37 | agent.extract 真实模型抽取、Schema/来源校验、最多一次修正、持久结构化结果 | 固定文档/Schema/模型、字段值断言、SDK result_ref |
| M2 / AT-38 | 两任务超时/取消/可重试错误/人工重试/真实 Worker kill | 快速故障测试和真实模型验收分层；进程 Kill/Wait、实际租约恢复 |
| M2 / AT-39 | 发布业务效果后、Complete 前 kill；重投与人工克隆不重复发布 | 业务唯一键/内容指纹、已发布产物复用、首次发布计数恰一 |
| M3 / AT-40 | 指标按实际状态转换计数，标签完整；重复/旧写/取消不误计 | Complete/Fail/重试耗尽/lease recovery 指标测试 |
| M3 / AT-41 | SDK→API→Gateway→Worker→业务→上报 TraceContext | OTLP + Collector + Trace 后端查询，两任务成功/失败/重试/恢复 |
| M3 / AT-42 | 仪表盘、告警和观测故障闭环 | Prometheus/Grafana API 验证，故障注入、观测离线任务仍完成 |

## 3. 接入与结果约束

- HTTP 保持 ADR-0002 的 `{"error":{"code":"...","message":"..."}}`；SDK 稳定异常类型，不自动重试业务提交。`INVALID_TRANSITION` 补齐为 HTTP 409。
- `result_ref` 为可选、不透明 UTF-8 文本，空串转换 SQL NULL / JSON null；非空最多 2048 字节，禁止 C0/DEL 控制字符。兼容现有 `effect:...`、`slept:...` 等引用；新业务用不携带凭据的 URI。禁止放置文档、模型内容或签名访问密钥。
- 查询沿用 tenant 过滤。引用本身不授予访问权限，业务存储独立鉴权；SDK 不自动请求引用 URL。
- Complete 的首次有效事务保存引用，重复同 lease RPC 只回 ACK；旧 token/owner 不得因“当前已终态”变为成功。
- 业务 payload 只含明确版本和预注册资源 ID / 业务键，不接受 shell、代码、文件路径、URL 或工具名作为执行指令。
- 每 attempt deadline 由 Go Runtime 管理。业务 HTTP 调用使用该 context 的更短超时；取消停止后续工作和发布，不能保证撤销模型后端已经开始或完成的计算。

## 4. 两个真实业务任务

首选无需付费的 Ollama 本地 HTTP 后端，Go Handler 直接调用；因此没有 Python Worker、Python 进程或额外租约持有者。Python 仅为控制面 SDK。部署配置可指向受信远程 Ollama 兼容接口，payload 不决定后端。

`rag.index`：固定 UTF-8 Markdown 小语料，确定性解析/分块，all-minilm:22m 真实向量；小规模向量矩阵持久化并以余弦相似度检索，明确版本和维数。`agent.extract`：固定合成采购文档，qwen2.5:0.5b 真实 Schema 约束抽取，输出结构与来源校验，最多两次模型调用；不满足契约返回明确失败。

业务产物按 tenant、任务类型、业务幂等键原子发布。输入内容及算法/模型版本参与指纹；同键异内容明确拒绝。同 job 重投与人工 retry 克隆保持同业务键，共享已经发布的效果；有意生成新效果必须使用新键或版本。模型计算可能重复，承诺仅限发布去重。未发布即从头重新执行，未实现断点续作。

限制：语料/输入≤64 KiB、分块≤64、单模型请求≤60s（且受任务 deadline 限制）、抽取输出≤16 KiB、完整业务产物≤2 MiB；rag.index 最多两次批量 embedding（文档与验收查询），agent.extract 最多两次 chat（初次+修正）。真实模型测试单列，mock 不能满足 AT-36/37。

## 5. 可观测性

OTLP 为可选配置；默认 stdout/none 体验保留。可选 Compose obs profile 提供 Collector、Jaeger、现有 Prometheus/Grafana。span 与日志只放 job/type/tenant/attempt、版本和固定错误分类，不记录完整输入输出或凭据。

Attempt outcome 使用 succeeded / failed_retry / failed_dead / cancelled / lease_expired。重试只计实际 retry_wait，DLQ 只计实际 dead；耗时口径和重启后计数局限在观测文档说明。过期租约、积压、Worker 存活有图表和告警。观测后端宕机不能改变任务状态。

## 6. 完成门禁

完整快速确定性检查、真实 PostgreSQL/race、Python 契约、真实模型、真实进程恢复、观测后端验证均实际运行，报告通过/失败/跳过/无法运行。修改热路径前后同环境定向性能比较，继续披露历史 W4 Claim 绝对门禁。交付可复现 Compose、SDK 安装、启动/清理命令、架构/故障/扩展指南、三分钟演示及产物检查方法。任何未完成项保留在[实施记录](../agent-rag-progress.md)，不标记整体完成。
