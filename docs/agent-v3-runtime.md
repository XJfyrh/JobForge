# Agent v3：固定 Linux Worker 与单步执行器

S1-C3b 实现 [PRD v0.11](product/JobForge_PRD_v0.11.md) / [ADR-0019](adr/0019-executor-confirmation-and-exit-contract.md) 的正式进程接缝：Go Worker、严格 checkpoint 投影、固定 Python guardian/step、独立普通与计量管道，以及真实控制 PostgreSQL/gRPC 的机制测试。逐项运行结果见 [C3b 证据](evidence/agent-v3-s1-c3-runtime-2026-09-16.md)，阶段状态见 [实施记录](agent-v3-progress.md)。本文中的复现命令不等于该层检查已通过。

**生产 `runtime_registry.REGISTRY` 仅登记 `support-fixed-v1`。** 它实现 ADR-0018 的 `support_fixed_v1` 固定只读流程与结构化方案，源合同见 [support-proposal-v1](../api/support/v1/README.md)。当前运行时加入 ADR-0020 的[供应商持久报告与批次停止](agent-v3-provider-audit.md)，默认部署仍不启用收费 profile/manifest。机制测试使用单独构建目标中的合成 adapter、业务 HTTP、embedding 和模型响应；它们不证明真实 DeepSeek、检索质量或 40 案通过。S1 整体及 S2～S5 仍未完成，历史 W4 失败、AT-25 跳过、远程模型和生产留存未验收继续保留。

## 执行权与组件

```mermaid
flowchart LR
    Control[agent-control / PostgreSQL] <-->|AgentService RPC| Worker[Go agent-worker]
    Worker --> Input[runinput 严格投影]
    Worker <-->|普通 FD 0/1| Guardian[固定 guardian + step]
    Worker <-->|计量 FD 4/5| Guardian
    Worker -->|FD 3 存活管道| Guardian
    Guardian -->|逐次确认许可后的 HTTP| Services[登记业务 / embedding / 模型]
```

PostgreSQL 仍是唯一事实源；Go 独占 Worker session、Run lease、心跳和控制 RPC。`internal/runworker` 持有原 v2 Conversation，`internal/runexecutor` 只管理固定进程和 I/O，不解释调度、预算或下一游标。Python 只执行当前登记单步，不 Claim、不续租、不重试 Run，也没有任务队列。交付仍为 at-least-once，外部副作用仍须业务幂等。

Worker 容量固定为 1。Register 的版本、Worker 允许的**所有 profile** 的 executor_version、部署 manifest 与 Python runtime 均须为 `linux-v2-audit-runtime-1`；控制面在登记及后续 session 校验中拒绝混合版本。全部执行 profile 还须有固定审计 policy 和 expected model。Worker 核对注册返回的完整 profile 集合。Claim 后先 GetCheckpoint，只有明确成功的 CommitStep 返回允许继续时，才重新读取服务端 checkpoint 开始下一步；本地不计算下一游标或恢复次数。

5s Heartbeat 与普通控制 RPC/管道独立运行。RPC 均有截止，普通控制请求最多 2s；20ms 本地 watchdog 独立检查 Linux `CLOCK_BOOTTIME`。Register/Claim/Heartbeat 用 RPC 发起单调时间和服务端锁后观测时间保守映射 session/lease/attempt/Run 截止，扣除整个往返等待；迟到回调不能恢复已失效的旧执行权。Heartbeat 可更新 lease/session，不能延长固定 attempt/Run 或已经授予的物理调用期限。Windows 原生不支持分支测试不能替代同一 Linux 容器验收。

## 输入、部署与秘密

`execute_step.input` 恰有 `schema_version/executor_version/adapter_id/tool_invocation_id/expected_response_model/provider_audit_policy`。工具 invocation UUID 来自 Go 已确认的 BeginTool；其他步骤为空串。model 和 policy 来自已验证的不可变 profile。没有请求者提供的命令、模块、文件路径、endpoint、query/messages 或秘密。

[运行输入源合同](../api/executor/v2/runtime-input.md)和 [schema](../api/executor/v2/runtime-input.schema.json)沿用 RPC 字段名，将 JSON bytes 投影为对象。Go 使用现有领域 canonicalizer 验证 accepted 结果、CommitHash、NextInputHash 全链、序号、资源和 result_ref；最新资源还须与原 Claim 一致。Python 验证形状与字符串链，不重新实现任意 decimal 规范化。原 JSON 与最终编码大小分别校验，超限不裁剪历史。

Go/Python 只读固定 `/etc/jobforge/executor.json`：顶层恰有 `schema_version:1`、`executor_version` 和 `profiles`；1～32 个 profile 项恰有 `profile_id/profile_hash/adapter_id`，profile_id 不重复。manifest 不含 endpoint 或秘密，也不能把未安装 adapter 变成可执行能力。

生产入口是 [cmd/agent-worker](../cmd/agent-worker/main.go)，不接命令行参数。配置由受信部署文件提供：

| 环境变量 / 文件 | 内容 |
| --- | --- |
| `/etc/jobforge/executor.json` | 固定版本与 profile/hash/adapter 对应关系，只读挂载 |
| `JOBFORGE_AGENT_WORKER_CONFIG` | JSON 文件路径；`schema_version:1`、完整 `profiles`、按 tenant 配置的 `business_origin/ollama_origin` |
| `JOBFORGE_AGENT_WORKER_CREDENTIALS_FILE` | 独立秘密文件路径；`control_token` 及按 tenant 配置的 `business_read_key/deepseek_api_key` |
| `JOBFORGE_AGENT_GATEWAY` | 控制 gRPC 的 `host:port` |
| `JOBFORGE_AGENT_GRPC_TLS` | 默认或 `true` 使用 TLS；`false` 仅供明确配置的本地/私网部署 |

只有 Go 持有 control token。子进程环境从白名单重新构建，不继承宿主环境、DSN、其他 tenant 凭据、代理或 PYTHONPATH。Go 按步骤投影所需配置：读票/提交不需要业务凭据，订单/物流只需业务读配置，政策检索另需固定 Ollama origin，模型步骤只需 DeepSeek key。生产 DeepSeek origin 保持代码常量。秘密不进入 argv、manifest、帧、结果或日志；stderr 正文不保存。

## ACK、退出与 Commit 屏障

每次物理 HTTP 必须先获得新持久许可；重复或不确定 Reserve ACK 不允许发送。正常继续前，免费、非 chat unknown 和最后一次调用也必须等待 ObserveCall 的明确持久确认及匹配 `call_observation_ack`。reported usage 还需同调用/同 report hash 的 settled 确认；chat 的 report/receipt/audit/usage 全部匹配。两条 FD 可以反序到达，原 Conversation 汇合确认；管道 write、flush 或 SettleUsage 都不能替代 ObserveCall。chat unknown、身份/模式不兼容或 report 冲突停止普通执行并持久停批，recorded 不是继续许可。

Python finalizer 复用 dispatcher 的原 Conversation，只写一次标准 step_result 后退出，不等待 Commit ACK。Go 只有在合法结果与完整确认链、当前执行权、实际 Wait、普通 EOF、计量 EOF、reader/writer Join、stderr ≤8KiB、旧组完全消失且无尾随协议错误全部成立时才 Commit。Commit ACK 不确定时，只对原身份/hash有界查询 GetAcceptedCommit；即使找到匹配回执，也不伪造下一游标或重发业务步骤，后续由新 Claim/GetCheckpoint 收敛。

退出码只是固定程序的本地事实：0 仍需上述全部屏障；64～70 分别映射输入、协议、资源身份、大小、依赖、超时和模型协议失败；71 仅表示本地停止。信号、未知码及 guardian 故障不能冒充供应商结果。Go 已知预算等领域拒绝不会被后来的 71/EOF 覆盖；真实 STOP 或失权则优先禁止 Fail/Commit。仅本地放弃时停止 Run 续租，不伪造取消或失败，交 lease/Sweep 回收；明确服务端 STOP 且进程已清理后才允许 AcknowledgeStopped。

计量 ACK 晚于 step 退出时，已关闭 writer 或仅 ACK writer 的 EPIPE 是暂定的输出事实。Go 封闭普通执行并最多等待 100ms 获取自然退出，然后仍执行有界清理；只有真实已定义非零退出、完整 EOF/Join/Wait/组消失且没有输入、大小或其他管道错误时，才保留原 TIMEOUT 等分类。退出 0 不能借此绕过确认屏障。可信计量超出预留会冻结账本并停止 Worker；进程组清理后，仍有执行权的原 Run 有界上报 MODEL_PROTOCOL_ERROR，取消或失权仍禁止该写入。

## 固定进程与清理

生产镜像安装 `jobforge_agent` wheel。Go 只执行 `/usr/local/bin/python -I -u -m jobforge_agent.guardian`，guardian 只启动同解释器的固定 `jobforge_agent.step`，两者在同一进程组。没有动态模块、脚本参数或用户 shell。

| FD | 用途 | 所有权 |
| --- | --- | --- |
| 0 / 1 | 普通输入 / 输出，单帧含 LF ≤384KiB | Go 与 step；guardian 启动后关闭自己的副本 |
| 2 | 有界 stderr | Go 只计数/排空，不保存正文 |
| 3 | 父进程存活 | Go 持唯一 writer，guardian 持 reader；step 不继承 |
| 4 / 5 | 计量 ACK 输入 / report 输出，单帧含 LF ≤8KiB | Go 与 step 独立读写；guardian 关闭副本 |

Stop 不可逆且非阻塞；独立清理路径发送 TERM，100ms 后 KILL 全组。实际 Wait、组消失、有限排空与 Join 共享 2s 本地期限，不能被 RPC、满 event 队列或计量阻塞。Go 死亡由 guardian 的 FD 3 EOF 清理；guardian 死亡由 Go 清理 step。容器必须使用 init，只有核验旧组完全消失后才能启动下一组。缺清理事实或清理超时使 Worker 停止 Claim/续租并退出，不把超时返回称作“已回收”。

普通协议失败后，独立计量通道仍只接受标准解码的完整原调用报告。已经完整收到的报告可在独立总计 ≤2s 内尝试结算；这不延长进程生命或执行权，不从坏 stdout 捞字段。未接收或未确认报告可能仍为 unknown/full hold；没有本地 journal 或自动退款保证。

## 构建与分层复现

在仓库根目录执行；Windows 使用 Docker Desktop 的 Linux 容器模式。生产镜像 [deploy/Dockerfile.agent-worker](../deploy/Dockerfile.agent-worker)固定 Go/Python 基础镜像、安装 wheel、以非 root 用户运行，不复制测试 registry、测试 origin 安装器或 gold：

```powershell
docker build --file deploy/Dockerfile.agent-worker --tag jobforge-agent-worker:c3b .
```

生产运行须只读挂载上述 manifest/配置/秘密文件，并设置 `--init`。默认 [compose.agent.yaml](../deploy/compose.agent.yaml)尚不登记正式 Worker 或收费 profile；support adapter 的存在不能代替 profile/预算/审计前置条件，不要把测试 manifest/adapter 放入生产构建。

[测试 Dockerfile](../tools/agentruntimecheck/Dockerfile)先构建安装包，再提供分开的目标。process/integration 使用已安装包的固定入口；Python全套测试会优先导入源码目录，因此单独标明其验证层次：

| target | 检查层次 | 不能代替的证据 |
| --- | --- | --- |
| `process-check` | race 编译的真实 Linux 进程、FD、Kill/Wait、残留/背压/故障检查 | 控制 PG/gRPC 或业务验收 |
| `python-check` | 安装包构建、Python源码全套确定性测试 | 已安装入口的进程行为、持久提交确认或真实云端 |
| `integration-check` | 真实 PG、TCP gRPC、正式 Worker/guardian/step，合成业务/embedding/供应商 HTTP | 真实业务检索、DeepSeek 推理和 40 案 |

前两层不需要模型凭据或 PostgreSQL，运行时可禁用外网：

```powershell
docker build --file tools/agentruntimecheck/Dockerfile --target process-check --tag jobforge-agent-runtime:process .
docker run --rm --init --network none jobforge-agent-runtime:process
docker build --file tools/agentruntimecheck/Dockerfile --target python-check --tag jobforge-agent-runtime:python .
docker run --rm --init --network none jobforge-agent-runtime:python
```

联合机制测试使用可重建的本地测试 PostgreSQL。先执行 Windows 的标准前置条件，再把容器内 DSN 指向宿主暴露的 5433：

```powershell
docker compose -f deploy/compose.yaml up -d postgres
$env:JOBFORGE_TEST_DSN = 'postgres://jobforge:jobforge@localhost:5433/jobforge?sslmode=disable'
docker build --file tools/agentruntimecheck/Dockerfile --target integration-check --tag jobforge-agent-runtime:integration .
docker run --rm --init --network none jobforge-agent-runtime:integration /app/worker.test '-test.v' '-test.timeout=120s'
docker run --rm --init --add-host control:127.0.0.1 -e 'JOBFORGE_RUNEXECUTOR_INTEGRATION_TESTS=1' -e 'JOBFORGE_TEST_DSN=postgres://jobforge:jobforge@host.docker.internal:5433/jobforge?sslmode=disable' jobforge-agent-runtime:integration
```

`worker.test` 单独验证协调器和真实 Linux 时钟，不访问 PG；`integration-check` 默认执行已用 race 编译的 `TestRunExecutor`、`TestRunSupportExecutor` 和 `TestRunProviderAuditExecutor`。support 场景经正式 adapter 验证完整方案和一次纠正；审计场景在真实 PG/gRPC/FD 中验证提交前数据库锁阻塞、提交后 ACK 丢失、停批和第二次纠正终态。固定回环模型仍是合成响应，不能当真实 DeepSeek 结果。Linux 宿主若没有 `host.docker.internal`，为最后一条命令追加 `--add-host host.docker.internal:host-gateway`（放在镜像名之前），或使用可达的专用测试 PG 地址。PG联合层不能加 `--network none`，否则无法连接 PG。**同一 DSN 同时只运行一个可能清理数据库的测试进程**，不要与宿主全仓集成/race并发运行。

默认还执行 `TestRunSupportLauncher`：安装后的 SDK driver、正式 Worker、launcher 与真实 PG/gRPC 联合验证 driver 在 Submit 后退出、Reserve 已提交但回复未交付时终止监管。`control:127.0.0.1` 仅把测试容器内的固定 SDK 地址指向本例 HTTP listener；真实部署仍使用 Compose 的 control 服务。

仅 integration target 在构建时运行 [test_install.py](../tools/agentruntimecheck/test_install.py)，将固定测试 registry 和固定 loopback 供应商 origin 安装到该测试镜像。它没有运行时 URL/模块开关，也不进入生产 Dockerfile。合成服务的调用计数用于检查“未确认时后续 HTTP 为 0”等执行机制，不能记作实际供应商调用。

常规 `go test -race ./...` 和 `python -m pytest python/tests` 继续验证可移植单元/共同向量；未启用 `JOBFORGE_RUNEXECUTOR_PROCESS_TESTS=1`、`JOBFORGE_RUNEXECUTOR_INTEGRATION_TESTS=1` 或非 Linux 导致的 skip 不计正式进程验收通过。CI 的 `agent-runtime-contract` 专门执行固定镜像进程/联合检查及生产镜像边界检查；Python 全套仍由 `python-lint` 执行。最终命令、结果、失败和未运行层次以 [C3b 证据](evidence/agent-v3-s1-c3-runtime-2026-09-16.md)为准。
