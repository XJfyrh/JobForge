# Agent v3 S1-C2：授权 HTTP 与 DeepSeek 模块验证

日期：2026-09-16。基线为已合并 C1 `b9ba908cc8434e3b3fc6aaddbbb5e1f4684a8826`。依据 [PRD v0.10](../product/JobForge_PRD_v0.10.md) / [ADR-0018](../adr/0018-deepseek-fixed-flow-and-executor.md)，本切片交付 Python 网络模块，不激活收费 profile。PR、最终 head 与独立复审结果在完成后补记；此时尚未合并。

## 需求与实际证据

| 需求 | 实现与测试 | 范围限制 |
|---|---|---|
| 单次物理授权 | `PreparedRequest`保存一次序列化的不可变 bytes；每次HTTP消费原v2许可，校验Run/snapshot/hash/固定路径；禁重试、redirect和环境proxy | hooks为测试协调者，不是Go/PG持久预留 |
| 期限和取消 | 原步骤/许可BOOTTIME、单所有者、取消并等待活动HTTP任务、有限关闭；Linux实际时钟+TCP执行 | 不撤销供应商已发送请求，不证明退款；正式guardian/进程组仍未交付 |
| usage与业务结果分离 | 完整usage在校验及异步等待前捕获；输出非法照常结算；缺失/矛盾/截断保持unknown；超界原值保存并停止 | 仅当前步骤有界内存，无持久journal；主机死亡可能unknown全额hold |
| 结果拒绝 | 已确认rejected允许C1 error step_result、禁止成功结果和新调用；确认失败永久停止 | 更高层纠正调度和CommitStep由后续Worker实现 |
| 业务工具 | get_order/get_delivery各一次HTTP，search_policy四次HTTP各自授权；前置身份/向量失败无后继请求 | 向量和业务响应合成，当前不是真实检索质量验收 |
| 校验复用 | 离线API与在线Run工具共用纯参数/结果校验，区分Run profile与index profile；float32平方和对齐Go/pgvector | 不改变PG/queue热路径，无新性能结论 |
| DeepSeek | 固定官方origin/model/配置，严格完整JSON、身份、usage及内容边界；业务schema校验委托预注册调用者 | 未请求DeepSeek；模型别名/指纹不是不可变服务端版本锁 |
| 安全和追踪 | 验证后traceparent透传；固定错误码且不保留秘密/正文的底层异常链；身份审计仅有界标识和digest | 当前只验证HTTP传播；FD审计传输、持久化与Trace后端仍未交付 |

## 已执行检查

| 检查 | 结果 |
|---|---|
| Windows全部Python | `pytest sdk/python/tests python/tests tools/agent_probe_data tools/executorprobe -q`：655 passed，0 skip。首轮649通过后补6项rejected确认反例并全量重跑 |
| 本切片新增模块定向测试 | dispatch 61项、run_tools 53项、DeepSeek 108项；另1项生产时钟测试。实际TCP传输，供应商/向量/协调者均为替身 |
| Linux镜像全部业务Python | `tools/agenthttpcheck/Dockerfile`实际构建，`--init --network none --cpus 1 --memory 256m --pids-limit 64`：477 passed，0 skip；实际Linux BOOTTIME路径执行 |
| Python静态检查 | Ruff check/format 48文件通过；mypy SDK+业务25文件及Linux业务/探针15文件通过 |
| Go编译和静态检查 | `go build ./...`、`go vet ./...`、golangci-lint通过（0 issues） |
| SQL/Proto | SQLFluff历史3项基线校验、migration lint、Buf lint通过；本切片未改SQL、migration、Proto或生成代码 |
| 可观测配置回归 | 仪表盘重生成无内容diff；promtool配置和5条规则测试通过；未改观测配置 |
| Windows全仓race | 29包、901个测试及子测试通过事件，0失败；真实控制PG、业务pgvector、Redis AOF及已安装SDK的Python。5个测试skip及11个无测试包单独记录，见下文 |
| 最终PR CI和独立审查 | 待完成；两份新上下文预审并行，不由代码作者自审代替 |
| 真实DeepSeek/40案/正式进程 | 未运行，不能由以上确定性结果代替 |

本切片Linux镜像基础digest为`sha256:782412e85d0f0984994c290652577d4018aff08145c85b262bb63dc0c7522254`；使用现有requirements，未新增第三方依赖，也未新增全传递依赖lockfile。精确复现命令见[模块指南](../agent-v3-authorized-http.md)。Go/真实依赖命令按[开发指南](../development.md)执行，Windows先启动5433 PostgreSQL并设置DSN，且同一DSN不并发运行多个集成进程。

全仓race的5个测试skip为`TestCancelAT25ControlStreamDegradation`、`TestRealTasksLifecycle`、`TestRealTasksSDK`、`TestBusinessWorkerProcessHelper`与`TestWorkerProcessHelper`。前者保持历史未验收；两个真实模型套件本轮未配置，两个Helper只是子进程入口。另11个包无测试文件，不能算作通过。SDK真实HTTP契约和独立业务PG/HTTP契约实际运行，不属于这些skip。

## 后续与历史边界

正式IPC仍需解决observe持久确认ACK、终止事实及完整usage的窄通道排空；必须先补增量契约，不能将管道write视为数据库已接收。本模块无持久授权能力，不把测试hooks当作可直接上线的执行器。schema、策略、数据版本、评分和真实云端批次在后续完整C验收中实现。

本切片无Go/SQL热路径修改，未新增性能对比；保留[C1定向比较及限制](agent-v3-s1c1-protocol-2026-09-16.md)。历史[W4性能门禁失败](../worker-capacity-performance.md)、[AT-25跳过及原验收限制](../agent-rag-review.md)、远程模型和生产留存未验收继续明确保留。整体S1、S2～S5尚未完成。

技术选择、独立审查和执行原始证据存于仓库外`E:\JobForge-notes\2026-09-16-agent-v3-s1`，不含秘密或模型/文档正文。
