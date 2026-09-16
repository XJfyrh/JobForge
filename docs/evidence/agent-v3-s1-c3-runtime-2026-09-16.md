# S1-C3b：固定执行器与持久控制确认验证

日期：2026-09-16。实现基线为 PR #45 合并提交 `193623659de8fe41bfc4f5f0cabc048a167148e4`；对应 [PRD v0.11](../product/JobForge_PRD_v0.11.md)、[ADR-0019](../adr/0019-executor-confirmation-and-exit-contract.md)。操作与部署说明见 [运行时指南](../agent-v3-runtime.md)。本记录在验收中持续更新；尚未完成的检查明确列出，不以计划代替通过。

实现 [PR #46](https://github.com/XJfyrh/JobForge/pull/46) 已于 `2026-09-16T12:07:36Z` 合并为 `0829a4c04a3a123a6ffe6c374e326859b79df1a8`。最终审查 head `512f0fc56ca3d1e87e12d3b99bf3328e396d1ca2` 与合并代码树一致；两份独立上下文 Agent 正式复审无剩余 P1/P2，[八项 CI](https://github.com/XJfyrh/JobForge/actions/runs/35093384805) 全部成功。该结论只完成 C3b，S1 整体仍未完成。

## 交付范围与要求映射

| 要求 | 本轮实现及证据层次 |
| --- | --- |
| C3-01 观察持久确认 | Go Worker 实际调用 TCP gRPC/真实 PG 的 Reserve/Settle/Observe；提交前行锁阻塞及提交后丢 ACK 时，下一业务 HTTP 为 0；免费、unknown 及 reported 的 ACK 另有共同向量和协调器测试 |
| C3-02 顺序与截止 | 固定双向普通/计量 FD、原 Conversation、BOOTTIME 与 RPC 发起锚；迟到排队 ACK 停止，write task 完成通知与子进程下一帧反序不误授执行权 |
| C3-03 固定失败事实 | 真实进程包覆盖正常/64～71/未知/信号退出和坏帧/大小/残留；协调器分别映射退出、大小和首个已知原因；真实 PG 预算拒绝仍为 BUDGET_EXHAUSTED，不被杀进程后的 EOF/71 覆盖 |
| C3-04 终止与计量独立 | 实际固定 guardian/step/父进程死亡、满管道、满事件队列、TERM/KILL/Wait、2s 清理失败回执；已完整原调用报告通过独立标准通道有界结算，不能恢复普通执行 |
| C3-05 Commit 屏障 | 结果只是候选；完整确认链、当前执行权、EOF/Join、真实 Wait、组消失和 stderr 边界之后才 Commit。真实已提交后丢响应只查询原 Commit，不 Fail、不重发当前步骤 |
| C3-06 版本与部署 | 固定 Go/Python 入口和 wheel、immutable manifest、注册/Claim/live session 的 executor_version 校验；旧版本原调用计量仍可补报；生产构建不复制测试 registry/origin/helper |

输入合同使用现有 checkpoint/RPC 字段与领域 canonical/hash，不新增状态、调度语义、SQL migration 或公共 RPC 字段。Go 容量 1，独立 5s 心跳与 BOOTTIME watchdog；Python 不持有控制面 token、DSN、lease 或下一游标权限。

## 已运行的初轮证据

仓库外原始记录统一位于 `E:\JobForge-notes\2026-09-16-agent-v3-s1`，不含真实秘密、文档或模型全文。

- 固定 Linux `process-check` 全套 race 通过：安装后的 guardian/step、FD/PGID/环境边界、父/guardian 死亡及恶意 peer 故障矩阵。记录 `s1c3b-process-linux-{build,race}.txt`，对应 exit 0。
- 固定 Linux Python 首次修正后全套 771 项通过、无 skip；Ruff/格式/mypy 通过。记录 `s1c3b-python-linux-tests2.log` 及对应静态检查日志。后续 fixture 断言改进与最终复验单列，不借旧结果覆盖新源码。
- 第一轮 `integration-check` 实际通过：正常六步、一次纠正七步、Observe 提交前锁等待/提交后丢 ACK、Commit 已持久后丢 ACK、预算拒绝、业务 HTTP 执行中取消及 guardian SIGKILL；同时通过两项真实 PG 版本门禁/旧调用计量测试。记录 `s1c3b-integration-first.txt`，exit 0。
- Worker/input/CLI 的 Windows 定向 race、vet、golangci 检查实际通过；平台受限的 BOOTTIME/进程 skip 需要专门 Linux 层覆盖。
- Python fixture 修订后固定 Linux 全套再次通过：771 passed、0 skip，`s1c3b-python-linux-final.txt` exit 0。加入完整模型内容纠正组合回归后，Windows SDK/业务/探针全套 945 passed、8 skipped，`s1c3b-python-windows-final.txt` exit 0；这些 skip 不算 Linux 进程通过。最新 Linux 全套另列最终检查。
- `TestRunExecutorWorkerSIGKILLRetainsUnknownAndRecoversCheckpoint` 已在实际 Linux/race 镜像、真实 PG/gRPC 下单独通过（5.58s）。独立 Go Worker 在模型 HTTP 中被 SIGKILL；旧 guardian/step 组消失后，测试仅加速隔离库中的 lease/session/retry_at 时间事实，再调用真实 Sweep/Claim。新 attempt 从第 4 个 accepted checkpoint 继续，前四步不重复；旧调用仍 unknown/full hold、新调用单独计费。记录 `s1c3b-worker-recovery2-final.txt`。这是已提交步骤恢复，不是模型推理或半步骤断点续作。

## 初次失败与修复

Python 双 FD 新测试 helper 的临时 FD 被 `dup2` 覆盖，随后在 EOF 处循环。停止该测试容器（exit 143），将临时 FD 复制到 ≥6 并在 EOF 立即失败，再重建执行通过；未将首次退出计为通过。另将三份重复键原始 JSON fixture 规范为恰一个结尾 LF，使测试验证重复键而不是偶然的多行拒绝。

生产镜像首次 Go 编译缺少间接依赖的嵌入业务 migrations；补齐构建阶段 COPY 后实际构建通过。该 COPY 不执行迁移，也不使 Worker 获得数据库凭据。

Worker 恢复定向命令首次遗漏 test binary，第二次未引用 PowerShell 的带点参数，均在测试开始前失败（127/2）；补齐 `/app/integration.test` 并引用 `'-test.v'`、`'-test.timeout=180s'` 后实际运行通过。运行时指南同步修正 Windows 命令，不把调用错误记作产品测试通过。

本轮审视还修正了输入/checkpoint/result 的内层大小错误分类、canonical 结果扩张超限的分类，以及排队普通 ACK 的原调用截止复核。大小拒绝集合未放宽，超时不能伪装成可纠正模型输出。

独立预审确认两项 P2：可信 usage 超界只冻结账本/停止 Worker，未按 ADR 永久失败仍有执行权的 Run；以及 step 已类型化退出后，迟计量 ACK 的 ErrStopped/EPIPE 将真实 69 覆盖成协议错误。两项分别补齐有界原身份 FailAttempt 和窄 ACK 关闭事实处理，并增加纯协调器、真实管道及 PG 联合回归。取消、失权、真实输入/大小错误、未知退出和清理失败仍优先，不能以类型化退出放宽成功屏障。另一项模型 JSON 纠正疑点经完整 dispatcher 复现证伪；仅补组合回归，没有修改生产纠正策略。

## 同环境控制面热路径对比

版本门禁增加了 live session 的 profile/version 校验，因此用原有 `TestRunHotPathPlansAndBaseline` 在相同机器、Go 1.26.5、PostgreSQL 16.14、`GOMAXPROCS=1` 下交替运行基线与候选各五轮。每轮 64 Runs、448 calls、192 tools、384 accepted steps；期间没有其他数据库测试或构建争用。基线为本页首行的 PR #45 合并提交，候选为新增版本校验的 Store。原始记录为 `s1c3b-perf-{base,candidate}-{1..5}.txt`，十轮 exit 均为 0；汇总为 `s1c3b-performance-summary.json`。

| 指标 | 基线 | 候选 |
| --- | ---: | ---: |
| 五轮 Runs/s | 4.416 / 5.094 / 5.251 / 4.879 / 5.087 | 5.077 / 5.211 / 4.985 / 4.997 / 4.963 |
| Runs/s 中位数 | 5.087 | 4.997 |
| Claim p95 中位数（ms） | 34.674 | 35.482 |
| 空 Claim p95 中位数（ms） | 3.161 | 2.750 |
| CommitStep p95 中位数（ms） | 19.560 | 19.516 |
| GetCheckpoint p95 中位数（ms） | 9.993 | 10.709 |
| Observe chat with usage p95 中位数（ms） | 14.200 | 14.987 |
| Reserve chat p95 中位数（ms） | 18.212 | 17.816 |
| Submit p95 中位数（ms） | 30.093 | 30.055 |

吞吐中位数变化为 -1.77%，区间存在重叠；这些小样本不证明性能提高或无退化。这里的 p95 中位数是五次独立 p95 的中位数，不是混合请求 p95。此测试不测新 Worker 的进程启动、HTTP 或云模型耗时，不构成历史 W4 门禁重验。

## 最终检查与剩余边界

| 检查 | 当前实际结果 |
| --- | --- |
| 最终固定 Linux process race | 43 个测试/子测试通过事件，0 fail/skip；含新增真实 EPIPE、Wait 后 ACK 和坏输入不能被 ACK 覆盖 |
| 最终固定 Linux worker race | 107 个测试/子测试通过事件，0 fail/skip；含迟 Settle 两种关闭时序、原 deadline、anomaly 和 100ms 有界收尾 |
| 最终真实 PG/gRPC/进程联合层 | 15 个通过事件，0 fail/skip（含 1 个仅用于子进程启动的 helper，不将其计作独立业务场景）；Worker 强杀恢复 5.75s，可信超界冻结/永久失败 3.68s |
| 最终 Python | Windows 945 passed / 8 skipped；Linux 775 passed / 0 skipped。均不发真实供应商请求 |
| 生产镜像 | 固定 Dockerfile 重建 exit 0；实际检查空 registry、官方 origin、无测试安装器和测试 manifest 通过 |
| 静态与配置 | Go build/vet/lint、Ruff/format、SDK 与 Linux agent/probe mypy、SQLFluff 历史基线/migrations、Buf lint/breaking、promtool 配置/五条规则、dashboard 生成一致性已运行通过；最终提交再核对 diff/链接/秘密 |
| Windows 全仓 race | `go test -race -count=1 -json ./...` exit 0，1231 个测试/子测试通过事件、33 个通过包；20 个具名 skip、另 11 个无测试包，未把 skip 计为通过 |
| PR 最终独立审查与 CI | 两份独立上下文 Agent 正式复审无剩余 P1/P2；上述最终 head 八项 CI 全部成功后 squash 合并 |

最终容器记录为 `s1c3b-final-{process-race,worker-race,integration-race,python,production-isolation}.txt` 及相应 exit 0；四个镜像构建日志分别记录。预审发现的两个 P2 均已修复并经最终提交复核；报告及文件指纹为 `s1c3b-pr46-{core,boundary}-review.md` 与对应 SHA256 清单，公开结果见 [PR 最终记录](https://github.com/XJfyrh/JobForge/pull/46#issuecomment-5697143304)。

Windows 使用按规范启动的 5433 测试 PG、独立 5434 业务 PG、6379 Redis 与已安装 SDK 的 `JOBFORGE_TEST_PYTHON`；记录 `s1c3b-final-windows-race.jsonl` 和机器汇总。20 个具名 skip 为：8 个需要 Linux BOOTTIME 的协调器子用例、7 个需要专用已安装进程环境的联合子用例，以及历史 AT-25、`TestRealTasksLifecycle`、`TestRealTasksSDK` 和两个专用 Worker helper。前 15 个已由上述固定 Linux 镜像实际覆盖；历史真实模型层与 AT-25 不因本轮通过而改变结论。

本轮合成 HTTP 提供真实网络故障与调用计数，**没有运行真实 DeepSeek 推理或真实业务评分**。生产 adapter registry 当前为空；support 策略/六字段方案、provider 身份与 reasoning 的持久审计、跨队列 TraceContext、40 案真实云端评分均仍需交付。初轮进程死亡证明清理，恢复重新领取证据另列，不能宣称本地模型推理断点续作或撤销已发出的外部 HTTP。

历史 W4 性能门禁失败、AT-25 跳过、远程模型和生产长期留存未验收继续保留。新 PG 控制面性能不是 W4 重新验收，机制替身不是云端或业务效果验收；S1 整体及 S2～S5 未完成。
