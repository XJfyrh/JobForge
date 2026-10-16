# S5 固定输入裁决诊断

按 [ADR-0031](../../docs/adr/0031-s5-controlled-decision-diagnosis.md)比较原提示 A、thinking high B、完整政策 C、精简提示 D。使用已见 v3 正式 20 案，各组重复 3 次；结果不计正式验收，不执行工具、审批或业务写入。

Python 只读准备与评分，Go 串行调用真实模型。生产代码、历史候选、政策及 gold 不变。输出、推理、凭据和 PG 导出全部留在仓库外。

[ADR-0035](../../docs/adr/0035-s5-batch-reservation-and-v5-replay.md)新增显式 schema 3，供 [v5 固定输入终裁回放](../support_s5/README.md)复用隔离发送器：20 案各一次、120s、不传 temperature、单批累计 ¥4 / S5 累计 ¥28。它不是旧 A/B/C/D 实验的继续，也不是生产 Run/IPC 验收。下述 schema 1/2 的历史参数与 ¥20 合同不变。

## 原始首段

以下是 schema 1 的原始准备过程，执行使用首段冻结的旧 runner。当前 runner 对旧实验只执行下述 schema 2 修订，不重新执行首段。

1. 停止旧收费入口。`python -m tools.support_diagnosis.preflight --root ROOT --output PREFLIGHT` 刷新 20 库账本及官方模型、价格、余额。核对峰值费率后，同命令增加 `--confirm-tariff` 保存 receipt；此阶段没有 chat 调用。
2. 构建 `go build -o RUNNER ./tools/support_diagnosis/runner`。创建专用 `jobforge_s5_diagnosis_...` 空数据库，设置 `JOBFORGE_DIAGNOSIS_DSN` 后执行 `RUNNER --init`。迁移只用于该诊断库，不加入生产迁移。
3. 从 `diagnosis_database_identity` 读取 UUID。执行 `python -m tools.support_diagnosis.prepare --root ROOT --output EXPERIMENT --runner RUNNER --preflight PREFLIGHT --database-name DATABASE --database-id UUID`，冻结请求、名单、库身份和 runner hash。核对 source receipt 与原始 103 次请求重建结果。
4. 对精确冻结包完成审查后，执行 `RUNNER --manifest EXPERIMENT/manifest.json --credentials PRIVATE_WORKER_JSON`。只接受固定官方 origin/path、模型及请求参数，不支持自动重试或更换数据库续费。
5. 执行 `python -m tools.support_diagnosis.score --root ROOT --experiment EXPERIMENT --ledger EXPERIMENT/ledger-export-TIMESTAMP.json --output NEW_REPORT.json`。评分逐项核对 PG 的 manifest、请求、响应、报告及 usage 绑定；未完成、拒绝和无效输出留在分母。

Windows 使用 `.venv/Scripts/python.exe`，并设置 `PYTHONPATH=python;sdk/python`。`ROOT` 为已有 S5 仓库外证据根目录。每个输出目录必须是新目录，证据文件禁止覆盖。

## 已批准修订

[ADR-0032](../../docs/adr/0032-s5-reasoning-observation-amendment.md)将空推理改为观测项。保留首次 15 条记录，构建新的 runner，执行 `python -m tools.support_diagnosis.continuation --parent ORIGINAL_EXPERIMENT --ledger ORIGINAL_PG_EXPORT --output NEW_EXPERIMENT --runner NEW_RUNNER`。使用同一 `JOBFORGE_DIAGNOSIS_DSN` 和新 manifest 启动，只发送剩余 225 条。Go 先验证原 manifest、连续 known 前缀、请求与费用基数；原 stop 和冻结账户不修改。导出含两段记录，评分使用新 manifest 与新 PG 导出，输出另存，不覆盖旧分数。

## 费用与停止

S5 累计上限 ¥20，包含冻结的历史 known+held 及诊断库所有 batch。单批不超过 ¥4，下一次调用先预留最坏输入/输出费用。完整可信 usage 按冻结峰值费率计算 known；这是 observed usage 估算，不是供应商账单。

进程持有整个诊断库的唯一 sender lock。已预留 trial 不重发；unknown、计量异常、身份错误、非思考组返回推理、报告冲突或遗留预留停止实验。停止后保留原库、hold 和原始响应，不通过新库、清表或改写旧 manifest 继续。thinking 请求的空推理仅作为观测项记录。

## 免费检查

Go 真实 PG 测试创建并清理自己拥有的独立测试库，需 `JOBFORGE_TEST_DSN` 和创建数据库权限。使用 `go test -race ./tools/support_diagnosis/...`；全仓并发回归、Python、类型及 SQL 检查见[测试指南](../../docs/tests.md)。Python 测试和所有 lint 均不调用模型。
