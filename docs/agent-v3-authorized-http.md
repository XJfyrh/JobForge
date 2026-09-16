# Agent v3 S1-C2：受控 HTTP 与云端适配

本切片按已接受的[ADR-0018](adr/0018-deepseek-fixed-flow-and-executor.md)实现正式执行器的 Python 网络边界。模块已通过本地确定性验证，等待 PR 独立审查与 CI；正式 Worker、guardian/FD 桥接、部署 profile、40 案固定流程和 DeepSeek 实际调用另行交付，不能由本页的模块测试代替。

## 所有权与调用顺序

固定适配器准备一个不可变请求：登记的 endpoint 别名、方法、固定路径及一次序列化的 body bytes。物理参数指纹绑定 Run profile hash、snapshot 内容 hash、subcall、方法、路径和 body 摘要；业务索引的 profile hash 单独校验。秘密、调用 ID、TraceContext 和 timeout 不进入业务参数 hash。

派发层向执行器协调者申请许可，使用 v2 Conversation 验证绑定、顺序和原期限，在实际发送前消费一次许可；同一 body bytes 只交 HTTP transport 一次。客户端关闭自动重试、重定向和代理环境继承，拒绝并发排队。在线期限统一为 Linux CLOCK_BOOTTIME；Windows 真实运行使用 Linux 容器，测试可以显式注入时钟。

`DispatchHooks` 是可信协调者的入口：authorize 必须返回对应许可，observe 必须等到 Go 的持久确认，settle 必须返回对应计量 ACK。当前仅测试协调者替身，未实现它们到 Go 的 IPC 桥接；现有 v2 没有 observation ACK，下一切片必须先补充受审契约，不能把写入管道当成持久确认。HTTP 模块本身不连接数据库或替代持久授权。

完整响应和业务有效响应分开。先有界读取并捕获完整可信计量，再验证业务结果，最后报告 observation。版本、模型 digest、向量、snapshot/index/policy 和来源绑定均须在 accepted 之前通过。search_policy 的 version、tags、embedding、search 四次请求分别授权；坏前置响应不能触发后继 HTTP。

计量通过独立窄通道确认，超界原值仍保留并立即停止后续派发；业务输出非法不能丢弃已取得的合法 usage。取消后不继续等待供应商输出，不发新请求。已捕获报告仅留在当前步骤的有界内存中供协调者收尾，不是持久 journal；Go/主机在数据库提交前死亡仍可能留下 unknown 全额 hold。网络层不能授予 Run lease 或决定恢复。

已确认 rejected observation 后，C1 状态机阻止新调用、允许合法 error step_result；超限、身份错误、取消、超时或未确认保持停止。模型内容非法与内部 `size_limit` 区分，后者不得进入有界纠正路径。W3C traceparent 经验证后随请求传递，不进入参数 hash；当前只证明 HTTP 透传，完整 Trace 后端闭环仍属后续验收。公开错误为固定码，删除底层异常链中可能保留的 Authorization 或模型正文。

## DeepSeek 约束

固定官方 Chat Completions 路径、`deepseek-flash`、非思考、非流式、JSON object、temperature=0、最多1024输出 tokens。模型可见业务内容16KiB、请求及完整响应各64KiB、模型内容16KiB；超限明确失败。适配器不装载业务 gold，不接收动态工具、模型 URL 或命令。业务方案校验器由后续登记策略提供，本层不决定政策结论或安排纠正。

usage 的原始整数必须精确且内部一致；缺失、矛盾、断连或截断保持 unknown，不填零。完整 usage 与模型内容 JSON/方案是否合法分别处理。模型响应身份用于审计和漂移检测，不能证明服务端版本被不可变锁定。费用计算、持久预留和5元批次硬上限仍由控制账本负责；本切片不注册可执行 profile，不发送收费验收。

协议依据：[Chat Completions](https://api-docs.deepseek.com/api/create-chat-completion/)、[JSON 输出](https://api-docs.deepseek.com/guides/json_mode/)；2026-09-16复核。供应商资料可能变化，真实批次前仍须重新核验身份、价格和账户可用性。

## 验收分层

| 层次 | 本切片要求 | 不能据此声明 |
|---|---|---|
| 纯校验 | 一次序列化/hash、严格 JSON/usage/身份/大小和来源，Go/Python 向量边界一致 | 模型质量或价格锁定 |
| 真实 loopback HTTP | 实际 TCP 验证请求次数、发送 bytes、截断/超时/取消、validator 与许可顺序 | PostgreSQL 持久授权或真实供应商已调用 |
| 工程 | Python 全测试、Ruff、mypy、Linux 实际时钟路径及仓库适用 CI；SDK/离线检索回归 | 正式 Worker 的 Kill/Wait/崩溃恢复 |
| 后续完整 C | 真 PG 许可→正式进程→真实业务/embedding/DeepSeek→SDK 查询→40 案评分 | 当前尚未验收 |

测试中的 provider、向量和协调者均为替身；真实 TCP 不等于真实模型。2026-09-16，审查修复后本地 Windows 全 Python 套件663项通过，固定 Linux 镜像内485项通过，无 skip。完整验证及 PR 状态见[切片证据](evidence/agent-v3-s1c2-http-2026-09-16.md)。历史 W4 失败、AT-25 跳过、远程模型和生产留存未验收继续保留。

## 本地复现

从仓库根目录运行；Windows 使用 PowerShell，Linux 把 `.venv/Scripts/python.exe` 换为 `.venv/bin/python`。不需要云端凭据，运行阶段不访问外网。

```powershell
python -m venv .venv
.venv/Scripts/python.exe -m pip install -r tools/requirements-lint.txt
.venv/Scripts/python.exe -m pip install --no-deps ./sdk/python
.venv/Scripts/python.exe -m pytest sdk/python/tests python/tests tools/agent_probe_data tools/executorprobe -q
.venv/Scripts/ruff.exe check .
.venv/Scripts/ruff.exe format --check .
.venv/Scripts/mypy.exe sdk/python python/jobforge_agent
.venv/Scripts/mypy.exe --platform linux python/jobforge_agent tools/agent_model_probe.py tools/executorprobe/executor.py
docker build --tag jobforge-agent-http:c2 --file tools/agenthttpcheck/Dockerfile .
docker run --rm --init --network none --cpus 1 --memory 256m --pids-limit 64 jobforge-agent-http:c2
```

Linux 验证镜像固定基础镜像 digest，并复用现有 requirements；传递依赖没有新增完整 lockfile。镜像仅复制 Python 模块/测试和协议 fixture，不包含业务 gold、秘密或正式模型。容器退出即清理；可用 `docker image rm jobforge-agent-http:c2` 删除本切片验证镜像。现有模型/数据库卷不受影响。
