# ADR-0027：S5 观测关联、内容留存与停写恢复

- 日期：2026-10-07（Asia/Shanghai）；状态：**Accepted，2026-10-07独立方案审查接受，随本PR合并生效**。
- 对应：[PRD v0.19](../product/JobForge_PRD_v0.19.md)。
- 增补：ADR-0013/0014 的 TraceContext、0017 的 S5 清理边界和0026的回执身份留存。仅在受保护内容实际清理后，取代0017/0026“已接受Submit/审批返回完整原Run/方案”的响应形式：Submit返回REQUEST_EXPIRED，GET/POST approval（包括已接受批准/拒绝重放）返回RESULT_EXPIRED。原决定、actor及操作身份保留，不重新派发；未清理记录仍按原合同幂等重放，不因retry窗口或批次到期被拒绝。历史profile、原状态机、审计/预算和签名内容保持。

## 选择与接缝

复用 Go 同源 HTTP、既有 OTel/Prometheus/Grafana、Run/attempt/步骤/调用账本和独立业务库。静态页面没有新框架或客户端权限系统，Python仍只执行预注册单步，Go持有执行权与业务写入能力。

本决策前 HTTP/SDK 已传 TraceContext，Worker/Python 缺少完整导出链路；控制与业务表有大量非级联外键，不能按7日直接删除 Run。unknown/full hold、冻结和30日晚到审计需要原调用、profile、attempt及账户身份。

## 1. 展示与观测

静态资源由控制服务 `/ui/` 提供，鉴权API仍使用显式Bearer。凭据只存页面内存；所有内容经textContent/DOM文本节点输出。CSP限定self、禁止内联脚本与框架嵌入。浏览器变更请求拒绝异源Origin；没有GET批准或cookie自动批准。现有非浏览器SDK无Origin请求保持。

请求span仅覆盖实际API处理。提交traceparent在Run薄元数据中持久保存；每次attempt产生有限的新root span并与原接纳建立link，Run ID和attempt_no只作trace/log属性，不保存前attempt上下文。各步骤、内部RPC、Python执行与外部调用桥接span建立父子关系。人工等待无span；审批请求结束后，后续Claim仍关联原提交及同一Run。TraceContext不参与业务指纹或费用身份。`GET /v2/identity`只返回当前受信tenant/role/actor，供页面呈现，不能赋权。

导出使用现有Go有界异步队列/短超时，不为collector故障重试业务或改变Run状态。Python不新增到collector的出网入口或新的观测协议：Go用实际guardian启动/清理区间记录Python执行，将其traceparent放进既有execute_step；Python沿既有HTTP出口转发。Go用原Conversation核验的call_permit/call_observation记录每次外部调用的许可至观测区间，标记verified_ipc_go_bridge、permit_to_observation；许可不证明已发送，缺失观测保留unknown，不能冒充精确网络耗时或供应商内部trace。业务服务的真实HTTP span继承Python执行context。既有IPC尺寸/数量及Go导出队列有界，观测背压可丢span但不能丢或修改许可、审计与执行结果。诊断不含endpoint、header或响应正文。模型/工具只记录固定能力、状态、调用身份，不导出完整payload或秘密。

指标标签限于状态、固定步骤/调用类别、固定结果及稳定错误分类，Run/tenant/actor/operation/trace ID不作label。PG快照提供积压、运行/审批等待、Worker存活和known/held/frozen；事件或请求路径提供步骤耗时、恢复、调用拒绝与动作结果。新增独立v3观测Compose profile，不将旧Job面板当成v3执行证据。

## 2. 薄记录与内容清理

选择**内容过窗，身份墓碑持久保留**，不删除整个Run家族或级联账本：

| 数据 | 清理规则 | 保留事实 |
|---|---|---|
| 终态Run/checkpoint/方案 | 首次终态后至少7日且原费用批次已到期；每批有界，锁Run后重新检查state/时间 | Run状态/首次结果引用、步骤身份/hash、attempt、操作键、原profile/snapshot身份 |
| 活动/待审批Run | 不进入终态TTL；总期限先由现有状态机收敛 | 完整checkpoint与授权 |
| 业务快照/结论内容 | 本阶段完整保留，覆盖至少30日；没有业务内容删除入口 | snapshot/request键、hash、版本与操作/回执身份及完整内容 |
| 调用与审计 | 本阶段保留薄调用/审计/原attempt，覆盖30日晚到窗口 | 原principal/session/binding/profile、known/held、冲突与冻结 |
| 账户/业务键/操作/回执 | 本阶段不删除身份、完整签名授权/回执JSON或累计余额 | old-key防重、完整授权/回执及原hash/效果 |

新增versioned迁移显式记录内容过窗标记及首次终态时间，已有终态保守以迁移/可证明最后更新时间起算，不倒推更早日期。step output允许过窗后空值，但保留原commit hash，不能将空内容伪装成原始结果。清理不改变终态、游标、计数、known/held或冻结。

GET Run继续返回薄状态/预算及内容可用性；已清内容的steps/result/approval返回固定HTTP410 `RESULT_EXPIRED`。`REQUEST_EXPIRED`仅用于已实际清理受保护内容的原Run对应Submit重放；未清理内容仍按原合同返回原对象，即使7日retry窗口、profile或batch已到期。清理后的旧业务键/操作键不会capture新快照或开户。过窗retry不能新建后继；已接受retry仍可返回既有薄对象，不重开执行。既有身份仍可鉴权后查询。审批/动作不可用不因内容清理重新变pending。完整签名与回执独立保留，effect/reconcile/原操作POST重放不会因checkpoint过窗伪报NOT_FOUND。新错误同步OpenAPI/SDK与正反例。

事务保持原锁序，控制清理只锁一个终态Run和必要子记录，不反向获取账户/业务请求锁。生产提供受信显式清理命令，默认dry-run、有界batch和目的标记核验；不新增全库清扫daemon。S5不删除业务快照、结论或回执，也不按控制内容清理结果推断业务引用已消失。未来若增加业务内容删除，需要新决策解决跨库引用与停写门禁；不能采用在线跨库检查后删除。

## 3. 停写一致备份与恢复

采用显式停写演练：停止接纳/扫描/Worker/业务writer，等待本地执行组清理，确认双库无应用写入者，随后逐库pg_dump。停写期间双库状态不再变化，备份manifest保存source/build、迁移版本、dump SHA-256与样本校验摘要。不实现在线跨库2PC备份或PITR。

只恢复到新建目的标记库。恢复配置保持profile disabled、无Worker/写凭据；数据库恢复不自动bootstrap清零、放出收费工作或业务写入。核对全部样本Run/步骤/操作/回执/三层账户/unknown及冻结，再测试旧身份重放不产生新效果。备份包含受保护内容，文件在仓库外受控目录，仓库只放脱敏摘要/路径。任何恢复后的重新执行需要单独明确授权，演练不授予它。

## 4. 公平比较的独立版本

历史 schema 1 的固定流程消息上限16KiB，Agent为64KiB，直接比较会混入输入限制差异。新增 schema 5 固定流程，沿用 `support-fixed-v1`、原工具图/提示词/一次纠错及统一预算，消息/请求上限64/128KiB，与 Agent 一致。使用独立 `linux-v2-fixed-comparison-runtime-1`，Python从受信 manifest / 原execute_step中的固定版本选择上限，没有新增IPC字段、adapter或模型入口；旧版本上限不变。

新增 schema 6 沿用 schema 4 的 Agent恢复/审批/执行器能力，与 schema 5 同样允许已绑定的S5数据包身份 `support-s5-2026-10-07-v1`，保留原政策与业务观察时点。两者也可先在原40案开发包回归。工具、模型参数、token/费用上限相同，固定图和Agent工具选择次数的实际差异属于策略结果。模型/价格核验日期和完整源码/数据/构建/评分摘要进入各自新profile；1–4的定义、已登记身份与历史工件不改。正式20案在开发回归后冻结策略，再创建/打开，任何后续策略修改使该集成为已见集。

## 后果、替代与验证

内容可过窗而薄身份持续增长，适合本轮小型演示；本决策不宣称生产全量历史容量已治理。暂保留薄回执比删除后重建身份更简单，也避免丢失晚到审计和旧键防重。直接删Run/账本、退款unknown、仅备份一库或恢复后启动默认收费Worker均不采用。

按[S5矩阵](../agent-v3/s5-acceptance.md)验证真PG期限/并发/外键/usage、浏览器批准拒绝冲突、真实OTLP与Prometheus/Grafana、collector失联、双库新库恢复与旧身份防重。全仓race和最新head的8项CI是工程门禁；真实模型和未见质量验收另列。
