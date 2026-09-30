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
