# S5 v8 完整政策前置读取验证

按 [ADR-0038](../adr/0038-policy-bundle-before-adjudication.md) 实现 Agent schema16 / prompt-v8 / executor5 与 Fixed schema17 / prompt-v2 / fixed-executor2。本记录是免费机制验证，尚无 v8 真实模型质量结果；本轮新增收费 0，S5 累计 observed-usage 估算仍为 **¥13.165020**，原账本 held/unknown/在途/未结束 attempt 均为 0。

## 原因与实现

v7 已见 004 缺 P05.1，但补证入口和工具余量均存在，首次 thinking 用满 16,384 completion 后未提交 final。这证明提示和可选补证没有保证必要政策实际到达；不能据此断言缺段导致输出耗尽。

新路径由 Go 在 `read_ticket` 后、任何模型调用前安排 `read_policy_bundle`，从授权快照索引固定读取 P01.1 至 P10.2 全部 20 段。每案同样读取，不依赖案例 ID、gold、关键词、初稿或模型分类。新只读 HTTP 不接受参数，保留实际来源六字段，不含 distance，独立 32 KiB 上限；缺段、额外别名、错误绑定、空正文或超限均失败关闭。

Agent 随后调查事实、提交 marker，再执行一次独立 thinking final；Fixed 沿固定事实读取图执行同一 final。两者终裁提示和相同事实下的 messages 完全一致，不带调查初稿、工具导航或补证分支，只接受嵌套 final envelope，不做格式失败重试。提示已重写，不再继承 v5 的逐项条件枚举；因此完整输入不代表旧语义行为必然保持，004/018/DEV-020 仍须真实回归。

新工具计一次逻辑工具、一次业务 HTTP 和一次物理调用，无 embedding/chat。原每 Run 8 工具、12 chat、44 物理请求、16,384 completion / 120 秒终裁不变，两策略 family token 均为 12,610,560。Proto、SDK、Go/Python、公开 calls enum、profile/schema、评分与冻结入口已接入；新增 migration0030 扩展现有工具/步骤约束。原租约、取消、提交和未知 hold 合同不变，已提交包恢复时不重读。

## 实际验证

| 检查 | 结果 |
|---|---|
| 全仓 Go race | `go test -race -count=1 ./...` exit 0；integration 515.334 秒、business 27.737 秒，真实 PG/AOF Redis/安装 SDK。平台与专用环境 skip 不计专项通过 |
| 新工具真实 PG | `TestRunPolicyBundle` 通过：双策略完整流程、单次工具计量、提交幂等、提交前/确认未提交/提交后恢复、取消、陈旧租约与工具额度；`TestCompletePolicyBundle` 验证完整/逆序装载、缺段/额外别名/跨租户拒绝和 HTTP 上限 |
| 固定 Linux | Python 1,497 passed、无 skip；实际进程与 Worker 协调测试通过。PG/RPC 联合层分两段完成：原完整命令在1200秒总超时退出，此前21项 Agent 子用例全部通过；尾部 Fixed/S5/Launcher 单独补跑 exit 0，共26.60秒。新 bundle Agent/Fixed/取消分别7.85/5.61/6.50秒通过 |
| 宿主 Python | Agent、SDK、evaluation、S5、恢复/审批/生命周期/诊断合计 2,225 passed、13 skipped；评分及冻结修复后 evaluation/S5 451 passed、4 Linux-only skipped |
| Go 静态检查 | build、vet、golangci-lint 通过，lint 0 issues |
| Python 静态检查 | Ruff 检查与 165 文件格式通过；mypy Agent 33 文件、SDK 19 文件、evaluation/S5 37 文件通过 |
| SQL / Proto | migration0030 SQLFluff 与历史基线检查通过；Buf lint、breaking against main 与生成通过；`git diff --check` 通过 |
| 历史兼容性 | 独立重建 v4/v5/v6/v7 共 157 个实际 Run、848/848 chat 参数哈希一致；200 案原分母保留。v5/v6/v7 四批评分对象完全一致；v4 原业务/安全/费用字段一致，仅有此前既有的离线度量增量 |
| 镜像与来源 | 新业务镜像及 Worker/Approval/S5 三层构建通过；安装 Python/SDK/operator 源码逐文件哈希与工作树一致，registry 仍只有两个生产 adapter，无 gold、测试 registry 或 host-only 守卫 |
| 远程 CI / 真实模型质量 | 未运行 |

独立审查已复核来源、同能力 Fixed、final-only、预算和版本隔离。审查发现的终裁闭合评分路由及冻结源码遗漏已修复；后续自查的评分器 16 KiB 原始步骤限制也已修复。两策略新摘要均为 schema3，只有唯一已提交 final 才计实际双闭合；Fixed 不伪造 draft。32 KiB 放宽只作用于新版本 bundle，旧结果上限不变。定向回归及独立复核通过。

原联合命令的 exit 1 保留为总超时，不记作一次完整命令通过；尾部补跑覆盖原先中断及未执行用例。测试镜像总期限已从1200秒调整为1500秒并重建，未改变单次模型、attempt 或 Run 的生产期限；未重复运行已经通过的前段。

Linux 模型、embedding 和部分业务 HTTP 响应为合成数据；真实数据库与进程通过不替代云端业务质量。

## 构建绑定与下一步

私有工件位于 `E:/JobForgeEvidence/s5-2026-10-07/v8-20261013`。来自未提交工作树，不是正式冻结；交接文档未修改或提交。

| 工件 | SHA-256 |
|---|---|
| 300 文件 source snapshot | `177958bfef1e393dd531b3f90273e5e25c58314d811d191052423095598f42ef` |
| Worker | `dde83c286bc0b9b2febe828b5545d90d273a48f91956ba7d4f134ece0b98e996` |
| Approval base | `6ec088423483ae57bdd4085e7b1e07aab0a922de0c9fc04937071876caa514d0` |
| S5 operator | `d67beca0d7c4ff52368c349179ee247e495dcda789da81b0431f42825d70cc5d` |
| Business | `6e6968a5db7e7690b29cec5334354b168014b75d30f4c0ec2834b81d7728841d` |

新 schema16/17 冻结还绑定协议会话、输入投影、executor schema 和生成 RPC 源码，构建 receipt 必须使用 `source_hashes(16)`。旧集合不扩大，旧 receipt 不能用于新 pair。

免费机制验证与独立复核已完成，下一步另审新身份的完整开发 40 案 manifest；第 10 案后独立语义复核，原正确 36/40、证据 38/40 与已见守卫不变。每批 ¥4、累计 ¥28，不续跑旧停止批次、不选择性重跑 004；当前未放行新收费，也未创建新未见集。
