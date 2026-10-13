# S5 v7 状态政策前置读取验证

依据 [ADR-0037](../adr/0037-status-policy-read-prerequisite.md)，新增 schema 15 / prompt-v7 / executor4，decision-v2 与 Fixed schema 12 不变。此记录覆盖免费机制验证，真实模型质量尚未验证；截至本次准备未新增收费，累计 observed-usage 估算 **¥11.644792**，held/unknown/在途/未结束 attempt 均为 0。

## 原因与变更

v6 的 DEV-031、DEV-033 最终输入都包含权威 `informational_only` 状态及 P08.1 补证目录，但只有 DEV-031 发起补证。DEV-033 漏读状态政策并提出错误行动；没有截断、期限或费用耗尽证据。问题在于必需政策的发现仍由模型自行决定，不是补证执行失效。

v7 在调查 marker 后、首次 thinking 前校验权威状态的实际政策来源：`informational_only` 必须取得 P08.1，`escalated` 必须取得 P08.2。缺失时由 Go 执行原固定 `ticket_status` 查询一次；已有别名不重读，查询已用但缺失则失败关闭。空结果、错误来源、跨快照或跨政策结果均不得进入 thinking。

这次前置读取不产生模型意图，也不占用原有一次可选补证，但共用原单 Run 工具、调用、费用和期限限制。它只要求读到政策，不强制业务行动；信息缺失、争议、冲突等例外仍由终裁判断。恢复沿用已提交 marker/read/final，发送准入同时检查前置条件。无数据库迁移。

## 实际验证

| 检查 | 结果 |
|---|---|
| 全仓 Go race | `go test -race -count=1 ./...` exit 0，integration 502.043 秒；真实 PG、AOF Redis、已安装 SDK。平台及专用环境 skip 不计专项通过 |
| PostgreSQL 状态读取 | 恢复提交边界、陈旧发送、取消、unknown full hold 和 7/8 工具边界通过，包含在全仓 race 中 |
| 固定 Linux 联合链路 | 5 项通过，58.31 秒：原受控补证、新前置读取、空结果、前置读取后另一次补证、首次 thinking 取消。成功路径 5 工具/13 步/6 chat/20 物理调用；另补证 6/15/7/25；空结果 failed/5 chat/19 物理调用，未发 thinking |
| Python Agent/evaluation/S5 | 1,892 passed，12 skipped；skip 不计验收通过。私有 driver/宿主守卫另 19 passed |
| Go build/vet/golangci-lint | 通过，lint 0 issues |
| Ruff/mypy | Ruff 检查与 162 文件格式通过；mypy Agent 32 文件、evaluation/S5 36 文件通过 |
| 历史请求与评分 | 独立重建 v4 开发、v4 已见、v5 开发、v6 开发共 612 个实际 parameter hash，全部一致；原 v6 完整 registration/score_export 重算与历史 score.json 完全相等 |
| 镜像与来源 | 三层构建及安装来源核对通过，registry 只有两个生产 adapter；无测试 registry、gold 或源码目录 |
| 远程 8 项 CI、v7 云端质量 | 未运行 |

Linux 测试使用真实 Go、安装 Python、进程监管、PG 和 RPC；模型、embedding、业务 HTTP 响应为合成数据，不替代云端质量验收。独立审查 `/root/s5_design_review` 已接受核心实现；发现的 v6/v7 registration 元组遗漏已修复，并用两版本完整评分入口回归。具体收费 manifest 仍须独立放行。

## 构建绑定

构建来自未提交工作树，不是正式冻结。私有目录：`E:/JobForgeEvidence/s5-2026-10-07/v7-20261013`。

| 工件 | SHA-256 |
|---|---|
| 278 文件 source snapshot | `c2952c3eb27bd6a2f41cb886890cd8c6dca33e484c4af6e42a9c33ddb0fa2638` |
| Worker | `de9b44698d20ce10f4f5163886747d7c3f62bccc969a69be524629a596b017d4` |
| Approval base | `610559b341bdf0f468327e4eff27e6534ac211109ed3f943612da12fec345509` |
| S5 operator | `ece8fc40d6baf62c608ef6492934d6e6c7311f52acdda463ab3e02e3bdaddb53` |
| Linux integration | `9fef8b8312803e347e798bf343428b664e3f362dc706ca2b7219822286260264` |

下一步为新身份的原开发 40 案，第 10 案后独立语义检查；每批 ¥4、累计 ¥28，逐案守卫和原分母不变。不续跑 v5/v6，不创建新未见集，不把免费机制通过计为方案质量通过。
