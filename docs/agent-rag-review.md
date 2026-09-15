# Agent/RAG 合并审查与验收边界（2026-09-15）

对应 [PR #33](https://github.com/XJfyrh/JobForge/pull/33)、[PRD v0.6](product/JobForge_PRD_v0.6.md) 和[实施记录](agent-rag-progress.md)。审查从 `8006033` 开始，基线为 `02c61e4`。本轮补修已推送，但 Windows 完整复验未通过，暂不按“无问题”自动合并；ADR-0011/0012 保留 Proposed。合并判定针对 v0.6 已约定范围，不代表历史门禁、未实现 P1 或生产部署已全部验收。

## 审查范围与修正

| 方面 | 核对内容与结论 |
|---|---|
| 接入契约 | 核对 HTTP 错误 envelope、SDK 异常类型、鉴权/租户、ID、attempt 时间线与 result_ref。发现部分畸形 HTTP 200 字段被直接返回，已补修。 |
| 状态机与事务 | 核对原子 Claim、owner/token fencing、终态不可变、Complete 结果同事务、失败/取消/恢复与重复 RPC；首次结果不会被重复上报覆盖。 |
| 业务可靠性 | 核对预注册类型、固定资源/模型、调用次数/时间/输出上限、真实向量与抽取 Schema、tenant/type/business key 唯一发布；恢复是重新执行或复用已发布产物。 |
| 安全边界 | payload 不能选择 shell、代码、路径、远程 URL 或工具；业务产物独立鉴权。模型响应及错误不直接写入日志/Trace；模型调用取消有明确边界。 |
| 可观测性 | 指标只按已提交的状态转换计数，queue/type 完整；OTLP 有界异步导出，后端故障不回滚任务。核对 Trace 关联、告警测试、仪表盘生成及真实故障证据。 |
| 迁移与兼容 | 仅追加 0020～0022；Proto 从源文件生成且为兼容新增；SDK 兼容可选字段缺失。0022 回退可能被新增 C1 引用阻止，已披露，不自动改写数据。 |
| 工程与证据 | 区分快速替身、真实 PostgreSQL、真实模型、OS kill、Jaeger 实际查询。CI 的失败管道传播已核对，不仅依赖绿色图标。 |

本次确定的问题：SDK 曾接受对象类型的 `result_ref`、字符串/布尔类型的整数、无效时间、非列表的 attempts、字符串类型的 `deduplicated` 和未知提交状态，调用方会拿到不符合声明类型的结果。新增 11 个回归用例先实际失败，再补字段校验；所有这些响应现在归为 `InternalError`。另两个用例证实解析异常的堆栈会包含被拒绝字段的原文，现对公开 SDK 解析异常抑制该异常链。共 13 个畸形字段/堆栈回归用例，加上可选字段、未知扩展字段和有效时间线的兼容验证；成功 HTTP mock 使用服务端接受的 UUID。修复没有新增依赖或改变队列状态机。

## 本次复验结果与合并阻塞

| 检查 | 结果与证据 |
|---|---|
| Python | 普通 wheel 重装后 51 项通过；ruff check/format、mypy 通过。 |
| Go 静态 / SQL / Proto / 观测配置 | build/vet/golangci-lint、SQLFluff 历史基线与 migrations、Buf lint/format/breaking、promtool config/rules、仪表盘生成一致性通过。 |
| `f4510ec` Linux 工程 CI | [run 34960791572](https://github.com/XJfyrh/JobForge/actions/runs/34960791572) 五项通过，真实 PostgreSQL/Redis/Python 的全量 race 集成 126.082s。 |
| `f4510ec` Linux 真实模型 CI | [run 34960791321](https://github.com/XJfyrh/JobForge/actions/runs/34960791321) 实际 PASS：集成 71.294s，12 生命周期场景 64.95s，SDK 5.23s，14 条 Jaeger 后端断言。 |
| Windows 全量首轮 | **失败**，集成 348.802s：AT-22 sustained fairness p95=6.9482107s / max=7.1726371s，门槛分别为 1s / 2s；未报告 data race。 |
| AT-22 定向对照 | 当前版本两轮 p95=40.8491ms / 252.2947ms；实施前 `02c61e4` 两轮为 30.6174ms / 56.7844ms，均通过。 |
| Windows 全量第二轮 | **失败**，集成 332.413s：AT-22 通过（p95=40.6962ms），AT-24 15s 内 0/20 Handler 启动。两个真实任务、12 场景、SDK 与 14 条 Jaeger 断言通过。 |
| AT-24 增加诊断后定向三轮 | 两轮通过、一轮失败：失败轮总耗时 5.20s，但 DB-clock signal p95=7.61869s；同轮 API 返回→context 取消 p95=4.5708206s。不计整组通过。 |

随后进行 40s 只读 `clock_timestamp()` 采样，证实 PostgreSQL 时间相对 Windows 单调时钟不稳定：约 200ms 的相邻采样出现额外 **1524.740ms** 的跳变，该次 SQL 往返仅 **0.537ms**，起始 DB/主机偏差约 -2.307s。[原始相邻样本](evidence/review-clock-2026-09-15.json)纳入仓库；完整 CSV、探针和测试日志在仓库外 `E:\JobForge-notes\2026-09-15-review`。

这可以确认本机时间测量环境不满足稳定时钟前提，不能把该环境的 SLO 失败直接判成确定的代码性能回归；也不能据此把所有失败都归因于时钟。AT-24 首次未启动的直接原因没有完整 RPC 日志，AT-22 首轮波动也未确证根因。因此增加 Worker 启动日志及提前退出诊断，**不改 15s 启动等待、250ms Poll RPC、6s signal SLO 或 AT-22 阈值**，不把失败改成 skip。

本轮暂不合并：先在时钟稳定的 Windows 测试环境复验并解释启动失败，或由维护者明确决定以已通过的独立 Linux 门禁作为合并依据。未擅自重启或校时共享 Docker/WSL 环境，以免影响其他项目。历史通过、本轮失败、定向通过和 Linux 通过分别保留。

## 远程模型：配置能力存在，真实后端未验收

已验收的是本地 CPU Ollama 0.32.5、固定 digest 的 `all-minilm:22m` 与 `qwen2.5:0.5b`，以及固定小语料和合成订单。真实索引、查询命中、抽取字段、超时/取消/重试/进程恢复都在这个范围内成立。

`JOBFORGE_OLLAMA_URL` 可配置可信远程 origin；原生进程支持 `JOBFORGE_OLLAMA_API_KEY`。适配器要求同样的 `/api/tags`、`/api/embed`、`/api/chat` 与确切模型 digest。它不是已验收的任意供应商、通用 OpenAI API 或 Ollama Cloud 接口；Compose 也没有自动注入远程模型密钥。

本轮没有实际远程服务、凭据和供应商配额，因而没有验证真实鉴权/TLS/代理链、远程限流及计费、模型版本供应、网络中断与取消行为。单元测试中的 401/429/5xx 等响应分类、故障代理及本地服务通过，不能替代远程验收。远程验收需指定端点与固定模型，用真实凭据跑两个 SDK→产物闭环及真实错误/预算/取消场景，并检查输出与敏感信息边界。

HTTP context 取消会停止等待和后续处理；已发送的模型请求可能继续计算或计费。租约恢复可能重新计算，持久业务幂等键只防止重复发布，不能保证模型只调用一次。也未验证未知文档准确率或跨硬件输出一致性。

## 生产留存：开发观测栈不提供留存承诺

需要区分三层数据：

| 数据 | 当前方式 | 未验收内容 |
|---|---|---|
| 任务、attempt、业务产物 | PostgreSQL；Compose 使用命名卷；真实 Worker 崩溃恢复已验证 | 生产备份/PITR、跨机灾备、HA、RPO/RTO、长期容量及产物清理策略 |
| Trace | Jaeger 开发内存存储；应用及 Collector 缓冲均非持久队列 | 重启后历史查询、长期存储、7/30/90 天留存、满盘/后端长时间故障恢复 |
| 指标与仪表盘 | Prometheus/Grafana 开发实例；规则与仪表盘配置纳入版本控制 | 显式生产数据卷、时序数据留存/备份、Grafana 运行数据恢复、容量与访问控制 |

“停止 Collector 后任务仍成功”验证的是执行可靠性与观测解耦，不是遥测最终必达。队列满、进程 kill 或观测后端故障可以丢 Span；事务提交后立即崩溃可以漏指标，计数器重启会清零。Trace/指标不能替代 PostgreSQL 审计。已有 published outbox 的 168h 清理窗口也不等于 Trace 或业务产物的 TTL。

生产观测留存需先确定保留周期、访问/删除策略及容量，再配置持久后端、卷或远程写入、备份和恢复；实际验证服务重启、长时间断连、磁盘压力与历史查询。当前告警只有本地规则状态，未配置外发通知接收端；示例阈值不能当作生产 SLO。

## 历史 W4：Claim 绝对比较仍为失败

[历史基准](benchmark.md) 中 W4 冻结的 `BenchmarkClaim` 为 **4.171091 ms/op**。M4 在重启 PostgreSQL、重建 schema 后测得 **5.900438 ms/op**，耗时增加 **41.5%**；按吞吐倒数计算约从 239.7 降至 169.5 ops/s，即下降 **29.3%**，超过 15% 阈值。后续 v0.4/v0.5 文档继续明确保留这笔债务。失败指向这个 Claim 微基准比较，不应扩大成所有 W4/W11 功能和端到端检查失败。

v0.6 用同一真实领取断言和同环境，对实施前 `02c61e4` 与本轮各跑 100 次 × 3 轮：Claim 中位数 **3.979 → 4.143 ms/op（+4.1%）**；Complete/Fail 和 Worker 定向对比亦有原始证据。这回答“本轮是否出现定向回归”，不能证明历史 W4 门禁已关闭。原 Claim 基准曾可能把空领取算作成功样本，本轮已修正；因此不能把新版测试的单个结果直接覆盖历史报告。

关闭该项需要单独性能调查：在可比环境运行历史/当前版本及一致的实际领取基准，固定数据量、schema、容器/Go 版本和资源，重复采样并定位差异。若历史基线不可比，需通过显式决策重建基线，同时保留原始失败；不能以“环境噪声”未经证实地归因。普通 PR CI 并不执行这个历史性能门禁，CI 全绿不表示它通过。

## AT-25：明确未实现，因此继续跳过

`TestCancelAT25ControlStreamDegradation` 当前仍是无条件 `t.Skip` 的骨架，对应 [ADR-0008](adr/0008-cancel-control-channel-heartbeat.md) 中可裁剪的 P1/M5 ControlStream。不是缺少测试环境，也不是已经通过。

已实现并验收的是 AT-24 的 P0 Heartbeat 取消信号：默认每 5s 心跳，数据库时钟口径 signal p95 ≤6s；Worker context 停止另行度量，失联时有 lease 到期恢复。两个真实任务的取消都走这条路径。

AT-25 未来需实现 Gateway→Worker 控制流、会话和多 Gateway 路由，并验证健康时 signal p95 ≤1s、断流重连及 Heartbeat 降级、通知丢失/错误 Gateway、旧 session/token 不误取消、容量占满和并发退出。当前不能承诺主动推送的 ≤1s 取消，也不能承诺撤销外部模型计算。本轮不扩大到这项 P1；跳过状态及完成声明的排除范围继续保留。
