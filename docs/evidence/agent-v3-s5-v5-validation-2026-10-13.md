# S5 v5 候选验证

日期：2026-10-13。契约：[ADR-0034](../adr/0034-s5-final-stage-separation-and-condition-enumeration.md)、[ADR-0035](../adr/0035-s5-batch-reservation-and-v5-replay.md)。本轮不修改提示词、政策、gold 或历史证据，不计新的未见正式验收。[机器摘要](agent-v3-s5-v5-validation-2026-10-13.json)保留逐案结果、费用与原工件摘要。

**结论：终裁回放18/20，开发回归因DEV-020再次出现双闭合缺口，在第20案后停止。原40分母中19案通过、1案失败、20案未执行；不进入后继批次或冻结。**

## 固定输入终裁回放

原 v4 已见 20 案各重放一次终裁，全部保留在分母。先逐字节重建原 118 次 chat 的 `parameter_hash`，再用实际 v5 adapter 与 `prepare_chat_request` 投影终裁请求。新请求不包含初稿、工具额度或检索导航，不补政策。

| 指标 | 原 v4 终裁 | v5 回放 |
|---|---:|---:|
| 正确方案 | 17/20 | 18/20 |
| 完整证据 | 17/20 | 18/20 |
| 协议 / 来源通过 | 20/20 | 20/20 |
| 新增调用 / 生产 Run | — | 20 / 0 |

017 修复，原正确的 17 案没有回退。004 仍漏结构化冲突，继承输入缺 P05.1；018 的结论与时效分支已正确，但输入未取得时效政策，用 P06.1 引用仍不满足覆盖。可引用闭合缺口为 018，定义闭合缺口为 004/018，均为旧调查输入的属性，不能据此判断 v5 调查成败。

20 次调用均取得 known usage，没有截断、工具返回或 unknown。耗时中位数 16.29 秒，最大 74.637 秒；最大 completion 为 14,025/16,384，剩余 2,359 tokens。20 次无协议失败不能证明该故障率归零。

本批 observed-usage 估算 **¥0.672523**，held 为 0。收尾重新读取 **24 个 PostgreSQL 数据库**，累计 **449 Runs、1,828 chat、¥9.948164**；held、unknown、在途和未结束 attempt 均为 0，距 ¥28 上限剩余 ¥18.051836。费用按冻结费率估算，不是供应商结算账单。

独立审查复算与原 `score.json` 完全一致，接受进入开发回归准备阶段。该回放经过隔离诊断 Go runner 的真实模型 HTTP 与 PG 费用守卫，**没有经过生产 Run、IPC v2、调查检索和审批链**，`thresholds_met=false`。

## 开发回归

原名单为 DEV-001..040。沿用生产 Go/Python/SDK 链与真实模型、检索及业务服务；宿主评分不反馈 gold 或修改建议给模型。外部 SDK 守卫在每案导出后阻塞，宿主核对闭合、来源、费用与剩余可达门槛后才继续。第 10 案另经独立语义审阅：原 SDK/outbound 重建、52 次请求哈希、时区与 48 小时边界均通过，才在原进程继续 DEV-011..040，没有重发前 10 案。

| 结果 | 实际值 |
|---|---:|
| 原分母 / 已执行 / 未执行 | 40 / 20 / 20 |
| 正确方案 / 完整证据 | 19/40 / 19/40 |
| 已执行案协议与来源通过 | 20/20 |
| 可引用闭合 / 定义闭合 | 各 19/20 完整；均在 DEV-020 缺失 |
| 已执行案四类硬失败 | 0 |
| 实际 chat / 政策检索 | 101 / 23 |
| 新增 observed-usage 估算 / held | ¥0.643598 / ¥0 |

DEV-020 的 `ticket.problem_description` 缺失，必需主张为 `missing:ticket.problem_description`。调查只取到 P02.1、P07.1、P07.2，未取到任一 P03.1/P03.2；因此可引用闭合与定义闭合同时失败。初稿只主张时效，遗漏必需的 missing 主张；终裁改为 missing，谓词为真但来源覆盖为假，最终仍有 `UNSUPPORTED_CLAIM` / `REQUIRED_CLAIM_MISSING`。其终裁消耗 7,660 reasoning tokens、45.489 秒，未截断、未触及额度上限。提示级枚举没有确保调查执行必要检索，增加终裁思考不能补回不存在的政策。

守卫在第 20 案导出后写入 STOP，DEV-021..040 保持未提交；已执行失败不重试，未执行不记为通过。收费 launcher 因该语义停止以 exit 1 结束，宿主完成证据、评分和账本收尾后 exit 0；Worker/driver 已回收，control/business 容器均已退出。未运行已见回归、Fixed 开发、候选冻结、新未见正式集、恢复或演示。

独立复核从原 SDK/outbound/业务回执重建 evidence、重算 score，并重建全部101个请求的 `parameter_hash`，结果均一致；接受本轮失败与停发处理，未发现计量、来源绑定或关闭缺口。DEV-020的completion为7,739/16,384，初稿和终裁均未达业务验收，不是“正确初稿被终裁破坏”。本轮没有格式失败证据，不支持追加终裁重试。

最终重新读取 **25 库**，S5 累计 **469 Runs、1,929 chat、¥10.591762**；held、unknown、在途与未结束 attempt 均为 **0**，距 ¥28 上限剩余 **¥17.408238**。这一余额不是继续本候选后继批次的理由：ADR-0034 的双闭合停止条件已触发。

下一步按 ADR-0034 的退出分支提出新的协议级调查闭合方案：让条件枚举及其证据成为可校验的 `investigation_complete` 标记，明确未闭合时如何返回调查阶段，再独立审阅。枚举完整不等于语义正确，不能嵌入 gold/评分器或无条件取齐政策。不能继续追加提示词后沿用本批，也不改变本次失败与评分。本轮未实施该契约变更。

## 审阅修复与免费验证

- schema 13 的正式包加载与直接正式评分均拒绝重新绑定的已见 v1/v2/v3 数据；显式历史重评继续可用。
- 回放评分绑定原始证据、原登记、历史包及冻结摘要，并重新投影请求进行逐字节比对。283 份受审源码已另行归档，旧回放不随后续工作树变化而失去源码依据。
- schema 3 诊断发送分支固定 20 案一次名单、thinking/high、16,384 completion、120 秒且不发送 temperature；跨账户仍累计本批 ¥4。旧 schema 1/2 保持原语义。
- `replay.py` 为宿主工具，不进入 S5 operator。CI 保留其他 operator 源码的逐文件哈希核对，新增 replay 与 diagnosis 不进入镜像的否定断言。

实际通过：Python 相关套件 **169 passed**；Ruff check/format；mypy 25 文件；诊断 Go lint/vet；真实 PostgreSQL 的诊断 `go test -race`；真实 PG/Redis 的全仓 `go test -race ./...`（exit 0）。Windows 平台 skip 不计 Linux 验收。

外部收费守卫另有 **32 passed**：实际串行 SDK 循环的第 10 案暂停、继续后仅提交余下 30、STOP/期限/凭证失败关闭、部分分母的可达上界、双闭合停止及独立审阅哈希绑定。独立审查发现宿主旧 PG 快照与新 ready 文件可能交错，已改为读取 ready 后重新核账并补回归测试；旧计划保留，新计划 r2 单独放行。

生产 Worker、approval operator、S5 operator 三层本地镜像构建成功，实容器核对源码、父层关系、生产 registry 仅两个 adapter、gold 与宿主诊断模块未进入镜像。构建父镜像使用本地标签并核对实际不可变 image ID，不能称为成功的直接 digest 构建。未运行远程 8 项 CI，未创建正式冻结。

## 原始材料

私有根目录：`E:/JobForgeEvidence/s5-2026-10-07/v5-20261013/`。原始模型正文、请求、响应、SDK 导出与凭据不入仓库。

| 工件 | SHA-256 |
|---|---|
| `final-replay-v5-batch1/manifest.json` | `7796dc9cc73e529247372d0188013e50209b39cc544f715caf4c655b2cd41549` |
| `final-replay-v5-batch1/score.json` | `2fc2df4b0c992eb38a01c86f3de8b5d88af961e5f78656ad1816ac6b6cc9ce41` |
| `final-replay-v5-batch1/ledger-export-1791865585788628100.json` | `ed5df8861dfaeb3ce705c1379895984506df18fde675e5b133ecaed9a6751bcb` |
| `batch1-reviewed-sources.zip`（283 文件） | `9fbbe951ba771220a51c6411ecc3d34c85387d423dbd75fbd3db9b5fd0e69106` |
| `closed-ledger-batch1.json`（24 库） | `d1e5295c9a31d43364e1b4f241665f160c7bf986bea241709b75e22fad1db9ee` |
| `budget-authorisation.json`（逐批 ¥4、累计 ¥28，会话批准转录） | `251113389847b6f427545491173cfabb5c45537ced39b04fd73f617b0e6f01b1` |
| `dev-agent/paid-plan-v2.json` | `a6c09f5aa51f3349597ecf229e072566d53eedb395b8385f17715d7d25a46191` |
| `dev-agent/score.json` | `0df2e04ac384f3ec05e103426f14ecbf2c9dfdb3719a2ef3dc2271a17bf4256a` |
| `dev-agent/evidence.json` | `b5f967c89204403feef6cec019d473d26c023b8d69fd3138964ae1f8a64cfa2b` |
| `dev-agent/closed-ledger.json`（25 库） | `6746c3f8869de4ac0c2a3c03849bb86bec4d2e666a5906611f0dde242915c86d` |
| `batch2-reviewed-plan.zip`（31 文件） | `0a3636d195a59ea9ef6480bdbb1be4742f4cd11ae22fd4d8c86e14be110250fd` |
| `final-review.json`（独立接受失败与停发） | `ccabff97d0387155f5c5cd11589daa1783b033313c3cb9886e65e2c383febb0f` |
