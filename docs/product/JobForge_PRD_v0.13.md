# JobForge PRD v0.13：S1 首批收费 profile 与 40 案启动边界

- 日期：2026-09-16；状态：**Proposed，待独立审查与 PR 合并；未实现、未启用收费、未运行云端验收**。
- 基线：[PRD v0.10](JobForge_PRD_v0.10.md)、[v0.11](JobForge_PRD_v0.11.md)、[v0.12](JobForge_PRD_v0.12.md)；对应 [ADR-0021](../adr/0021-first-cloud-batch-admission-and-launcher.md)。
- 仓库核对基线为 support 实现 PR #48 的 `bffe566`。ADR-0020 已接受；provider 审计实现仍为独立工作，本文不声称其已合并或验收。
- 本增量细化 C-01/C-07/C-08 与 A-06/A-08 的正式部署、资源接纳和外部评估驱动，不改变固定 support 流程、方案 schema、计量合同或既有费用授权。

## 1. 用户可见行为

首批使用一个明确登记的收费 profile、一个共享 batch、一个 Go Worker 和预登记的 40 案。公开 Submit 仍只选择不可变 profile ID 和已授权 batch；调用者不能传入模型、prompt、政策、索引、endpoint 或执行代码。离线配置生成不是执行授权，也不证明数据库或供应商状态可用。

收费 profile 必须冻结固定模型/参数、策略/schema、prompt/adapter 版本、价格快照以及业务资源约束。控制面在捕获快照后、首次接纳 Run 前，核对工单/租户、政策版本与语料摘要、索引身份/内容/profile hash 和观察时点。资源不符不得产生可 Claim 的 Run 或新费用家族，不能等 Worker 已开始后再由 SDK 检查。

已接纳 Run 永远绑定原 profile/price/snapshot 身份。同一 Submit 或已接受 Retry 的幂等重放返回原对象，不重新捕获、不延长期限、不恢复额度。真正新的人工 Retry 仍继承原业务意图、profile 和全部账户；新快照必须满足同一冻结资源约束，不得通过 Retry 切换版本。本首批驱动不自动调用 Retry，也不通过新意图或新 batch 恢复停止的收费执行。

`awaiting_approval` 是 S1 **方案完成**的合法结束点：方案与审批绑定已持久，租约已释放，实际工单未写入。它不是 Run 的通用终态，更不是批准或 applied；驱动只导出方案并可转向下一案例。`succeeded` 的 no_action 也是正常完成。唯一纠正再次产生不合格方案时，仅符合 ADR-0020 原报告/普通观察/原步骤终态屏障的单例失败可计失败后继续；不能仅凭 `failed` 放行。

## 2. 可信部署证明与运行时约束

| 边界 | 必须证明或强制执行的内容 | 不作出的承诺 |
|---|---|---|
| 离线生成与登记 | typed profile、完整定义与 hash 自洽；固定 adapter/model/参数；control、Worker、manifest 的同一 profile/hash/version | 配置文件存在不等于 PG 已登记、账户未消费或模型请求已发生 |
| 部署取证 | 固定构建源码、prompt/schema、镜像/安装包摘要；固定受审 v2 seed、只读业务运行权限；不含 gold/测试 registry | snapshot 不携带整个 seed 或源码，不能从 snapshot/hash 反推这些事实 |
| 新 Run 接纳 | 以现有 SnapshotBinding 的真实字段逐项核对冻结资源；最终事务再核对 profile、账户、时限和重试关系 | 不增加 execute_step 字段，不把评测 case/gold 下沉到调度核心 |
| 既有执行与审计 | Go 独占许可；保留持久 chat guard、普通 ACK、Commit/退出屏障和原调用晚到确认 | 单 Worker、restart=no 或本地停止文件不能替代 PG 执行权，也不能证明 HTTP 原文 |

完整评分器及其反例、开发数据和评分规则必须在首个收费请求前独立审核并冻结。已有离线数据 validator 和 20 查询检索结果不能代替完整评分器或 40 案云端验收。审核主体按事实记录为 Agent 或人类，不虚构人工确认。

## 3. 批次和导出

继承 ADR-0018/0020 的唯一共享 **5,000,000 microyuan（5 CNY）**、固定 **6 小时**和 Worker/tenant/profile 容量全为 **1**。north/south 各 20 案绑定同一 batch；不把两个 tenant 上限相加为 10 元。原次数/token/单次保守预留不变，未知费用不退款；这些上限不是 40 案完成承诺。

首个 Submit 前原子保存全部 40 行、固定次序及每行稳定的业务键/幂等键。每行至多一次 Submit 尝试，外层只做有界只读轮询和 SDK 导出；提交响应不明确立即停止下一例，保留“已尝试但接纳未知”，不能写成未尝试或改 key 重发。驱动不 Claim、不恢复步骤、不批准方案、不调用模型或业务 HTTP。

固定生产容器单实例运行，`init=true`、`restart=no`。batch frozen、chat unknown、报告/控制确认不确定、Worker 异常退出、清理不确定、额度/期限不足或前置核验失败均停止后续提交及本地新工作。停止后只允许读取原事实并导出；没有自动解冻、增资、新 session、新 batch、换供应商或收费续跑入口。持久停发原因及窄终态例外完全沿用 ADR-0020。

保留全部 40 行，包含失败和未尝试；经 SDK 获取 Run/result/steps/events/calls，记录抓取时点与导出摘要。区分观测 tokens 与已结算 tokens、known 与 held、历史未采集与缺报告；晚到变化追加新导出，不改写早期取证。受保护步骤内容仅进入仓库外受控评估归档，普通日志/Trace/公开总结不保存完整敏感正文。安全成绩由实际调用、角色权限及业务写入核对证明，不能采信模型自报。

## 4. 验收映射

固定 launcher 与 Worker/SDK driver 同属一个 init/restart=no 容器，互相失联时由 launcher 停止两子进程；launcher 死亡由容器生命周期回收整个 PID namespace。启动须核对专属 Worker/session 与本批接纳历史，零用量不等于从未启动；部署绑定的固定持久尝试标记与排他锁不能用新输出目录绕过。具体规则见 ADR-0021 §5.1。L-04～L-06 须实际覆盖 driver 在 Submit 已提交但首 chat 预留前死亡、launcher 死亡，以及零用量旧 session/ready Run、丢失状态卷、新目录和并发启动的拒绝路径。

| ID | 要求 | 必须实际验证的事实 |
|---|---|---|
| L-01 | 确定性可信 profile | 离线无 DSN/凭据/网络依赖；完整定义、价格与 profile 的精确 hash 向量；篡改任一冻结项、混 version/adapter/model、未知字段均拒绝；启用开关不改变身份 |
| L-02 | 接纳前资源校验 | 正确 v2 快照通过；错 tenant/ticket/policy/revision/corpus/index/content/profile/观察时点在创建可执行 Run 前拒绝；注册校验不能只信来值 hash；真实 PG 无残留接纳/家族/额度变化 |
| L-03 | 不可变身份与重试 | 已接受 Submit/Retry 在 profile 禁用、依赖不可用及期限过后仍可重放原结果；新 Retry 继承同账户/profile并拒绝错资源；并发接纳与直接 Store 新建入口不能绕过；不因重放重捕获 |
| L-04 | 共享 batch 与部署 | 两 tenant 同一 5 CNY/6h batch，三层上限不重置；bootstrap 幂等保持 frozen/usage/期限；正式镜像单 Worker、init/restart=no、容量全 1、gold 与秘密隔离 |
| L-05 | 40 行串行驱动 | 先落全部 40 行再首次提交；超时/不明确响应/本地记录失败后下一 Submit 为 0；proposal 在 awaiting_approval 完成评估等待、业务未写；普通单例失败与停批严格区分 |
| L-06 | 停止不可绕过 | 沿用 A-06 的 PG 持久 guard 与完整第二纠正失败例外；停止/崩溃后不自动重启或 Retry；第二 driver/新 session 不能绕过未完成 chat 屏障；本地未知不冒称 DB frozen |
| L-07 | 可核对完整导出 | 实际安装 SDK/HTTP 覆盖所有 40 行及 calls 的 typed/null/hash/大小边界、完整有限分页、captured_at、追加晚到快照、租户隔离；unknown/held 不伪装成零账单 |
| L-08 | 分层真实验收 | 先完成审计桥、完整评分冻结、确定性 PG/HTTP 与正式 Linux 进程故障，再在原额度内执行真实 40 案；40 案均实际执行且安全硬失败为 0 才满足原 C-07，业务错误逐例报告 |

实施顺序与具体负例见[精简实施映射](../plans/agent-v3-first-cloud-batch.md)。本稿不增加 API/Proto/Run 状态，不增加价格/模型探测请求，不承诺额外质量百分比。任何停批导致少于 40 案实际执行时，保留原分母并明确 S1 未完成。S2～S5、生产长期留存与历史 W4 失败/AT-25 跳过不受本增量改变。
