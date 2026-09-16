# S1 完整真实验收与交付

2026-09-17（UTC+8），固定开发集 **40/40 实际执行并完成导出，安全证据40/40完整、硬失败0**。业务正确率为 **11/40（27.5%）**，不是全部业务正确。正式运行源码为 `353ded3675ad8bf3d10519f66c3b916159ca9109`；本次实现与文档由 [PR #51](https://github.com/XJfyrh/JobForge/pull/51) 交付。S1范围闭合，S2～S5未启动。

[机器评分](agent-v3-s1-delivery-2026-09-17.json)、[部署与逐案元数据](agent-v3-s1-delivery-receipt-2026-09-17.json)、[复现运行指南](../agent-v3-cloud-batch.md)。公开报告只包含有界元数据；原始SDK响应、凭据和完整模型内容保存在仓库外。

## 实际链路与结果

本次是新的独立批次 `31e15e34-fb02-426a-8b73-9eef5a8287f6`，profile为 `support-deepseek-20260917-v2`，固定执行器 `linux-v2-audit-runtime-1`。单Worker串行，真实业务库 `jobforge_support_v2_20260916` 与原v2索引保持不变；新控制库 `jobforge_s1_delivery_20260917` 不覆盖两次历史运行。没有跨批拼接案例、修改gold/scorer或额外收费探针。

| 项目 | 结果 |
|---|---|
| 正式执行 | 40例实际接纳、调用、结束；未尝试0、中断0 |
| 状态 | 39个awaiting_approval方案，1个no_action结果succeeded；awaiting_approval只是持久方案完成，未批准、未写入、未解决工单 |
| 结构 | `{"passed": 40}` |
| 来源 | `{"passed": 40}` |
| 业务 | **11/40（27.5%）**，所有失败仍在40例分母内 |
| 安全 | **40例证据完整、硬失败0**；七张业务事实表前后count/hash一致，reader写权限全部false |
| 实际调用 | 40 chat（0次协议纠正）、118业务工具、40真实本地embedding、80metadata；共278物理调用 |
| 计量 | 87,109 known tokens，unknown chat 0，held tokens 0，measurement anomaly 0 |
| 延迟 | 全40例Run创建到完成导出时的状态更新时间，nearest rank：p50 3.734005s，p95 4.190391s，max 6.029242s；不是生产SLO |

每例的tenant、Run、snapshot/index/profile/price、持久步骤、调用ID、方案引用及耗时均在receipt中。driver使用镜像内安装后的SDK查询Run、步骤、Calls和结果，保留原执行时导出。之后Run达到原deadline的状态变化不反向改写完成时的方案证据。

业务错误统计（可重叠）：REQUIRED_CLAIM_MISSING：19；UNSUPPORTED_CLAIM：25；WRONG_ACTION：11；WRONG_CONCLUSION：5；WRONG_DECISION：4；WRONG_REQUESTED_FIELDS：4；WRONG_TARGET_TICKET_STATUS：7。完整案例理由见机器评分。S1是固定流程开发基线，没有额外正确率门槛；本轮不为提高分数重跑、不改标签，保留集及动态Agent质量属于后续阶段。

## 工程修复和费用决策

[连续短任务心跳饥饿](agent-v3-s1-heartbeat-2026-09-17.md)已有确定性红绿回归与历史会话事实；删除每次短Run后推迟idle heartbeat的代码，实际新批持续越过60秒会话初始期限并完成全部40例。[停止竞态](executor-stop-publication-2026-09-17.md)和[计量诊断/有界新批准入](agent-v3-s1-held-usage-2026-09-17.md)分别有独立证据。没有放宽lease、fencing、Commit/Wait/Join或原调用确认。

维护者明确授权按任务合理调整预算，预算本身不再作为外部阻塞。本次无需扩额：仍用原新增累计5 CNY核算，并按[ADR-0023](../adr/0023-held-unknown-cross-batch-admission.md)保留上一批完整hold，为新共享batch配置2,846,003 microyuan持久上限。旧unknown/report/账本/冻结/attempted和原始失败全部保留。

| 范围 | known费用（CNY） | 未释放monetary hold（CNY） |
|---|---:|---:|
| 原首批（旧授权单列） | 0.097526 | 0 |
| 上一收尾批 | 0.048653 | 2.105344 |
| 本次完整40例 | 0.137418 | 0.000000 |
| 新增授权累计 | 0.186071 | 2.105344 |

known是按登记费率逐调用向上取整计算的microyuan估算，**不是供应商结算账单**；hold是保守占用，不写成已消费或零费用。原首批另有512 embedding-token hold。按共享batch聚合一次，不重复叠加tenant/family镜像。模型/账号只读预检成功，官方价格HTML与前次快照相同；所有真正推理都走正式链路。

## C-01～C-08 验收映射

| 验收项 | 对应证据 |
|---|---|
| C-01 profile/费用 | 新冻结profile、价格/源码/配置hash、逐调用Reserve/report/usage及三账户有界预算；本次40案账本与评分一致 |
| C-02 正式进程 | Windows启动同一固定Linux镜像、init/单Worker；最终CI真实进程/race，具名容器实际退出且无重启/OOM |
| C-03 派发/计量 | 真实PG确认/故障CI；本次实际计量和方案Commit完整，旧unknown全hold和冻结仍保留 |
| C-04 真实业务链 | 40例SDK→控制面→正式Worker→真实业务HTTP/pgvector/Ollama→DeepSeek→持久方案/结果，逐案工具路径和来源可核对 |
| C-05 可评分方案 | 结构/来源/动作/claim分层计分，最多一次协议纠正，业务错误如实保留 |
| C-06 冻结开发集 | 独立审查的v2数据/标签和scorer摘要不变；运行镜像只含driver/export/launcher，不含gold/scorer，保留集未打开 |
| C-07 40例真实运行 | **40/40完整执行，安全40/40完整且硬失败0**，全部费用/延迟/Run/步骤关联保留 |
| C-08 工程/复现 | 最终代码八项CI、独立代码/准入/证据审查；[独立干净环境复现](agent-v3-s1-reproduction-2026-09-17.md)及完整运行/停止/清理指南 |

## 验证与交付边界

[代码CI35129644678](https://github.com/XJfyrh/JobForge/actions/runs/35129644678)绑定353ded3，8项全部通过，包含真实PG、全仓race、固定Linux进程、Python/SQL/Buf及可观测配置。此前a484147的[CI35129001572](https://github.com/XJfyrh/JobForge/actions/runs/35129001572)也为8项通过。最小诊断测试87项、共享费用cap真实PG竞争、心跳回归及独立审查分别记录；最终文档提交与必需CI绑定在PR中，不拿此前代码CI冒充最终提交检查。

复现环境已从独立空库完成迁移、v2数据/真实向量工件、安装SDK、租户/只读权限与停止清理；本次沿用该有效证据，不为复现再收费跑40例。具名验收容器已停止，原数据和证据保留。历史两次不完整云端报告仍可查：[首批](agent-v3-s1-first-cloud-2026-09-16.md)、[上一收尾批](agent-v3-s1-closeout-2026-09-17.md)。W4性能失败、AT-25跳过、RQ-06检索未命中（19/20）与生产长期留存未验收继续披露；本次不宣称动态Agent、审批写入、保留集泛化或生产稳定性通过。
