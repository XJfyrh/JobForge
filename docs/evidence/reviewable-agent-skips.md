# 普通 Go suite 的 77 项 skip 复核

来源：本分支第一阶段隔离全量 race 日志 `.cache/verification/linux-aCf6FKNd/go-test.jsonl`。这里计数包含父测试和子测试事件，不等于 77 个独立功能。逐项匹配专用日志中的同名 `PASS`，不是根据套件总退出码推断。

73 项在专用容器日志中找到同名通过；2 项旧 Jobs 的真实 Ollama 验收未运行；2 项为仅由父测试 re-exec 的 helper，直接枚举时有意 skip。没有删除这些测试。

| 测试 | 普通层跳过原因 | 单独证据 / 未覆盖 |
|---|---|---|
| `internal/runexecutor/TestMeteringACKAfterActualWaitReportsStopped` | 需要固定 Linux 镜像及专用开关 | `runtime-process-final.log` 同名 PASS |
| `internal/runexecutor/TestMeteringACKClosedReaderKeepsTypedExitAndInputFailures/metering_closed_timeout` | 需要固定 Linux 镜像及专用开关 | `runtime-process-final.log` 同名 PASS |
| `internal/runexecutor/TestMeteringACKClosedReaderKeepsTypedExitAndInputFailures/metering_closed_bad_frame` | 需要固定 Linux 镜像及专用开关 | `runtime-process-final.log` 同名 PASS |
| `internal/runexecutor/TestPhysicalExitFactsAndProtocolFailures/success` | 需要固定 Linux 镜像及专用开关 | `runtime-process-final.log` 同名 PASS |
| `internal/runexecutor/TestPhysicalExitFactsAndProtocolFailures/trailing` | 需要固定 Linux 镜像及专用开关 | `runtime-process-final.log` 同名 PASS |
| `internal/runexecutor/TestPhysicalExitFactsAndProtocolFailures/wrong_ordinary` | 需要固定 Linux 镜像及专用开关 | `runtime-process-final.log` 同名 PASS |
| `internal/runexecutor/TestPhysicalExitFactsAndProtocolFailures/wrong_metering` | 需要固定 Linux 镜像及专用开关 | `runtime-process-final.log` 同名 PASS |
| `internal/runexecutor/TestPhysicalExitFactsAndProtocolFailures/oversize` | 需要固定 Linux 镜像及专用开关 | `runtime-process-final.log` 同名 PASS |
| `internal/runexecutor/TestPhysicalExitFactsAndProtocolFailures/metering_oversize` | 需要固定 Linux 镜像及专用开关 | `runtime-process-final.log` 同名 PASS |
| `internal/runexecutor/TestPhysicalExitFactsAndProtocolFailures/fragment` | 需要固定 Linux 镜像及专用开关 | `runtime-process-final.log` 同名 PASS |
| `internal/runexecutor/TestPhysicalExitFactsAndProtocolFailures/metering_fragment` | 需要固定 Linux 镜像及专用开关 | `runtime-process-final.log` 同名 PASS |
| `internal/runexecutor/TestOrdinaryEOFPreservesActualExitClassification/0` | 需要固定 Linux 镜像及专用开关 | `runtime-process-final.log` 同名 PASS |
| `internal/runexecutor/TestOrdinaryEOFPreservesActualExitClassification/64` | 需要固定 Linux 镜像及专用开关 | `runtime-process-final.log` 同名 PASS |
| `internal/runexecutor/TestOrdinaryEOFPreservesActualExitClassification/65` | 需要固定 Linux 镜像及专用开关 | `runtime-process-final.log` 同名 PASS |
| `internal/runexecutor/TestOrdinaryEOFPreservesActualExitClassification/66` | 需要固定 Linux 镜像及专用开关 | `runtime-process-final.log` 同名 PASS |
| `internal/runexecutor/TestOrdinaryEOFPreservesActualExitClassification/67` | 需要固定 Linux 镜像及专用开关 | `runtime-process-final.log` 同名 PASS |
| `internal/runexecutor/TestOrdinaryEOFPreservesActualExitClassification/68` | 需要固定 Linux 镜像及专用开关 | `runtime-process-final.log` 同名 PASS |
| `internal/runexecutor/TestOrdinaryEOFPreservesActualExitClassification/69` | 需要固定 Linux 镜像及专用开关 | `runtime-process-final.log` 同名 PASS |
| `internal/runexecutor/TestOrdinaryEOFPreservesActualExitClassification/70` | 需要固定 Linux 镜像及专用开关 | `runtime-process-final.log` 同名 PASS |
| `internal/runexecutor/TestOrdinaryEOFPreservesActualExitClassification/71` | 需要固定 Linux 镜像及专用开关 | `runtime-process-final.log` 同名 PASS |
| `internal/runexecutor/TestOrdinaryEOFPreservesActualExitClassification/72` | 需要固定 Linux 镜像及专用开关 | `runtime-process-final.log` 同名 PASS |
| `internal/runexecutor/TestOrdinaryEOFPreservesActualExitClassification/99` | 需要固定 Linux 镜像及专用开关 | `runtime-process-final.log` 同名 PASS |
| `internal/runexecutor/TestOrdinaryEOFPreservesActualExitClassification` | 需要固定 Linux 镜像及专用开关 | `runtime-process-final.log` 同名 PASS |
| `internal/runexecutor/TestBadOrdinaryFrameStillDrainsIndependentMetering` | 需要固定 Linux 镜像及专用开关 | `runtime-process-final.log` 同名 PASS |
| `internal/runexecutor/TestStderrOverflowAfterOrdinaryEOF` | 需要固定 Linux 镜像及专用开关 | `runtime-process-final.log` 同名 PASS |
| `internal/runexecutor/TestFullEventsDoNotDelayKillOrInventEOF/full_events` | 需要固定 Linux 镜像及专用开关 | `runtime-process-final.log` 同名 PASS |
| `internal/runexecutor/TestFullEventsDoNotDelayKillOrInventEOF/full_metering` | 需要固定 Linux 镜像及专用开关 | `runtime-process-final.log` 同名 PASS |
| `internal/runexecutor/TestBlockedMeteringWriteDoesNotDelayStop` | 需要固定 Linux 镜像及专用开关 | `runtime-process-final.log` 同名 PASS |
| `internal/runexecutor/TestContextCancellationDuringStopPublicationCleansProcess` | 需要固定 Linux 镜像及专用开关 | `runtime-process-final.log` 同名 PASS |
| `internal/runexecutor/TestRepeatedProcessesJoinReadersAndReleaseFDs` | 需要固定 Linux 镜像及专用开关 | `runtime-process-final.log` 同名 PASS |
| `internal/runexecutor/TestBlockedWriteCancelAndConcurrentCallAreBounded` | 需要固定 Linux 镜像及专用开关 | `runtime-process-final.log` 同名 PASS |
| `internal/runexecutor/TestSurvivingGroupIsNotACleanExit` | 需要固定 Linux 镜像及专用开关 | `runtime-process-final.log` 同名 PASS |
| `internal/runexecutor/TestFixedGuardianFDIsolationAndGroup` | 需要固定 Linux 镜像及专用开关 | `runtime-process-final.log` 同名 PASS |
| `internal/runexecutor/TestFixedGuardianDeathAndCancellation/guardian_death` | 需要固定 Linux 镜像及专用开关 | `runtime-process-final.log` 同名 PASS |
| `internal/runexecutor/TestFixedGuardianDeathAndCancellation/context_cancel` | 需要固定 Linux 镜像及专用开关 | `runtime-process-final.log` 同名 PASS |
| `internal/runexecutor/TestFixedParentDeathUsesLivenessEOF` | 需要固定 Linux 镜像及专用开关 | `runtime-process-final.log` 同名 PASS |
| `tools/executorprobe/TestRealProcessContracts/echo` | 需要固定 Linux 镜像及专用开关 | `probe.log` 同名 PASS |
| `tools/executorprobe/TestRealProcessContracts/bad_json` | 需要固定 Linux 镜像及专用开关 | `probe.log` 同名 PASS |
| `tools/executorprobe/TestRealProcessContracts/wrong_id` | 需要固定 Linux 镜像及专用开关 | `probe.log` 同名 PASS |
| `tools/executorprobe/TestRealProcessContracts/oversize` | 需要固定 Linux 镜像及专用开关 | `probe.log` 同名 PASS |
| `tools/executorprobe/TestRealProcessContracts/stderr` | 需要固定 Linux 镜像及专用开关 | `probe.log` 同名 PASS |
| `tools/executorprobe/TestRealProcessContracts/trailing_bytes` | 需要固定 Linux 镜像及专用开关 | `probe.log` 同名 PASS |
| `tools/executorprobe/TestStderrOverflowAfterProtocolEOF` | 需要固定 Linux 镜像及专用开关 | `probe.log` 同名 PASS |
| `tools/executorprobe/TestCancelTimeoutAndGuardianDeath/cancel` | 需要固定 Linux 镜像及专用开关 | `probe.log` 同名 PASS |
| `tools/executorprobe/TestCancelTimeoutAndGuardianDeath/timeout` | 需要固定 Linux 镜像及专用开关 | `probe.log` 同名 PASS |
| `tools/executorprobe/TestCancelTimeoutAndGuardianDeath/guardian_kill` | 需要固定 Linux 镜像及专用开关 | `probe.log` 同名 PASS |
| `tools/executorprobe/TestParentKillControlEOF` | 需要固定 Linux 镜像及专用开关 | `probe.log` 同名 PASS |
| `tests/integration/TestRealTasksLifecycle` | 未设置固定 `JOBFORGE_REAL_MODEL_URL` | 未运行，不算通过；属于兼容 Jobs 真实模型质量层 |
| `tests/integration/TestBusinessWorkerProcessHelper` | 旧真实模型 suite 的子进程入口 | 本次未触发；随真实模型层保留 |
| `tests/integration/TestRealTasksSDK` | 未设置固定 `JOBFORGE_REAL_MODEL_URL` | 未运行，不算通过；属于兼容 Jobs 真实模型质量层 |
| `tests/integration/TestRunExecutorWorkerSIGKILLRetainsUnknownAndBlocksChatRestart` | 需要固定 Linux 镜像及专用开关 | `runtime-integration.log` 同名 PASS |
| `tests/integration/TestRunExecutorGuardianKilledDuringBusinessHTTP` | 需要固定 Linux 镜像及专用开关 | `runtime-integration.log` 同名 PASS |
| `tests/integration/TestRunExecutorUsageAnomalyFailsLiveRunAndStopsWorker` | 需要固定 Linux 镜像及专用开关 | `runtime-integration.log` 同名 PASS |
| `tests/integration/TestRunExecutorCompleteRegisteredFlow/normal-six-steps` | 需要固定 Linux 镜像及专用开关 | `runtime-integration.log` 同名 PASS |
| `tests/integration/TestRunExecutorCompleteRegisteredFlow/one-correction-seven-steps` | 需要固定 Linux 镜像及专用开关 | `runtime-integration.log` 同名 PASS |
| `tests/integration/TestRunExecutorObserveUnconfirmedHasNoFollowingHTTP/before-commit-lock` | 需要固定 Linux 镜像及专用开关 | `runtime-integration.log` 同名 PASS |
| `tests/integration/TestRunExecutorObserveUnconfirmedHasNoFollowingHTTP/after-commit-lost-ack` | 需要固定 Linux 镜像及专用开关 | `runtime-integration.log` 同名 PASS |
| `tests/integration/TestRunExecutorCommitACKLossDoesNotFailOrRepeatStep` | 需要固定 Linux 镜像及专用开关 | `runtime-integration.log` 同名 PASS |
| `tests/integration/TestRunExecutorBudgetRejectionRetainsOriginalFailure` | 需要固定 Linux 镜像及专用开关 | `runtime-integration.log` 同名 PASS |
| `tests/integration/TestRunExecutorCancelDuringRealHTTPStopsProcess` | 需要固定 Linux 镜像及专用开关 | `runtime-integration.log` 同名 PASS |
| `tests/integration/TestRunProviderAuditExecutorProviderStop/incompatible` | 需要固定 Linux 镜像及专用开关 | `runtime-integration.log` 同名 PASS |
| `tests/integration/TestRunProviderAuditExecutorProviderStop/usage-absent` | 需要固定 Linux 镜像及专用开关 | `runtime-integration.log` 同名 PASS |
| `tests/integration/TestRunProviderAuditExecutorProviderStop/reasoning` | 需要固定 Linux 镜像及专用开关 | `runtime-integration.log` 同名 PASS |
| `tests/integration/TestRunProviderAuditExecutorConfirmationWindows/reserve-before` | 需要固定 Linux 镜像及专用开关 | `runtime-integration.log` 同名 PASS |
| `tests/integration/TestRunProviderAuditExecutorConfirmationWindows/reserve-after` | 需要固定 Linux 镜像及专用开关 | `runtime-integration.log` 同名 PASS |
| `tests/integration/TestRunProviderAuditExecutorConfirmationWindows/report-before` | 需要固定 Linux 镜像及专用开关 | `runtime-integration.log` 同名 PASS |
| `tests/integration/TestRunProviderAuditExecutorConfirmationWindows/report-after` | 需要固定 Linux 镜像及专用开关 | `runtime-integration.log` 同名 PASS |
| `tests/integration/TestRunProviderAuditExecutorConfirmationWindows/observe-before` | 需要固定 Linux 镜像及专用开关 | `runtime-integration.log` 同名 PASS |
| `tests/integration/TestRunProviderAuditExecutorConfirmationWindows/observe-after` | 需要固定 Linux 镜像及专用开关 | `runtime-integration.log` 同名 PASS |
| `tests/integration/TestRunProviderAuditExecutorConfirmationWindows/commit-before` | 需要固定 Linux 镜像及专用开关 | `runtime-integration.log` 同名 PASS |
| `tests/integration/TestRunProviderAuditExecutorConfirmationWindows/commit-after` | 需要固定 Linux 镜像及专用开关 | `runtime-integration.log` 同名 PASS |
| `tests/integration/TestRunProviderAuditExecutorFinalCorrectionFailureAllowsNextCase` | 需要固定 Linux 镜像及专用开关 | `runtime-integration.log` 同名 PASS |
| `tests/integration/TestRunSupportAgentExecutor` | 需要固定 Linux 镜像及专用开关 | `runtime-integration.log` 同名 PASS |
| `tests/integration/TestRunSupportExecutor/with_order_proposal` | 需要固定 Linux 镜像及专用开关 | `runtime-integration.log` 同名 PASS |
| `tests/integration/TestRunSupportExecutor/missing_order_one_correction` | 需要固定 Linux 镜像及专用开关 | `runtime-integration.log` 同名 PASS |
| `tests/integration/TestRunSupportLauncher` | 需要固定 Linux 镜像及专用开关 | `runtime-integration.log` 同名 PASS |
| `tests/integration/TestWorkerProcessHelper` | 子进程入口，禁止独立启动 | 父测试通过 `startTestWorkerProcess` 实际 re-exec，见下文 |

## 子进程入口与边界

`tests/integration/worker_process_test.go` 中 helper 只有在 `JOBFORGE_TEST_WORKER_HELPER=1` 时运行，由父进程显式传入隔离 DSN 与测试端口。普通枚举不启动一个没有父监管的 Worker。`TestFaultAT02CrashBeforeACK` 在上述 JSON 日志中 PASS，实际启动两个 Worker，杀死未 ACK 的进程并验证重派与完成；helper 自身的顶层 skip 不代表那些父测试跳过。

`TestBusinessWorkerProcessHelper` 由 `real_tasks_test.go` 的真实模型生命周期测试调用；没有固定 Ollama 后端时，父测试和入口本次均未运行。合成模型的 S2 demo 不能替代它。

正式进程日志覆盖失权、父/guardian 死亡、FD/组清理；联合机制日志覆盖真实 PG/gRPC 的 Reserve/report/Observe/Commit 确认窗口、预算、取消、固定与动态方案及 launcher 停止。重跑命令集中于[验证指南](../verification.md)；模型质量、scale、保留集是独立的未运行层，不隐藏在这份统计中。
