# S1 首批收费合同的实施与验收映射

2026-09-16 历史实施映射；**随 [PRD v0.13](../product/JobForge_PRD_v0.13.md) / [ADR-0021](../adr/0021-first-cloud-batch-admission-and-launcher.md) 经 PR #49 独立审查通过并合并时接受**。接受当时只拆分增量工作，未启用 profile、未运行服务或收费请求；support PR #48 为当时代码基线，ADR-0020 审计实现当时仍待交付。下文保留该首批实施顺序与验证要求。

**2026-09-17 当前状态：PR #50 审计、#52 停止竞态修复、#53 / [ADR-0022](../adr/0022-s1-closeout-cumulative-authorization.md) 新批次累计授权已合并；PR #51 保持 Draft，S1 未完成。** 原首批及修复后新批次均已实际调用 DeepSeek，但未完成 40 案。新批次 14 案 `awaiting_approval`、DEV-015 中断、25 案未尝试，业务 9/40；安全证据 14 案通过、1 案不完整、25 案未执行。中断调用在 HTTP 200 响应头后未取得完整正文，以 `CHAT_USAGE_UNKNOWN` 停批，新增已知 48,653 microyuan、hold 2,105,344 microyuan。必要确认不完整，不得依新授权换 batch 绕过；详情见[收尾证据](../evidence/agent-v3-s1-closeout-2026-09-17.md)。S2～S5 未开始，旧批及全部历史证据保留。

## 切片与最小落点

| 顺序 | 最小实现范围 | 验收映射与关键负例 |
|---|---|---|
| 1. 可信配置与接纳 | `cmd/agent-control` 离线 prepare-support/只读 inspect-support；`internal/run` typed support definition/hash/纯快照校验；Service.create 与 Store.Admit 新建分支；businessclient 的 as_of 一致性 | L-01～L-03：整份 hash 向量、固定参数与未知字段拒绝；逐项错 policy/corpus/index/profile/time；新 Submit/Retry 无 ready Run/新家族/额度变化；已接受回放不重捕获；直接 Store、并发与来源继承 |
| 2. 单 Worker 部署与驱动 | 独立 opt-in compose；复用生产镜像和原 Worker；`tools/support_evaluation` 固定 40 行 SDK launch/export；只更新需要的运行指南 | L-04～L-07：同 batch/6h/容量1、bootstrap 不重置；40 行先落盘；提交未知/记录失败下一提交为0；awaiting_approval 完成方案但0写入；只读导出/晚到追加；停止后仅 export |
| 3. 依赖汇合与真实批次 | 已接受 ADR-0020 的持久审计/guard/跨 FD 桥；完整 scorer 及数据独立审核冻结；正式安装包与实际进程联合验证 | L-06/L-08：沿用 A-01～A-08 与 C-01～C-08；正式生产路径通过后，在原5 CNY/6h批次运行40案，全部失败/未尝试保留 |

不改 API/Proto/execute_step 输入，不新增 migration、调度实体、收费恢复或审批执行。具体新增 Go/Python 文件名由实施 PR 选择；原逻辑尽量复用，同一个 pure helper 负责资源比较。独立 scorer 是既有 ADR-0018 工作，不能以此合同或数据 validator 代替完成。

## 检查分层

| 层 | 必须运行的检查 | 不能据此声称 |
|---|---|---|
| 纯离线 | prepare 无 DSN/凭据/网络；两次输出稳定；精确 price/profile/index 向量；完整 Definition 变更和混版本拒绝；批次/key/6h边界；40行/导出记录逻辑 | PG 已登记或真实模型通过 |
| 真实 PG | 不可变登记；错 snapshot 接纳无副作用；Submit/Retry 接受回执优先；同来源并发单后继；禁用后旧回放/晚到确认；重复 bootstrap 不改 frozen/hold/期限 | 外部模型/正式进程验证通过 |
| 实际 HTTP/安装 SDK | 跨租户与权限；分页完整；calls bounded typed/null/known/held；awaiting_approval 的真实方案读取；提交断连停下一例；导出未知行不伪造零费用 | 合成响应是云端40案结果 |
| 固定 Linux 生产进程 | 相同 profile/manifest/adapter；init、restart=no、唯一Worker、gold/测试registry隔离；实际FD/PG/gRPC故障、旧组消失；Reserve/report/Observe/Commit前后丢ACK与新session/第二driver guard；正确二次纠正失败例外 | 本地停止就等于PG冻结，或S3完整恢复验收完成 |
| 真实云端 | 官方合同/账号前置、固定v2业务数据/索引、完整scorer冻结、一次既有批次；实际calls/steps与业务零写入证据；全部40行评分及费用 | 提升预算、少于40案也算C-07、或S2～S5完成 |

工程实现按仓库要求完成适用 race/lint/SDK/容器门禁；Windows 的非 Linux skip 不计正式进程通过。真实 PG 只使用协调者分配的可重建测试库，不能并行清理同一 DSN 或以收费库做测试。

## 启动和退出证据

固定 launcher 是 init 的主子进程，在一个 restart=no 容器内拥有唯一 Worker 和 SDK driver；driver 无 Docker/控制 RPC 权限，任一子进程退出/未知都触发另一方有界退出，launcher 死亡由容器 PID namespace 回收。真实故障需在 Submit 已提交但首 chat 未预留时杀死 driver，另杀死 launcher，分别记录检测、各派发方停止屏障和实际组消失。只有派发方停止确认后才能断言不发新 Claim/Reserve、不消费迟到许可/不开始新 HTTP；检测前在途控制 RPC 可随后提交，原已交付命令/HTTP 按可能发送和 hold 保留。无已交付许可的确定性注入断言 HTTP 为零；另用延迟 Reserve 提交验证真实预留不回滚、迟到许可不消费。固定状态卷中的持久 attempted 标记和排他锁、inspect 中的本批 Run/intent/call 与专属 Worker session 历史共同拒绝重新 launch；增加零用量已有历史、卷缺失、新输出目录、并发启动负例。该状态只管启动/取证，不新增 Run 调度实体或 DB 执行权。

启动前受审非秘密清单应覆盖固定源码/prompt/schema/镜像/安装包、runtime seed/索引资源、完整评分与40案意图键、实际 PG setup receipt、同一共享 batch 和明确起止。默认 profile disabled；离线生成与只读预检不启用它、不新增外部调用或授权。

每行区分：未尝试、Submit 已尝试但接纳未知、已接纳、方案完成/业务失败及停批；这些只是外部评估分类，不增加 Run 状态。先写全部40行，再一次尝试一行；不自动 Retry。awaiting_approval 是 S1 方案完成，批准/业务写入仍属于 S4。一次模型业务失败是否解除收费屏障由 ADR-0020 的 PG 原事实判断，驱动不复制内部执行权。

任何 stop/未知时保存完整40行和取得的事实，不伪造回执、Commit、退款或DB冻结，不更换 batch/意图键继续。只读 export 可以追加晚到报告的新抓取时间/hash，不能覆盖先前证据。实际调用/费用/质量结果单独追加 evidence 文档；本合同不填任何通过成绩。
