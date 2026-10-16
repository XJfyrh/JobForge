# S5 v6 受控补证机制验证

依据 [ADR-0036](../adr/0036-s5-controlled-policy-supplement.md)，新增 schema 14 / prompt-v6 / executor3 / decision-v2。此记录只覆盖免费机制，不代表真实模型质量通过；没有新增收费，累计 observed-usage 估算仍为 **¥10.591762**，held 0。旧 v5 失败、原输出与停止事实保留。

## 实现与修复

- 首次 thinking 可提交严格的补证意图；Go 调度一次固定政策查询，全部指定别名必须绑定实际来源，才继续独立 final-only 裁决。补证不产生 Proposal，不提供任意工具入口，也不重试格式失败。
- 已提交 intent、检索及 final 分别复用原持久化恢复路径。两次 thinking 各自预留、结算和审计；unknown 保留 full hold。
- 原 marker 双闭合保留；新增实际 final 输入双闭合、逐次 thinking 容量与期限。schema 14/12 配对只豁免已批准的 token 容量差异，并在结果中披露；其他比较字段继续严格一致。
- 独立审查与真实 Linux 测试发现并修复三处遗漏：执行检索前误调用旧查询入口、已接受补证被旧结果 finalizer 拒绝、配对报告误要求两策略 token 容量相同。补证目录不再列出禁止重复的固定查询，回归测试覆盖这些入口。

## 验证记录

| 检查 | 结果 |
|---|---|
| Python Agent、S5 与 evaluation 联合套件 | 1,828 passed，12 skipped；skip 不计验收通过。后续配对修复相关 15 passed，S5 全套 152 passed；最终 Agent v6 定向 25 passed |
| 旧恢复与审批验收工具 | 103 passed，1 skipped；未将旧计划扩展为 schema 14 |
| 真实 PostgreSQL 补证恢复与计量 | `go test -race ./tests/integration -run '^TestRunPolicySupplement' -count=1` 通过，覆盖 intent/search/final 提交边界与第二次 unknown |
| Go build/vet/golangci-lint | 通过，lint 0 issues |
| Ruff 全仓检查与格式 | 通过，157 个 Python 文件格式符合 |
| mypy | Agent 30 个文件、evaluation/S5/lifecycle/diagnosis 43 个文件、旧恢复/审批 18 个文件通过；后续配对修复的 S5 17 个文件通过 |
| 历史实际请求重建 | v4 开发 40、v4 已见 20、v5 已执行 20，共 435 个 parameter hash 一致；报告 SHA-256 仍为 `8592e43e79214ece4fb19dcee321f1e91d5c9a04fc2c4cd64a373598034b7ed3` |
| 全仓 Go race | 串行 `go test -race -count=1 ./...` 通过，exit 0，integration 463.267 秒；使用真实 PG/AOF Redis 与已安装 SDK。首轮因另一工作区同时使用测试服务而主动中断，不计通过；平台/专用环境 skip 不计专项验收通过 |
| 固定 Linux 实链 | 4 项通过，44.75 秒：旧裁决、补证成功、空检索、第二终裁取消。成功为 14 步/7 chat/21 物理调用，空检索为 failed/6 chat/20 物理调用且不发第二终裁；取消保留 full hold |
| 生产 Worker 镜像 | 构建通过；registry 仅含两个生产 adapter，30 个已安装 Python 源码 hash 与工作树一致，无测试 registry、测试 manifest 或 `/src` 目录 |
| 远程 8 项 CI、v6 真实模型批次 | 未运行 |

Linux 使用真实 Go/安装 Python/进程监管/PG/RPC，模型、embedding 和业务响应为合成 HTTP，不替代 DeepSeek/MiniLM 的质量验收。独立审查 `/root/s5_design_review` 已复核核心、冻结、新颖性及计量，无未解决确定阻断；最终免费检查已通过，可进入收费准备，不是收费放行。`git diff --check` 与本轮 Markdown 本地链接检查通过；没有执行 commit，交接文档保持不变。

镜像：integration `sha256:b88a4e16d080b7ae76f3ef9286e45db259f485fc8f700996d442b06370625571`；生产 Worker `sha256:0c3630dddf4610df7f7f4c1ac71d8a1e8d70050695f14f18e51e7c9754e38265`。构建来自未提交工作树，不称正式冻结。

## 下一步

新建开发 40 案 manifest 与逐案守卫，守卫改读 `final_input_closure`，不能直接复用 v5 的 marker 守卫；补证失败、未到 final 不记闭合通过。免费机制和具体包审查后再启动，前 10 案独立语义检查、每批 ¥4、累计 ¥28 不变，不续跑已停止的 v5。新未见集须开发达标并干净提交冻结后创建；旧恢复验收计划尚不支持 schema 14，后续须单独扩展，不能复用旧计划冒充通过。
