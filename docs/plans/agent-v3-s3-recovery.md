# S3 持久步骤与故障恢复：实施与验收计划

2026-10-04（Asia/Shanghai）；**D0 独立方案审查通过，合同随本决策 PR 合并生效；D1～D3 待实施与验收**。对应[PRD v0.17](../product/JobForge_PRD_v0.17.md)与[ADR-0025](../adr/0025-confirmed-step-recovery.md)。基线为 `e2b9bad`（[PR #56](https://github.com/XJfyrh/JobForge/pull/56)），合同分支 `XJfyrh/docs/agent-v3-s3-recovery`，开始时工作区干净。实施会话承担源码、测试和文档，原规划会话负责方案审查与结果验收；合同 PR 按授权提交、推送并在必需 CI 通过后合并，随后实施免费工程层。功能 PR 另行审查，收费仍等待 D4 清单放行。

## 1. 已核对的接缝与最小落点

| 现有实现 | 结论与计划改动 |
|---|---|
| [Run政策](../../internal/run/policy.go)、[PG生命周期](../../internal/run/postgres/lifecycle.go) | 30s lease、5s heartbeat、180s attempt、三次恢复/1-2-4s退避及取消优先已有；复用Claim/Sweep，仅在S3关闭事务保存待执行步骤和恢复序号 |
| [步骤提交](../../internal/run/postgres/checkpoint.go)、[动态决定](../../internal/run/support_agent.go) | step/游标/事件原子提交；新Worker读取已提交前缀。复用已有结果校验、hash链、资源与Go计算下一步，不增加清游标或任意Complete |
| [审计guard](../../internal/run/postgres/provider_audit_guard.go) | 当前只认原attempt Commit/窄终态失败；新增按原call/profile判断的完整审计恢复分支及恢复完成的新step证明，保持同batch排他 |
| [profile定义](../../internal/run/support_profile.go)、[snapshot校验](../../internal/run/support_profile_snapshot.go) | 增加闭集schema3/recovery_policy和固定runtime版本；新定义/日期/hash，共同向量；旧schema/hash不改写 |
| [协调器](../../internal/runworker/coordinator.go)、[Commit确认](../../internal/runworker/coordinator_commit.go) | S3纯信号/72执行丢失及Commit确认不确定停止Run续租，交自然回收；已知协议/大小/身份/unknown/冻结及STOP优先，不凭本地标志放行 |
| [固定进程](../../internal/runexecutor/process_linux.go)、[guardian](../../python/jobforge_agent/guardian.py) | 实际Wait/EOF/Join/组消失仍必需；不新增退出码、FD、任意入口或收费fixture开关 |
| [运行时测试镜像](../../tools/agentruntimecheck/Dockerfile)、[CI](../../.github/workflows/ci.yml) | 新S3真实Linux/PG故障进入现有integration target；只在测试构建安装合成供应商，生产registry边界继续检查 |

拟新增0026 migration仅包含attempt的`recovery_step/recovery_ordinal`及约束；不改0023～0025、不回填历史证明。详细谓词、三种持久事实和锁序已在ADR明确。实现时保持 Run→family/tenant/batch→call/attempt→worker/tenant/profile slot；batch guard只读其它Run，禁止反向锁旧Run。关闭不因冻结拒绝清理、不退款；新许可仍在账户锁内检查冻结、完整审计和额度。

## 2. 切片与审查门禁

| 切片 | 交付与退出条件 |
|---|---|
| D0 合同（独立方案审查通过） | 三份文档及索引、原实现证据、环境/费用前提；链接、敏感信息、diff检查已完成。按独立决策 PR 流程提交、推送，必需 CI 通过后合并生效 |
| D1 持久恢复合同 | 新profile/source schema、typed校验/hash/共同fixture、0026、原关闭证明和guard；真实PG正反例、迁移前滚/up-down、取消/冻结/锁竞争及预算不重置 |
| D2 正式进程恢复 | 最小S3协调器退出/确认分类；实际Worker/guardian/step死亡、自然30s lease、新principal接管、已提交前缀复用及旧权限拒绝；未知窗口保持停止 |
| D3 免费门禁与独立实现审查 | 实际改动对应单元/SDK/HTTP/PG/Linux/race/lint/生成一致性；最终适用CI。结果交主规划会话审查，尚未执行的真实云端层不计通过 |
| D4 有限真实实验（主规划会话放行清单后） | 使用选定的新增累计最多5 CNY上限；先冻结实验登记/源码/构建/profile/数据/评分，复核当天供应商与资源；运行下述少量实验、完整导出与报告。独立验收及最终必需CI通过后按PR流程交付 |

按顺序完成D1～D3，再由主规划会话放行D4执行清单；不重做S1/S2、40案或无变化的观测配置。对本次新增查询做定向真实PG执行计划/锁等待比较；不扩成全旧队列benchmark。若发现需要新API/状态/故障语义，先补本ADR并审查，不在实现中扩大合同。

## 3. 故障矩阵

每个故障使用数据库提交事实、RPC/HTTP屏障或真实进程事件确定注入点，禁止固定sleep猜窗口。时间推进可用于已有纯政策/事务测试；**下面新自然恢复验收不执行 UPDATE lease_until、session.expires_at 或 next_attempt_at**。没有新的测试时钟/短lease生产开关，至少主恢复路径直接等待固定30秒lease；同principal重登记另等60秒session保护自然结束。

| ID | 注入/竞争 | 必须核验 |
|---|---|---|
| F01 | 模型或读工具获得任何物理许可前死亡 | 当步HTTP为0；原前缀/hash不变，自然回收后只从原待执行步骤继续 |
| F02 | chat Reserve已提交但发送前死亡 | 次数已占、原call无报告/unknown全hold；不能证明未发出，新chat/其它案例受guard阻断，即使batch frozen尚未落库；读工具的免费预留另测新ID/新工具次数及原hold保留 |
| F03 | 成功响应+known report+accepted ordinary observation均已持久，Commit前死亡 | 原step缺失；S3关闭证明匹配原call，新的attempt只重做该步；新call/新费用，旧报告/费用仍在；提交后再推进两步，guard仍可核验旧证明 |
| F04 | report/usage已持久但普通观察缺失/错hash，或观察事务未提交 | known不够；新Claim/Reserve拒绝，无新HTTP、无补假Commit；反序/ACK丢失以实际PG事实区分 |
| F05 | 中间Commit事务已落库但ACK丢失，再终止Worker | step/游标/事件一次；自然新Claim复用前缀，已提交模型/工具HTTP新增0；Found=false不当回滚证明 |
| F06 | Commit成功后、下一步BeginTool/Reserve前死亡 | 与F05同样的前缀复用；下一步按新执行权发送，不能由旧内存cursor继续 |
| F07 | 实际杀死Python step，分别在发送前/完整普通观察后 | guardian转述实际退出72，Go真实清理；只有无协议/大小等事实时按S3执行丢失停止续租；审计不完整的chat仍停止收费 |
| F08 | 实际杀死guardian，step仍在组内 | Go实际Kill/Wait/EOF/Join/组消失；无残留，再次执行受相同PG谓词约束；清理超时不得计通过 |
| F09 | 实际Go Worker SIGKILL | parent FD EOF使guardian/step消失，测试父进程实际Wait；不伪造Fail，另一预登记principal等待自然lease/Sweep/退避接管 |
| F10 | lease过期但未重领、自然回收、新Claim与session保护 | DB等号到期；旧执行权拒绝，新attempt/fence增加；记录实际lease/关闭/ready/Claim时间，不能强制缩短session |
| F11 | 旧owner/token的Commit/Observe/Heartbeat与新call晚到 | STALE_LEASE/原停止映射、当前cursor/hash/active_call不变；原身份合法窄usage重放/补报只能改变自身账本及必要冻结，不能使用新session替代 |
| F12 | profile、runtime、snapshot/语料/index/embedding资源错配或不可得 | 接纳/登记/执行输入在对应边界明确拒绝，无静默替换、无新收费；历史checkpoint/Calls仍可读 |
| F13 | Cancel/Run deadline/attempt deadline与回收、新Reserve或Commit竞争 | 同Run锁决定先后，取消→Run期限→段超时恢复；停止后0新增许可。至少一例实际180s attempt截止，其余精确边界复用政策/锁等待测试；未知账本不能因超时恢复解锁 |
| F14 | 连续四次实际丢失、重复Sweep/关闭请求 | 首次外最多三次恢复、1/2/4s自然退避；第四次无恢复机会且failed，recovery_count不重复增加；proof序号连续1..3，attempt数与预算分别核验 |
| F15 | 恢复到次数/token/费用边界、旧unknown、frozen/冲突/超预留 | family/tenant/batch累计不重置、工具与物理ID全新、旧known/held保留；unknown不计零、无自动退款/thaw/新batch。取消/冻结先提交则新许可0 |
| F16 | S3请求与历史S1/S2 call共享batch；证明逐字段删改；并发Claim/Reserve与报告重放 | 以原call/profile决定；legacy null不能豁免；原worker/session/fence/step/ordinal错配拒绝；batch行锁保证仅一条未越过屏障的chat |
| F17 | 同hash重复Commit、不同hash、yield/终态ACK丢失 | 复用现有真实PG覆盖：live lease同内容幂等、STEP_CONFLICT无覆盖、旧lease仍陈旧；yield/终态只读确认不增加恢复次数 |

F03及F05/F06至少使用动态`support-agent-v1`正式入口、真实PG/gRPC和真实HTTP计数，读取SDK Steps/Calls/Run和原始SQL；纯coordinator替身或S0进程探针不足。坏帧、尺寸、stdout残片、计量确认不完整和冻结的负例必须在新runtime回归，不能靠信号分类吞掉。

既有可复用覆盖为`run_checkpoint_test.go`的重复/冲突/取消/终态事务，`run_lifecycle_test.go`的次数/分类/期限竞争，以及provider audit存储/HTTP/guard/确认窗口测试。它们有手工改时间的场景，继续作为事务/政策证据，**不宣称已证明自然S3恢复**。原`TestRunExecutorWorkerSIGKILLRetainsUnknownAndBlocksChatRestart`加速了lease/session/退避，不能代替F09/F10。

## 4. 检查层次与测试成本

| 层 | 命令或入口 | 费用/边界 |
|---|---|---|
| 文档切片 | git diff --check、变更文档本地链接、状态/编号与敏感信息核验 | 免费；不代表运行时通过 |
| Go/Python纯合同 | `go test`对应包；`pytest python/tests tools/support_evaluation sdk/python/tests`；源schema/共同fixture与生成一致性 | 合成数据，免费；不读S5保留集 |
| 真实PG/SDK HTTP | Windows先`docker compose -f deploy/compose.yaml up -d postgres`；设置`JOBFORGE_TEST_DSN`及安装SDK后的`JOBFORGE_TEST_PYTHON`；定向`TestRun*` | 可重建控制测试库，禁止用收费库；一个DSN同时只有一个可能清DB的进程 |
| 固定Linux进程 | `tools/agentruntimecheck/Dockerfile`的process-check与integration-check，均`--init`；联合层设置专用环境开关和容器可达5433 DSN | 真实Go/Python/FD/PG/gRPC，模型/向量/业务响应仍为合成fixture，无供应商费用 |
| 强制门禁 | Go build/vet/golangci-lint/全仓race；ruff/check+format、mypy SDK/Agent、Python全套；SQLFluff基线+migrations；Buf及适用生成/registry边界 | 按[CONTRIBUTING](../../CONTRIBUTING.md)和[开发指南](../development.md)；并发修改必须`go test -race ./...`，skip单列 |
| 真实云端 | 已批准有限profile/batch、正式安装包/SDK与真实业务HTTP/embedding/pgvector | 仅D4；不能由上述fixture替代 |

新增自然故障集按顺序运行，估算30秒回收场景及四次丢失约需数分钟，实际180秒attempt场景另需至少3分钟；建议专门联合套件有12分钟上限，记录实测后只调整必要CI/test timeout，不缩短生产lease。镜像构建与全仓门禁另计，尚无本次实测耗时；该估算不是SLO。自然等待期间使用有界最终断言/进程屏障，不让单次阻塞调用妨碍进度输出。

完整新免费验收预计包含一次固定镜像构建、一轮定向PG/进程故障、一轮适用全仓门禁；有失败只重跑修复项与受影响门禁，不循环重复已通过或无关验收。观测配置无改动时不另追加本地promtool/dashboard循环，现有必需PR CI保持。依赖或专用开关缺失造成skip，必须补对应层或明确未执行。

## 5. 真实云端环境与费用前提（本轮只读）

已先查仓库外 `E:/JobForge-notes/2026-09-17-agent-v3-s2` 的目录与配置用途，没有输出凭据/DSN口令/业务正文：

| 材料 | 用途与本次处理 |
|---|---|
| cloud-v1..v4、cloud-response-cut及-v2 | 历史候选/注入失败与各自账本，全部保留；不重启旧batch |
| cloud-v4/source.json、build-receipt.json、registration.json、budget-admission.json | 原定义与四份来源摘要、源码/镜像/包绑定、评分登记及历史S2预算；供复用资源和核验，不能改成S3授权 |
| prepared/{control.disabled,control.enabled,worker,executor,launch,rows} | 同一冻结profile和启用集合、运行时及40行驱动；只作为来源，不覆盖旧文件或套用旧40案launcher给S3实验 |
| secrets/worker.json与driver.json | DeepSeek、业务reader、控制Worker/operator凭据；字段非空，留在仓库外，worker prepared文件本身不含秘密 |
| control.override.yaml、readonly-control.override.yaml | 指向原独立控制库及disabled只读配置；只枚举服务、环境变量名与挂载，未打印DSN |
| exports/outbound/state、summary/report/evidence、原始日志及审查 | 原Calls/步骤/费用/故障/停止证据，S3在新目录追加记录，不倒填历史缺失事实 |

Docker可读取清单，仍有`jobforge-agent-v3_business-pgdata/control-pgdata/business-models`卷与S2业务/控制/Worker镜像；**当前无JobForge容器**。没有启动或修改旧服务、运行migration、查询持久库内容或核验真实索引健康，因此不能声称业务/PG/模型服务已经可用。后续先核对保留卷/配置、恢复安全只读业务环境，再用独立S3控制库；不得删除旧卷或以重seed覆盖历史索引。

本机`.venv`中jobforge SDK和jobforge-agent分发包均为0.1.0；实现后重新安装/记录受审源码包，再设置解释器变量，版本号本身不证明对应当前源码。已用外部凭据仅GET官方`/models`及`/user/balance`：HTTP200、Flash可见、账户可用且CNY余额足够5 CNY。没有调用Chat、embedding或业务API；余额可用不代替执行范围与清单审查。

2026-10-04核对的[官方价格](https://api-docs.deepseek.com/zh-cn/quick_start/pricing/)仍列`deepseek-flash` / `DeepSeek-V4.1-Flash`，CNY峰时每百万token：miss输入2、hit输入0.04、输出8；[官方模型接口](https://api-docs.deepseek.com/api/list-models/)和账号响应给出上下文1,048,576。S3沿用峰时保守价及1024输出上限，单chat全额hold仍为2,105,344 microyuan；当天实际执行前再次核对，日期/来源/hash存入新profile，不改2026-09-16/17历史定义。官方别名和metadata不足以证明不可变模型。

新仓库外目录 `E:/JobForge-notes/2026-10-04-agent-v3-s3` 保存只读planning receipt（SHA256 `353501456e63831dbe9066bf0c77c70476cb2c2fb1b4644a1ef743ba3cd2a20c`）及官方HTML价格快照（SHA256 `5a7b1832592387340f2fc456399b34b89b05f3fa167c2e35909e2fa4afe021e3`）。receipt只含白名单状态/模型元数据/余额是否够建议值；没有API key、实际余额或DSN。

主规划会话依据维护者自主S3规划/实施及常规技术选择的授权，选定**新增累计最多5 CNY、6小时、最多11个预登记实验Run**的执行上限；金额由主规划会话选择，不称用户明确口授。无自动增资/充值、无关收费探针或隐含重跑，S2余额不转用。收费仍须等免费机制层、独立实现审查、适用CI通过及主规划会话明确放行执行清单。按S2实测小样本调用量推测known费用可能远低于5元，但不作为上界或完成承诺；两条unknown就可能占用4.210688元并不足下次保守预留。

执行清单放行后，以各已启动S3 batch账户一次聚合 `R=5,000,000-Σknown-Σheld`，新batch持久cap≤R；不叠加family/tenant镜像、不重置消费。一次只运行一个实验批；缺报告、unknown、provider异常、冲突/冻结或上界失效立刻停止后续收费，保留所有实验行。不能用新batch绕过未解决窗口；需要改变预算或实验清单，由原规划会话先决定。S1/S2历史known/held照原交付报告单列，不退款、不转授权。

## 6. 预固定成本对照

仅选已开放开发集 **DEV-002、DEV-027、DEV-035**：原S2报告分别有1/2/3次政策检索，可覆盖短路径、补充取证与较长前缀。选择依据为已公开调用路径，未读S5保留集、不改gold。运行前冻结实验清单与触发条件；不能因为真实模型改变路径而临时换更好案例。

| 实验 | 预登记故障边界 |
|---|---|
| DEV-002一对 | F06：首次search_policy完整Commit后，下一model_decision尚未Reserve |
| DEV-027一对 | F05：首次实际读工具Commit已落库但ACK被外部故障代理丢弃 |
| DEV-035一对 | F05：模型选择第二次search_policy的model_decision已Commit，但ACK丢失 |
| DEV-002独立恢复 | F03：完整模型响应/report/accepted ordinary observation后、Commit前SIGKILL Go Worker；不并入从头成本对照 |
| DEV-035独立恢复 | F07：完整模型响应/report/accepted ordinary observation后实际杀Python step；不并入从头成本对照 |

每对包含checkpoint组原故障Run C、从头控制组原故障Run H0及重新开始的独立Run H1，共9个Run；另2个单独恢复Run，总计最多11个。C与H0使用同一S3 profile、冻结源码/模型/策略/提示/工具/数据和相同语义触发点。自然非确定性导致未到触发点/路径不同，则原结果与费用保留，标为无法组成该边界对照，不替换或拼接成功。

checkpoint组让正式新Worker自然Claim原Run，不再次Submit/Retry。控制组只在已提交前缀的F05/F06边界运行：确认H0所有chat均实际Commit、旧组消失、自然关闭attempt后，用既有API明确cancel H0，再Submit预登记独立业务意图H1从输入完整执行；H1不是生产retry、不清H0 checkpoint、不重置原家族账户。控制组总成本含H0和H1，实际Run/家族/批次映射事前登记；若H0留下未提交chat/unknown/冻结则停止，不新batch绕过。实验驱动不能新增服务端恢复或清游标接口。

各Run快照UUID可因合法独立Capture不同；必须核对同tenant、工单/订单/物流内容、全部版本向量、as_of、政策、语料和index profile/content一致，保存映射与实际hash，不伪称同UUID/hash。事实源前后摘要和reader权限证明没有业务写入。

故障注入放在仓库测试或外部验收驱动/受控RPC代理，不加入生产Dockerfile的合成adapter/供应商origin/kill开关。只转发正式控制事务，在PG屏障确认后阻塞/丢ACK或杀实际进程；不改供应商正文、不伪造usage/观察/Commit。恢复接管是受限实验进程监管：已登记两principal、同一批/同一Run、旧进程实际Wait/组消失后启动唯一正式替代Worker；Go/PG仍独占Claim/恢复预算。原S1/S2 attempted/restart=no规则不被原地放宽，新S3实验清单单独绑定这种预期替换。

所有实验行事前登记，保留未尝试/边界未到达/模型业务失败/监管失败/费用停止；计划不是成功承诺。成本报告同时给出：

- 完整物理请求分类：chat、query embedding、metadata、业务HTTP、逻辑工具、纠错；区分Reserve次数、真实发出次数、known/unknown、已提交前缀、恢复新增与未提交重复。
- 原始physical_call_id/step/attempt/parameter hash与语义动作映射；跨快照不只凭参数hash比较重复，不把控制RPC或SDK查询算模型调用。
- C/H0/H1各自及每组累计known、held、tokens、次数、recovery_count；先给本组总费用，再给故障后的新增费用，不能遗漏前缀或失败请求。
- Kill/Wait/组消失、最后有效lease、关闭/ready/new Claim、首次新步骤、结束时点；恢复等待、活跃执行、从故障到恢复完成和总耗时分别报告，不能把等待lease从总耗时中隐去。
- 最终方案、完整SDK导出、原业务评分/来源/安全检查、所有失败。相同模型不承诺相同最终方案，调用减少/费用/耗时不保证同时改善；小样本不推出总体节省百分比。

## 7. 取证与当前检查边界

每轮记录源码commit/tree、未提交差异、命令/退出码、镜像digest/安装包摘要、profile/price/schema/prompt/数据/评分摘要、注入点、实际进程事实、PG只读摘要及SDK导出/抓取时间。原日志与报告追加保存于本次外部目录；公开仓库只提交必要脱敏摘要和哈希，秘密、原始业务/模型全文不进日志或普通Trace。失败原始记录不被修复后的通过覆盖。

D0 只运行文档/差异检查和环境只读核对；没有生产代码/migration/Proto改动，也未运行本次Go/Python回归、真实PG、Linux进程或云端推理。这些层次仍待D1～D4实际执行，不计通过。合同已通过独立方案审查；本决策 PR 的 CI 与合并结果、功能实施和验收将另行记录。S3 收费调用仍为 0。
