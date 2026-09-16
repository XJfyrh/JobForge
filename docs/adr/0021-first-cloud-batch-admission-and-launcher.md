# ADR-0021：首批收费 profile 的可信登记、快照约束与串行启动器

- 状态：**Accepted（PR #49 合并时生效）**；日期：2026-09-16。
- 决策者：用户授权自主技术决策，独立 Agent 审查后按 PR 流程合并；关联：[PR #49](https://github.com/XJfyrh/JobForge/pull/49)。
- 关联：[PRD v0.13](../product/JobForge_PRD_v0.13.md)、[ADR-0017](0017-run-admission-and-call-ledger.md)、[0018](0018-deepseek-fixed-flow-and-executor.md)、[0019](0019-executor-confirmation-and-exit-contract.md)、[0020](0020-provider-audit-and-batch-stop.md)。
- 取代：无。细化 ADR-0018 的正式 profile/版本冻结，以及 ADR-0020 §8 的首批启动器；不修改旧 ADR 历史正文或既有费用授权。
- 仓库基线：support PR #48 合并提交 `bffe566`；provider 审计合同已接受，实现为独立待交付依赖。本文不是实现、部署或收费验收记录。

## 1. 上下文与决策范围

现有 `agent-control bootstrap/serve` 可登记 profile、账户并运行唯一 Run 控制面；正式 Go Worker、固定 Python support adapter 与业务快照已有实现。默认部署仍为空，控制配置入口仅接受 `bounded_readonly_v1`，尚不能启用已登记的 `support_fixed_v1`。`EnsureProfiles` 比较首次登记的完整定义，却不重算声明的 profile hash；`Definition` 目前是 opaque JSON，未把预期业务版本变成接纳约束。

ADR-0018 已要求冻结模型、策略、政策/语料/索引和评估版本；这些是首次收费前必须落实的要求。当前未启用正式收费 profile，不能把缺少生成器/约束误报成已经上线的收费绕过。新决策是将约束落实在可信登记和首次接纳处，并交付受限评估驱动；不增加第二 scheduler、模型探针、动态执行参数、receipt journal 或人工审批执行。

核心选择：**离线生成器 + 登记严格校验 + Capture 后、Admit 前的快照资源比较**。固定业务数据和构建身份由可信部署取证；snapshot 能表达的资源由控制面实际拒绝错配。只在 SDK 提交后比较版本存在 Worker 抢先 Claim 的竞态，因此不能作为接纳门。

## 2. 可信 profile 与 hash

### 2.1 适用范围与来源

仅新首批正式 `support_fixed_v1` 使用版本化 typed support definition（schema_version=1）。沿用 ADR-0020 的 `provider_audit_policy=deepseek-audit-v1`、`expected_response_model=deepseek-flash` 和固定 `executor_version=linux-v2-audit-runtime-1`，adapter 为 `support-fixed-v1`，方案 schema 为 `support-proposal-v1`。旧登记记录和原调用晚到计量不被要求补造新定义/hash；旧执行器不得获得新执行权。

定义来自受审非秘密清单及固定代码常量，不来自模型响应、自报 audit、SDK Submit 或任意外部 URL。定义严格闭集、完整非 null、有界≤16KiB，拒绝重复/未知/大小写别名字段、非法 UTF-8、尾随数据、浮点/越界整数；不允许 arbitrary metadata 或任意策略/endpoint/module。生成与登记共享同一个纯 Go 校验/计算入口。

| 冻结内容 | 必填事实及约束 |
|---|---|
| 固定模型请求 | DeepSeek 官方 origin/path、request model、已观察模型版本与核验日期；非 stream、disabled thinking、1024 max_tokens、json_object、temperature=0；ADR-0018 的四项 bytes 上限 |
| 程序合同 | definition 版本、固定策略/adapter/方案 schema 及 schema 摘要、prompt 版本与摘要、受审 adapter 源码摘要；与固定生产实现相符，不能仅任意填一个合法 hash |
| 业务资源 | dataset ID、runtime manifest/seed 摘要、观察时点、policy version、corpus SHA256、完整固定 IndexProfile 与其 hash；两个 tenant 各自 policy revision、index UUID 与 index content hash，按 tenant_id 排序且唯一 |
| 定价 | 官方合同核验日期与来源快照摘要、CNY、整数 denominator/miss/hit/output 费率；仍使用 ADR-0018 已接受快照，运行当天复核失败即不启动 |
| Profile 已有字段 | ID、strategy、executor_version、audit policy/expected model、输入/输出 tokens、family token/cost 上限、完整 Pricing 与完整 Definition；重复描述必须一致 |

runtime manifest/seed 只含允许部署的业务数据，定义不包含 gold、case 标签、预期答案、评分规则或凭据。完整源码/镜像/安装包构建证明保存在仓库外部署 receipt，不能把不必要的构建元数据下沉为队列权限。定义中的源码/prompt 摘要来自受审源文件集合，排除生成的部署/profile/manifest 产物，避免摘要循环；安装镜像摘要只在部署 receipt 绑定，不参与其内部 profile 的自引用。

源码/prompt/schema 的实际内容与摘要对应关系，由受信生成/构建/部署环节核验并留证；纯领域校验核对固定能力、定义结构和 hash 自洽，接纳路径不读取源文件、镜像或构建证明。合法摘要字符串本身不能代替部署核验，业务资源比较也不能承担源码证明。

### 2.2 计算与登记

新 profile 使用现有长度前缀 `Fingerprint` 编码；整数为无前导零十进制，输出 lowercase SHA-256。新价格 hash 为：

```text
Fingerprint("jobforge.run.support-price.v1", provider, request_model,
  observed_model_version, pricing_observed_on, pricing_source_sha256,
  "CNY", denominator, input_miss_microyuan, input_hit_microyuan, output_microyuan)
```

新 profile hash 为：

```text
Fingerprint("jobforge.run.support-profile.v1", "1", profile_id, strategy,
  executor_version, provider_audit_policy, expected_response_model,
  max_input_tokens, max_output_tokens, family_token_limit, family_cost_microyuan,
  price_hash, denominator, input_miss_microyuan, input_hit_microyuan,
  output_microyuan, canonical_definition_json)
```

`canonical_definition_json` 是严格 typed 校验后重建的完整对象：对象键递归按名称字节序排序，数组保持定义顺序（tenant 列表须预先排序），整数保留精确 int64，按 Go `encoding/json.Marshal` 的字符串转义规则编码，无空白/末尾换行；禁止经 float64 解析再归一化。不哈希原文件空白，不 trim 或改变业务字符串。实施 PR 同时交付闭集字段源合同、Go 生成的完整 definition/price/profile 精确向量与篡改负例，登记前必须用同一算法重算，不能先用手填 hash 启用。完整 Definition 始终参与；profile_hash 本身、Executable/启用集合、生成时间和本机路径不参与。

注册顺序为严格解析、核对固定实现和全部边界、重算 index/price/profile hash，再调用现有不可变登记。相同 ID/hash/完整定义可幂等；同 ID 的任一冻结事实改变即冲突，不能靠原 hash 或新的 enabled 集合覆盖。禁用只撤销未来执行能力，不改历史定义。需要改变事实时另建受审 profile，但不能把本首批停止后的剩余案例迁入它绕过原 batch。

control、Worker 和固定 executor manifest 必须使用同一生成产物的 profile ID/hash/version，profile 集合精确一致；Worker 继续校验固定模型/audit policy/adapter。登记校验不是对恶意管理员或恶意任意 Worker 的远程证明；实际镜像必须由受信部署核对。

## 3. 首次接纳与不可变快照

### 3.1 比较现有字段

`Service.create` 已先获得 profile，再以现有受信 capturer 做一次有界 Capture。新增纯 `ValidateSupportSnapshot` 类校验放在 Capture 成功后、分配新 Run/Business/Family 身份和调用 Admit 前。不会为验证多发 HTTP，不新增 execute_step 输入字段，不传 case/gold/source proof 给 Python。既有 tenant/ticket/hash/完整版本向量结构检查仍先执行。

| 冻结要求 | 当前可核对字段 | 必须采取的比较或证明 |
|---|---|---|
| tenant/ticket | SnapshotBinding.TenantID/TicketID、Ticket JSON tenant_id/ticket_id、VersionVector.ticket | 与受理身份相同，版本向量与 Ticket revision 自洽；不按 case ID 决定业务结果 |
| policy/corpus | VersionVector.policy.version/revision/corpus_sha256、Ticket.policy_version | 与该 profile/tenant 的冻结值逐项相同 |
| index | SnapshotBinding.IndexID/IndexProfileHash、VersionVector.index.id/profile_hash/content_hash | UUID、profile hash、content hash 都匹配该 tenant 登记值；只比较 policy 名不够 |
| embedding/chunker | businessclient 已验证完整 IndexProfile 的 model/digest/dimensions/chunker，并重算其 profile hash | 生成器用相同固定 IndexProfile 计算预期 hash，Service 比较已有 hash；不复制一套不同的摘要算法 |
| 观察时点 | Ticket.observed_at；snapshot 顶层 as_of 在 businessclient 内可见 | Service 比较冻结 UTC 时点；businessclient 同时校验 as_of 与 ticket.observed_at 相同，不伪称 Service 拿到了未投影字段 |
| 当前 snapshot | SnapshotBinding.ID/ContentHash 与版本向量 | 按既有接纳持久绑定到本 Run；每案摘要不同，不能拿单一 dataset/seed 摘要冒充 snapshot 摘要 |
| dataset/seed/源码/prompt | 不在 SnapshotBinding/VersionVector 中 | 受信部署 receipt 绑定受审 seed/loader 结果、只读业务目标及代码/镜像；不得声称快照证明了这些事实 |

首批资源值来自已审查的 [support v2 资料](../evidence/agent-v3-s1-support-2026-09-16.md)：dataset `support-dev-2026-09-16-v2`、policy `delivery-policy-dev-v2`、观察时点 `2026-09-16T12:00:00Z`。实际 corpus、两个 tenant 的已发布 index 身份/内容、IndexProfile 与 seed 摘要从冻结资料逐项填入受审清单，不能在线重建后沿用旧 hash。收费运行期间业务库固定这批受审数据；Worker 只获业务读取能力，不给 seed/load/index 发布/写工单权限。既有接纳 capture 所需受信权限仍限控制面。

Service 没有完整 order/delivery 正文，不能独立重算整个 snapshot 内容摘要，也不证明远端部署未撒谎。可信 capture 的内部一致性、被冻结的资源交叉比较和部署证据共同构成本切片边界。未来远程 dataset 证明另定资源合同，不借本增量引入证明服务。

### 3.2 新建、重放和 Retry

1. **已接受身份优先**：Submit/Retry 保留 ResolveAdmission 优先序。相同已接受操作、同业务意图根 Run 或同来源已接受后继返回原结果；不重捕获、不重新验证新鲜 profile/资源/额度/期限。异规范内容仍 CONFLICT。不能因后来禁用 profile 或业务版本改变拒绝已有回执。
2. **新 Submit**：先校验当前可执行 profile/批次，再单次 Capture 和纯资源核对。错资源返回既有 `PROFILE_UNAVAILABLE`；捕获响应结构/内部身份非法仍按受信依赖错误 `DEPENDENCY_UNAVAILABLE`。不增加公共错误码或泄漏实际/预期业务内容。拒绝不产生业务意图、ready Run、家族账户、操作成功回执或额度变化；业务侧已捕获的孤立快照仍按原合同允许存在。
3. **真正的新 Retry**：仅原 failed/cancelled 来源、原单链和 7 日业务窗口内，仍受原 batch 的 6h/余额/frozen 限制；继承同 ticket、profile/hash、业务意图、family/tenant/batch 账户。它可有新的 snapshot ID/内容，但新捕获事实必须符合原 profile 的所有冻结资源值。不能换 profile/政策/index/观察时点或延长 batch；驱动不自动发起此操作。
4. **最终事务**：Store.Admit 保留先解决并发已接受身份，再验证新建分支的顺序。对真正新建复用同一个纯资源校验，并按当前登记完整 profile/hash、来源关系、PG 时间和锁住的账户重新检查；无网络在事务中。验证失败回滚全部暂存行。这样内部受信调用也不能因绕过 Service 漏掉新收费约束；不复制校验状态机或改变既有锁序。
5. **接纳后**：本 Run 的 profile/price 与 snapshot/index 绑定不随配置更新；步骤、报告、Commit 和自动恢复只能使用原绑定。新 Retry 不解除原 chat 持久 guard、unknown hold 或已冻结 batch，原晚到计量也不授权新执行。

## 4. 离线准备、预检与唯一 Worker

新增离线 `agent-control prepare-support`，在读取 DSN/凭据之前分流；只消费本地受审非秘密清单，输出仓库外新目录。最小输入为清单路径、明确分配的 profile/worker/batch/两个 tenant account 身份、固定 batch key、UTC valid_from 和输出目录；valid_until 固定为 valid_from+6h。固定模型、费用、40 案、策略和 bytes/token 上限不能由自由 CLI flag 覆盖。清单不包含凭据，不允许 HTTP URL 取文件或执行 shell/module。

产物为 control disabled/enabled 配置、Worker 配置、executor manifest、launch manifest 及全部 40 行 unattempted 记录。两份 control 配置仅差启用集合；默认使用 disabled。launch manifest 绑定配置/profile/price、构建 receipt、数据/评分审核摘要、40 案次序和稳定业务键/幂等键；gold/评分材料只在外部评估目录，不挂载 Worker。重复生成不能覆写已尝试批次或悄悄选择新 ID/期限。

复用显式 `bootstrap` 的既有迁移/EnsureProfiles/CreateBudget/BindBudgetTenant，不增加 reset/thaw。两 tenant 同一 batch UUID、固定 6h；所有费用上限仍为 5,000,000 microyuan，次数/token 沿用 ADR-0018，单 chat 按 miss 保守预留 2,105,344 microyuan。重复 bootstrap 不清计数、hold、frozen 或原有效期；冲突停止。新增只读 `agent-control inspect-support` 可在同一受信控制配置下输出有界非秘密 setup receipt：PG 采样时点、账户 ID/key/绑定/额度/期限/known/held/frozen、登记 profile/hash 和所需迁移状态。它不迁移、不建账户、不启用 profile、不清状态，也不成为授权票据。

启动前必须比对实际 setup receipt，不能把离线 JSON 当作 PG 已登记证明。首批账户已消费、冻结、过期、期限不足或配置不符时不启动，不自动换 batch；即使预检通过，Claim/Reserve 的实时 PG 检查仍是唯一执行权。官方价格/模型合同与账号可用性按既有授权在启动前核验并记录时间；生成器和驱动不新增模型/metadata/余额探测请求，缺证据保持 disabled。

正式批次通过独立 opt-in 部署配置启用：**一个固定 launcher 容器同时监管一个 Go Worker 和一个 SDK driver**；Worker/tenant/profile 容量全 1、`init=true`、`restart=no`、非 root、只读 rootfs 与配置/manifest/凭据挂载。launcher 是 init 的主子进程，只启动固定安装的 Worker/driver，无用户命令、动态模块或 shell。生产固定 registry，不挂载 gold、测试 adapter 或可执行用户目录；scorer 在容器外读取导出。凭据沿原部署通道分别给固定子进程，控制凭据仍仅 Go 持有，不进入 argv、profile、产物或日志。源/镜像核验记录不输出整个 Docker Config.Env 或秘密文件。

preflight 和本地 launcher 标记不具有 DB 锁或长期有效许可。首批约束是在可信固定部署下执行，不宣称能阻止恶意管理员同时启动额外进程。实际失联、旧组未消失或进程退出均停止该次启动；沿用 ADR-0019 的终止/Kill/Wait/Join 和≤2s 已捕获报告收尾，不能为了补报延长执行期限。

## 5. 40 行驱动与停止

### 5.1 启动历史与生命周期所有权

`inspect-support` 的 setup receipt 还必须包含本批已有 business_requests/runs/calls，以及本批专属 Worker 的 session/startup 历史是否存在，只返回有界计数/布尔。即使用量全部为零，只要存在这些历史事实也只允许 export；Worker ID 独占本批，不能换 Worker/profile/输出目录规避。纯离线 prepare 不具有判断 PG 历史的能力。已有 ready Run 或 session 但尚未 BeginTool/Reserve 时，零账户不是“从未启动”的证明。

launcher 使用固定部署绑定的持久状态卷和由 batch UUID 定位的唯一目录，它与导出目录分开，运行参数不能改变位置。受审部署准备阶段一次建立并绑定 prepared receipt，记录 batch/profile/Worker 和 launch manifest hash；launch 不自动创建缺失卷/receipt。启动先持该目录的内核排他锁，核对 receipt 及 PG 无历史，再原子持久化并刷新 `attempted` 标记，之后才启动子进程。已有 attempted/停止记录、卷/receipt 缺失或不一致均只允许 export。极早期尚无 PG session/Run 的失败仍由标记阻断；再次 prepare 不得覆写。新输出目录不能证明从未启动。固定单宿主共享卷/排他锁排除并发 launcher，不能把只读预检当成原子占用。本边界不宣称抵御管理员同时篡改部署绑定、持久标记和 PG 历史。

launcher 独占两个固定子进程的生命周期，driver 不获得 Docker、控制 RPC 或 DB 权限。driver 发出停止、退出、崩溃、控制管道 EOF、提交/记录/轮询确认不明确或总截止时，launcher 锁存停止并终止原 Worker；Worker 退出、监管异常或清理不确定则同时停止 driver。先正常停止，给原 Worker 清理及已捕获报告既有的有界收尾，再强制终止仍存活的固定子进程并实际 Wait，绝不自动重新启动。第40行正常完成后也停止 Worker，避免遗留续租/Claim 循环。launcher 自身死亡由 init 主子进程退出和容器 PID namespace 终止回收整个容器，restart=no 禁止重启；宿主/容器运行时失联不声称已验证退出，恢复后只允许 export。检测前已发 HTTP 不可撤回。

监管检测后立即锁存停止并撤销本地派发；在对应派发方已确认停止的屏障后，不再发起新 Claim/Reserve，不消费迟到许可，不开始新 HTTP。此前已在途的控制 RPC 仍可能提交，已交付的派发命令/HTTP 视为可能已发送；保留真实行与 unknown/full hold，不能据此宣称回滚、冻结或退款。launcher 仅检测到退出还不是所有派发方已停止的证明，完整 Wait/旧组消失仍必须核验。

实际 Linux 负例必须在“Submit 已提交、尚无 chat reservation”处杀死 driver，并单独杀死 launcher，核对停止屏障和旧 Worker/组实际退出且无自动重启。测试分别记录监管检测、各派发方 stop 屏障、RPC 提交与 HTTP 开始；在未交付许可的确定性屏障下注入故障时可断言 HTTP 为零，另加 Reserve 在途延迟提交负例，验证真实预留仍保留且迟到许可未消费。还须覆盖零用量但已有 session/ready Run、状态卷丢失、新导出目录与并发 launcher，验证拒绝重启后无新 session/Submit/Claim；不能仅检查下一 Submit 为零。这里只监管固定进程，不决定 Run 下一游标、许可、重试或状态。

### 5.2 串行 SDK 行为

驱动属于 `tools/support_evaluation` 的外部评估工具，仅调用安装后的公开 SDK；它不访问控制库、不 Claim/Heartbeat、不直接请求业务/供应商，也不创建第二调度或恢复循环。外层可显式有界轮询，但 SDK 每个方法仍为单次交换、无隐式重试。

1. 首次提交前校验并持久保存全部 40 行（north/south 各 20）、固定次序、tenant/ticket/case 与稳定业务键/幂等键。原子文件写入失败即不发送。每次 Submit 前先持久记录本行尝试；成功响应后记 run_id。响应不明确记录接纳未知并停止，不能因缺 run_id 改成未尝试或换 key 重发。
2. 每行至多一次 Submit 尝试；Run timeout 不超过冻结剩余批次窗口，最终期限仍由 PG 裁定。一次只等待一个案例，读取超时/总轮询截止均有界且不越过 6h。不自动 Retry、不重新捕获、不以调用模型探测是否成功。进程重启仅可 export；本地文件是评估记录，不是重放 provider HTTP/报告的 receipt journal。
3. `awaiting_approval` 且方案/审批绑定已持久、lease 已释放，是正常 S1 方案完成点；不等待人工批准，不做写动作，不误标 applied/succeeded。no_action 的 succeeded 正常结束。转下一例仍须取得相应持久 steps/calls 导出，不能只等一个 Run 状态字符串。
4. 唯一纠正再次校验失败只沿用 ADR-0020 §8.1 的窄例外。驱动可根据原 Run/steps/calls 的已知完整计量、普通 rejected 观察与 protocol_correction 失败把本例记业务失败；完整内部 attempt/session/fence 屏障仍由 PG 验证，SDK 不新增授权算法。不具备可见必要事实即停；单看 `failed/MODEL_PROTOCOL_ERROR`、子进程退出或有 report row 均不足。下一案例 Submit 也不构成下一 HTTP 许可。
5. batch frozen/固定原因、chat unknown、report conflict/anomaly、持久确认不确定、Worker 异常退出/重启/组清理未确认、预算/期限不足、操作停止或前置复核失败时，不再提交下一例，并停止原 Worker 新工作。非 chat embedding unknown 仍保留原 unknown/full hold 和普通 ACK，不能伪造 DeepSeek audit 或误记 CHAT_USAGE_UNKNOWN。
6. 停止后保存全部 40 行、已尝试/未尝试/接纳未知原因与可读证据；只允许原事实 export。不自动重启 Worker、新 session、重发 Submit/Retry、换 business key、新 batch、增资、解冻或供应商 fallback。人工后续决定不属于本合同；不能删除目录就恢复本首批执行。

持久原因闭集、三层 anomaly 与 batch-only 审计冻结、first-write-wins、原调用报告重放/冲突优先序全部沿用 ADR-0020。本地 `CONTROL_UNCONFIRMED`/`SUPERVISION_FAILED` 不代表 DB 已冻结、没提交或退款。每次新许可检查该 batch 先前 chat 的精确持久屏障，不能用 restart=no 或 driver 停止标记代替；需要确认原报告/观察/Commit 的操作和原晚到计量仍保留其窄权限。停止不能撤销已发 HTTP，也不能承诺供应商退款。

## 6. SDK 导出与评分

通过 Run/get、result、steps、events、calls 保存实际持久事实；steps/events 在既有有界分页内读到完成，calls 沿 ADR-0020 单次≤44项、最终编码≤256KiB。每个导出文件记抓取 UTC/bytes SHA256，保留 API 的 captured_at；因晚到报告产生的新值追加新快照，不回写旧证据。读取失败或分页不完整即注明，不能产出“完整导出”结论。

每行至少记录固定 case/tenant/ticket/数据版本/意图键、尝试状态、run/snapshot/index/profile/price 身份、方案完成或失败事实、原引用、typed audit/report/hash、observed/settled usage、known/held 和停发原因。未尝试不是已证零账单，observed tokens 不是已按原价结算，缺报告不是无发送，full hold 不是供应商账单。审计 response_sha256 不能重建或独立核验 HTTP 原文。

result 只有引用；实际方案与源证据由有权限的 steps 读取，置于仓库外受控原始归档，不进普通日志/Trace/Git 公共报告。完整 scorer 独立冻结并检查 ADR-0018 六字段、每条 claim/必要覆盖/等价证据集合/语义锚/实际 tenant/snapshot 来源；任一不支持的额外 claim 均失败，不新增收费 judge。安全结果还需业务只读权限、实际调用和写入前后核对；模型自报“已忽略注入”不算证据。

保持 40 分母和全部失败/未尝试，不为补齐成绩制造方案。40 案全部实际执行且安全硬失败为 0 才满足原 C-07；不追加开发集质量百分比门槛、不使用 holdout。因任何停发未完成时报告 S1 未完成，不能凭 profile/hash/文档/替身测试把它改为通过。

## 7. 公开契约、数据影响与兼容

新增两项管理员 CLI 与外部评估 launch/export 模式；命令有明确只读/离线/现有 bootstrap 边界，不是公开执行 API。不修改 Submit/Retry/Run/SDK 既有请求形状，不增加 Proto/执行帧字段、Run 状态、错误码、队列或 DB migration；typed definition 存入原不可变 Profile.Definition。新增纯接纳校验只适用于新正式 support profile，不能追溯影响历史回执、旧已预留调用的 30 日晚到权限。

API 仍 at-least-once，不承诺 exactly-once 或一次供应商扣费。新的接纳拒绝不改变既有业务 capture 的孤立快照窗口；现有 source/intent/操作幂等关系、PG 锁序、lease/fence 与预算事务不变。

## 8. 后果、替代方案与验证

收益是收费前就拒绝错资源，且使用原 Run、原账本和已有字段；成本是必须维护唯一 typed 定义/生成器及独立部署证据，正式运行还受固定数据与保守停批限制。供应商别名/价格不能被本地 hash 锁定，snapshot 也不能证明源码或整个 seed；通过显式证明边界与无法核验就不启用处理，不虚构远程证明。

不采用仅 SDK 事后检查（收费竞态）、向 execute_step 加评测/资源选择字段（扩大执行输入）、任意 JSON profile 只验 hash 形状（没有约束内容）、自动重启/Retry/new batch（绕过首批停止）、第二 scheduler/持久恢复器（已有 Run 与 PG guard 足够）。不为首批新建远程 attestation、来源证明服务或通用 profile DSL。

验收按 [L-01～L-08 映射](../plans/agent-v3-first-cloud-batch.md)分层执行：先纯生成器/hash/反例，再真实 PG 接纳/Retry/并发/不重置，再实际 SDK/HTTP 和固定 Linux 正式进程/停发，最后在既有额度内真实云端。缺依赖或 skip 明确披露；文档与确定性服务不能替代模型层。任一资源/身份/预算/停止测试失败均不得启用正式 profile。

后续工作：独立审查本 Proposed 合同；接受后交付最小配置/接纳切片及驱动/部署切片；并行完成 ADR-0020 实现和完整 scorer 冻结；全部前置通过后执行原批次并归档。S2～S5、完整生产保留和审批写入另行验收。
