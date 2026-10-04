# Agent v3：已确认步骤恢复

[PRD v0.17](product/JobForge_PRD_v0.17.md)与[ADR-0025](adr/0025-confirmed-step-recovery.md)规定 S3 的持久恢复合同。schema 3 的 `support_agent_v1` profile 显式声明 `confirmed_uncommitted_v1`，并固定 `linux-v2-recovery-runtime-1`。原 schema 1/2 的 definition、hash、runtime 和晚到审计保持原合同，未提交的历史 chat 不能借用新 profile 的豁免。免费实现证据见[S3 检查记录](evidence/agent-v3-s3-free-2026-10-04.md)；真实云端实验尚未运行，收费调用为 0。

## 原关闭事务与新执行权

已提交的 `run_steps`、cursor、hash 链与事件继续由 CommitStep 在原事务提交。原 attempt 按可恢复分类关闭时，先锁原 Run，再按 family/tenant/batch、call/attempt 和 slots 的既有顺序清理；schema 3 同事务保存待执行 step 的完整九字段与连续恢复序号。unknown 或 frozen 不阻止关闭清理，也不释放旧 hold。其他关闭与 schema 1/2 的两个新字段均为 null。

新 Claim 仍需自然 lease/Sweep/1、2、4 秒退避及合法 session；最多三次恢复。取消、Run deadline、attempt deadline 的优先顺序和 30 秒 lease、5 秒 heartbeat、180 秒 attempt 保持原值。新 attempt/fence、owner/session 在同一事务授予，不能从旧 Worker 的内存 cursor 继续。

batch guard 在原账户锁内只读其他 Run/attempt，避免反向 Run 锁。豁免必须由原 call 的不可变 profile 决定：原执行身份、step、binding、完整 known report、accepted observation、结算、关闭时间和连续证明全部匹配，且原账本没有 unknown、异常、冲突或冻结。未提交 chat 只豁免同 Run 当前待执行 step；其他 Run 仍被阻断。

恢复必须重新调用该未提交模型步骤，使用新物理 ID、许可和费用。新 Commit 指向真正的新 call；原 call 不能充当 checkpoint。之后 guard 核验完整相同逻辑 step 的新 attempt 提交和新调用审计，因此 cursor 再前进仍能验证旧证明。family/tenant/batch 的次数、known、held 与费用累计不重置。

## 实际进程丢失

S3 协调器只把实际 signal 或 guardian 的既有退出 72，结合 Wait、普通/计量 EOF、reader/writer Join、stderr 上界和组消失，认作执行丢失并停止 Run 续租。已有领域拒绝、坏帧、残片、大小、身份、unknown 或缺少清理事实不能被该分类吞掉。未提交结果不伪造 Fail/Commit，由正式控制扫描自然关闭。

Commit ACK 不确定只允许原身份/hash 的有限 GetAcceptedCommit 查询，总计最多两次/两秒；无论 Found 与否，Worker 放弃本次执行，不能发出下一步许可。新的 Claim/GetCheckpoint 以数据库提交事实复用前缀。原结果、usage 报告和晚到结算各自保持权限边界；旧 Observe/Commit/Heartbeat 不得修改新 active call 或 cursor。

## Migration 与复现

[0026 up](../migrations/0026_attempt_recovery_proof.up.sql)仅添加 nullable 证明对和 CHECK，无历史回填；原迁移不修改。DDL 用事务内 2 秒 lock_timeout。锁超时整次回滚，release 后可重试；真实 PG 用例核验无半列/半版本状态。[down](../migrations/0026_attempt_recovery_proof.down.sql)只允许尚无任何证明时回滚；已有证明应前滚修复，禁止删除证据后 down。

Windows 先启动本地开发 PostgreSQL、设置 `JOBFORGE_TEST_DSN`，安装 SDK 并设置 `JOBFORGE_TEST_PYTHON`。`go test -race ./...` 包含持久合同、预算、迁移、并发和 SDK 真实 HTTP；Windows 的进程 skip 不计 Linux 通过。

正式 Linux 层沿用[固定运行时指南](agent-v3-runtime.md)的 `process-check`、`python-check` 与 `integration-check`，使用 `--init`。联合层默认正则包含 `TestRunRecovery`，实际执行 Worker/guardian/step 死亡、自然 30 秒 lease、同 principal 自然 session 保护、180 秒 attempt 和四次丢失。固定上限 1200 秒，CI job 35 分钟，依据新增自然等待调整；生产计时未缩短。同一 DSN 只运行一个可能清理数据库的测试进程。

专用镜像安装的 `runtime_recovery_faults` 仅在测试构建插入确定性屏障；生产镜像不会包含模块、安装脚本或测试 manifest。生产 registry 继续仅登记 `support-fixed-v1`、`support-agent-v1`，供应商 origin 固定为官方地址。

## 有限实验准备

[S3 独立清单](../tools/support_recovery/plan.py)复用原 preparation 来源检查，但只冻结 DEV-002/027/035 的 C、H0、H1 及两例独立恢复，共 11 个独立意图。它生成新的 `control.s3.disabled/enabled.json`，只添加第二个预登记 principal，原 S1/S2 文件与 attempted/restart=no 入口不修改。

`sources.py` 根据当天实际价格快照和当前 adapter/prompt/schema 摘要生成新的 schema 3 来源文件；四份构建/数据/评分/价格回执仍需独立审查。`plan --check-plan` 重新核对受审源码、开发数据与 preparation，并输出绑定原清单 SHA256 的 preflight receipt。运行前另核对真实官方 metadata/价格，不把 D0 GET 或示例文件当执行授权。

[外部 gRPC 代理](../tools/supportrecoveryproxy/main.go)只转发原控制事务与鉴权。F05 在真实成功 Commit 后丢 ACK；F06 返回原 Commit ACK 后在完整相同执行身份/下一 step 的 BeginTool 或 Reserve 前阻断；F03/F07 在真实 accepted Observe 持久后持有 ACK。屏障文件以完整临时文件原子且独占发布，active 文件只在原 RPC 窗口内存在。

[Linux Supervisor/driver](../tools/support_recovery/driver.py)只管理固定二 principal 的正式 Worker，实际 Wait/旧组消失后才启动替代者，不调用 Claim、不改数据库计时、不创建 retry 接口。F07 SIGSTOP 实际 step 后释放真实 Observe ACK，核验普通管道确有字节，再杀 step；证据写为 ACK queued、Python consumed=false。

driver 需要主规划会话另行发布的 release，绑定原清单、preflight、实际构建、安装包、生产镜像及原批次累计 known/held。验证实际二进制/配置/manifest 后，只读 inspector 显式使用已校验的配置路径，并核对 profile ID/hash、batch ID/key/limits 和未使用历史。随后每行只 Submit 一次，先持久记录 attempted；失败先清理 Worker，再用独立十秒总预算导出 Run/Steps/Calls/Events，保留最初错误与所有未尝试行。所有秘密和原始正文仅在仓库外私有目录。

清单生成不等于允许收费。D4 要待免费门禁、独立实现审查、适用 CI 及主规划会话明确放行；新增累计最多 5 CNY/6 小时/11 Runs，不沿用 S2 余额。unknown、冲突、冻结或窗口失败停止后续收费，不能换 batch、thaw、退款或增资。成本与业务评分仍需结合实际 outbound、业务只读前后摘要和全部原始导出，不能由本轮合成 HTTP 验收代替。

外部实验镜像使用[独立 Dockerfile](../deploy/Dockerfile.support-recovery)，基于已核对 digest 的正式 Worker 镜像，仅加入已安装SDK、真实控制CLI、代理与进程监管/只读预检代码，不安装gold、评分器、合成registry或fault hook。原生产Dockerfile和S1/S2 launcher不变。`report.py`在容器外保留并评分全部十一行，H0和H1费用累计后再与C比较；缺少账本导出保留unknown。`known_cost_microyuan` 是按冻结定价和 observed usage计算的账本值，不能称供应商已结算费用；余额差值如取证须另列且说明无法精确归因到本实验。

报告按已提交step实际物理调用所绑定的attempt/tool将全部检索subcalls纳入前缀，同逻辑step旧attempt仍为未提交调用；无故障H1单列。Supervisor分别记录信号发出、Worker Wait完成和进程组消失确认时点；无child窗口明确记录且无虚构组消失时间。外部操作进程以[只读SQL](../tools/support_recovery/control_audit.sql)导出原attempt关闭/新Claim与首个新step时间，报告的 `--control-audit` 绑定原batch。SDK故障前lease仅称观测样本，不称最后一次续租事实；自然关闭等待、关闭到Claim、活跃执行和Run总耗时分别保留。
