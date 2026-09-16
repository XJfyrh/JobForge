# S1 support 固定流程与开发数据 v2 验证

基线 `d449bc709b99ad78d3563b2924802e535541a08a`，实施分支 `XJfyrh/s1-support-fixed`。
本切片落实已接受 ADR-0018 的固定业务流程，未启用收费 profile，未发送 DeepSeek 推理请求。
provider 持久审计按已合并 PR #47 的 ADR-0020 另行实现；完整评分器、40案真实云端、S2～S5尚未完成。

## 需求与证据

| 要求 | 实现与实际检查 | 边界 |
|---|---|---|
| 预注册固定流程 | 生产 registry 仅 `support-fixed-v1`；Go按 `support_fixed_v1` 选择有限步骤；缺订单跳过物流读取 | 无动态工具、任意代码或通用DAG |
| 闭集方案 | [源schema及共同fixture](../../api/support/v1/README.md)，模型恰6字段，持久方案恰8字段；Go/Python实际消费10有效、43无效向量 | 格式通过不等于政策判断正确 |
| 来源与模板 | 绑定实际snapshot/tenant/实体/版本/index，事件必须存在于唯一绑定物流；可信代码展开refs和summary | 不按case ID改答案；`order_delivery`允许状态冲突，不允许外订单ID |
| 事务与重放 | `TestRunSupportStorageContract`在真实PG/race验证三种结束分支、错误来源/模板拒绝、重放、游标、checkpoint、审批槽释放 | 写建议只进入awaiting_approval；无实际工单写入 |
| 正式执行器 | `TestRunSupportExecutor`真实Linux Worker→TCP gRPC→PG→guardian→step→生产SupportFixedAdapter；有订单、缺订单且纠正一次均通过 | HTTP业务/模型为合成服务；不能替代真实模型 |
| 依赖与隔离 | 生产镜像只注册固定adapter；无测试安装器、测试manifest、evaluation，供应商origin保持官方DeepSeek | 登记adapter不代表生产profile启用 |
| 开发数据 | 40条原expected不变，v2更新政策与ticket版本；11个语义锚、20查询、关系/hash由28项离线测试核对 | 不访问保留集；validator不是完整模型输出评分器 |
| 真实检索 | 重新向量化v2，发布两个租户索引、实际20查询、PG重启/幂等snapshot/跨租户404 | 19/20命中，RQ-06未命中保留；无云端chat |

模型输入由固定system和实际业务user对象组成，外部文本不成为指令权限。
消息总计≤16KiB、序列化请求≤64KiB、输出≤1024 token，至多一次协议纠正；完整上下文按profile预留，不用字节估算token。
标准JSON Schema验证使用开发依赖 `jsonschema==4.26.0`，不进入生产包依赖。
runtime-input schema通过本地URN registry引用support schema，禁止以联网解析代替本地合同。

## 实际运行层次

| 检查 | 结果 |
|---|---|
| Windows全仓 `go test -race -count=1 ./...` | 1319个测试/子测试事件与33包通过，0失败；20个具名skip及11个无测试包逐项保留在[汇总](agent-v3-s1-support-windows-race-2026-09-16.json) |
| Linux正式进程/race | 43个测试/子测试通过，无skip；真实guardian、step、父进程死亡、取消、FD/组清理 |
| Linux coordinator/race | 107个测试/子测试通过，无skip；实际BOOTTIME与确认顺序 |
| Linux真实PG/gRPC/进程 | 18个测试/子测试事件通过，无skip（含一个进程helper入口）；新增2个support分支，其余包含SIGKILL、取消、ACK丢失、unknown、异常计量等C3b回归 |
| Python Windows | `python/tests` 930通过、8个Linux专用skip；这些路径由下面Linux层实际执行 |
| Python Linux | 固定镜像内938通过、0 skip；已安装包镜像，测试以源码目录执行 |
| SDK、旧探针、开发数据 | 合计206通过；其中数据validator 28通过；无模型调用 |
| Go build/vet/golangci-lint | 全仓通过，lint 0 issues |
| Ruff check/format、Mypy | 全仓Ruff通过；SDK与正式包Linux类型检查、离线数据类型检查通过 |
| SQLFluff/Buf | 历史基线有效，migration lint与Buf lint通过；无migration/Proto变更 |
| 生产镜像 | 构建与registry/origin/文件隔离检查通过 |

Windows全仓race在新增Linux专用support进程测试前运行；其后新增的Linux路径在固定镜像实际执行。
最终PR CI将再检查提交head。Windows的15个协调器/执行器环境skip由Linux层补验；
AT-25、两个旧真实模型用例及两个helper入口的skip不改成通过。
新support流程测试校验6个提交步骤、7次实际合成HTTP，分别为有物流+一次chat和无物流+两次chat；
两者均验证已知usage、拒绝次数、审批等待、原工单未变以及进程消失。

首次integration镜像构建失败：Go领域新增业务类型依赖，而测试build stage缺少 `migrations/business` 源包。
补充build stage的 `COPY migrations` 后，重建及上述Linux检查全部通过；原失败日志保留。
Worker SIGKILL测试为已提交步骤复用和未提交步骤重执行；通过数据库时钟加速lease/session/backoff到期，
不宣称等待完整自然TTL，也不宣称模型推理断点恢复。

## 真实v2索引与检索

新建独立可重建数据库 `jobforge_support_v2_20260916`，保留原v1演示数据库。
按[业务指南](../agent-v3-business.md)初始化、导入v2、准备和发布索引；本机Compose覆盖仅切换业务数据库名。
固定Ollama 0.32.5、`all-minilm:22m`，digest
`1b226e2802dbb772b5fc32a58f103ca1804ef7501331012de126ab22f67475ef`，384维、20段。

| 项目 | 实际值 |
|---|---|
| corpus SHA256 | `c23799485a532eda42d09b6bf6fbfaad96ce5152bc74a59dacf1e8d165f89198` |
| profile hash | `6c2409708d68e3a628fa04f0d0c8b138972de638be652e2e3782efca523e165e` |
| 北租户index | `ca7a1ab0-119e-476b-99f8-e4bde0d5cbf3` |
| 南租户index | `b6582d29-15c1-4456-861f-f8e0946d564b` |
| snapshot | `b544c0fa-1da1-4253-b463-fc78ff38e037` |
| 全20条结果 | Hit@3 19/20=0.95；MRR@3 0.8083333333333333；RQ-06未命中 |

重复发布得到同一index、相同request_key返回同一snapshot；实际重启PostgreSQL后snapshot内容hash和index保持不变，
南租户读取北snapshot返回404。逐行rank/引用/hash见[元数据证据](agent-v3-s1-support-retrieval-v2-2026-09-16.json)，不提交完整模型输入输出。
v1的19/20结果仍单列，当前同分不表示复用了旧向量或旧报告。

## 定向性能

同一Windows Go 1.26.5、Docker PostgreSQL 16.14、5433数据库、GOMAXPROCS=1，
按base/candidate交替5组、每组64Run运行 `TestRunHotPathPlansAndBaseline`，全部退出0。
基线d449bc7；每组448调用、192工具、384步骤。原始全部轮次见[汇总](agent-v3-s1-support-performance-2026-09-16.json)。

| 指标 | 基线中位数 | 当前中位数 |
|---|---:|---:|
| Run/s | 5.159 | 5.192 |
| claim p95 ms | 32.919 | 32.970 |
| checkpoint p95 ms | 10.049 | 9.797 |
| commit-step p95 ms | 18.773 | 18.711 |
| reserve-chat p95 ms | 18.525 | 21.036 |
| observe-chat p95 ms | 14.391 | 14.362 |
| submit p95 ms | 31.577 | 26.264 |

吞吐范围base 5.112～5.252、candidate 4.861～5.217；reserve p95中位数升约13.6%，各轮范围重叠。
宿主有其他容器及构建负载，未丢弃慢轮；这些观测不足以判定显著性能提升或回归。
此测试检查原Run热路径分支，不代表support模型耗时。历史W4门禁失败、AT-25跳过继续保留。

## 复现与审查边界

完整Windows前置DSN、固定镜像build/run命令见[运行时指南](../agent-v3-runtime.md)。
同一DSN只运行一个可能清理数据库的测试进程。当前integration镜像默认同时运行
`^TestRun(Executor|SupportExecutor)`；生产profile仍须后续正式审计合同落地后启用。
数据校验执行 `python -m pytest tools/support_evaluation`；fixture正常测试逐字节核对生成结果。

独立数据审查核对全部40条标签/11个锚及原始字节，无P1/P2；报告
`s1-support-v2-independent-review.md` SHA256
`170eeb9e0e59e5e91440bfd8634facbf2b97000a947597e792bfd94bf32936ff`。
随后只更新review/真实检索状态元数据，政策、gold expected、锚和查询正文未变；实现PR另做独立上下文审查。
仓库外日志/决策归档 `E:\JobForge-notes\2026-09-16-agent-v3-s1`，无凭据和完整模型正文。

仍未完成：完整claim/policy评分器及冻结、provider审计持久桥、收费profile和串行批次、40案DeepSeek，
以及S2动态Agent、S3完整故障矩阵、S4审批写入、S5保留集/展示/观测。远程推理和生产长期留存未验收；
真实本地embedding和合成HTTP机制成功不覆盖上述边界。
