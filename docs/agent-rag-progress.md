# Agent/RAG 增量实施与验收记录

基线：2026-09-14，`02c61e4`（main 与 origin/main 一致），工作区初始干净。
工作分支：`XJfyrh/feat-agent-rag`。范围与门禁见 [PRD v0.6](product/JobForge_PRD_v0.6.md)。

| 阶段 | 状态 | 完成证据 | 剩余 |
|---|---|---|---|
| M0 调查/文档/基线 | 完成 | 初始无未提交改动；规则/文档/实现已核对；同环境 Claim/Complete/Fail 基线已保存 | 无 |
| M1 接入契约 | 已实现并验证 | SDK 34 项测试；独立环境安装；真实 HTTP/Python/Gateway/Worker 联调；AT-34 PostgreSQL 结果事务/竞争/迁移测试；初轮及最终全量 race 通过 | 无 |
| M2 真实任务 | 已实现并验证 | 两个真实模型、SDK、产物、12 类生命周期；干净 Compose / SDK 安装；发布前后真实 kill；已移除专属 Handler/注册/当前示例 | 无实现缺口 |
| M3 观测与运维 | 已实现并验证 | Jaeger 实际链路查询；Compose 停 Collector、模型和 Worker；指标/告警触发恢复；Grafana 11 面板实际检查 | 无实现缺口 |
| M4 全量验证/交付 | 实现与验收完成 | 本地全量 race（含 Compose CPU 模型/Jaeger，集成 211.350s）；Linux 工程 CI 和真实模型验收通过；三分钟脚本已实跑 | [PR #33](https://github.com/XJfyrh/JobForge/pull/33) 待评审合并，ADR 保留 Proposed |

所有模型替身只计快速测试；真实模型结果、真实进程 kill 和观测查询另列。历史 W4 Claim 绝对门禁未通过的既有披露继续有效，不以本轮相对性能比较覆盖。

## M1 验证（Windows / PostgreSQL 16，2026-09-14）

- 通过：`go build ./...`、`go vet ./...`、golangci-lint、`go test -race ./...`（设置测试 DSN、Redis URL、Python 解释器）；pytest（34）、ruff check/format、mypy；SQLFluff 历史基线、migration lint；buf lint。
- 首次全量 race 中 AT-24 的取消信号父 Span 断言仍指向提交 Span；传播改为 Worker 子 Span 后已同步断言，并实际重跑全量通过。取消 p95 约 4.60 s，未出现 data race。
- 完成路径同环境每轮 100 次、3 轮中位数：Complete 4.625 → 4.053 ms（约 -12.4%），Fail 5.476 → 3.866 ms（约 -29.4%）。Complete 分配增加（50 → 68 alloc/op），换取持久化结果、精确重复确认与事务内计数元数据；不代表生产负载吞吐。
- Claim 原基准会把空领取计入样本；已用数据库过去一天的时间、新队列和每次真实领取断言修正。把同一基准复制到 `02c61e4` 隔离工作树对比，100 次 × 3 轮中位数 3.979 → 4.143 ms（约 +4.1%，91 → 93 alloc/op），新增结果字段增加扫描分配。早期无真实领取断言的数据作废；本次并非历史 W4 绝对性能门禁复验。
- 原始日志与技术决策档案位于仓库外 `E:\JobForge-notes\2026-09-14-agent-rag`。M1 当时不包含真实业务/观测验收，后续证据分列如下。

## M2 实际模型与故障证据

- `TestRealTasksSDK`：真实本机 Ollama、HTTP、Python SDK、独立 Worker 进程，rag.index 6×384 维持久向量 / 3 查询命中，agent.extract 1 次模型调用、Schema 与预定义字段值正确；双方产物引用和跨租户 404 通过。该次测试耗时 5.8s。
- `TestRealTasksLifecycle`：两个类型分别通过暂时错误自动重试、TIMEOUT→dead、取消后人工重试、发布前 kill、发布后/Complete 前 kill、发布后取消再人工重试。真实 OS Kill/Wait，等待 DB 自然租约过期，attempt=2、token 增长、旧 token STALE_LEASE；首次产物引用/内容/时间不变，已发布效果复用不再调用模型。
- 故障代理只注入一次 503 或持有真实模型响应，不返回模拟业务数据。单元测试中的替身另列为快速层。`TestTaskArtifactsConcurrentPublicationAndIsolation` 验证 16 并发发布只有一次生效、DB 约束和鉴权先于模型调用。
- 原始结果：`m2-real-sdk.txt`、`m2-real-lifecycle.txt`；随后干净 Compose CPU 模型与独立 venv 安装实际通过（`m2-clean-compose-sdk.txt`），全量 race 通过（`m2-race-final.txt`，集成 183.866s）。远程模型选项未执行，不宣称其他供应商兼容。

## M3 最终观测与工程验证

- `m3-real-jaeger.txt`：真实模型两类型 × 六场景，以及 SDK 完整成功链路，实际查询 Jaeger 的已保存 Span；成功链路 parent 图完整，故障链路包含 Fail/Retry/Cancel/Recover/Complete。整体集成 80.990s，12 场景 72.51s。强杀进程允许丢失尚未导出的 Span，后继 attempt/恢复可查。
- `m3-compose-faults.txt`：真实停止 Collector 后两任务仍成功且结果不变；停止两 Worker 后 ServiceDown 与 BacklogWithoutWorkers firing，恢复后清除；两类型分别停止模型触发自动 retry、暂停模型触发 TIMEOUT→dead→人工 retry、SIGKILL Worker 后自然 lease recovery。最终每种类型的进程计数为 succeeded=5、failed_retry=1、failed_dead=1、lease_expired=1；DLQ/LeaseRecovery 告警 firing。可审查原始 JSON 摘录见[Compose 验收证据](evidence/agent-rag-compose-2026-09-14.jsonl)。
- Grafana 实际浏览器检查：积压归零、live workers=2、attempts=16、dead=2，两个类型的结果/时长/重试/租约曲线可见。发现 Docker Desktop 未传播文件 watch 事件，按 Grafana 官方机制改为 20s polling 并重新加载验证；非静态截图或 mock 数据。
- `m3-race-final.txt`：`go test -race -count=1 ./...` 全量通过，集成 211.350s。指定独立 PostgreSQL、Redis AOF 容器、安装好的 Python、Compose CPU Ollama（11435）、OTLP 与 Jaeger 地址。自动化覆盖 AT-32～42；故障回收测试直接使用生产存储事务，Compose 演练同时验证真实 Scheduler 进程。
- 通过：Go build/vet/golangci-lint（0 issues）、ruff check/format、mypy（7 个源码文件）、pytest（34）、SQLFluff 历史基线（3 条）与全部 migration lint、Buf lint/format/breaking、Prometheus config/rule tests、仪表盘确定生成检查。SQL migration 0020/0022 up/down/up 与 UTF-8/C0/DEL/C1 边界均使用真实 PostgreSQL。
- 最终补修：重复 Cancel 在 cancelling 中只 ACK，状态版本/取消时间不变；畸形 HTTP job_id 返回 INVALID_ARGUMENT；负亚毫秒 Complete duration 被拒绝；Handler 超时/取消后返回 nil 不能写成功。
- Worker 热路径同环境 100 jobs × 3 轮（固定 1ms Handler、实际 Gateway/PG、每任务一成功 attempt）：capacity=1 中位 78.59→80.55 jobs/s；capacity=4 为 181.0→244.6 jobs/s，基线样本 127.8～209.7 波动较大，不解释为稳定提速，只报告未见定向回归。原始 `m3-worker-baseline.txt` / `m3-worker-current.txt`。历史 W4 Claim 绝对门禁未重跑，原有失败披露继续保留。

## M4 远端验收与交付

- 首轮 Linux CI：普通工程检查全部通过（run `34841059677`）；真实模型 run `34841059870` 的 12 场景中一个 Trace 隐私断言失败，不能计作通过。测试密钥 `business-a` 与随机 `business-<UUID>` Worker ID 存在前缀碰撞；现改用明确的密钥标记并同时检测两租户密钥，仍禁止记录凭据。原 `go test | tee` 未启用 pipefail，导致该工作流错误显示绿色；现显式开启，失败必须传递到 CI。原始失败日志保留在仓库外 `ci-real-first.txt`。
- 补修后本地 `go test -race ./tests/integration -run '^TestRealTasks(SDK|Lifecycle)$' -count=2 -v` 连续两轮全部通过（147.620s），包括实际 Jaeger 隐私断言；日志为 `final-real-recheck.txt`。另实测 Bash 失败管道退出码为 1，产物服务启动错误脱敏回归与 golangci-lint 均通过。
- 代码提交 `d5fa176`：[Linux 工程 CI](https://github.com/XJfyrh/JobForge/actions/runs/34842236224) 五项全部通过；[真实模型验收](https://github.com/XJfyrh/JobForge/actions/runs/34842236243) 实际 PASS，集成 64.307s，12 生命周期场景 59.19s，SDK 4.01s。启用 pipefail 后检查完整日志，确认不是仅依据绿色状态；14 条 Jaeger 后端断言及两个真实产物检查通过。原始日志 `ci-engineering-final.txt` / `ci-real-final.txt` 保存在仓库外。
- 已推送 [PR #33](https://github.com/XJfyrh/JobForge/pull/33)。干净模型/数据库卷与普通 SDK 安装已验证；三分钟脚本中的四个故障窗口实际通过（29.477s），默认 SDK 两任务实际成功。当前独立演示项目为 `jobforge-agent-rag`（PG 55433），可重建测试库为 5433；Grafana、抽取成功 Trace 和崩溃恢复 Trace 已在浏览器实际检查。

## 跳过、限制与复现范围

- 明确跳过：既有 AT-25 ControlStream 可裁剪 P1 骨架；helper 测试主进程标记 skip、真实场景会作为 OS 子进程实际执行。全量这次没有因缺 PostgreSQL、Redis、Python 或真实模型而跳过相应验收。
- 未运行：可信远程模型配置、不同硬件/未知文档准确率、生产观测持久留存与容量评估。这些不作为本轮本地验收前提。
- Jaeger 开发内存存储重启清空；遥测可能丢失、计数随进程重启清零，不能替代 PG 审计。曾观察到 Docker/主机时钟跳变导致 Prometheus out-of-order 与 Jaeger skew 提示，跨进程时间戳不用于精确耗时结论。
- 固定小语料与合成订单是真实推理验收范围；恢复是重新执行/产物复用，不是断点续作，不能撤销已开始的外部模型计算。0022 downgrade 若遇新 C1 引用可被旧约束阻止，不自动修改数据。
- 复现入口：[真实任务](real-tasks.md)、[观测故障](observability.md#自动化故障复现与排障)、[三分钟演示](demo-script.md)。ADR-0011/0012 仍为 Proposed，等 PR 审查后接受；不把本地验证等同于架构决策已合并。
