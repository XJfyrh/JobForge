# S1 收尾：独立环境最小复现记录

日期：2026-09-17（UTC+8）；执行时间约 00:18～00:25。独立 Codex Agent 从 `s1-cloud-launch` 工作树构建，开始时源码 HEAD 为 `3790da9083e5a99277f74aa737e03f801ddefef9`。本记录只证明干净部署、安装 SDK、业务读取和停止/清理步骤；收费模型请求 **0**，本次新 embedding 请求 **0**，不作为新的 40 案云端验收。

操作路线见[云端运行指南](../agent-v3-cloud-batch.md)，业务初始化详细命令见[业务指南](../agent-v3-business.md)。未改 seed、gold、scorer 或保留集；未重跑无关全量测试。

## 环境与构建

独立 Compose project `jobforge-s1-closeout-repro`，业务/控制 PG 分别使用 55461/55462，HTTP 18094/18095，RPC 19095，端口均绑定 loopback。两个全新 volumes 和独立 network 只属于该 project；未使用共享测试 5433，未迁移或清理真实 5434/5435 库。未启动 Worker、Ollama 或收费 profile。

| 工件 | 实际身份 |
|---|---|
| business 镜像 | `sha256:3bdd430f364b0fce234286bc60afbf7a0a38c1b3a224f896ceb1e1d023d64826` |
| control 镜像 | `sha256:c0591a9d6d7cd2e530467726d9c5a49b108f7e7410563a1e35f5b64cbf440479` |
| 安装后的 SDK | 全新外部 venv 中 `jobforge==0.1.0`，确认 import 来自 site-packages；构建 wheel SHA256 `d627cbfdc3cdc08c1c20f157ac643fe44294bb45b642e63e815821d51890e9e9` |
| 重放向量工件 | 原真实 prepare `8e4d0751-4bb5-4d51-96e1-5aee3b217d72`，upload SHA256 `9818fdbdf70b7464d3cda1b824c5c18cc0ab5a819a6e5f41ddbf861169e3d525` |
| 固定向量 profile | all-minilm:22m，384 维，20 段；digest `1b226e2802dbb772b5fc32a58f103ca1804ef7501331012de126ab22f67475ef` |

原真实向量及 19/20、RQ-06 未命中的质量结果见[既有 v2 证据](agent-v3-s1-support-2026-09-16.md)。本次仅验证原工件 SHA256、以正式 loader 导入并查询 pgvector，不重复计算 embedding，不报新的检索质量分数。

## 实际执行结果

| 步骤 | 结果 |
|---|---|
| 构建 | 使用仓库 `Dockerfile.business` / `Dockerfile.agent-control` 构建独立镜像成功 |
| 业务初始化 | 干净 PG 执行 `migrate --initialize` 成功，loader `seed --file .../runtime/seed.json` 成功；40 工单、38 订单、37 物流 |
| 发布索引 | 北/南各 `publish-index` 一次成功；实际 profile/content hash 与冻结 v2 一致 |
| 控制初始化 | 空配置 bootstrap 应用全部 24 migrations；profile、Run、physical call、Worker session 均为 0 |
| readiness | 业务/控制均 HTTP 200 |
| 快照与读取 | operator 创建测试快照；同一 request key 返回同一快照；reader 读取 snapshot/order/delivery 均 200 |
| 租户与权限 | 南租户读取北快照 404；reader 创建快照 403；7 个业务事实表无表级/列级写权限 |
| 索引 HTTP | 使用原工件的一个真实段落向量查询，返回 3 个匹配且原段排首位；仅说明传输/索引可用 |
| SDK | `pip install ./sdk/python` 后用 `RunClient.list(limit=20)` 读取真实空控制 API，items 空、cursor null |
| 停止 | `compose stop --timeout 10` 后四容器均 exited 0、OOM false、RestartCount 0 |
| 清理 | 先留存退出事实，再删除仅该 project 的四容器、两个 volumes 和 network；相应 label 查询为空，原真实业务/控制/PG/Ollama 五容器仍运行 |

新索引 UUID 为 north `4f6fdc05-5fc7-4bd5-879d-0fc6c3795fd9`、south `5d783490-c27a-4830-9a1b-d743456666ae`；profile hash `6c2409708d68e3a628fa04f0d0c8b138972de638be652e2e3782efca523e165e`、content hash `a3a9eab29b1e7bd740c66c58084bf7b42a78ddf00049da2cbb087a5e06190267`。这证明干净库产生自己的索引身份，source 必须填实际发布结果，不能沿用旧库 UUID。检查快照 `8b6c6fde-a18b-4d77-bc6a-a5bdff70e018` 仅存在于已清理的复现库。

## 复现中的部署边界

实际保留业务库是 `jobforge_support_v2_20260916`，由外部 `support-v2-compose.override.yaml` 指定；默认 `jobforge_business` 仍是另一个历史库。只读核对实际 v2 库后，其所有事实摘要与首批 business-after 一致，reader 写权限全部 false。云指南已把审计数据库参数显式化，避免在错误库上取证。

收尾新身份在旧控制库 bootstrap 时受到 tenant `(scope, scope_key)` 唯一约束拒绝。该准备失败发生在 Submit、收费和 attempted 之前；旧库保留 disabled 新 profile、零用量新 batch 及失败日志。控制面和 launcher 随后同时配置独立控制库 `jobforge_s1_closeout_20260917`，保留已冻结的新身份，不删除旧库，不恢复原批。该新库于 `2026-09-16T16:26:07.005007Z` 的只读 inspect 显示 migrations/config 匹配、三个账户未冻结且 known/held/用量全零、business request/Run/call/session/startup 全零；这只是部署准入事实，不是模型验收。

后续收费仍以 [PR #53](https://github.com/XJfyrh/JobForge/pull/53) 中增量合同独立审查通过并合并生效为前提；累计 5 CNY 及所有旧批停止/确认屏障不因新控制库而消失。无需更改预算 schema、覆盖旧 tenant 账户或清除旧记录来完成本次部署。

## 原批只读准入核对

这是保留真实库的只读核对，与上面的可重建复现分开。采样 `2026-09-16T16:13～16:15Z`：原收费容器 exited/PID 0、ExitCode 1、OOM false、restart 0，原 Worker session 已过期，没有 active lease/call 或未到期调用 deadline。账本已知 97,526 microyuan（0.097526 CNY，登记费率估算）、52,701 tokens；货币 hold 0，保留一个 unknown embedding 的 512-token hold。原费用属于旧授权，不能被写成供应商结算账单或退款。

`2026-09-16T16:21:24.679131Z` 使用外部 Go `-overlay` 临时测试，在 `REPEATABLE READ / READ ONLY` 事务中调用实际 `checkBatchAuditGuard`；`transaction_read_only=on`。全部 **28 chat = 23 原步骤 Commit + 5 窄二次纠正终态失败**通过。真实生产 guard 校验原报告、known usage、普通观察及其 hash、原 attempt worker/session/fence、执行绑定/Commit hash 和窄失败条件；没有仅凭“28 known”推断确认完整。该检查不改变数据库，也不重跑收费模型。

原 Run 后来均已 failed：5 `MODEL_PROTOCOL_ERROR`，11 `RUN_DEADLINE_EXCEEDED`。后者发生于 15:26～15:27Z；不覆盖首批原始导出的 10 份 awaiting_approval 方案、5 个纠正失败及第 16 案不完整事实，也不补造旧中断根因。

## 证据与限制

仓库外原始记录位于 `E:\JobForge-notes\2026-09-16-agent-v3-s1`：

- `s1-closeout-repro.compose.yaml`、`s1_closeout_repro_http.py`、`s1-closeout-repro-http-result.json`：精确环境及 HTTP/SDK 结果。
- `s1-closeout-repro-business-audit.json`、`s1-closeout-repro-control-audit.txt`：事实摘要、权限及控制库计数。
- `s1-closeout-repro-running.jsonl`、`s1-closeout-repro-stopped.jsonl`：容器运行/退出事实。
- `s1-closeout-reproduction.md`：完整命令及仅本 project 清理记录。
- `s1-closeout-preflight-audit.md`、`s1-closeout-guard-audit.txt`、`s1_closeout_guard_audit_test.go`：原批费用与实际只读持久 guard 核对。
- `cloud-closeout-20260917/bootstrap.log`、`bootstrap-isolated.log`、`setup-before.json`：同库准备失败保留、新控制库初始化和未启动检查。

构建镜像缓存和独立评估 venv 保留；未执行系统 prune。私有原始正文和秘密不进入本报告。未执行新的收费 40 案、模型质量重验、审批写入、S2～S5 或生产长期留存。历史 W4 性能失败、AT-25 skip 和 RQ-06 未命中仍单列；本次部署复现不能代替正式模型报告、最终 CI 与独立 PR 审查。
