# AI 应用后端下一阶段调研依据

- 调研日期：2026-09-16。
- 仓库基线：5c82834，main / origin/main一致，开始时无未提交改动。
- 范围：只读源码 / 文档审查、一手资料研究与计划编写；没有调用付费服务、读取密钥或运行新的模型验收。
- 对应交付：[初版计划](../plans/ai-backend-next-stage.md)、[路线v2](../plans/agent-execution-roadmap-v2.md)；最新候选为不受历史兼容约束的[路线v3](../plans/agent-execution-roadmap-v3.md)。第1～7节保留此前研究过程，第8节记录新选择，不把旧建议作为v3的限制。

## 1. 岗位目标与投入取舍

用户主要投AI应用后端。官方校招职位可以作为方向样本：百度2027 Agent应用全栈岗位列有RAG/Agent应用和评测，关注成功率、稳定性、成本及延迟。这支持把业务质量与调用治理作为下一阶段重点；单一职位不能代表整个校招市场。[百度校园招聘](https://talent.baidu.com/jobs/list?projectType=3&recruitType=GRADUATE)

结合仓库已存在的租约、业务幂等与故障测试，建议优先补独立效果评测、单个远程后端和解释实验结果的能力。此优先级是针对本项目的判断，不是招聘方对本项目的评价，也不是录用保证。

## 2. 评测方法与依赖选择

Sentence Transformers的IR evaluator将queries、corpus、relevant_docs分开，并提供Recall、MRR等指标定义。这里借用评测结构和指标；首轮样本很小，无需为此把Sentence Transformers或Ragas引入Go业务运行时。[InformationRetrievalEvaluator](https://www.sbert.net/docs/package_reference/sentence_transformer/evaluation.html#sentence_transformers.evaluation.InformationRetrievalEvaluator)

测试集参与参数选择会泄漏评测信息。按文档模板、主题和问题改写族分组，是将分组切分原则应用于本项目的建议。先冻结标注与划分，再在开发集做有限实验，最后在同一保留集运行baseline与已选candidate；此前不运行保留集推理。[scikit-learn交叉验证与分组切分](https://scikit-learn.org/stable/modules/cross_validation.html)

BEIR针对不同领域检索评测提出异构数据集，说明单一同质数据不能充分评价检索能力。本计划的小数据集只支持自身语料与任务范围的结论，不作为BEIR成绩或域外泛化证明。[BEIR原论文](https://arxiv.org/abs/2104.08663)

仓库抽取结果是固定业务Schema，字段级正确率、整单正确率和来源证据能够由确定性计分与人工标注核对。第一轮不引入LLM-as-judge；避免额外模型成本和评判不确定性。模型或提示词效果不佳时保留失败样本，不能通过只统计成功产物提高分数。

Ollama官方建议将Schema信息用于提示词并在客户端验证；可作为一条抽取候选实验，但收益须实测。同一页面当前说明Ollama Cloud不支持structured outputs，因此它不能直接作为现有本地Schema调用的远程替换。[Ollama结构化输出](https://docs.ollama.com/capabilities/structured-outputs)

## 3. 远程候选及最小适配

### 3.1 首选候选：百炼同步API

官方模型页在调研日列出qwen3.7-flash-2026-07-15快照及北京地域结构化输出能力，可与text-embedding-v4组成一个供应商覆盖两个任务的候选。它尚未被本项目实际调用；供应商、地域、权限和最终模型仍待确认。[Flash模型及快照](https://help.aliyun.com/zh/model-studio/qwen3-7-flash)

text-embedding-v4支持1024等维度，不含当前本地384维；单批最多10条，每条最多8192 Token。原生同步API还提供document/query编码模式。由此推导：当前64块上限可能需要七次物理请求，必须显式修改v2业务调用预算、记录向量空间，不能只在内部拆批后仍宣称两次调用。[同步embedding API](https://help.aliyun.com/zh/model-studio/text-embedding-synchronous-api)

结构化输出指南提示设置max_tokens可能截断JSON。JobForge仍需保留硬输出预算；选型试运行必须检查所选模式是否支持有效的输出上限，截断进入明确失败或一次修正。若某模式无法同时满足限额与契约，应换模式/候选，不能通过移除上限提高通过率。[结构化输出约束](https://help.aliyun.com/zh/model-studio/qwen-structured-output)

错误码文档中部分429对应额度、账单或未购买，而非可稍后重试的临时限流。适配器按供应商有界错误码映射，并保留未知错误的保守策略；不把原始响应、模型输出或凭据放进异常。限流参数按实际模型/地域核对。[错误码](https://help.aliyun.com/zh/model-studio/error-code)、[限流说明](https://help.aliyun.com/zh/model-studio/rate-limit)

模型快照有生命周期，公开模型ID也不一定等于权重摘要。产物记录请求和响应模型身份、profile与参数hash、运行日期；模型下线后旧索引查询不可自动切换向量空间。恢复路径为保持旧profile可用或显式生成新索引。[模型下线机制](https://help.aliyun.com/zh/model-studio/model-depreciation)

### 3.2 备用：远程自托管Ollama

沿用当前两个固定模型和manifest digest，接入代码变化较小；适合没有商用API账户、但已有可信主机资源时验证远程TLS、鉴权和网络故障。它能提供的证据与商用服务的usage、配额、账单和模型生命周期不同，验收报告应明确是哪条路线。自行购买或租用主机不在当前授权内。

### 3.3 架构选择依据

优先继续Go标准HTTP薄适配，不引入供应商Agent框架、Python Worker或异步Batch。模型客户端提供计算与有界元数据，不取得job lease、不调度重试、不提交任务状态。

现有Model接口需要增加provider/model/usage等业务元数据，profile需要贯通worker和artifact检索服务；具体落点为internal/tasks/contracts.go、service.go、rag.go、extract.go、ollama.go、cmd/jobforge/main.go及cmd/jobforge/artifacts.go。新源文件名由实施时确定，不修改生成代码或队列核心以适配模型。

## 4. 费用与证据

价格会变化。调研日官方北京Flash模型页列出的短输入档原价为输入0.2元/百万Token、输出0.8元/百万Token；embedding-v4同步API表为0.0005元/千输入Token，即0.5元/百万Token。仅用于准备预算公式；实施时核对精确模型、地域、阶梯、思考Token等收费范围与价格日期，不据此承诺总费用。[Flash价格](https://help.aliyun.com/zh/model-studio/qwen3-7-flash)、[embedding价格](https://help.aliyun.com/zh/model-studio/text-embedding-synchronous-api)、[统一价格表](https://help.aliyun.com/zh/model-studio/model-pricing)

免费额度有账号、模型、时间及地域条件；在账户未核对前不将其作为已获资源。账户若提供额度用尽停止，可作为额外保护，不能替代应用侧调用预算。[免费额度与停止规则](https://help.aliyun.com/zh/model-studio/new-free-quota)

例如只估算100份文档、每份最多两次抽取、每次实际计费输入恰2000/output恰512 Token，以上述单价计算为0.16192元。该算例不含embedding、检索、故障重跑、未知用量或其他收费项，也没有证明这些Token条件能被每次请求满足；不作为验收报价或费用上限。正式预算以计划中的B和冻结调用manifest为准。

持久调用额度是本项目候选设计：先预留后发请求，超时或crash后的未知调用仍消耗额度，人工retry共享验收批次额度。它保护请求数量；实际货币消费仍需供应商账单或usage核对。该状态属于业务调用，不是第二套任务状态机。

证据分三层：

| 层 | 可支持的结论 | 不自动支持的结论 |
|---|---|---|
| 真实供应商调用 | 指定账号/地域/模型的真实结果、响应元数据和可见usage | 其他模型兼容、所有429行为、取消后停止计费 |
| 故障代理＋真实模型 | JobForge对注入故障的分类、预算、取消、重投与业务发布幂等 | 供应商实际发生该故障或承诺退款 |
| 确定性替身 | 协议解析、错误映射、边界输入与并发额度逻辑 | 真实模型质量、真实远程可用性 |

## 5. 观测、性能与运维取舍

业务字段首先保持自有版本和有界命名，避免引入不必要升级。调研日OTel的GenAI语义规范入口已经提示迁移到独立仓库；实施若采用标准字段应锁定对应版本，不能假定这些字段与当前SDK版本一起稳定。内容采集继续关闭，job/case/request ID不作metrics标签。[OTel GenAI规范入口](https://opentelemetry.io/docs/specs/semconv/gen-ai/)

性能报告重点是测量边界：现有JobStore基准、真正服务链路和AI模型任务分开。W4历史失败单列；强制lease过期后的回收调用耗时也不能当自然故障恢复耗时。相关本地证据见[性能报告](../benchmark.md)、[可靠性报告](../reliability-report.md)和[四类边界审查](../agent-rag-review.md)。

最小恢复演练可使用PostgreSQL的custom-format dump恢复到新实例。pg_dump只备份单个数据库，角色等集群全局对象需另外处理；计划要求记录角色/配置重建步骤并核对任务、产物与租户隔离。这只支持限定快照恢复，不等同于PITR或HA。[PostgreSQL 16 pg_dump](https://www.postgresql.org/docs/16/app-pgdump.html)

## 6. 未采用的扩展及理由

| 扩展 | 本阶段取舍 |
|---|---|
| 多供应商统一网关 / 自动fallback | 首个供应商尚未验证，先把单个profile、错误和预算做完整 |
| 向量数据库 / reranker / 大型评测框架 | 首轮≤64块和固定指标可用现有矩阵与轻量scorer完成；有实际瓶颈再考虑 |
| 无界自主规划、多Agent、动态工具发现 | 继续后置；路线v2单独提出在一个Handler内选择三个预注册只读工具，需新的PRD/ADR |
| AT-25 ControlStream | 现有心跳取消已验收，收益低于业务质量和远程接入 |
| 完整生产观测留存与灾备 | 校招阶段优先有限恢复演练与明确边界，长期容量/HA另立需求 |

这些取舍可在真实评测显示具体需求时重新评审；不是对未来永久禁止。当前技术决定只写在计划草案中，正式修改公开契约须先经过增量PRD与ADR。

## 7. 路线v2补充：以有界Agent执行为主线

用户指出初版偏向RAG，要求新版供审阅。当前agent.extract只有结构化抽取及有限修正，尚未证明模型能根据工具反馈选择后续动作。新版提出agent.review_order，保留抽取与检索作为底层能力，首轮只输出核查报告。

Anthropic的架构文章区分预定义代码流程与模型主导的工具循环，并强调工具反馈和最大迭代等停止条件。本项目据此提出单job内有限循环，仍通过固定条件流程对照决定动态选择是否有价值；没有采用该文中的开放执行或多Agent范围。[Building effective agents](https://www.anthropic.com/engineering/building-effective-agents)

Ollama官方提供tool-calling协议与工具结果回传示例。这只证明平台接口存在；目前的qwen2.5:0.5b抽取验收不等于Agent能力验收。路线v2要求先测试具体候选的工具选择、参数、结果利用及结束能力，再固定模型版本和资源预算。[Ollama tool calling](https://docs.ollama.com/capabilities/tool-calling)

主要调整：三个只读工具、一个Go租约持有者、报告幂等发布；首轮不做checkpoint或人工审批暂停恢复。v0.6排除动态工具选择，需要在后续增量PRD/ADR中显式限定放开，当前Accepted文档不改写。

公平评测同时报告共享真实抽取产物的决策对照与原文到报告的端到端链路。基线具备同等工具、合理条件分支和模型资源上限；不以人为削弱固定流程证明Agent收益。检索评测收缩为诊断集，60份采购文档在抽取与核查中复用。

远程接入优先验证chat/Agent工具调用，首轮可继续使用本地embedding。供应商及预算仍待选，原文第4节的抽取费用算例不适用于新增Agent多轮预算。所有准备任务、查询、纠正与故障重算须重新纳入验收批次。

## 8. 路线v3补充：重新设计可恢复业务Agent

用户明确不考虑历史包袱和兼容性。新版重新选择售后工单场景：模型查询订单、物流与政策，形成方案，经一个人工审批点执行一种真实工单写入。RAG作为工具，抽取按需成为内部步骤，不再要求两条独立任务产品线。该选择基于场景的动态查证、清晰动作和可检验结果，不是招聘市场统计结论。

重新比较纯Go、纯Python自研PG运行时、Python配合成熟框架和Go/Python分工。推荐Go控制Run/lease/步骤提交、Python执行单个模型或工具步骤；同时将跨语言生命周期成本列为S0试验，而非因既有代码而默认保留。LangGraph已有checkpoint与interrupt能力，若采用它作为事实来源应重新划分恢复责任，不能直接与另一套持久运行时叠加后声称并发和审批安全已经解决。[持久化](https://docs.langchain.com/oss/python/langgraph/persistence)、[中断](https://docs.langchain.com/oss/python/langgraph/interrupts)

步骤恢复和受控写入进入主线：已提交步骤复用、未提交调用可能重做；外部已提交但本地回执未知时，依靠接收方的稳定操作ID和事务去重。取消终态后只读核对回执，不为收集结果重新执行写入。这是本项目候选契约，不是某个框架自动提供的跨系统保证。[活动与幂等参考](https://docs.temporal.io/activities)

新检索方案建议采用pgvector的精确查询与版本/租户过滤，不预先引入近似索引；官方项目提供精确与近似检索能力，具体性能与隔离仍需本项目实测。[pgvector](https://github.com/pgvector/pgvector)

业务方案质量与动作执行正确性分别评测。最低业务门槛在打开保留集前冻结；人审不能掩盖模型错误，写入成功也不等于客户问题解决。新版预算和24～32日估算覆盖步骤持久化、一个审批点和真实写入；远程服务与费用上限仍未选定，本轮没有运行新模型或付费请求。
