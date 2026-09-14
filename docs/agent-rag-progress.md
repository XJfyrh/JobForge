# Agent/RAG 增量实施与验收记录

基线：2026-09-14，`02c61e4`（main 与 origin/main 一致），工作区初始干净。
工作分支：`XJfyrh/feat-agent-rag`。范围与门禁见 [PRD v0.6](product/JobForge_PRD_v0.6.md)。

| 阶段 | 状态 | 完成证据 | 剩余 |
|---|---|---|---|
| M0 调查/文档/基线 | 完成 | 初始无未提交改动；规则/文档/实现已核对；同环境 Claim/Complete/Fail 基线已保存 | 无 |
| M1 接入契约 | 已实现并验证 | SDK 34 项测试；独立环境安装；真实 HTTP/Python/Gateway/Worker 联调；AT-34 PostgreSQL 结果事务/竞争/迁移测试；全量 race 通过（集成 132.4 s） | 后续阶段变更后复验 |
| M2 真实任务 | 进行中 | Ollama 0.32.5；已固定下载 MiniLM 22m / Qwen2.5 0.5b；真实向量检索和抽取冒烟通过 | 任务适配、产物 API、AT-36～39、移除 PageWise |
| M3 观测与运维 | 待实现 | 已有 stdout OTel、Prometheus/Grafana 配置 | AT-40～42 |
| M4 全量验证/交付 | 待实现 | 无 | 门禁、干净启动、演示、审查 |

所有模型替身只计快速测试；真实模型结果、真实进程 kill 和观测查询另列。历史 W4 Claim 绝对门禁未通过的既有披露继续有效，不以本轮相对性能比较覆盖。

## M1 验证（Windows / PostgreSQL 16，2026-09-14）

- 通过：`go build ./...`、`go vet ./...`、golangci-lint、`go test -race ./...`（设置测试 DSN、Redis URL、Python 解释器）；pytest（34）、ruff check/format、mypy；SQLFluff 历史基线、migration lint；buf lint。
- 首次全量 race 中 AT-24 的取消信号父 Span 断言仍指向提交 Span；传播改为 Worker 子 Span 后已同步断言，并实际重跑全量通过。取消 p95 约 4.60 s，未出现 data race。
- 完成路径同环境每轮 100 次、3 轮中位数：Complete 4.625 → 4.053 ms（约 -12.4%），Fail 5.476 → 3.866 ms（约 -29.4%）。Complete 分配增加（50 → 68 alloc/op），换取持久化结果、精确重复确认与事务内计数元数据；不代表生产负载吞吐。
- Claim 原基准会把空领取计入样本；已用数据库过去一天的时间、新队列和每次真实领取断言修正。把同一基准复制到 `02c61e4` 隔离工作树对比，100 次 × 3 轮中位数 3.979 → 4.143 ms（约 +4.1%，91 → 93 alloc/op），新增结果字段增加扫描分配。早期无真实领取断言的数据作废；本次并非历史 W4 绝对性能门禁复验。
- 原始日志与技术决策档案位于仓库外 `E:\JobForge-notes\2026-09-14-agent-rag`。当前没有真实任务生命周期、进程崩溃或后端观测验收完成的声明。
