# S1 云端运行与评分入口

日期：2026-09-16。基线为 [PR #50](https://github.com/XJfyrh/JobForge/pull/50)；按已接受 [ADR-0021](../adr/0021-first-cloud-batch-admission-and-launcher.md) 实现。运行命令见[云端批次指南](../agent-v3-cloud-batch.md)。以下确定性检查与[真实DeepSeek首批结果](agent-v3-s1-first-cloud-2026-09-16.md)分开：真实批次未通过，PR #51暂不合并。

## 需求与实现

| 范围 | 实现与证据 |
|---|---|
| L-01 可信登记 | typed definition、Go canonical/profile/price hash、源 schema 与向量；prepare 只接受固定能力和外部实际 receipt，输出 disabled/enabled 配置及40行 |
| L-02～03 接纳 | Capture 后与 Store 新建事务内比较冻结资源；旧回执优先、Retry 继承；实际 PG 验证错资源不产生 ready Run/账户、不可变登记及重放 |
| L-04～05 预检与监管 | 只读 inspect 返回真实账户和启动历史；固定 launcher 持有 Worker/driver 生命周期与一次启动标记；helper 与真实联合测试分别报告 |
| L-06 串行驱动 | 安装 SDK、每案最多一次 Submit、原子保留40行、拒绝不明确继续；公开 Calls 的二次纠错领域码为 MODEL_PROTOCOL_ERROR |
| L-07 导出评分 | 原始 API 响应/分页/精确数字留档；独立评分器固定40分母，检查实际来源与业务声明；出站元数据加业务库只读权限、前后事实摘要 |
| L-08 真实模型 | 实际运行未通过：28次chat、15案执行结束，第16案中断，24案未尝试；业务评分0/40 |

## 已运行检查

- Profile 接纳、注册与 Retry 三组真实 PostgreSQL 验证通过；首轮测试注入将所有账户期限一并回拨，违反家族有效期约束，修正测试后失败子例通过，未改生产语义。
- Inspect 三项真实 PostgreSQL/race 通过，0 skip；Go 领域、业务捕获、HTTP/gRPC、Store 和 prepare 纯测试通过。最终 Python 源码冻结后，source 样例摘要一致性检查通过。
- 固定安装 Linux 镜像的 support 正常方案和一次纠正两例通过，0 skip。第一次 PowerShell Docker 参数拆分错误未运行测试；显式引用参数后通过，未把启动失败算作测试结果。
- 独立评分器180项通过；新 driver/assembler 13项通过。新 launcher 的三个真实 Linux helper 生命周期测试通过；实际 Docker 主进程 SIGKILL 后退出137、PID namespace结束、RestartCount为0。helper不代表正式Worker/PG联合验收。
- 新增正式 Worker、安装 SDK/driver/launcher、真实 PG/gRPC 联合负例两项通过（race、10.14s、0 skip）。Submit 已提交时杀 driver，ready Run 保留；Reserve 已提交、回复仍扣住时终止 launcher，实际进程组退出后释放回复，原 hold 保留且 chat HTTP 为0。两例均只有一个 session/Submit，同批复启拒绝；首轮夹具误用原 DSN，修正为隔离测试库后通过。第二例是延迟交付，不宣称模拟了晚数据库提交或容器 SIGKILL。
- 实际出站适配原定向检查83项通过；审查发现同步审计写盘位于最终许可检查之后，可能越过许可期限。已把最终检查移至写盘之后、紧邻发送，并通过含慢写入回归的14项定向检查。
- 新评估工具16文件 Linux mypy、Ruff check/format通过。真实业务库以部署 reader 执行 `business_audit.sql`，七个业务事实表写权限均为false，40条工单/38条订单/37条物流记录及实际索引可读。

生产 Worker/云端镜像已实际构建，安装包源码摘要与登记 source 一致，固定 registry 与镜像无评分资料检查通过。运行前以 disabled profile 完成真实 bootstrap/inspect，实际账户匹配、用量和启动历史均为0。提交 `4f85470` 的[八项CI](https://github.com/XJfyrh/JobForge/actions/runs/35114357422)全部通过。没有修改 Claim/Reserve/Commit 热路径，本轮不重复上一 PR 的性能实验。[历史性能与残余开销](agent-v3-s1-provider-audit-2026-09-16.md)继续保留。

## 审查、限制与记录

独立审查还发现 driver 错用 FD 的 OUTPUT_INVALID 作为公开 Calls 领域码，以及 cloud overlay 未接入专属 Worker 凭据映射；均已修正。冻结提交 `4f85470` 的独立差量复核未发现新增P1/P2。实际运行随后暴露Worker退出诊断缺口和业务语义错误，不能由静态审查覆盖。元数据发送尝试不证明远端已收到，前后事实摘要单独也不能证明期间从未写入，需结合部署只读权限。

真实云端40案、生产长期留存、S2～S5仍未验收；W4历史门禁失败、AT-25跳过不被本轮覆盖。PR #50此前一次Worker退出超时未复现、根因未确认，后续CI通过不等于已修复。原始日志、构建和独立审查记录保存在仓库外 `E:\JobForge-notes\2026-09-16-agent-v3-s1`；业务正文和秘密不进入公共报告。
