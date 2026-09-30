# 正式 Worker 显式出站：离线准备记录

基线：`9d2e5b939afe4431061fa28817add689ecb81389`，分支 `feat/reviewable-agent-release`。本记录对应后续正式 Worker 真实模型验收的准备阶段；尚未取得实际安全配置的单次确认，未启动真实模型整链路。

## 已完成

- Worker 部署配置按租户增加可选代理 origin / 公共 CA 文件字段，默认关闭；通过显式白名单传入固定 Python 执行器。
- Python 仅允许固定 DeepSeek endpoint 使用这组设置；HTTP/HTTPS 代理不接收用户信息、路径、query，公共 CA 成对配置且保持 hostname 与证书验证。通用环境代理不继承，业务 / embedding 不使用该代理。
- 验证现有凭据读取器可接受 POSIX 匿名管道，无需新建读取任意环境秘密的接口；只用合成凭据测试，不写入真实 key。
- [ADR-0025](../adr/0025-explicit-provider-egress.md)与[运行时说明](../agent-v3-runtime.md)给出配置边界，实际启用另需确认。

## 验证

- Python Agent 全套：**1376 passed**；包括新增 16 项配置拒绝、无效 CA、实际回环 CONNECT 407、零重试、CONNECT 不泄露供应商凭据、业务绕过环境代理及 HTTP/HTTPS 代理 TLS 配置检查。
- Go Worker 入口、执行器、协调器：`go test -race ./cmd/agent-worker ./internal/runexecutor ./internal/runworker` 通过。普通 Go 层的专用进程 skip 不算通过。
- 规范测试镜像 `process-check`：**45 PASS、0 FAIL**，非 root、`--init --network none`，验证默认正式进程生命周期与新 Go 环境检查；不涉及真实代理或收费模型。
- Go lint 0 issues，Ruff check/format 与 Python Agent mypy 通过。
- 发现并修正 httpcore 对 HTTP 代理不接受 proxy_ssl_context 的约束；只为 HTTPS 代理配置代理端 TLS context，目标端 TLS 校验始终保留。最终 Python 全套通过；该设置的实际环境出站仍待确认后验收。

本机日志：`.cache/verification/explicit-egress-{go,python,lint,process,process-build}.log`。没有新建业务数据库、没有实际应用真实凭据或运行时代理配置、没有新增收费调用。测试容器按 `--rm` 清理，保留镜像和日志供复查。

## 待确认的单次动作

1. 应用沿用既有 `DEEPSEEK_API_KEY`，通过匿名管道在内存中供给临时正式 Worker，不写文件、镜像、Docker 创建环境或日志。
2. 仅固定 `https://api.deepseek.com` 请求使用现有代理和只读公共 CA；不放开环境继承、不关闭 TLS、不扩大持久权限。
3. 新建隔离 PG / 控制与 Worker，执行一个合成任务，并发 1；不部署生产、不接触客户数据。

确认前不执行依赖这些动作的步骤。该单次确认要求来自本轮用户的明确安全边界，不是对既有总预算授权的重复请求。

## 预算预检

当前官方中文价格页重新读取为 HTTP 200：`deepseek-flash` / DeepSeek-V4.1-Flash，高峰每百万 tokens 缓存命中输入 0.04 元、未命中输入 2 元、输出 8 元；公共页面 SHA256 为 `5a7b1832592387340f2fc456399b34b89b05f3fa167c2e35909e2fa4afe021e3`。

[正式预留计算](../../internal/run/ledger.go)依据 profile 的 1,048,576 输入上限和 1,024 输出上限，单次 chat 预留 **2.105344 元**。最初提出的 1 元批次无法发送，已在启用前纠正为 **3 元批次上限**；不降低合同上界来获得调用。计划本轮预留 3 元，未知则保留并停发；最终以真实 PG 账本和 usage 核对。

总授权仍为 50 元，此前保守计费 0.000152 元，当前可用 49.999848 元。本准备阶段新增费用 0、尚未实际启用新的付费批次。真实链路、审计/费用、结果和清理证据必须在获准执行后另记，不能由本页替代。

## 后续正式 Worker 验收状态

本页预算和待批准状态是当时记录。已获批准后的单次实模尝试失败并触发未知费用停发；最新累计保守暴露 **2.105496 元**，剩余 **47.894504 元**。详见[正式 Worker 验收证据](worker-real-provider-2026-09-30.md)，不将早期独立 API 成功算作完整链路通过。
