# Go/Python 执行器 S0 探针

本目录是路线 v3 的进程边界可行性试验，**尚未接入生产 Worker，也不是已冻结的生产步骤协议**。只运行固定程序中的合成操作，不调用模型、网络、数据库或业务工具；不能以本探针通过替代真实 Agent 验收。

## 运行

在仓库根目录执行，Docker Desktop 使用 Linux containers；不依赖宿主 Python 或预编译产物：

```powershell
docker build --tag jobforge-executor-probe:s0 --file tools/executorprobe/Dockerfile .
docker run --rm --init --network none --cpus 1 --memory 256m --pids-limit 64 jobforge-executor-probe:s0
```

Dockerfile 从固定 Go 1.26.5/bookworm 镜像构建 Linux `-race` 测试二进制，在固定 Python 3.12.14/slim-bookworm 镜像中运行；两者均锁定 digest。运行用户为非 root。可额外加 `--read-only --cap-drop ALL --security-opt no-new-privileges`，探针不需要写盘或额外权限。

正常请求演示：

```powershell
docker run --rm --init --network none --cpus 1 --memory 256m --pids-limit 64 jobforge-executor-probe:s0 ./executorprobe
```

以上容器均 `--rm`，不创建 volume，不占用端口；运行结束自动删除容器。若取消 Docker 客户端后容器仍在运行，仅按 `docker ps` 确认的本次探针容器 ID 执行 `docker rm -f <id>`。可选执行 `docker image rm jobforge-executor-probe:s0` 删除自己的探针镜像，不清理共享镜像或用户 volume。

## 进程和协议边界

```text
Go owner
  └─ Python guardian（独立 PGID，控制 stdin 只归 Go 所有）
      └─ Python fixed step（每次最多一个，与 guardian 同组）
```

- 一个逻辑执行器是一个受监管进程组，活跃时最多两个 Python 进程。guardian 不执行业务计算，只处理长度有界的管道数据和子进程状态。
- 入口固定为 `python3 -I -u executor.py`；仅支持源代码白名单中的探针操作。请求值始终作为 JSON 数据，不拼 shell、代码、模块路径或动态可执行文件。
- guardian 只继承部署 PATH、固定 locale 和 hash seed，不继承 Go 进程的全部环境变量。未来模型凭据必须通过明确的部署白名单传入，不能默认暴露控制面数据库或运维秘密。
- 请求是一行 JSON，包含 `v/id/op/value`；**总帧含换行最多 4 KiB**。响应为至多一个 `started` 和一个 `result` 帧，单帧 JSON 最多 16 KiB。未知字段、错版本/请求 ID、错误帧顺序、额外响应和超大帧由 Go 拒绝。
- stderr 内容不保留、不转发到日志；只计字节，超过 8 KiB 终止该执行器。测试诊断仅含固定合成文字。
- 探针调用者提供 10 秒总 deadline，包含解释器启动。收到 step 的实际 `READY` 屏障后再启动 1 秒活跃步骤计时；计时超出或外层取消均返回明确错误。开始执行前超时仍会清理进程，不能无限等待启动。
- Go 正常取消/失败对整个进程组发 TERM，最多等待 100ms 后 KILL；始终 Wait 自己的直接子进程。即使 guardian 已退出也清理其仍在运行的 step。
- Go 被 SIGKILL 后无法运行 defer。guardian 通过控制 stdin EOF 主动 KILL 整组；模型步骤阻塞不会占用 guardian 的解释器。guardian 被单独 SIGKILL 时，Go 观察响应管道关闭并清理整组。
- Docker `--init` 负责回收已终止的孤儿；**init 不替代主动 KILL**。测试通过 PID/PGID `ESRCH` 验证整组已经消失，不能把 zombie 当作清理成功。

这只保证受信固定程序及保持同组的后代。任意代码、`setsid` 逃离进程组、Linux 不可中断内核等待、整个宿主失联不在探针保证内；初版业务能力不得创建脱组后代。终止 Python 不能撤销已经发出的远程模型请求或业务效果，生产运行时仍须持久预算、步骤 fencing 和接收方幂等。

## 测试分层

```powershell
go test -race ./tools/executorprobe
go vet ./tools/executorprobe
.\.tools\bin\golangci-lint.exe run ./tools/executorprobe
.\.venv\Scripts\python.exe -m pytest tools/executorprobe/test_executor.py
.\.venv\Scripts\ruff.exe check tools/executorprobe
.\.venv\Scripts\ruff.exe format --check tools/executorprobe
.\.venv\Scripts\mypy.exe --platform linux tools/executorprobe
```

Windows 的 Go 命令只覆盖跨平台协议；POSIX API 类型检查必须指定 Linux。Linux 真进程用例由 `JOBFORGE_EXECUTOR_PROCESS_TESTS=1` 显式启用，Dockerfile 已设置；普通 `go test` 未启用时会明确 skip，**skip 不算进程验收通过**。CI 单独构建并运行上述容器，以实际覆盖 Linux runner 的 race 和孤儿回收。

真进程场景：正常请求、非 JSON 响应、错请求 ID、超大 stdout、超大 stderr、合法结果后的无换行残片、步骤超时、取消、单独杀 guardian、实际杀 Go 父进程并 Wait。取消用例中的 step 忽略 SIGTERM，以验证 KILL 后备路径，而非仅证明合作退出。

## S0 发现与限制

[2026-09-16 实际验收记录](acceptance-2026-09-16.txt) 包含最终源码 SHA256、镜像身份、`-race` 构建与资源限制命令。最终 Linux race 层 10 个真进程场景全部通过、无 skip；Python 协议守卫 10 个测试通过。最后一次实测取消后整组回收约 104ms，步骤 1s 超时后总回收约 1.104s，Go 父进程 SIGKILL 后约 13ms 整组消失；均为该机器该次运行的观测，不是生产 SLO 或统计分位数。

独立审查发现早期 guardian 会吞掉合法结果之后、EOF 之前的无换行残片，使错误输出被判成功。现只转发完整换行前缀，保留残片并在 EOF 拒绝；Python 回归覆盖同一次/不同次管道 read 的分块，真实 Linux `trailing_bytes` 用例验证 Go 最终返回协议错误并回收整组。合法 result 帧本身不足以使执行成功，还须整个协议结束和进程退出有效。

首次试验把 500ms/3s deadline 与解释器启动混在一起，负载下 Python 两进程启动约 2.7～3s，导致正常请求和取消屏障超时。镜像 COPY 后仍复现；单独标准库 import 曾耗时 1.13s，因此不能把原因归结为 Windows bind mount。后续空闲时正常请求约 0.3s，也说明该启动成本随环境变化，不能直接宣传为稳定性能。

修复以实际 started 屏障区分启动与活跃步骤，但两段始终受总 deadline 约束；没有降低步骤自身的 1s 超时要求。正式执行器应分别观测启动和步骤耗时，并在 S0 后根据真实模型/容器环境冻结生产上限。

guard 和 step 每一步重新启动 Python，带来解释器及模型 SDK import 成本。本阶段优先证明进程生命周期；是否在同一 Run 内复用 guardian/step、如何绑定 profile、如何传递 TraceContext 和租约下的预算/结果提交，仍需正式协议与后续实现验收。本探针不实现持久 checkpoint、租约、审批或业务写入。
