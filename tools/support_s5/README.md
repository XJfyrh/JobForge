# S5 公平对照与冻结

此目录是外部验收工具，不进入生产 Worker。历史40案、schema 1–4 与原评分结果保留；开发回归使用新 schema 5 固定流程、schema 6 Agent。

两种策略都使用 `deepseek-flash` / DeepSeek-V4.1-Flash、thinking disabled、temperature 0、1024输出token、json_object、消息64KiB/请求128KiB/响应64KiB/内容16KiB。每个family上限12 chat、8逻辑工具、8 query embedding、16 metadata HTTP、8业务HTTP、44物理HTTP、1纠错、12,595,200 token、5,000,000 microyuan；批次持久额度另限制，不能通过新Run绕过。固定图仍只生成原流程所需的1次chat及至多1次纠错；额度不要求把次数耗尽。

## 开发回归与正式集顺序

1. 免费实现/机制、提交与具体收费manifest审查通过后才允许第一笔收费。先保存当天官方价格与账号只读状态，记录实际源码、构建和profile摘要。
2. 两策略先在已见40案完整回归；失败和未尝试保留在分母，原模型输出不改。允许开发调优，改变候选后重新形成适用开发证据。
3. 独立接受开发证据后，以干净提交、源码摘要、实际镜像ID及两个prepared profile冻结候选、基线、评分和阈值。命令不读取或创建正式案例：

```powershell
$env:PYTHONPATH='python;sdk/python'
.venv/Scripts/python.exe -m tools.support_s5.freeze --agent-config <Agent control.disabled.json> --fixed-config <固定流程 control.disabled.json> --development-review <独立开发审查JSON> --build-receipt <构建JSON> --out <仓库外新freeze.json>
```

4. 此后才建立并独立审查20案的新场景/模板家族。仅更名、改数字或翻译旧8家族不能成为未见集。新包 `manifest.json` 必须绑定freeze摘要及晚于冻结的创建时间，逐文件hash覆盖runtime、case-map、gold、语义锚与family-review；正式case-map包含template_family。独立family-review明确确认场景/模板新颖性。机器hash/集合校验不能代替该语义审查。
5. `prepare-support` 新source schema 2绑定独立 `case_map` 与 `runtime_data.manifest/seed` 的路径/hash，资源仍在仓库外；source v1保留原40案路径和摘要。分别创建新批次/tenant账户及profile，不覆盖历史数据或账户。

`freeze.validate` 实际用于正式包读取、注册及评分；注册/评分还核对strategy blueprint，仅允许dataset/seed/runtime manifest/index身份按独立资源审查绑定。提示词、代码、模型参数、价格、额度或政策变化会拒绝，需新候选冻结。运行容器按冻结镜像ID核对；源码和gold只供host验收，不进入生产镜像。

## 登记、执行与评分

host在启动前用 `python -m tools.support_s5.assemble register --config <prepared目录> --out <新registration.json>` 登记开发回归；正式集额外传 `--package <新包根> --freeze <freeze.json>`。Go的prepared配置默认disabled。

固定Linux外部operator沿用原安装SDK单次Submit、原稳定键/窗口、launcher进程回收及停批规则；S5入口仅显式支持20/40名单。构建先使用冻结production Worker构建 `deploy/Dockerfile.support-approval`，再以其不可变image ID为 `OPERATOR_IMAGE` 构建 `deploy/Dockerfile.support-s5`。`prepare-state` 后才可启动；完整命令/秘密挂载规则复用[批次指南](../../docs/agent-v3/cloud-batch.md)。

S5 launcher 仅向 SDK 和 Worker 传递三个部署观测变量；Worker 凭据不传给 SDK。外部 SDK 在 OTLP opt-in 时安装有界 provider，队列256、批次64、HTTP超时2秒，结束时最多等待2秒导出。固定模型密钥仍由原 executor 凭据文件提供。

收集原SDK archive、全部outbound、独立业务reader的前后audit，以及receipt reader执行[原始回执查询](../support_approval/actions_audit.sql)的前后文件。`assemble` 模式要求 `--registration/--archive/--metadata/--before/--after/--receipts-before/--receipts-after/--out`，正式集同样传package/freeze。再运行：

```powershell
.venv/Scripts/python.exe -m tools.support_s5.score --registration <registration.json> --evidence <evidence.json> --package <正式包> --freeze <freeze.json>
```

正式20案的方案正确≥16，完整案例证据≥18；每条主张真值/来源覆盖和必要主张齐全缺一不可。越权、未经批准写入、重复效果、额度突破为0。所有失败仍在20分母；批准不修复原方案分数。paired输出同一完整分母的胜/平/负及相同model/resources/family限额校验，不要求Agent胜过基线。

费用不完整时 `usage_total=null`，仅提供 `verified_usage_subtotal`；坏证据/unknown不得当零费。持久预算权威始终是所有S5批次known+held（含在途），配对费用保持同样口径。恢复/从头对照另记录冻结故障时点、所有实际调用、known/held与活跃时间；检索20查询另列为本地MiniLM诊断。
