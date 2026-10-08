# S5 免费实施证据（2026-10-07）

同源 Run 页面、实际 OTLP 关联、v3 面板、终态内容清理和停写双库恢复已完成免费机制验证。公平比较新增 schema 5 固定流程、schema 6 Agent，以及外部数据绑定、冻结校验和完整案例评分。S5 新收费调用为0；开发40案回归、策略冻结、正式20案、实模恢复费用对照和演示待运行。正式未见集尚未创建或打开。

公开数值与原工件摘要见[机器报告](agent-v3-s5-free-2026-10-07.json)；实现范围见[验收矩阵](../agent-v3/s5-acceptance.md)，操作见[页面与数据指南](../agent-v3/operations.md)。原 SDK/PG/outbound/trace、截图、双库 dump 和私有凭据保存在 `E:/JobForgeEvidence/s5-2026-10-07`，公开报告不含完整内容或签名。

## 实际机制与观测

安装 SDK、真实 Go/Python 进程及业务 HTTP/PG 产生方案；模型端使用显式合成响应。真实浏览器检查批准、拒绝、版本冲突、取消与 unknown，operator/approver 分工、同源限制及恶意文本输出均通过。批准后得到唯一业务回执；拒绝保留原方案且没有工单更新；版本冲突显示未确认结果和核对入口。最终小幅文案经源码独立审查，未重复三项已通过浏览器检查。

修复 SDK `traceparent` flags 校验后，3个 Run 的5次 attempt 均 link 到对应原 Submit span。Go RPC、真实 Python guardian 和业务 HTTP 形成关联；Python 模型/工具调用 span 是已核验 permit→observation 区间，不能解读为精确网络时间。人工审批约6分钟等待未进入有限 attempt。初轮缺失 link 的记录保留为修复前证据，使用修复后记录作结论。

实际 Grafana 页面和14条 PromQL 查询通过，包含运行状态、调用、预算、步骤和 CommitStep。Prometheus 配置与7组规则用例通过。collector 停止时免费生产执行链路在9.703秒内到达方案待审，步骤/调用一致；随后取消该 Run 并恢复 collector。

## 内容与恢复

真实 PG/race 检查证明：终态至少7日且批次到期才清理内容；活动、待审、unknown/held/frozen 保留；30日晚到计量仍走原身份与窗口。清理后旧 Submit 返回 REQUEST_EXPIRED，steps/result/GET与POST审批（包括已接受批准/拒绝重放）返回 RESULT_EXPIRED；原 actor/决定/操作和费用不变，没有新 enqueue。

业务快照、结论和完整身份/回执本阶段持续保留。停写双库演练先封锁源库应用连接并终止会话，再分别 dump，恢复到固定 pgvector 镜像、network=none 的新容器。控制31表、业务11表的行数及规范字节摘要全部相等，原 HBA 两侧恢复完成。HBA 还原失败的回归验证报告不会提前记录通过，并继续尝试还原另一侧。

恢复后的控制读取服务使用空 profile 集合、无 Worker。安装 SDK 重放原 Submit/批准/拒绝得到原身份，新提交 PROFILE_UNAVAILABLE；Run/步骤/审批/调用/账户/操作/回执不增加。业务克隆库使用真实 receipt reader、read-only transaction，以原签名重放得到原唯一回执；改参数冲突、新动作拒绝。其后只受控老化关系时间到31日，原签名与回执 JSON/hash 未改，再次重放及拒绝写入通过。备份逐表相等结论对应老化前快照。

## 本地检索与有限耗时

真实 `all-minilm:22m` / 384维、pgvector 与业务 HTTP 运行20条本地政策检索，19条 Hit@3，MRR@3 为0.8083333333。RQ-06 未命中保留。这是检索诊断，未产生云端模型调用。

机器为 Windows 11、Ryzen 7 7840HS、16逻辑核、约32GB内存，Docker Desktop Linux 容器。串行100次安装 SDK 只读 GET 的 p95 为17.184ms，费用快照不变。仅筛选修复后3个 Run 的5条 attempt trace，33次 CommitStep RPC 的 p95 为151.262ms，包含网络和 ACK；不混入初轮或 collector 故障样本。

另一次真实业务提交响应丢失后终止 Worker，经过自然30秒租约和回收，以回执优先完成；从终止到终态为31.015秒，attempt共3、回执查询2、业务写入1、恢复新增模型调用0。此单样本测量不代替实模恢复/从头费用对照。

## 工程检查与待执行项

- Windows 真实PG与Redis AOF：`go test -race -count=1 ./...` 通过，集成部分411.819秒；Linux专用测试和该轮未配置的独立业务PG层单列。
- 固定 Linux Go/Python、真实PG：新 schema 5 固定流程通过；自然业务提交丢响应恢复通过；恢复库 SDK 和业务 receipt-only 两项通过。
- Python/SDK/工具完整回归1853通过、13跳过；后续 launcher 观测参数改动的相关回归205通过、4跳过。跳过项来自平台/专用环境，未计通过。
- Go build/vet/golangci-lint、Buf lint、SQLFluff 历史基线与迁移检查、Ruff/mypy 通过。生产 Worker registry 检查为两个登记 adapter，无测试 hook 或 gold。
- 最新 PR head 的8项CI见[PR检查](https://github.com/XJfyrh/JobForge/pull/61/checks)。独立收费放行、开发回归后的策略冻结、正式20案及实际演示记录待完成；收费分批范围见[执行计划](../agent-v3/s5-paid-plan.json)。
