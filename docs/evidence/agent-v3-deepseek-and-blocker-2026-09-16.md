# DeepSeek 接入调查、独立复审与环境阻塞

日期：2026-09-16（Asia/Shanghai）。工作分支：`XJfyrh/agent-v3-s0`；入口：[草稿 PR #35](https://github.com/XJfyrh/JobForge/pull/35)。本记录追加于 e6b0038 之后，不覆盖旧提交证据。

## 本轮范围与状态

维护者授权自主推进，指定云端优先支持 DeepSeek，并明确遇阻塞停止后报告。本轮完成只读接入检查、独立上下文审查及已发现问题的修复；Docker Desktop 启动失败后停止 S1 开发，仅收尾审查修复和证据。S1 尚未实现，PR 保持草稿、未合并。

## DeepSeek 实际检查

仅访问官方 HTTPS 域名 `api.deepseek.com`，使用15秒超时、禁用重定向与环境代理，无自动重试。结果仅保留非秘密字段：

| 请求 | 实际结果 |
|---|---|
| `GET /models` | HTTP 200；`deepseek-flash`、`deepseek-v4-pro` |
| `GET /user/balance` | HTTP 200；`is_available=true`；币种 CNY |
| `POST /chat/completions` | 未调用，0次 |

凭据仅在本次进程内使用，没有写入仓库、证据文件或发送给审查 Agent。没有记录余额数值、响应原文或鉴权头。只读检查说明账号可用，不证明模型推理、工具调用或业务质量已验收。

官方调查支持首轮采用 Flash、非思考、非流式、并发1、小输出上限，并由部署固定 endpoint/profile；这些参数仍待S1实现。稳定JSON模式仅保证JSON格式，业务Schema和引用仍须本地校验。按最高无缓存价格做调用前预留，禁用隐藏重试；超时后供应商是否已完成未知时保留预留额度，不假定断连会取消计费。未证明输入token上界前，不把粗略字符估算当作费用硬上限。本轮未落实或执行推理预算。

资料：[模型列表](https://api-docs.deepseek.com/api/list-models/)、[余额接口](https://api-docs.deepseek.com/api/get-user-balance/)、[调用接口](https://api-docs.deepseek.com/api/create-chat-completion/)、[JSON模式](https://api-docs.deepseek.com/guides/json_mode/)、[工具调用](https://api-docs.deepseek.com/guides/tool_calls/)、[价格](https://api-docs.deepseek.com/zh-cn/quick_start/pricing/)。模型别名和价格需在实际接入时记录，不能宣称已固定云端权重版本。

## 独立上下文审查与修复

审查 Agent 使用 `fork_turns=none`，自行读取仓库规则、PR diff、代码和测试；没有共享用户凭据，也没有修改文件。审查后又只读复核最终补丁。

1. 模型协议探针曾接受重复引用：只读取一个工具、重复三次该引用仍可能通过。现在要求当前案例三个工具全部实际读取，结果包含三个不同的预期引用。增加5项确定性回归，模型探针共32项通过。
2. 执行器收到有效结果和stdout EOF后，迟到的stderr超限可能被忽略。现在在 `cmd.Wait` 完成并收齐stderr后复核上限。固定helper通过信号屏障确定性复现旧代码误成功，修复后WSL定向回归通过；[完整命令与hash](../../tools/executorprobe/stderr-eof-regression-2026-09-16.txt)。

复审确认两项代码问题已消除，但未将缺少最终固定容器/race验证视为通过。协议fixture不代表真实业务工具验收。

## 环境阻塞证据

Docker Desktop 4.83.0 常规启动失败。主机日志 `C:\Users\12898\AppData\Local\Docker\log\host\com.docker.backend.exe.log` 在 `2026-09-16T03:55:17.471356100Z` 记录：

```text
starting services: initializing Inference manager:
listening on unix://C:/Users/12898/AppData/Local/Docker/run/dockerInference:
remove C:/Users/12898/AppData/Local/Docker/run/dockerInference:
The file cannot be accessed by the system.
(listener: The filename, directory name, or volume label syntax is incorrect.)
```

桌面错误页报告 `An unexpected error occurred`；`docker version`、`docker desktop status`、`docker info` 无法完成。定位到Inference manager监听路径初始化失败，底层文件不可访问的原因尚未查明，不能归因于JobForge代码。

影响：无法按仓库要求启动可重建PostgreSQL、进行后续pgvector业务验收，或复验当前补丁的固定容器进程/race场景。已终止本轮挂起的Docker客户端；没有重置Docker、删除路径/卷/数据或修改全局配置。

恢复条件：Docker Linux engine能正常响应 `docker info`，随后按仓库要求启动测试PostgreSQL并设置测试DSN。恢复后先复验当前补丁及PR检查，再推进S1云端适配、调用账本和真实业务工具；当前不以WSL非race结果替代固定环境验收。

## 验证边界

| 检查 | 当前补丁结果 |
|---|---|
| Python SDK＋模型探针＋执行器guardrails | 93/93通过（51＋32＋10），无模型请求 |
| 模型探针 Ruff/format/mypy | 通过 |
| 执行器 Linux vet/lint、Python Ruff/format/mypy | 通过 |
| stderr EOF确定性回归 | WSL通过；`CGO_ENABLED=0`，未启用race |
| 当前补丁固定Docker完整进程/race | 无法运行，环境阻塞 |
| 当前补丁Windows宿主真实PG集成 | 未运行，环境阻塞 |
| 旧提交 e6b0038 PR CI | [六个job通过](https://github.com/XJfyrh/JobForge/actions/runs/35006231149)，不充当当前补丁结果 |
| 真实云端推理、新业务/审批/步骤恢复 | 未运行/未实现 |

未修改队列核心、migration或公开接口。原有远程模型与生产长期留存未验收、历史W4性能门禁失败、AT-25跳过继续保留，不因本轮只读云端检查而改变。
