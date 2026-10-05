# S3 有限实验工具（2026-10-04）

> 历史快照：以下 release、ordinal 与窗口仅适用于已完成的原实验，不构成新收费授权。结果见[最终 S3 报告](../evidence/agent-v3-s3-cloud-2026-10-04.md)。

## 有限实验准备

[S3 独立清单](../../tools/support_recovery/plan.py)复用原 preparation 来源检查，但只冻结 DEV-002/027/035 的 C、H0、H1 及两例独立恢复，共 11 个独立意图。它生成新的 `control.s3.disabled/enabled.json`，只添加第二个预登记 principal，原 S1/S2 文件与 attempted/restart=no 入口不修改。

`sources.py` 根据当天实际价格快照和当前 adapter/prompt/schema 摘要生成新的 schema 3 来源文件；四份构建/数据/评分/价格回执仍需独立审查。`plan --check-plan` 重新核对受审源码、开发数据与 preparation，并输出绑定原清单 SHA256 的 preflight receipt。运行前另核对真实官方 metadata/价格，不把 D0 GET 或示例文件当执行授权。

[外部 gRPC 代理](../../tools/supportrecoveryproxy/main.go)只转发原控制事务与鉴权。F05 在真实成功 Commit 后丢 ACK；F06 返回原 Commit ACK 后在完整相同执行身份/下一 step 的 BeginTool 或 Reserve 前阻断；F03/F07 在真实 accepted Observe 持久后持有 ACK。屏障文件以完整临时文件原子且独占发布，active 文件只在原 RPC 窗口内存在。

[Linux Supervisor/driver](../../tools/support_recovery/driver.py)只管理固定二 principal 的正式 Worker，实际 Wait/旧组消失后才启动替代者，不调用 Claim、不改数据库计时、不创建 retry 接口。F07 SIGSTOP 实际 step 后释放真实 Observe ACK，核验普通管道确有字节，再杀 step；证据写为 ACK queued、Python consumed=false。

driver 需要主规划会话另行发布的 release，绑定原清单、preflight、实际构建、安装包、生产镜像及原批次累计 known/held。验证实际二进制/配置/manifest 后，只读 inspector 显式使用已校验的配置路径，并核对 profile ID/hash、batch ID/key/limits 和未使用历史。随后每行只 Submit 一次，先持久记录 attempted；失败先清理 Worker，再用独立十秒总预算导出 Run/Steps/Calls/Events，保留最初错误与所有未尝试行。所有秘密和原始正文仅在仓库外私有目录。

清单生成不等于允许收费。D4 要待免费门禁、独立实现审查、适用 CI 及主规划会话明确放行；新增累计最多 5 CNY/6 小时/11 Runs，不沿用 S2 余额。unknown、冲突、冻结或窗口失败停止后续收费，不能换 batch、thaw、退款或增资。成本与业务评分仍需结合实际 outbound、业务只读前后摘要和全部原始导出，不能由本轮合成 HTTP 验收代替。

外部实验镜像使用[独立 Dockerfile](../../deploy/Dockerfile.support-recovery)，基于已核对 digest 的正式 Worker 镜像，仅加入已安装SDK、真实控制CLI、代理与进程监管/只读预检代码，不安装gold、评分器、合成registry或fault hook。原生产Dockerfile和S1/S2 launcher不变。`report.py`在容器外保留并评分全部十一行，H0和H1费用累计后再与C比较；缺少账本导出保留unknown。`known_cost_microyuan` 是按冻结定价和 observed usage计算的账本值，不能称供应商已结算费用；余额差值如取证须另列且说明无法精确归因到本实验。

报告按已提交step实际物理调用所绑定的attempt/tool将全部检索subcalls纳入前缀，同逻辑step旧attempt仍为未提交调用；无故障H1单列。Supervisor分别记录信号发出、Worker Wait完成和进程组消失确认时点；无child窗口明确记录且无虚构组消失时间。外部操作进程以[只读SQL](../../tools/support_recovery/control_audit.sql)导出原attempt关闭/新Claim与首个新step时间，报告的 `--control-audit` 绑定原batch。SDK故障前lease仅称观测样本，不称最后一次续租事实；自然关闭等待、关闭到Claim、活跃执行和Run总耗时分别保留。

## 中断后的受控续执行

`chat_barrier` 接纳真实已提交的 `model_decision` 纠正标记：完整 known 审计、rejected/MODEL_PROTOCOL_ERROR、唯一 physical call、完整 StepRecord/profile/snapshot/input/hash 链及 correction_required=true、proposal=null 必须匹配。未提交 rejected 仍被拒绝；这沿用原 Commit 分支，不扩展 ADR-0025 的未提交豁免。SDK step 不含 attempt_no，执行权仍由原 Go/PG 决定，续执行预检另以 PG 的实际 step.attempt_no 对照调用。

[专用续执行工具](../../tools/support_recovery/continuation.py)仅适用于本次原七项已关闭、剩余 ordinal 8–11 从未 Submit 的中断。原清单、source/build/preflight/release、SDK/PG/outbound、费用和失败报告逐文件绑定；只允许外部实验工具的明确源码差异，profile/生产镜像/模型/提示/数据和原累计账户保持原值。启动前重新只读核验七项 Run/attempt 均关闭、全部调用已观察、无 unknown/held/conflict/freeze，原 SDK/PG 事实未变。新输出独占创建，每个剩余意图只 Submit 一次，原 Worker 自然 session 保护照旧；未知 Submit 不自动重试。合并报告写到新目录，前七行及其导出逐字节保留。

续执行需要另行审查并放行原四项，准备 manifest 的 approved=false 不构成收费授权。不得新增 batch、重跑前七项、修改业务评分结果或重置原 5 CNY/6 小时/11 Runs 上限。

等待审批的旧 Run 如在准备期间到期，只允许原 Sweep 的 awaiting_approval→failed/RUN_DEADLINE_EXCEEDED，updated_at 不早于原 deadline。原 attempt/step/call/proposal/Result、身份/profile/snapshot/cursor/recovery 与费用必须不变，新增事件须准确匹配原状态转换；其他漂移拒绝。最新 SDK/PG/账户取证与原执行快照分别冻结，启动再核对最新快照，事后等待超时不改写原评分或执行耗时。
