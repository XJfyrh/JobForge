# Docker 恢复后 S0 复验与合并

- 日期：2026-09-16，Asia/Shanghai。
- 被测源码：`ace6b74520de4bc7baff08a8cb2a91f3ae9b842a`。
- [PR #35](https://github.com/XJfyrh/JobForge/pull/35)于 `2026-09-16T04:19:10Z` squash合并，提交 `1bf92c2f936c9011f8fc571b880ff2373305e6fc`；两者代码树相同。
- 本轮只重试被环境阻塞的检查并记录结果，未修改运行代码、migration、模型配置或历史负证据。

## 恢复与真实依赖

维护者重启Docker Desktop后，`docker version` 和 `docker info` 正常返回，Linux engine为29.6.2。本轮未进行工厂重置、全局配置变更或删除数据。启动仓库规定的可重建开发PostgreSQL及Redis，测试只使用本地开发数据；没有启动API、Worker等可能竞争测试数据的应用容器。

```powershell
docker compose -f deploy/compose.yaml up -d postgres
docker compose -f deploy/compose.yaml --profile durable-events up -d redis
.venv/Scripts/python.exe -m pip install --no-deps ./sdk/python
$env:JOBFORGE_TEST_DSN = 'postgres://jobforge:jobforge@localhost:5433/jobforge?sslmode=disable'
$env:JOBFORGE_TEST_REDIS_URL = 'redis://localhost:6379/0'
$env:JOBFORGE_TEST_REDIS_CONTAINER = 'deploy-redis-1'
$env:JOBFORGE_TEST_PYTHON = 'E:\JobForge\.venv\Scripts\python.exe'
go test -race -count=1 -json ./...
```

Windows命令退出0，共355个测试通过事件（含子测试），0失败事件；集成包耗时139.782秒。真实SDK HTTP契约、Redis停止/恢复、核心PostgreSQL并发/租约/幂等与Worker故障用例实际执行。NFR-303采用默认60秒broker停止窗口，没有缩短故障期。

[机器可读摘要](agent-v3-windows-race-2026-09-16.json)包含所有包结果、跳过原因和原始日志hash。完整JSONL留在仓库外 `E:\JobForge-notes\2026-09-16-agent-v3-s0\docker-recovery`，不把运行日志混入产品源文件。

五个skip明确保留：AT-25 ControlStream是历史未实现P1；`TestRealTasksLifecycle`、`TestRealTasksSDK`未启用独立真实模型层；两个subprocess helper只由父测试按需调用，不作为独立业务验收。没有将上述skip计入通过。

## 固定容器进程/race

由单独Agent在相同源码上重新构建固定Go1.26.5/Python3.12.14镜像，容器启用init、无网络、1CPU、256MiB及64PID上限。构建输入前后SHA256一致；镜像、完整命令和原始输出见[新增证据](../../tools/executorprobe/docker-recovery-2026-09-16.txt)。

**11个真实进程场景＋1个纯协议测试全部通过，0 skip、0 fail、0 race warning。** 真实场景包含正常执行、非法JSON、错请求ID、超大stdout/stderr、EOF残片、新增EOF后stderr超限、超时、取消、guardian死亡及真实SIGKILL Go父进程后整组回收。

这证明固定合成执行器的进程边界，不代表生产Worker、持久步骤、模型或业务工具已实现。

## 独立审查与CI

新审查Agent使用 `fork_turns=none`，自行读取仓库规范、PRD/ADR、整个PR diff及CI日志；未接触凭据、未修改源码。审查覆盖步骤/lease、停止顺序、审批/回执契约，以及探针协议、安全、资源生命周期和证据范围。另独立运行42项Python探针guardrails、Go协议测试、diff检查，均通过。结论：本地复验通过后无S0合并阻断，结论不扩展到S1～S5。

[ace6b745 CI run 35054474424](https://github.com/XJfyrh/JobForge/actions/runs/35054474424)的Go构建/vet/lint、全仓race与真实PG/Redis/SDK、Python/SQLFluff、固定Linux进程race、Buf、Prometheus/Grafana六项全部成功。Python SDK与两个探针确定性测试合计93项；未用CI成功替代云端模型验收。

根Agent确认代码HEAD、全部检查和独立审查后，使用 `gh pr merge 35 --squash --match-head-commit ace6b74520de4bc7baff08a8cb2a91f3ae9b842a` 合并。ADR接受状态按仓库流程在合并后的文档PR补记，不提前宣布未完成的功能通过。

## 当前边界

S0契约/探针已交付，S1环境阻塞解除，但S1～S5仍未实现。DeepSeek只读鉴权已通过，推理请求仍为0；本轮不调用模型。下一阶段包括真实业务HTTP工具、pgvector/embedding、固定流程、DeepSeek适配及最小持久调用账本。

历史远程模型与生产长期留存未验收、W4性能门禁失败、AT-25跳过均保持原结论。此次没有修改核心热路径，不产生新的性能门禁结果。
