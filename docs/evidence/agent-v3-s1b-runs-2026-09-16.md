# S1-B Run / 调用账本验证

- 日期：2026-09-16；契约：[PRD v0.9](../product/JobForge_PRD_v0.9.md)、[ADR-0017](../adr/0017-run-admission-and-call-ledger.md)，随 [PR #39](https://github.com/XJfyrh/JobForge/pull/39) 接受。
- 实现分支：`XJfyrh/s1-run-ledger`，基于 `696404e`。本记录对应实现工作树；PR最终head及独立审查结果另行追加，不能将基线提交当作实现已合并。
- 环境：Windows + Docker Desktop、Go race、真实 PostgreSQL 16、独立业务 pgvector PostgreSQL、AOF Redis、已安装本地 Python SDK。Run测试使用正式migration创建独立可重建数据库；同一集成DSN的进程串行运行。
- 收费模型调用：**零**。本切片默认无模型profile/预算；确定性fixture不代表DeepSeek、真实Agent质量或正式执行器验收。

## 需求映射

| 要求 | 实际结果与证据 |
|---|---|
| B-01 API/SDK | 通过。`TestRunPythonHTTPContract`经真实TCP HTTP/PG调用已安装SDK；覆盖严格JSON、角色/租户、提交冲突、并发retry、分页、结果与错误映射。`run_admission_test.go`证明已受理根Run/后继在profile关闭、批次冻结/过期、retry过窗或capture故障后仍可换操作键复用。 |
| B-02 快照/接纳 | 通过。真实独立业务HTTP/PG校验完整版本向量；业务成功后控制插入故障不留下Run/家族/操作身份；显式重发同快照键复用；retry捕获新快照。capture在控制事务外、无隐式重试。 |
| B-03 执行权/容量 | 通过。双Worker并发Claim容量不超限，session/fence/过期lease拒绝；实际账户锁等待跨deadline后重新读取数据库时间；Worker TCP gRPC鉴权、deadline与心跳验证。 |
| B-04 停止/恢复 | 通过。取消阻止后续授权/步骤；stopping不续租；超时后到取消优先；停止ACK和lease到期收敛，最多三次恢复，重复扫描不重复关闭。数据库恢复测试不代替S3正式进程Kill/Wait。 |
| B-05 步骤/首次结果 | 通过。并发重复Commit仅一个结果，异内容冲突；服务端计算游标；未观察调用/外来证据拒绝；JSONB数字往返去重；聚合256KiB上限；最终ACK丢失只读确认。 |
| B-06 方案/效果 | 通过。方案、审批绑定、attempt关闭和释放三个容量槽同事务；注入最终event失败验证整体回滚。仅no_action成功；审批过期与Run期限收敛。没有批准或业务写入接口。 |
| B-07 三层预算 | 通过。两Run竞争最后batch额度恰一成功；family/tenant/batch各层不足均无部分借记；锁等待过期拒绝；retry共享家族额度；预算耗尽仍可读checkpoint。 |
| B-08 每次发送 | 通过。丢Reserve ACK不获得第二许可；每次search四个子请求分别授权。真实HTTP接收后断连/截断保留unknown全额hold，新发送需新ID/额度。取消后已经在途响应可能返回，不能再推进步骤。测试发送器不是生产guardian。 |
| B-09 usage隔离 | 通过。合法usage与输出是否合法独立，首次/重复/冲突结算；终态后与旧attempt补报不推进状态或清新active_call；30日边界与原身份鉴权；异常usage原样保留、三层冻结、原hold不返还。 |
| B-10 工程/协议 | 本地适用检查通过，PR CI和独立审查待完成。OpenAPI、生成Proto、Go/Python执行器共同fixture、Windows全仓race、SDK、SQLFluff、Buf均已执行。新热路径基线及自然EXPLAIN见[性能报告](agent-v3-s1b-performance-2026-09-16.md)。 |

对应源测试位于 `tests/integration/run_*_test.go`、`tests/integration/business/run_capture_test.go`、`internal/run`、`internal/runprotocol`、`sdk/python/tests` 和 `python/tests/test_protocol.py`。稳定工具图使用合成profile/价表，仅证明合同执行；模型价格/效果不由这些测试推导。

## 本地运行结果

| 检查 | 结果 |
|---|---|
| 全仓 `go test -race -count=1 -json ./...` | 退出0；765个测试通过事件、5个测试skip。真实PG/Redis/业务库/已安装SDK均配置。后续增补的四项接纳/Claim/审批定向测试另跑通过（7.113s）。[结构化摘要](agent-v3-s1b-windows-race-2026-09-16.json)保留运行范围。 |
| Run PG/race | 全Run用例通过39.343s；后加真实HTTP故障定向通过6.298s；性能测试16Run race通过4.87s。 |
| 热路径128Run | 通过；128成功、896物理调用、384工具、768步骤、剩余容量占用0。24.773s，Claim p95 37.409ms、chat Reserve p95 16.243ms；仅新基线，不是历史门禁翻案。 |
| Go build/vet/golangci-lint | 全仓通过，lint 0 issues；后续修改再执行相关定向检查。 |
| 控制入口边界/关停 | 最后启动审查修复部署字段别名/null、跨公开/内部凭据复用与HTTP取消传播；`go test -race -count=1 ./cmd/agent-control`通过7.369s、定向lint 0 issues。真实TCP活动HTTP、RPC和scanner均有界退出，错误不回显配置路径、DSN或token。 |
| Python | SDK、业务/协议与两个S0探针合计296通过；Ruff check/format、SDK/业务Mypy、探针Linux平台Mypy均通过。 |
| SQLFluff | 3条历史baseline有效；所有新migration lint通过；0023 up/down/reapply和旧jobs保留在真实PG验证。 |
| Buf | lint、对main breaking检查通过；重新generate后的全部pb.go hash完全相同，未手改生成代码。 |
| 本地控制服务启动 | Docker新控制库完成1～23迁移，bootstrap成功，8093 ready与鉴权list成功。默认零模型/零预算，未伪造业务执行。 |
| S0专用Linux进程/race | 按CI构建固定镜像，再以 `--init --network none --cpus 1 --memory 256m --pids-limit 64` 独立运行；14 PASS（5顶层、9子测），0 FAIL、0 SKIP。实际cancel/timeout/guardian退出/Go父进程SIGKILL后进程组被回收；不代表S1-C正式执行器已交付。 |
| 既有Prometheus/Grafana配置 | promtool配置与告警规则测试通过；仪表盘重新生成后Git内容diff为零。未添加新Run观测面板，也未宣称S5观测验收。 |

全仓race中的5项skip：AT-25控制流降级；`TestRealTasksLifecycle`、`TestRealTasksSDK`需要旧真实模型专层，本轮未启用；两个Worker进程helper在父测试进程按设计skip（父故障测试通过并分别启动helper）。没有测试文件的包单独列入摘要，不能混算为用例skip。

首次checkpoint验证因测试构造的过期session违反数据库时间约束而失败，修正同时回退seen/expires后定向与全Run通过；首次锁等待测试错误地只寻找预算锁，修正观察同用例数据库内真实Lock等待后通过。失败原因与修复保留在仓库外档案，没有放宽生产约束。

## 复现与剩余范围

命令、服务启动、SDK接入与故障判断见[Run指南](../agent-v3-runs.md)。全仓检查还需要按开发指南启动Redis和业务数据库并配置对应测试环境变量；仅设置控制DSN不足以证明其它依赖层通过。

S1-C正式Go监督/Python执行器、DeepSeek真实调用、40例固定流程与评分仍未交付；S2动态Agent、S3真实checkpoint进程恢复、S4审批写入、S5评测展示/观测/保留仍未完成。已有S0进程探针不能代替这些阶段。取消只停止后续处理，不能撤销已发出的供应商请求；unknown费用不能当作零。

历史**远程模型未验收、生产长期留存未验收、W4性能门禁失败、AT-25跳过**继续保留。本切片没有自动清理/unknown退款，也未宣称长期保留、真实云端成本或新Run的Trace/Grafana全链路通过。

仓库外档案：`E:\JobForge-notes\2026-09-16-agent-v3-s1`，记录选择、修复和原始安全测试输出，不含API key、完整模型内容或秘密。

## PR独立审查

实现 [PR #40](https://github.com/XJfyrh/JobForge/pull/40) 首个head `2b5fa97` 的[七项CI](https://github.com/XJfyrh/JobForge/actions/runs/35071585157)全部通过。三个全新上下文分别审查可靠性/租约/检查点、预算/usage/快照、HTTP/SDK/RPC/帧/入口；前两份无可确认阻断，合同审查发现一项P2：RPC丢失 `CHECKPOINT_TOO_LARGE` / `MODEL_PROTOCOL_ERROR` 原因而返回 `INTERNAL`。

修复保留两类永久结果错误的ErrorInfo，RPC类别为InvalidArgument；未知内部异常继续脱敏。真实TCP+PG追加测试通过3.981s，验证未推进Run游标，随后同名FailAttempt永久结束且不消耗恢复次数。审查者独立overlay复现也在修复工作树通过。全仓lint再次0 issues。最终修复head仍须CI与审查确认后才合并；外部报告为 `pr40-reliability-review.md`、`pr40-budget-review.md`、`pr40-contract-review.md`。

部署验证使用UTC PostgreSQL会话。非UTC/DST配置未验收；当前migration的7日约束使用PostgreSQL日历interval，不能把所测UTC下的168小时行为推广到跨DST会话。该限制与生产运维留存验收一并保留。
