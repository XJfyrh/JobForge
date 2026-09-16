# Agent v3 S1-C1：协议、时间与计量验证

日期：2026-09-16。基线`01177e9726b601e7210df0c9a758f4cfdb356916`；[PRD v0.10](../product/JobForge_PRD_v0.10.md)/[ADR-0018](../adr/0018-deepseek-fixed-flow-and-executor.md)已随PR #41接受。当前切片仅实现C-02/C-03/C-08的协议/RPC前置部分，不把它们标成完整C验收。

## 已运行与未交付

| 范围 | 实际证据 | 边界 |
|---|---|---|
| v2 codec/顺序与窄计量 | Go/Python共用11合法帧、19非法帧、49会话序列；独立v2 Python测试131通过，Go v1/v2 race通过 | fixture是确定性协议输入，不代表实际FD监管或云端发送 |
| RPC权威时间 | 真实TCP/gRPC/PG锁屏障覆盖Register首创/重报、idle/active/stop Heartbeat、带lease及空Claim | 时间来自锁后DB，不能用transport墙钟代替；空Claim不续session |
| 异常/晚到计量 | 真实PG/RPC测试验证终态/过期session后1025输出token报告、三层冻结/full hold、重复/冲突，Run不回退 | 没有向供应商发送请求；不代表云端usage已观察 |
| 共享时钟 | 固定Linux镜像中Go/Python实际CLOCK_BOOTTIME相互核对；3个顶层测试、12子测试通过，0跳过 | Windows非Linux分支明确不支持live clock；真实运行要求同容器/time namespace |
| S0生命周期回归 | 同一镜像重新执行5顶层、9子测试事件，0失败/跳过；含Go SIGKILL、guardian死亡、EOF/尾随输出、取消/超时与回收 | 这是已有探针，正式Worker/guardian尚未交付 |
| 机械门禁 | Go build/vet/golangci-lint通过（0 issues）；全仓race 29包、896个测试及子测试通过，0失败；全部Python合计427 passed；Ruff check/format、mypy 22文件及Linux探针2文件通过；SQLFluff历史3项基线/迁移lint通过；Buf lint/breaking/重新生成hash一致 | PR CI待执行；全仓race的5个测试跳过见下，不能把缺依赖skip当通过 |
| 云端/业务验收 | 未运行DeepSeek推理，未执行40案固定业务流程 | 正式Worker、HTTP许可钩子、profile、方案/数据/评分由下一切片交付 |

计量反例覆盖单字段合法、input+output合计超过safe整数范围：报告仍可归档并立即关闭普通执行，留给账本保存异常，不按1024或预留截断。普通observation错usage hash也不能让另一个通道的合法原调用报告丢失。任何settled确认均不重开已停止/过期会话。

第一轮两项新PG/RPC race为PASS，3.976s；最终全仓race覆盖追加的过期STOP断言。源码测试分别为`tests/integration/run_authority_rpc_test.go`、`internal/runclock`、`internal/runprotocol/v2`、`python/tests/test_protocol_v2.py`。没有改写migration，Proto只兼容新增并通过Buf生成。

全仓race使用已启动的控制PostgreSQL、Redis、业务PostgreSQL和已安装SDK的Python。5个测试级skip为`TestCancelAT25ControlStreamDegradation`、`TestRealTasksLifecycle`、`TestRealTasksSDK`以及两个仅供子进程入口使用的`TestBusinessWorkerProcessHelper`/`TestWorkerProcessHelper`。此外11个包没有测试文件，单独记录，不计为通过。真实任务层未在本切片重跑，AT-25继续跳过；实际运行的SDK跨语言契约不属于这些skip。

## 同环境性能比较

沿用未修改的`TestRunHotPathPlansAndBaseline`，Windows 11/AMD Ryzen 7 7840HS、Go 1.26.5、GOMAXPROCS=16、Docker 29.6.2、PG16.14、pool=16。同一主机先base后candidate各128 Run，均无race；独占测试DSN并暂时停止已有control扫描器，结束后已恢复。未同时运行容器构建、模型或其它PG套件。

| 指标 | base | candidate |
|---|---:|---:|
| 完成Run / call / tool / step | 128 / 896 / 384 / 768 | 128 / 896 / 384 / 768 |
| 结束执行槽used | 0 | 0 |
| 执行阶段 | 23.9515273s | 23.8327486s |
| Run/s | 5.344 | 5.371 |
| Claim p95 | 29.8727ms | 29.4536ms |
| CommitStep p95 | 17.4644ms | 17.0056ms |
| chat Reserve p95 | 15.8403ms | 15.2785ms |
| chat Observe p95 | 13.0492ms | 12.6812ms |

两轮均通过既有计数断言。仅各一轮、固定顺序、未清缓存且存在共享主机背景噪声，约0.5%差异不作性能提升或无回归门禁结论。测试通过生产Store直接调用、合成capture/工具/模型结果；不测gRPC编码、v2 IPC、模型/业务HTTP或真实执行器时延。

自然EXPLAIN的稀疏ready计划也保留差异：base为`runs_ready_claim_idx` Index Scan（0.231ms、shared hit=29），candidate为`runs_deadline_idx` Bitmap Index/Heap Scan（0.382ms、shared hit=124）。密集分布均Seq Scan；SQL/索引未修改，不能把单轮小表/并发更新/统计与缓存差异归因为代码改进或退化。旧[W4失败](../worker-capacity-performance.md)和AT-25跳过仍保留；本报告不替代历史门禁。

原始Windows JSONL、两轮性能输出/版本hash及外部决策记录保存于仓库外`E:\JobForge-notes\2026-09-16-agent-v3-s1`。性能测试源SHA256为`7cf875c7fc7949943abeb2631d7b42d11f1a64d76338320925b3c3fe1bd6533a`，两侧相同且测前后candidate受测输入未变。

## 复现

```powershell
docker compose -f deploy/compose.yaml up -d postgres
$env:JOBFORGE_TEST_DSN='postgres://jobforge:jobforge@localhost:5433/jobforge?sslmode=disable'
$env:JOBFORGE_TEST_PYTHON=(Resolve-Path .venv/Scripts/python.exe).Path
go test -race -count=1 -run 'TestRunWorkerRPCAuthorityObservationsUseLockedDatabaseClock|TestRunWorkerRPCReportsLateMeasurementAnomalyWithoutRefund' ./tests/integration
go test -race -count=1 ./internal/runprotocol/... ./internal/runclock
.venv/Scripts/python.exe -m pytest python/tests/test_protocol_v2.py
docker build --tag jobforge-executor-probe:c1 --file tools/executorprobe/Dockerfile .
docker run --rm --init --network none --cpus 1 --memory 256m --pids-limit 64 jobforge-executor-probe:c1 ./clock.test '-test.v' '-test.timeout=30s'
docker run --rm --init --network none --cpus 1 --memory 256m --pids-limit 64 jobforge-executor-probe:c1
```

PowerShell执行编译后测试二进制的`-test.*`参数须加引号。首次手动clock命令因未加引号在测试开始前失败；修正后实际通过，失败调用没有计为通过。全仓/其它依赖门禁按[开发指南](../development.md)配置。远程模型、生产留存以及S1-C完整运行/S2～S5仍未验收。
