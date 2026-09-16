# S1 供应商持久审计：实现与验证

日期：2026-09-16。基线 `bffe5665f266f0510de94d7bf13a3f8dbc963df2`（PR #48）。合同为 [PRD v0.12](../product/JobForge_PRD_v0.12.md) / [ADR-0020](../adr/0020-provider-audit-and-batch-stop.md)。本切片覆盖 typed report、PG 原子记录、跨 FD 确认、批次 guard 和只读 HTTP/SDK；完整评分器、收费 profile 登记、首批启动器和真实 40 案仍在后续切片。没有 DeepSeek 推理调用，不宣称整个 ADR-0020 或 S1 完成。

## 需求与证据映射

| 要求 | 实现与实际检查 |
| --- | --- |
| 安全、精确、有界 provider 事实 | Go/Python strict audit/report；null 与零、safe integer、重复/额外键、2048 byte 上限、reasoning/usage/identity 矩阵；Go 生成共享 hash 向量，两端实际消费 |
| 原始绑定与首次报告 | Reserve 保存 execution binding；report 绑定原调用/参数/usage/audit；receipt 用实际身份重算；PG 首次记录、结算、冻结同事务 |
| 重放、冲突、错误定价 | 同 hash 幂等；不同第二份先判冲突，即使其超 token 上限也不改第一份计量或误记新 anomaly；不兼容完整计数只作 observed，保留 hold |
| 晚到与历史原调用 | 原 principal/session/attempt/fence 及 30 日窗口；终态/禁用 profile 不撤销原窄结算权；不改 Run/cursor/新 active call；0024 down/reapply 保留 legacy 已知计量 |
| 双 FD 确认与普通屏障 | report_hash 五类 ACK，observation.v2 绑定 audit；只有匹配 settled 与持久普通 ACK 汇合才能继续；recorded/anomaly/conflict/unconfirmed 不能恢复执行 |
| 持久停止与竞争 | 新 Claim/BeginTool/Reserve 持 batch 锁检查旧 chat；两并发 Reserve 仅一份许可；Report/Observe 不代替原 Step Commit；第二纠正失败仅精确原身份终态例外 |
| 进程死亡与确认丢失 | 固定安装包、真实 guardian/step/FD、PG/TCP gRPC；实际 SIGKILL 后只保留一份 unknown chat 和四个已提交步骤，新 session 不再收费；提交前/后丢 ACK 分别检验数据库事实 |
| API/SDK 隔离 | reader/operator 与跨租户 404；最多 44 行、256 KiB、无分页查询参数；installed SDK 查询五类真实 PG 记录；observed/settled、missing/unavailable、null/0 分开 |
| 生产边界 | 生产镜像仅 support-fixed-v1、固定官方 origin/新 executor version；无 fixture registry、测试安装器、gold 或默认收费 manifest |

`query_embedding` 未知计量仍沿用原 full hold + ordinary ACK；它不伪造报告，也不触发 chat unknown 停批。完整 embedding usage 则需新的 usage-only report 确认。两端有专门继续/错误 ACK 回归。运行限制见[审计指南](../agent-v3-provider-audit.md)。

## 实际检查

所有 PG 检查使用可重建测试数据库；Windows 先执行 Compose PostgreSQL 前置和标准 DSN。同一 DSN 上各测试进程串行。固定 Linux 镜像使用 `--init`；机制供应商与业务/embedding 响应为明确标记的替身。

| 检查 | 状态 | 证据 |
| --- | --- | --- |
| Windows 全仓 Go race 首轮 | 失败，已定位并修复测试提取器 | 唯一失败 `TestRunHotPathPlansAndBaseline`：完成 16 Run 后，旧 AST 提取器未读取 ledger.go 的共享列常量，未能执行该 SQL 的 EXPLAIN；保留失败日志 |
| Windows 全仓 Go race 完整重跑 | 通过与明确跳过，随后 Commit 优化单列 | 优化前 1549 个测试/子测试通过事件、33 包通过，零失败；27 个具名 skip，22 个平台/专用环境场景由固定 Linux 实跑补齐；详见[机器摘要](agent-v3-s1-audit-windows-race-2026-09-16.json) |
| Commit 查询/锁优化后定向 PG race | 通过 | 64 个测试/子测试事件、65.796s；含 audit/legacy 锁等待、冻结竞争、禁用 profile 的已接受重放、新提交拒绝和 stale 优先级；vet/lint 通过 |
| 新 PG storage/guard/migration 与旧账本回归 | 通过 | 新层 race 31.076s；旧 Ledger/Checkpoint/Session/Migration race 30.026s；含首次/冲突/晚到、并发 guard、44/45 行与迁移约束 |
| 安装 SDK 的新旧 Run HTTP 契约 | 通过 | 新五类场景：missing、known rejected、incompatible、unavailable、conflict；旧 Run HTTP 契约同时通过 |
| SDK 与 S0 确定性探针 | 通过 | 206 tests，其中 SDK 163；不访问真实模型 |
| Windows Python 执行器 | 通过与明确跳过 | 1307 passed、8 skipped；这 8 项属于 Linux FD 条件，不记为通过 |
| Linux Python 全套 | 通过 | 1315 passed，零 skip |
| Linux 实际进程 / Worker / PG 集成 | 通过 | 分别 43 / 124 / 32 个测试及子测试通过事件，零 fail/skip；Go race 编译 |
| Commit 优化后最终 Linux/生产复验 | 通过 | 重建后的默认实际进程/PG 集成32项、新 Commit 族6项均零 fail/skip；正式生产镜像隔离检查通过 |
| Go build/vet/lint | 通过 | Windows build/vet；Windows 与 Linux 目标 golangci 均 0 issues |
| Python lint/types | 通过 | 相关 62 文件 Ruff/format；SDK 与执行器 Linux mypy 37 源文件 |
| SQL 与 Proto | 通过 | SQLFluff 历史基线及全部 migrations；Buf lint 和相对基线 breaking 均通过 |
| 生产镜像构建与隔离 | 通过 | 实际导入已安装 wheel，核对 registry/origin/version、无 fixture/gold/收费 manifest |
| 性能对比 | 已运行，仍有剩余开销 | 每轮五组交替基线/候选；优化前吞吐中位数 5.149→4.041 Run/s（−21.5%）；优化后 5.107→4.829（−5.4%），Commit p95 中位数 18.943→18.7824ms；保留[两轮完整汇总](agent-v3-s1-audit-performance-2026-09-16.json)，不宣称历史 W4 通过 |
| PR CI / 确切 head 独立复核 | 待完成 | 首轮独立上下文代码审查无 P1/P2，最终 head 另行核验 |

PR #50 的 `4e45817` 已通过最终独立上下文审查。CI `35109237665` 七项通过，Linux runtime 的 `report-before` 子例发生一次20s未退出、清理5s未Join；这次失败明确保留。同镜像/资源的精确子例3次及默认完整顺序32项均未复现，不能据此声称已修复。新增只在超时时打印有界 goroutine 栈的测试诊断，保持原超时与生产代码不变，再由CI采集实际卡点；没有延长等待掩盖失败。

Linux 确认窗口测试覆盖 Reserve、report、Observe、Commit 四处各两种故障。提交前以实际行锁和 `pg_blocking_pids` 证明 RPC 已进入数据库并阻塞；提交后取得服务端成功再丢弃 ACK。Worker 全部停止新 Claim；无 reservation 的 Reserve 前失败，以及事务已完成的 Commit 后失败，允许新 session 依据数据库事实继续。其它未完成收费窗口拒绝新 session，且不伪称 batch 已冻结。provider 身份错、无 usage、正数 reasoning 停批各实跑；合法 reasoning 计入已有 output，不重复收费。第二次结构化输出无效保存合法计量与两份 rejected observation，符合窄终态例外后允许下一案例。

优化前 Linux 集成镜像：`sha256:3b0309b547f32872e9a7a418da3f50cd1669cf3234ae6d7d7c139b4685fa96a9`；进程镜像：`sha256:02c7b281f43f6e746e0cccb0e049918b2415fd5024aa6c6bdef977a0c13e4ade`；Python 镜像：`sha256:33b8d245f31fbf124e277817774f294ba03de4dc393cafacccce37dd92dd2241`；生产镜像：`sha256:1eb8f7ba65fe367b1c14a7770e8ebf4398e9f472baac091a783d1fedf9f49b2b`。

优化后最终集成镜像 `sha256:61f21a5deaeec4100051ca3df54369aee50623951ce8ed6220dadf91078074b7`、生产镜像 `sha256:cab909b72f222dc5e0b62dec6e2bbda427079e1415aae0683c14b4aee296239f` 已实际重建和复验，源码对应实现提交 `b5f5056`。Commit 族首次 Docker 启动因 PowerShell 参数传递错误退出2、未执行测试；保留日志，改用显式参数数组后6项通过。没有将启动失败算成测试通过。

性能场景覆盖原有注册 Run 的公共控制热路径，每次 64 Run、448 次调用、192 工具及384步骤，业务结果为合成数据，无模型 HTTP。最初新 Commit 无条件锁三层账户并重复读取 profile，给无需审计的旧路径增加了五次 SQL；改为复用不可变 profile、仅审计 profile 锁账户，保留原账户锁顺序及锁后数据库时钟/执行权/冻结检查。新增真实 PG 锁竞争回归防止以移除约束换取速度。优化后其它 p95 中位数：Claim 31.6007→35.0037ms、Reserve 17.2421→20.0013ms、Observe 14.4046→14.9452ms。账本新增持久字段/校验仍有开销，共享 Windows/Docker 主机也有噪声；没有删除慢样本、重复挑选最佳轮次或推断新的容量承诺。

## 复现与限制

安装 SDK 后设置 `JOBFORGE_TEST_PYTHON`，执行 `go test -race -count=1 ./...`；固定容器完整构建/运行命令见[运行时指南](../agent-v3-runtime.md)。共同向量可分别使用 `go test ./internal/run -update-provider-audit-fixture`、`go test ./internal/runprotocol/v2 -update-audit-wire-fixtures` 和 `go test ./internal/run/httpapi -update-call-fixtures` 重新生成，普通测试只核对一致性。

首次 SDK 安装禁用 build isolation 时因环境缺 hatchling 失败；按标准 `pip install --no-deps ./sdk/python` 的隔离构建成功，之后真实 HTTP 验证通过。原日志与镜像构建/测试输出保存在仓库外 `E:\JobForge-notes\2026-09-16-agent-v3-s1\audit-*`，决策记录不包含秘密或模型正文。

未知费用保留 hold，调用已发出不等于供应商支持撤销。缺报告前主机死亡可能永久丢失可核对计量；别名/fingerprint 不构成服务端不可变版本或价格锁。没有自动退款、thaw、top-up、新 batch 绕过或报告归档清理。真实 DeepSeek/40 案、完整 Trace 后端、生产长期留存和 S2～S5 未在本切片验收。历史 W4 失败、AT-25 跳过继续明确保留。
