# 正式 Worker 单次实模验收：未知费用停发

2026-09-30，在本次临时安全配置获得明确批准后，从本地提交
`6d1d8c1cd00a8c0b2769c15bcf7caa3bd19df43c` 开展一次合成任务。
**验收未通过：正式 Worker 已运行并登记一次真实供应商尝试，但没有完整响应、usage 或最终方案。**
环境可执行；不是权限拒绝或工作区丢失。未知费用规则已触发，没有重试。

## 链路和边界

Go 服务、隔离 PostgreSQL、gRPC、正式 agent-worker、安装后的 Python guardian/step 和正式
support-agent-v1 均实际运行。镜像 provider-check 从正式 runtime 继承，不注册测试 adapter、
不替换 DeepSeek origin。业务 HTTP 和 embedding 是合成 fixture；不涉及客户数据或质量评测。
gRPC 控制面仅容器内回环，使用合成 token、明文测试连接；供应商 HTTPS 始终验证证书和主机名。

既有凭据仅经宿主进程内存 → Docker attach stdin → Worker 匿名管道传入。
不进入 argv、Docker Config.Env、源码、日志或镜像；Worker 子进程使用显式环境白名单。
只为固定 `https://api.deepseek.com` 配置现有代理和只读公共 CA。
首次准备发现 Docker 客户端自动注入通用代理，启动前检查拒绝执行；当时尚未启动 Worker，
未提交模型请求。随后显式清空这些变量，完成唯一一次实模任务。

构建镜像 ID：`sha256:221c9f25508864f57580d8485cfb9f17b0de670ab9d5dc981d38b3fd45814c55`。
实际执行日志保存在工作区 `.cache/verification/approved-worker-live-20260930-final/`；
可长期审阅的脱敏账本见[JSON 证据](worker-real-provider-2026-09-30.json)。

## 实际结果

- Run：`24338339-916e-42da-90e4-93a0f518f90b`。
- 唯一 physical call：`d3fbee7f-56f5-4a5e-8506-345e57cd043c`，attempt 1、ordinal 1。
- 15:27:44.191132Z 预留，15:27:49.278011Z 持久记录供应商报告。
- `response_complete=false`、`http_status=0`、usage 与响应模型均不可用，
  `settled_at=null`，批次冻结 `CHAT_USAGE_UNKNOWN`。
- 配置请求模型为 `deepseek-flash`；不能声称收到该模型响应，不能确认供应商是否实际处理或扣费。
- Worker 停止，观察到的两个 guardian 进程组消失。原测试等到 140 秒超时；
  本次补充提前检测 Worker 退出，之后只做离线验证。
- 导出时 Run 为 running，未产出方案、没有 SDK 最终结果验收。本专用 harness 未启动生产 scanner，
  因此也没有验证失联后的最终 Run 状态收敛；批次冻结和费用保留已在 PG 验证。

停止收费调用后，只做不含凭据、无隧道内模型 HTTP 请求的网络诊断：
DNS、代理 TCP、CONNECT HTTP 200、目标 TLS 验证均成功。这不能证明原请求未计费，
也未定位约 5 秒失败的准确断点。没有发送第二次模型请求或鉴权诊断请求。

## 预算与清理

重新读取[官方中文价格页](https://api-docs.deepseek.com/zh-cn/quick_start/pricing/)，
公共页面 SHA256：`5a7b1832592387340f2fc456399b34b89b05f3fa167c2e35909e2fa4afe021e3`。
高峰每百万 tokens 缓存命中输入 0.04 元、未命中输入 2 元、输出 8 元。
按正式 profile 输入上限 1,048,576、输出上限 1,024，单次预留 2.105344 元。

| 项目 | 人民币 |
|---|---:|
| 总授权 | 50 |
| 此前保守已记费用 | 0.000152 |
| 本批已知费用 | 0（不代表实际免费） |
| 本批未知费用保留 | 2.105344 |
| 累计保守暴露 | 2.105496 |
| 扣除已知与未知保留后的额度 | 47.894504 |

没有查询账户扣款；本批没有 usage，不能计算实际费用。未知保留不得因测试数据库清理而释放。
所有本次新建的 PG、runner、诊断容器和匿名数据卷均已清理；镜像与脱敏日志保留。
没有推送、PR 或部署。后续任何重试须先处理本次未知费用并重新确认执行范围，
不能复用本次批准自动启动新批次。

## 离线交付与验证

新增独立 opt-in Go 整链路测试、未修改生产 registry 的 provider-check 镜像、
匿名管道启动器及累计预算/账本一致性回归。普通 CI 不运行收费测试。
启动器要求显式传入累计已知费用加未释放 hold，价格页发生任何变化即拒绝，
同一证据目录禁止复跑；它不是跨进程共享的全局账户管理器，操作方必须保持独占和真实累计账本。
密钥仅由已授权应用读取到内存使用，不提供手工导出接口。

- Python SDK、Agent、probe、support evaluation 与启动器离线测试：1810 passed。
- Ruff check/format、启动器 mypy：通过。
- Go lint：0 issues；新建隔离 PostgreSQL 16 的 3 项 support profile race 回归通过，资源已清理。
- 新实模失败后没有再次执行付费验收；提前退出检测等 harness 修正不声称经第二次实模验证。

## 零付费故障定位（同日追加）

本轮没有发送供应商模型或鉴权请求，没有修改原 JSON 账本。未知费用仍保留 2.105344 元，累计暴露 2.105496 元，剩余 47.894504 元。

**确定的缺陷是验收诊断丢失；原请求失败根因仍无法唯一确定。**

原镜像没有创建可选目录 `/var/lib/jobforge/outbound`，`outbound_audit.begin` 因此返回 None；Worker stdout/stderr 丢弃，Wait 错误也未保存。PG 的 unavailable 报告是计量事实，不区分连接失败、读超时、截断或取消；HTTP 0 也不能证明服务器从未返回 headers。原容器已清理，缺失现场无法补造。

| 层 | 已核实事实 | 结论 |
|---|---|---|
| HTTPX | `dispatch._client` 显式 connect=5，读/写/pool=60 秒，retries=0 | 5 秒不是意外继承默认值 |
| HTTPcore | 安装包 `http_proxy.py` 将 connect timeout 用于 CONNECT 后的 start_tls | TCP 连通不能排除 TLS 阶段的 5 秒超时 |
| 许可 | 原 call_deadline 为 15:28:44，报告在 15:27:49 | 没有到达该 60 秒调用截止时间 |
| Worker | 控制 RPC 上限 2 秒；step 总期限由服务授权约束 | 没有证据表明是固定 5 秒 Worker 总时限 |
| Python | HTTP timeout 映射为 TIMEOUT；取消、协议、网络失败均保留未知报告 | 无法从原报告反推具体异常 |
| 进程 | 报告已落 PG，后续 Worker/guardian 停止 | 无法判定先被杀还是先网络失败 |

新增三个回环代理回归使用生产 dispatcher/HTTPX、合成凭据，不连接外部地址：

1. CONNECT 200 后不完成 TLS：保留生产 connect=5，在 4.5～10 秒断言范围内触发 `connect_timeout`，留下未知 usage，只有一次 CONNECT。
2. 不返回 CONNECT 响应：仅测试内缩短 read timeout，得到 `read_timeout`。
3. CONNECT 407：得到 `proxy_error`，不重试，不泄露供应商 Authorization 或正文。

已有响应截断回归保留 HTTP 200 元数据，归类为 `remote_protocol_error`；取消、尺寸和编码失败回归继续通过。TLS 停顿能复现约 5 秒症状，但只是候选机制，不能当作原故障根因。没有因此调大超时或关闭 TLS。

修复仅增强诊断：细分 connect/read/write/pool timeout 和 proxy/connect/remote-protocol 错误，使用闭集字符串，不记录异常文本、完整 URL、正文或任意 headers。专用 provider-check 镜像创建短期元数据目录，启动器在清理容器前导出；导出失败不会释放 hold。harness 保存数字 Worker exit code，不复制 stderr。默认生产镜像配置不变。

本轮 Python 全套 **1813 passed**（新增 3 个代理复现），Ruff check/format、Agent 与启动器 mypy、Go lint 通过。镜像离线检查不代表新实模验收。

### 最小用户操作与后续决定

原报告没有 response_id，也没有留存供应商 request ID；没有可用于单请求查询的标识，未编造用量查询接口。请在原 DeepSeek 账户控制台查看 **2026-09-30 15:27:44～15:27:50 UTC** 附近 `deepseek-flash` 的用量/扣款；北京时间为 **23:27:44～23:27:50**。只需脱敏 token/扣款摘要及是否能对应本次请求，不提供 key 或模型正文。若只有汇总账单，不能据此认定该请求免费，仍保留未知额度。

当前批准不允许自动重试。下一步需要用户决定是否在保留旧 hold 的前提下，另行批准一次带新诊断的隔离实模验收；旧冻结批次不得重开，剩余额度本身不是新调用授权。

新镜像 `jobforge-review-runtime:provider-diagnostics` 已构建，ID 为
`sha256:d0f3843543e154f5c8db0069c0c4acab1fa6dc03a279fa43d4b67169128ecc8e`。
使用 `--network none`、镜像默认非 root 身份验证正式 registry/origin 未替换，
合成 `connect_timeout` 元数据可写入并通过 Docker cp 导出；检查容器已删除。
这没有启动收费 Worker。未来获准后必须重建 provider-check target，并显式选择已更新镜像
（启动器 `--image`），不能误用早期缓存镜像。验证日志位于工作区
`.cache/verification/provider-diagnosis-{python,go-lint,build}.log`。
