# 首批真实 DeepSeek：未通过，已停止

> 阶段历史记录。2026-09-17 的修复合并、累计新授权及实际重验见[最新收尾证据](agent-v3-s1-closeout-2026-09-17.md)；本页原批事实不改写。

实际运行于 2026-09-16 23:20:23～23:21:49（UTC+8），实现提交 `4f854702212993aba8aa897bca31d97f2643c508`。[机器评分](agent-v3-s1-first-cloud-2026-09-16.json)、[运行与证据摘要](agent-v3-s1-first-cloud-receipt-2026-09-16.json)均来自实际批次；原始 SDK 响应、步骤与方案保存在仓库外受限目录，公共报告不含正文或秘密。

**本批没有完成 S1，PR #51 暂不合并。** 不以机械检查通过代替业务验收；没有自动恢复 Worker、换 batch、补跑剩余案例或放宽评分。

## 实际结果

| 项目 | 结果 |
|---|---|
| DeepSeek 运行前检查 | 模型列表/账户 HTTP 200，固定 deepseek-flash 可用；这些只读检查不是推理验收 |
| 固定40案例 | 16个Run已接纳；15个执行结束；第16个中断；24个未尝试 |
| 已结束15个 | 10个 awaiting_approval 方案；5个二次纠错后失败。执行结束不等于业务正确 |
| 实际云端调用 | 15个案例共28次chat，13次纠错；计量完整、无unknown chat或measurement anomaly |
| 业务评分 | 0/40；固定分母包含24个未尝试。10个方案均有不支持的额外claim；其中一个还有错误结论/缺必要claim |
| 费用 | 登记高峰费率估算97,526 microyuan，即0.097526 CNY；不是供应商账单。总known tokens为52,701，另有512个embedding token hold，人民币hold为0 |
| 安全证据 | 15案验证通过、0个已确认安全硬失败；第16案出站证据不完整；前后业务事实摘要一致且实际reader无事实表写权限 |
| 启动器 | 容器退出1、非OOM、0次重启；两个子进程完成Wait。3秒内Wait成功不等于Go内部所有清理合同已成功 |

评分器退出0仅表示报告生成。`actual_acceptance_evidence_complete=false`，不存在完整40案通过结论。

## 两个待处理问题

**运行时退出原因缺证据。** DEV-016 的 `query_embedding` Reserve于15:21:48.658448Z提交；此前metadata请求返回200并有普通观察。launcher于15:21:49.782786Z检测退出，49.897003Z完成Wait。此调用未保存普通观察和出站元数据；元数据是尽力记录，文件缺失不能证明没有发送。该案没有chat预留。15:22:18.37986Z的 `LEASE_EXPIRED` 是之后的回收结果，不能作为原退出原因。

Worker stdout/stderr此前被launcher丢弃，停止文件没有保存Worker退出码、触发进程和内部固定原因。因此无法区分内部清理、控制确认、执行权限或其它退出，不能归咎于DeepSeek。后续只读取证确认driver记录OPERATOR_STOP、最后SDK读取均成功；但旧记录仍无法区分Worker先退出与launcher收到外部信号。指定时段的Docker历史事件当前查询为空，不能据此排除外部信号。保留原Run、账本、hold和证据，不重启这批Worker。[后续已补最小退出诊断](agent-v3-s1-cloud-fixes-2026-09-16.md)，不把诊断改动等同于根因已修复。

**模型存在业务语义错误。** 独立上下文只读抽查DEV-001/002：两例来源链、登记绑定、持久方案/result、顶层动作/结论和timing均通过；额外ticket_status主张不成立。DEV-001实际工单为open，却主张informational_no_action；DEV-002同为open，却主张preserve_escalated，引用政策也不支持。按ADR-0018，任一不支持的额外主张使整例失败；两例未发现scorer接缝错误，不放宽评分。后续应完善业务提示/约束和纠错反馈，不把评分规则或gold挂入Worker。

## 工程检查与边界

该实现经独立上下文审查，三项P2已修复；[CI 35114357422](https://github.com/XJfyrh/JobForge/actions/runs/35114357422)八项全部通过，包含全仓race、真实PostgreSQL、Python、SQLFluff、Buf、实际Linux进程及观测配置。运行前后的所有SDK读取只追加导出，没有重复Submit或模型请求。

按维护者“遇阻塞停止后报告”的要求，停止收费执行并记录本次失败。后续批次/复现需基于已修复的问题和明确运行安排，不能删除attempted标记绕过原合同。S1整体、S2～S5、生产长期留存仍未验收；历史W4性能门禁失败、AT-25跳过以及PR #50此前未定位的CI超时继续保留。
