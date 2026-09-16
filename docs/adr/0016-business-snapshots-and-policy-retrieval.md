# ADR-0016：独立业务快照与版本化政策检索

- 状态：Accepted；随 [PR #37](https://github.com/XJfyrh/JobForge/pull/37) 于2026-09-16合并接受；S1-A实现验证见[证据](../evidence/agent-v3-s1-business-2026-09-16.md)。
- 日期：2026-09-16。
- 关联：[PRD v0.8](../product/JobForge_PRD_v0.8.md)、[ADR-0013](0013-durable-agent-run-and-step-commit.md)、[ADR-0014](0014-supervised-python-executor-and-call-budget.md)、[ADR-0015](0015-approved-business-actions-and-receipts.md)。
- 范围：细化S1业务读路径、资源准备及新依赖；不改变已接受Run状态、审批或费用预留语义。

## 上下文

现有 `internal/tasks` 使用PG保存JSON向量产物并在Go内计算余弦排序，模型探针工具使用fixture；它们不是新工单业务HTTP或pgvector。新阶段需要真实且可版本核对的业务事实，又不能将业务表、模型协议和规则塞入执行队列状态机。

## 数据库与部署

1. 新增独立业务HTTP进程和专属业务数据库，业务表位于 `business` schema；控制表仍由控制服务独立管理。相同PostgreSQL技术不等于业务与控制共享数据库权限。
2. 业务库新增独立versioned migration序列及追踪表 `business_schema_migrations`，目录 `migrations/business/`。不改现有应用过的SQL，不在普通队列启动中执行pgvector迁移；禁止将核心迁移版本号和业务迁移版本号混用。迁移入口必须验证目标数据库的业务标记。
3. 固定依赖为 `pgvector/pgvector:0.8.6-pg16-bookworm@sha256:ccc6e83d6e35e931dc7c5def2022729d5a6c370318d099181995567ff1fb4d6b`；pgvector采用PostgreSQL License，PG16。扩展安装于仅迁移身份可写的 `extensions` schema，并校验实际extversion=0.8.6。只做精确余弦距离搜索，不建HNSW/IVFFlat或额外向量服务。[官方项目](https://github.com/pgvector/pgvector)
4. 分离迁移/种子操作者与运行时数据库角色；schema由NOLOGIN owner持有；操作者可迁移/装载，运行时仅授予明确的schema USAGE及表权限，安全search_path不包含任意可写schema。运行时无DDL、角色管理、其他数据库/控制表权限。公开业务读服务只能读取事实/索引并按约束创建不可变快照，不能任意更新订单或物流。未来S4写入使用单独受信身份。
5. 新Compose部署使用独立项目、数据库和volume，loopback开放验收端口。固定模型/业务准备完成前启动失败应明确报告，不能返回fixture。现有v0.6部署不与新Run调度器并行组成双队列；S1-A仅为独立业务依赖，不是第二个调度器。

## 业务事实与快照

表族至少包括工单、订单、物流聚合/事件、政策版本、政策chunks、已发布索引和业务快照。每条事实具有tenant与版本，工单绑定订单；没有订单关联与查不到其他tenant资源是不同情况。物流新增事件必须增加聚合版本。

创建快照时在同一个repeatable-read事务中读取工单、订单、物流、当前政策与已发布索引，并保存有界的不可变副本、版本向量、as_of和内容hash。没有可用索引返回DEPENDENCY_UNAVAILABLE，不发布不完整快照。所有后续工具只读取该snapshot，源表更新不改变已有快照。

`tenant + request_key` 唯一约束是快照创建幂等点。请求hash绑定ticket_id与schema版本；相同请求返回首次快照，即使当前业务已变化。并发唯一冲突或序列化冲突进行有界事务重试后读取首次记录；同键异输入返回CONFLICT。需要新事实版本须使用新的业务请求键，不能覆写旧快照。

快照是受保护业务内容，不进入日志或普通Trace。事实引用格式为 `business-evidence:<snapshot_uuid>:ticket|order|delivery`，缺失事实也返回该范围内明确的可核验结果；政策引用为 `business-policy:<index_uuid>:<chunk_id>`。任何引用的查询仍校验tenant与快照绑定，知道引用本身不授予访问权限。

## HTTP与三个只读工具

服务使用部署配置的Bearer key→tenant映射。操作者接口和在线只读身份分开；tenant不从body、query或模型输出取值。S1-B控制服务在确认工单授权后绑定snapshot，Python只能取得当前Run授权的资源及必要业务只读凭据；固定工具适配器不接受任意URL或凭据。

| 接口 | 输入/语义 |
|---|---|
| `POST /business/v1/snapshots` | 操作者/控制身份；严格JSON `schema_version=1, ticket_id, request_key`；创建或复用快照 |
| `GET /business/v1/snapshots/{id}` | 返回当前tenant的快照元数据与受保护工单；限制响应大小 |
| `GET /business/v1/snapshots/{id}/order` | get_order；只允许快照关联订单，返回订单副本或明确missing事实 |
| `GET /business/v1/snapshots/{id}/delivery` | get_delivery；只允许快照关联物流聚合，返回副本与aggregate_revision |
| `POST /business/v1/snapshots/{id}/policies/search` | search_policy内部路径；接收384维有限非零query向量与固定embedding profile；服务端固定tenant/index/top-k，不接受客户端索引或tenant覆盖 |
| `GET /business/v1/snapshots/{id}/evidence/{kind}` | kind白名单ticket/order/delivery，租户内解析事实引用 |
| `GET /business/v1/snapshots/{id}/policies/{chunk_id}` | 仅查询快照绑定已发布索引中的chunk，校验tenant与索引版本 |

Python的模型可见工具参数为get_order/get_delivery的关联order_id或缺失订单约定，以及search_policy的query文本；适配器校验参数并用Go提供的snapshot绑定HTTP调用。order_id不同于快照关联对象直接拒绝。query不拼URL/SQL，向量由固定embedding适配器真实生成，不由模型提供。

严格解码拒绝未知字段、尾随JSON、错误类型和超大请求。创建请求≤4KiB，搜索请求≤32KiB，工具响应≤8KiB，query≤512 UTF-8字节，top-k=3。默认服务并发上限8、每请求10秒，超过容量立即429；网络不在数据库事务中。数据库超时/内部错误返回固定脱敏code，不记录正文。

错误封装保持 `{"error":{"code":"...","message":"..."}}`：400 INVALID_ARGUMENT、401 UNAUTHORIZED、403 FORBIDDEN、404 NOT_FOUND（含跨tenant）、409 CONFLICT/PROFILE_UNAVAILABLE、429 RATE_LIMITED、500 INTERNAL、503 DEPENDENCY_UNAVAILABLE。需要暴露已过期/无效snapshot时使用固定错误，不返回SQL详情。

## 索引准备与检索

准备流程使用登记的固定英文政策语料，按段落切分且每chunk≤768 UTF-8字节；语料总量≤64KiB，chunk≤64。profile绑定语料hash、政策版本、分块版本、384维、模型名和manifest digest。

真实免费embedding复用已验收的 `all-minilm:22m`，digest `1b226e2802dbb772b5fc32a58f103ca1804ef7501331012de126ab22f67475ef`，Ollama0.32.5，镜像digest `sha256:4dea9fb511947e24a84237bb636b0203abcb2ff0d3fbc7b4ff865deb91362131`。MiniLM模型许可Apache-2.0，初版验收语料为英文；模型上下文256token，字节限制不作为token上界，超限必须由truncate=false明确拒绝，并在真实语料验收中验证全部分块可嵌入。Python适配器每次发请求前核对digest；禁止截断、重定向和隐式重试，校验向量数量/维度/有限值/非零范数。模型不可用或不匹配即明确失败，不能以手工向量验收。[Ollama API](https://docs.ollama.com/api/embed)、[模型](https://ollama.com/library/all-minilm:22m)

资源初始化是独立、受信操作者执行的固定命令，不是Run、队列或后台调度器；只接受仓库登记语料与固定免费本地模型。最多4个embedding批次、每批最多16chunks、每请求60秒，总期限300秒；开始前在操作者指定的输出目录原子落盘prepare_id/profile准备报告，随后输出有界的向量装载文件，由仅操作者可用的固定Go装载命令校验后提交业务库；报告不是Run事实源，权威已发布索引只在PG中。重复发布依靠tenant+profile唯一约束复用首次已发布索引。实际调用/失败计数与版本进入准备报告；未能结算的准备保留unknown，不能把本地文件当成已发布索引。重复未完成准备允许重算免费embedding，已发布索引不被覆盖。

此准备身份仅用于免费、本地、固定资源初始化；不允许云端embedding或chat凭据，不授予在线业务执行权。将来若加入付费资源准备，必须先扩展已接受Run/批次预算协议并复审，不能沿用本入口绕过费用预留。在线query embedding仍由Run控制流程约束次数/时间，属于三个只读工具的内部动作。离线检索验收仅允许固定20条免费本地查询、每条一次embedding和一次检索、总期限1200秒；报告真实调用/失败数，不接受动态云端endpoint或凭据。这是资源验收命令，不接收用户任务或后台恢复。

所有向量先在事务外生成并校验，再在一个短事务中保存索引元数据、chunks和向量，并发布索引。快照只绑定完整已发布索引；同版本异内容冲突，重复准备返回首次索引。政策升级保留旧索引以服务已有快照。

查询参数绑定 `embedding operator(extensions.<=>) $query::extensions.vector(384)`，在SQL中同时限定tenant和快照绑定index；按距离与chunk_id稳定排序，返回最多3条带版本引用的文本。小语料不创建近似索引，便于核对精确top-k；检索性能不外推到生产规模。

## 验证与替代方案

真实PG验证迁移up/down、角色边界、快照幂等/并发、源数据更新后的不可变读、tenant/关系隔离、policy版本/profile与无效向量拒绝。HTTP/SDK工具测试走实际服务；纯数据库测试可用明确标记的合成向量，但不能替代真实embedding验收。固定20条检索查询报告原始命中和失败，保存模型/语料/index身份，实际重启后检查产物。

替代方案：继续JSON内向量可减少依赖，但无法兑现本轮pgvector契约；远程向量库增加凭据/运行服务；Python直接访问全部业务表减少HTTP却模糊权限与工具边界。选择独立Go业务HTTP＋PG/pgvector、Python薄适配，不引入完整客服框架或通用工具发现。

本ADR已接受，作为S1-A实现依据。它不宣布云端chat、固定流程、调用账本或整个S1已经完成。

## 2026-09-16实现澄清（PR #38）

以下为S1-A实现期的明确增补，保留PR #37接受的原有决策正文；具体复现和修复见[实施证据](../evidence/agent-v3-s1-business-2026-09-16.md)。

- `as_of` 取版本化工单的受信 `observed_at`（业务观察时点），开发语料固定该值，配送判断不能随验收机器当前时间漂移；`created_at` 单独记录快照事务的实际捕获时间。在线调用者不能覆盖两者。
- 向量转换到float32后，显式按float32乘法和累加计算平方范数，还须位于float32最小正值至最大值的一半之间。实际PG验证表明仅检查分量有限或float64总范数不能排除下溢/溢出误导距离，包括384个1e-23分量。无法安全表示的范数返回INVALID_ARGUMENT，固定模型正常输出在范围内。
