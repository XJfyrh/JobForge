# S1-A 真实业务与政策检索验收（2026-09-16）

范围为[PRD v0.8](../product/JobForge_PRD_v0.8.md)的S1-A，基于PR #37合并后的`dc22ac5`实施。代码、数据和命令见[业务指南](../agent-v3-business.md)。S1-B/C和整个S1尚未完成；本报告没有DeepSeek推理、Run账本、Agent决策循环或业务审批写入成功声明。

## 真实层

Windows Docker Desktop重启后，以独立Compose项目运行固定PG16.15/pgvector0.8.6和Ollama0.32.5。新建模型volume下载`all-minilm:22m`并验证digest；新业务库初始化独立迁移与角色，装载40工单、38订单、37物流、两个tenant的政策绑定。语料hash为`2b0fef040119960b504b8f55e64d9e8b73095e1b1c2943947abb1fbfb800d62f`，10份政策20个段落，全部在truncate=false下完成真实384维embedding。

| 验收 | 实际结果 |
|---|---|
| 新模型volume准备 | 2个embedding批次，4个版本/digest元数据请求，零失败；报告状态prepared，不等同PG publication |
| 原子发布与重发 | 两个tenant各20 chunks；重复装载同一profile/content返回首次index；更换prepare_id不产生重复索引 |
| 真实20查询 | 全部返回、0调用失败；Hit@3=19/20（0.95），MRR@3=0.808333；RQ-06未命中，保留原始排名而不调整gold |
| 物理重启 | 实际重启业务PG容器与HTTP容器；原snapshot/index/profile仍可查询；使用真实已生成P01.1向量重新搜索，top1=P01.1、distance=0 |
| 注册语料检查 | Python准备与Go装载均核对登记原始bytes/hash、chunk/source/text；不能仅伪造corpus hash发布其他文本 |
| 云端调用 | 0次chat推理；未使用费用账本之外的临时收费探针 |

首次准备曾复用已有11435模型服务，2个embedding批次成功；随后为干净复现新建11436模型volume再执行2批，输出装载文件hash一致。总计本轮准备4个embedding批次加20个检索query embedding；不把两个准备报告合并伪装成一次。重启检查使用已保存真实向量，没有新增embedding。报告中的requests字段是发送前持久化意图，未结算/中断场景不能据此推断精确实际调用量；这些正常返回场景均已结算。

[逐查询证据JSON](agent-v3-s1-retrieval-2026-09-16.json)保留20条排名、距离、版本引用、评分、准备信息及重启结果。删去重复政策正文以减少不必要内容，原始报告SHA256随文件记录，完整原始报告与向量装载文件位于仓库外`E:\JobForge-notes\2026-09-16-agent-v3-s1`。此开发小样本成绩不代表生产检索质量、40工单模型评分或未见保留集效果。

## 确定性、真实PG与工程检查

- Python SDK、S0模型/执行器guardrails、S1工具/准备边界及检索评分器合计153项通过；其中S1适配器59项。它们不调用真实模型。
- 真实业务PG/race已覆盖迁移up/down/re-up、未标记库拒绝、低权限角色、缺seed/index不就绪、12并发同键幂等、源更新后不可变、跨tenant/错关联隔离、完整版本升级、HTTP错误/大小边界。
- 并发一致视图使用pgx测试tracer屏障，12轮让另一个事务在快照读取order后、读取delivery前提交成组更新；快照每轮仍读取一致旧版本。没有靠sleep猜时序，也没有生产测试钩子。
- 新旧policy/index快照隔离、物流新增事件必须增加aggregate_revision、未知工具/错误对象拒绝均有直接测试。
- Build、vet、golangci-lint、Ruff check/format、SDK/新Python/旧Linux探针mypy、SQLFluff三条历史基线与全部新迁移、Buf lint均已执行通过；Prometheus配置及5条告警规则、Grafana再生成一致性检查通过。
- 全仓Windows `go test -race -count=1 -json ./...`退出0：405个测试通过事件、0失败、19包通过、11包无测试。[汇总](agent-v3-s1-windows-race-2026-09-16.json)明确列出5个测试skip：AT-25、两项旧真实模型专层未启用、两项子进程helper；它们不计验收通过。新Go↔Python真实HTTP契约已执行通过（4次HTTP、0模型调用），不是skip。极端范数保护补丁之后，又对业务/JSON/真实PG/Python契约完整定向race复验通过。
- 2026-09-16补记：[PR #38](https://github.com/XJfyrh/JobForge/pull/38)最终head `db8206cb563707a9d26036c3f66e75fc38c1134e`经过两份全新独立上下文审查，无剩余阻断；[机械CI](https://github.com/XJfyrh/JobForge/actions/runs/35059776718)七项及[原有真实模型工作流](https://github.com/XJfyrh/JobForge/actions/runs/35059776725)一项通过，已squash合并为`6924b71`。原有工作流不是新Agent或DeepSeek验收。

初审修复三处边界：写准备报告的fsync耗尽期限后仍可能发送请求；标准Go JSON接受大小写别名覆盖/null数值；pgvector对极端有限向量计算错误距离。分别增加发送前及返回前deadline复核、精确typed JSON形状校验、float32安全范数检查和回归测试。初版全局角色marker绑定随机测试DB导致重复环境不可重建，也在未发布迁移中修正为专属实例项目标记；数据库自身仍严格绑定用途和名称。

PR #38的全新独立审查进一步发现均匀极小分量可以通过float64范数检查；实际PG对384个1e-23分量返回错误余弦距离0。修正为显式float32乘法/累加并增加纯单测及实际HTTP 400回归，业务全套定向race与lint通过。检索评分口径同步固定为原始chunk排名，按policy ID判断相关；不对同政策段落去重后重新编号。

## 明确保留的限制

本切片未改变旧队列热路径，不新增定向性能对比或覆盖原门禁。**历史W4性能门禁失败、AT-25跳过、远程模型和生产留存未验收继续保留**。真实进程租约恢复仍只有已合并S0及旧任务证据；新的Run/步骤恢复属于后续阶段。动态Agent、审批写入、40例云端基线、20例保留集和新Agent观测展示尚未完成。
