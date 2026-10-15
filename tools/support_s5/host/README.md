# 新批次共享工作流

当前入口用于完整政策候选 schema16/17/18 的完整开发40案、当前Agent18原已见V3完整20案及当前Agent18/Fixed17新冻结V4正式20案，规则见 [ADR-0039](../../../docs/adr/0039-s5-startup-review-and-automatic-health.md)。它取代逐版本复制的 `prepare_phase.py`、`manifest_checks.py`、`ledger.py`、`run_phase.py` 和私有 operator wrapper。历史脚本及证据只读保留；旧批次不迁移、不续跑。

先完成一个明确假设的最小诊断，收益成立才实现并完整回归；v8 已完成实现，直接回归。固定输入诊断复用现有 `tools.support_diagnosis`，冻结复用 `tools.support_s5.freeze`，不增加新平台或阶段次数/时间预算。

在仓库根目录激活开发 `.venv`（或用 `.venv/Scripts/python.exe`）。共享宿主入口明确加载本 checkout 的 `python`、`sdk/python` 源码，避免旧批次已安装包漂移；不依赖 pytest 或额外 `PYTHONPATH`。配置从 [example.config.json](example.config.json) 复制到仓库外的当前工作目录，填入真实路径及预先确定的范围：

- `source` 是现有 Go `prepare-support` 接受的候选源文件；候选、模型/价格、资源、构建/数据/评分/价格收据均由该文件绑定。提示词修改只维护必要版本。
- `build_receipt` 必须绑定当前干净提交的 `source_hashes(16/17/18)` 和四个固定 image ID。审计标签修复后必须重建，不能沿用旧 v8 镜像。构建仍复用现有 Dockerfile/构建流程。
- `inventory` 明列全部历史控制库及一个新库，逐行指定 `container/user/database`，诊断库加 `diagnosis: true`。`prior_ledger` 指向最近完整历史账本；不能遗漏其中任何库或复用其库名。以最新完整阶段账本为准，包含所有已停止与零调用库，再加一个新库；原始文件不改写。已记载的容器/角色绑定不能改变，各历史库 known 不能下降；旧账本未记容器时通过这份受审清单一次明确，仍检查 known 连续性。合法 full hold 结算下降不受阻。
- `secrets` 是私有目录，包含 `control.env`、`business.env`、`worker.json`、`driver.json`；控制 DSN、Worker 身份/凭据与新库、新 profile 保持一致。Fixed无action origin，不携带Agent的action reader/writer密钥；准备时在创建控制库前检查租户与动作凭据范围。只记录文件摘要，不输出内容。网络、ollama 与 business-postgres 别名沿用现有部署，业务数据/索引事先准备。
- `identity` 填入新的 profile、worker、batch、batch key、两租户预算 UUID 和 UTC 起点；既有准备命令生成六小时有效窗。`container_prefix` 使用唯一 `jobforge-s5-...` 名称。
- 同一 S5 使用同一个绝对路径 `stage_lock`，包括并发会话。其旁的 `.inventory.json` 记录已启动身份及最新账本，在发送前登记、闭账后刷新；后启动的旧配置遗漏新库会被拒绝，须复核配置差异，不能删除或重建清单绕过。金额可收紧，当前上限每批 ¥4/累计 ¥28；`full_hold_microyuan` 须与登记价格重新计算结果一致。

```powershell
python -m tools.support_s5.host.workflow prepare --config E:/CURRENT/config.json --output E:/CURRENT/NEW-BATCH
```

`prepare` 生成新目录、新身份的 prepared 配置、登记、固定 Docker 命令、账本和 `manifest.json`；创建新控制库并 bootstrap disabled，准备原 launcher 的单次启动状态。它不启动 Worker、不提交 Run、不调用模型。失败时保留准备痕迹，不能覆盖原目录/数据库重试；先查明原因，必要时用新身份准备。它不自动生成新候选、诊断结论或正式保留集。

维护者批准 manifest 的具体范围和金额，独立审查核对实现、免费检查及 manifest；将实际意见转录到 [example.startup-review.json](example.startup-review.json) 的结构，附原始授权/审查证据路径和 SHA-256。这份 JSON 是证据绑定记录，不能替代人实际作出的授权或独立审查，也不自带签名认证。工作流方案的批准不能填作具体收费批准。

```powershell
python -m tools.support_s5.host.workflow check --batch E:/CURRENT/NEW-BATCH --review E:/CURRENT/startup-review.json
python -m tools.support_s5.host.workflow run --batch E:/CURRENT/NEW-BATCH --review E:/CURRENT/startup-review.json
```

`check` 只核对摘要和版本；`run` 才启动批准的批次。健康检查覆盖每案，包括第 10 案，自动释放下一案。容器仅收到身份/哈希及 continue/stop，不收到 gold 或分数；任何宿主检查异常都不会放行下一案。普通错答、引用不全、一次已闭账格式失败计为质量失败；unknown、身份/来源异常、未经批准写入、政策包实际缺段、门槛不可达或预定义系统性失败停发。每次模型调用仍由 Go/PostgreSQL 许可和完整预留保护。

生成的 `checkpoints/NN` 保留全部逐案证据、score、累计账本和决策；批后保留 `evidence.json`、`score.json`、`closed-ledger.json`、`result.json`，费用标记为 observed-usage 估算。异常保留 `workflow-error.json` 和已有原始文件；清理失败保留启动锁，需要核对残留进程后人工处理。无自动重试、补样、旧名单续跑或自动版本开发。

36/40正确、38/40完整证据及其可达上界是Agent候选门槛。已验证注册为schema17 / `support_fixed_v1` / prompt-v2 / fixed-executor2的基线收集完整40案，不按Agent分数提前停止；其余安全、费用、绑定、政策包及系统性格式停发规则相同。角色由绑定的prepared注册源推导，无配置开关。Fixed结果记录`model_quality_applicable=false`、`model_quality_passed=null`和独立的`baseline_collection_complete`，低分不称候选质量通过。

批后一次审阅全部结果并决定下一步。`engineering_complete` 与 `model_quality_passed` 分列；这里的模型门槛是开发回归，不能代替 S5 总体验收。修复后只审查差异；范围/候选/模型/费用/权限变化须形成新绑定。文档只维护当前状态和实际结果，历史证据不改写。

## 已见输出约束诊断

`python -m tools.support_s5.host.contract_probe` 仅用于[当前三案缺陷](../../../docs/evidence/agent-v3-s5-v8-contract-analysis-2026-10-15.md)的六案固定输入对照。`prepare` 复建原22次chat哈希，生成V8/V8C共12次请求；`score` 沿用原真值/来源谓词，保留全部12分母。两者不调用模型，不注册生产候选。通过诊断也不能代替完整开发回归。

`run --experiment E:/CURRENT/EXPERIMENT --review E:/CURRENT/startup-review.json` 才启动原隔离Go诊断发送器。`startup-manifest.json` 必须绑定实际根目录、所有来源/请求/runner/私有配置摘要、干净提交、完整阶段库存及起止窗，并通过与开发批次相同的 `validate_review`。新诊断使用独立零调用库、共享stage锁和完整预留，禁止已尝试库重入。Go `--check --manifest ...` 可在无数据库、无模型调用时核对实际二进制和固定清单。

`prepare --plan tools/support_s5/host/description-probe.json` 复用同一入口生成C1/C2单句删减对照：配置固定DEV-016/017/020，各组三次，共18次；C1原字节保留，C2仅删除“问题描述是独立缺口”的额外提醒。私有诊断manifest5绑定名单和重复数，沿用deadline、结构健康检查、PG禁止重入及费用守卫，旧manifest4语义保持不变。候选须全部9次正确且完整，并至少一次DEV-016当轮配对改善；两组都通过时不宣称增益。固定配对完成与候选支持判据分开。

`prepare --plan tools/support_s5/host/critical-timing-probe.json` 只验证原V8追加critical/必要timing完整性要求，固定DEV-026/027、各组三次，共12次；不加入C1的上游关系或描述提醒。候选须全部6次正确且完整，并至少一次DEV-027当轮配对改善。manifest5的`probe.kind=critical-timing-completeness`绑定此队列，原省略kind的描述对照保持原语义。

`prepare --plan tools/support_s5/host/timing-evidence-probe.json` 以原V8T字节为基线，只追加每条outstanding timing自身须引用`E2#/delivery/events`的要求；026/027各三对共12次，候选须全6正确/完整且至少一次当轮同案同轮改善。模型、端点及权限保持固定，host仅接受登记的提示kind和评分目标。

schema5可用`groups`记录两个不同、最多64字节、仅字母数字/横线/下划线的组标签；顺序始终是baseline/candidate，冻结进manifest、trial及请求路径。省略groups时旧C1/C2、V8/V8T序列保持。组名不决定执行模式：schema5请求固定为final-only、thinking enabled/high，无temperature或调查字段；响应按实际已校验请求的thinking模式审计。更换标签不需要修改Go白名单，也不能改变模型、端点、权限或计费规则。

诊断库初始化会由PostgreSQL生成UUID。先只读查询实际`current_database()`及`diagnosis_database_identity.database_id`，再填入准备参数并与清单逐项核对；不能另生成一个UUID代替。数据库名最多63字节，超长配置在免费校验时拒绝。准备草案和失败痕迹保留，禁止用修改数据库身份绕过绑定。

诊断只改变final system，原生产V8和镜像内策略不变；费用仍每批¥4、累计¥28，不新增阶段次数或时间预算。授权有效窗传到Go请求context，父进程保留15秒结算宽限；unknown/full hold保留并留锁，不能把到期中断写成零费。

## 已验证提示的新候选

[独立引用对照](../../../docs/evidence/agent-v3-s5-v8-timing-evidence-2026-10-15.md)达到预登记支持条件后，Agent schema18 / `support-agent-prompt-v9`只将精确5,751字节final提示注册为新候选，复用v8调查、完整政策和executor5，旧schema16提示不变。新候选与Fixed17配对；两者仍具有同模型、工具/政策、数据和资源额度，Fixed保留原固定提示。冻结绑定全部三个已见正式集，新候选不能把旧集重新标为未见。完整40案需新库、新身份、实际重建镜像和一次联合启动审查；诊断通过不等于开发或S5质量达标。

## 原已见v3完整回归

配置schema1保留原DEV40行为。schema2只接受当前Agent18的原已见v3完整20案，增加`seen_v3`对象的两个绝对路径：`package`指向原包，`freeze`指向该包原冻结文件。source仍使用Go的schema2、原case-map/runtime摘要和已审资源；名单必须与原包全部20案顺序一致，policy固定为case_count20、minimum_correct16、minimum_full_evidence18，其余费用与健康规则保持。

原freeze只证明历史包来源，不代表v9已冻结。入口钉住原manifest和freeze的字节SHA，拒绝同ID重标的新包；source的case-map、runtime manifest/seed路径和摘要以及definition资源摘要必须绑定同一原包。准备、组装和评分均显式使用该历史包，全部原件和freeze摘要进入新manifest；评分固定`seen_diagnostic=True`，kind为`s5-seen-diagnostic`且`thresholds_met=false`。包、gold和评分只在宿主，不挂入operator；不能通过新增historical开关、更换dataset或删减名单绕过未见集守卫。

结果分别记录适用20案回归门槛和`freeze_guard_cases`（S5V3-004/018）。即使16/18通过，任一guard失败仍不能冻结；DEV-020另由同候选已接受开发结果证明。此模式不接受新未见集，不创建新案例，不自动冻结或重试；仍使用新身份和同一次具体启动审查。

## 当前冻结与未见V4

当前Agent18冻结会自动核对独立审阅中的两个完整开发报告及原V3完整回归。`development_registrations`按两个strategy记录`{path,sha256}`，`seen_regression`分别绑定`report`和`registration`；全部字节摘要及strategy blueprint必须匹配冻结候选。Agent开发36/40、38/40与已见16/20、18/20门槛不变；Fixed只要求完整基线，不套Agent门槛。DEV-020和S5V3-004/018须正确、证据完整且调查/最终的可引用和定义闭合均实际测得并通过。缺失、unattempted、unknown或硬失败拒绝冻结。该校验落实既有前置，不创建新案例。

配置schema3使用`formal_v4: {package, freeze}`两个绝对路径，只接受`support-s5-2026-10-15-v4`及当前18/17配对。包加载固定为严格非历史模式，核验当前freeze、创建顺序、全部三已见包和开发家族新颖性；source的case-map、runtime manifest/seed及资源摘要绑定同一新包。建库前核source blueprint，生成登记后再核实际blueprint，评分显式传入freeze且`seen_diagnostic=false`。不能用schema2、旧dataset、historical开关或缺freeze替代正式验收。

仍完整20名单、16正确/18完整证据及原安全/费用/系统性规则，Fixed沿用完整基线角色。新包和freeze原件进入manifest字节绑定，gold/评分仅在宿主；operator挂载与执行权限不扩展。代码提交、实际镜像构建与新freeze先完成，再创建并独立审查实际未见20案和具体收费manifest。
