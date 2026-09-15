# Windows 验收：时钟与 Worker 启动诊断

适用 Windows 原生 Go/Python + Docker Desktop WSL2 PostgreSQL。测试数据库必须可重建；禁止对演示或生产 DSN 执行 integration，禁止两个测试进程同时重建同一个数据库。

## 一次复现

按[开发指南](../development.md)安装 Go、Docker、`.venv` 和锁定 Python 工具。快速层：

```powershell
pwsh -NoProfile -File tools/test-windows.ps1
```

真实模型层先按[开发指南](../development.md)启动固定 CPU Ollama、Collector、Jaeger 并准备两个模型，再执行（11435 对应 Compose CPU 测试模型端口）：

```powershell
$env:JOBFORGE_REAL_MODEL_URL = 'http://localhost:11435'
$env:JOBFORGE_TEST_OTLP_ENDPOINT = 'http://localhost:4318'
$env:JOBFORGE_TEST_JAEGER_URL = 'http://localhost:16686'
pwsh -NoProfile -File tools/test-windows.ps1 -RealModels -EvidenceDirectory E:\JobForge-notes\windows-acceptance
```

脚本启动规定的 PostgreSQL/Redis、安装普通 SDK wheel、运行 Python 单测，先检查 60s 时钟/SQL 环境，再运行快速层全量 race；`-RealModels` 随后单独运行两个真实模型集成测试。两层顺序执行，不共享并行重建。每一步非零退出均停止，不能让输出重定向吞掉 Go 失败。输出目录保留 JSON 时钟样本及两层日志；脚本不修改系统时钟或 WSL 配置。它不替代 CONTRIBUTING 中其余静态检查。快速层的真实模型 skip 与 AT-25 的未实现 skip 必须分别披露。

## DB-clock 仍要求稳定的时钟

ADR-0008 用同一 PostgreSQL 的 `cancel_requested_at` 与 `clock_timestamp()` 避免跨主机偏差；两次采样之间的数据库时钟跳变依然会污染耗时，也会影响 ready/lease 的时间谓词。不得换成客户端耗时来掩盖失败，更不能放宽 6s signal 或 AT-22 p95 1s / max 2s 的门槛。

只读探针可以独立运行，不重建 schema：

```powershell
$env:JOBFORGE_TEST_DSN = 'postgres://jobforge:jobforge@localhost:5433/jobforge?sslmode=disable'
go run ./tools/clockcheck -duration 60s > clock.json
if ($LASTEXITCODE -ne 0) { throw 'clock/SQL environment failed' }
```

探针对照 Windows 单调时钟与 PostgreSQL 墙上时钟；默认偏差/步进上限各 100ms、SQL 往返上限 250ms，只是验收环境诊断条件，不是新的产品 SLO。偏差及步进计算扣除 SQL 往返不确定区间，慢查询单独报告。无 DSN、连接/查询失败、样本不足、越界均非零退出，不输出连接串。可用 `-duration 10m` 在长测试期间另行只读采样；短预检通过不保证之后永不跳时。

## 本机 2026-09-15 根因与修复

Windows 11 build 26200.9445、WSL 2.6.1/kernel 6.6.87.2、Docker Desktop 4.83.0 下，Ubuntu-24.04 的 `systemd-timesyncd` 为 active/enabled，同时 `hv_utils.timesync_implicit=Y`。Windows Time 服务为 stopped/manual。NTP 在 11:34:54Z 报告 offset −2.372883s，PG 只读探针同刻后退 2.372892s（SQL RTT 0.503ms），约 15s 后又前进 2.370716s。Linux RAW 单调时钟约 40s 只相差 Windows 25µs，没有证据证明 TSC 故障。

这与 Ubuntu NTP 与 Hyper-V 宿主同步相互调整一致。[Canonical 官方说明](https://ubuntu.com/wsl/docs/stable/explanation/time-sync/)明确指出 Ubuntu 24.04 及更早版本默认启用 timesyncd；采用宿主同步时应禁用它以避免冲突。旧 Hyper-V issue 的特定旧版本前提不符合本机，未据此更换内核或 clocksource。

先只读核对正在运行的发行版、同步方式及原配置：

```powershell
wsl --list --verbose
wsl -d Ubuntu-24.04 -- timedatectl timesync-status
wsl -d Ubuntu-24.04 -- systemctl show systemd-timesyncd -p ActiveState -p UnitFileState
wsl -d Ubuntu-24.04 -- cat /sys/module/hv_utils/parameters/timesync_implicit
w32tm /query /status
```

**仅在确认重复校时，且选定由 Windows/WSL 提供时间时**，对实际发行版执行以下可逆修复。本次已执行，没有重启 Docker/WSL 或停止其他项目容器：

```powershell
wsl -d Ubuntu-24.04 -u root -- systemctl disable --now systemd-timesyncd.service
```

原状态的撤销命令是 `systemctl enable --now systemd-timesyncd.service`；不要在宿主同步仍启用时盲目恢复冲突配置。多个 WSL 发行版共享内核时间，需核对其他校时服务；不要一概禁用生产 Linux 的 NTP。Windows 的绝对 UTC 准确度仍应由主机时间服务保障，探针只验证主机与数据库的相对稳定性。本次没有改动 Windows 时间服务或强制校时。

## 启动失败与取消超时分开判定

AT-24 保留 Worker 注册、Poll 错误、慢 Claim 耗时/领取数及提前退出日志。15s 内未启动 Handler 是 readiness 失败，不能当作已经测得取消信号超时。定位时分别检查注册、Poll deadline、已提交的 running job 与尚未领取的 ready job。Claim 超过 RPC deadline 会回滚；若提交成功但响应丢失，则等待 lease recovery，不能重用或覆盖已提交 token。

本次稳定时钟下的复现拿到了直接证据：250ms 的测试专用 Poll 超时反复打断 20 个任务的批量 Claim（失败调用 249～376ms）；一次 Claim 在 243ms 提交 20 个任务，但 RPC 仍在边界超时，Worker 未收到任务，30s lease 尚未到期时 15s readiness 已失败。这符合原有 at-least-once 语义，测试的启动预算不应模拟该故障。

修复仅让 AT-24 使用 Runtime 已有默认 30s Poll RPC 预算，15s readiness、5s 心跳、6s signal 断言全部不变。新增 400ms 有界慢领取回归，旧配置实际以 0/20 启动失败；新配置必须启动并继续通过完整取消、指标与 Trace 断言。没有缩短 lease、绕过 fencing 或把状态改成直接成功。AT-22 另记录 Claim/Complete 耗时，便于把排队积压与数据库往返成本区分。

时钟稳定后仍需按原门槛跑 AT-22/AT-24 以及完整 race；定向通过不覆盖先前整组失败。历史 W4 性能失败、AT-25 P1 跳过、远程模型与生产留存未验收继续见[合并审查记录](../agent-rag-review.md)。
