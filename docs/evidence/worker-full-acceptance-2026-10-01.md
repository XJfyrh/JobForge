# 修复后的完整 Worker 验收

**最终通过：真实任务到达 awaiting_approval，正式 SDK 已回读完整待审方案，所有模型调用结算，无新增未知费用。**

本次在用户追加的自主修复与范围内重试授权下，继续使用原 50 元总预算。
固定 DeepSeek、并发 1、匿名管道既有凭据、显式代理及只读公共 CA、TLS 校验不变。
每个真实批次上限 3 元，创建独立测试资源；不推送、不部署、不建立持久访问。

## 免费完整配置路径

`tools/test-provider-proxy.sh` 创建 Docker internal 网络、独立 PostgreSQL 和非 root 正式镜像。
内部网络没有外网路由；本地代理只返回 CONNECT 407，不转发、不建立隧道。
固定策略和 Agent 策略均经过真实 Go coordinator 的 stepEnvironment、agent-worker、guardian、step。
仅合成凭据。代理只收到一条无 Authorization/Proxy-Authorization 的 CONNECT。

测试在模型和业务请求发生时读取本次合成子进程的环境，仅检查变量名称，不保存值：
模型 guardian/step 有 key/proxy/CA，业务 guardian/step 没有这三项；各子进程均无
通用代理、测试 DSN 或 Worker 凭据入口。CA 只读挂载，实际建立客户端需要成功加载它。
因此该测试覆盖曾漏掉的协调器筛选层，并未绕过它独立构造 HTTPX。
固定策略运行了真实业务调用链；Agent 验证模型决策入口。两个子测试均通过，资源清理。
已纳入 CI，未来没有真实模型凭据也能防止同类回归。

## 两个验收设施问题及处理

完整[中间账本](worker-acceptance-intermediate-2026-10-01.json)保留失败证据，不删除或改写结果。

1. 修复代理后，Run `8944cb65-1aaf-4270-85ac-124aaf2c3487` 成功获得四次真实模型响应，
   全部结算，但运行被 repeated_tool_call 保护终止。原机制 fixture 不论 query 都返回
   `P01.1: Synthetic mechanism policy`，缺失订单场景所需 P03 不可能检索到。
   这是将机制 fixture 用于真实语义验收的错误，不是模型传输失败。
   改为同一个合成快照返回仓库公开运行时 P03.1 原文；保留认证、向量和绑定检查。
   不使用 gold、holdout 或预先提供模型答案，不放宽重复调用保护，也未修改产品 prompt。
   四次真实 chat 的高峰价保守计费合计 0.010893 元，新增未知 hold 为 0。
2. 修复资料后，Run `8666da49-67fb-4c5e-9cb2-f88b82687735` 已达 awaiting_approval，
   三次 chat 全部结算，合计保守 0.004174 元，新增未知 hold 为 0。
   验收 SQL 却要求每个免费业务调用也必须有 known token usage，误判失败并跳过 SDK 回读。
   修正为只有 chat 必须 known，所有调用都必须有观察确认且无异常。
   免费回归验证“免费调用已确认 + 唯一被拒 chat”只计一个未确认调用，防止误判复发。

这些批次没有自动 HTTP 重试；同一任务中的多次 chat 是经许可、已结算的 Agent 决策。
后续批次均将前次已知费用加入 prior exposure，未重置预算。

## 确定未发送与未知 usage

2026-10-01 00:38 的 attempt 有完整本地失败路径：origin TCP started → failed，
没有连接完成，异常为 ConnectTimeout，故在这条可信执行路径上没有发送模型 HTTP。
可以在人工审核的预算核销记录中归类 `confirmed_not_sent`，释放该笔本地占用；
但不能把供应商 usage 伪造成观测到的零，也不能重开旧批次。

当前生产计费契约只有 known/unknown，只有有效 usage 能执行正常结算并减少 hold；
`CloseAttempt` 明确不退款。直接 UPDATE、覆盖原报告或单凭“日志中没看到 send”自动退款均不合适。
本次不扩大为新的计费迁移/管理接口；原账本保持不变，累计控制仍保守保留该笔 2.105344 元。
昨日缺少阶段 trace 的旧 attempt 不套用这个结论，继续按未知预留处理。
两笔 hold 是软件预算占用，不是实际支出。新增真实响应的费用来自 usage 按已核实价格估算，
只有最初 0.000076 元已有用户控制台账单确认，不能把后续估算写成账户扣款实查。


## 最终通过证据

Run `3eaf3a15-276a-4d3d-9df3-40a7fee229b3`：正式 Go/PG/gRPC/Worker/Python/DeepSeek
执行 7 个步骤、8 个 physical calls，其中 3 个 chat（不是 HTTP 重试）。模型响应均为
`deepseek-flash`、HTTP 200、nonthinking、usage complete，无计量异常或 report 冲突。
每次 chat 均有代理 CONNECT、目标 TLS、目标 HTTP headers/body 完成发送、完整响应阶段，
并保留真实 response ID/响应 hash。普通观察与计量确认均完成，batch_frozen=false。

安装后的 SDK 经真实 HTTP 读取 Run、steps、calls 和 result：awaiting_approval，结果可用，
方案为 request_information，requested_fields=[ticket.order_id]，conclusion=insufficient，
引用工单/缺失订单证据和公开政策 P03.1，建议票据状态 awaiting_information。
只是待人工审阅方案，没有实际修改客户工单或执行业务动作。

最终 3 次 chat：

| 次序 | 输入 tokens | 缓存命中 | 输出 tokens |
|---|---:|---:|---:|
| 1 | 3163 | 2944 | 17 |
| 2 | 3275 | 2944 | 38 |
| 3 | 3482 | 3200 | 91 |
| 合计 | 9920 | 9088 | 146 |

最终批次保守费用 **0.003196 元**，hold=0，Worker exit=0，7 个已观察 guardian 组消失。
测试 PASS（8.91 秒）。[最终结构化证据](worker-full-acceptance-2026-10-01.json)包含 SDK 方案、
PG 调用账本及阶段 trace；没有凭据或模型完整请求/响应正文。

镜像 `jobforge-review-runtime:provider-final`：
`sha256:17921c1f07480140f39bd5beb3c47d7f97f775c6362ff80a7f79b98cbc2ec1ec`。
工作区原始证据目录 `.cache/verification/approved-worker-final-20261001/`；所有本次临时资源清理完成。
无需继续请求模型；保留镜像和证据，未推送。

## 累计核算与验收范围

- 用户账单确认的实际费用：0.000076 元。
- 后续三个已完整结算真实批次，按高峰价保守核算合计：0.018263 元（0.010893 + 0.004174 + 0.003196）。
  usage 已实查，账户扣款未实查，不能把估算冒充账单。
- 两笔旧软件 hold：4.210688 元，未记作实际费用，也未自动释放。
- 已确认账单、后续保守费用与 hold 合计占用：4.229027 元；原 50 元授权余量：45.770973 元。
- 本次追加开发共收到 10 个成功模型响应，分别属于上述三个任务；每个 physical call 重试 0。
  00:38 的 TCP 失败没有进入模型 HTTP；昨日无 trace 的失败不推定已发送或未发送。

真实 Worker 模型接入及合成方案回读已完整通过。业务/embedding 服务仍为明确的合成 fixture，
政策文字来自公开 runtime 资料；本验收不是生产部署、真实业务数据库/检索质量、40 案或 holdout 评测。
免费子进程隔离测试已证明配置投影；本次没有改变生产费用状态机或新增自动退款。

## 验证汇总

- 最终真实任务：PASS，正式 SDK 回读 proposal/result/calls/steps 完成。
- 最终镜像的固定策略与 Agent 免费子进程路径：2 个子测试 PASS，使用最终脚本重新执行，网络/容器已清理。
- 独立 PostgreSQL 的 Run SDK、provider audit SDK 和 3 项 support profile 回归：5 项顶层 race 测试通过。
- 本轮构建包含真实进程集成测试 race 编译；Go lint 0 issues，Ruff check/format 通过，脚本语法、CI YAML、JSON 和文档链接有效。
- 前一阶段 Python 全套 1814 passed、Worker/executor/coordinator race 通过；本阶段没有修改 Python 产品代码，未重复声称整仓所有层均重跑。
- CI 已添加免费的完整代理路径任务；没有推送，所以尚未声称远端 CI 本次通过。
