# 受控 HTTP 与模型适配

固定 Python adapter 只发送已登记且获得持久许可的有界请求，关闭自动重试/重定向和代理环境继承。网络层负责请求、响应与来源/计量校验，Run 执行权和下一步由[Go Worker](runtime.md)与控制账本决定。

## 所有权与调用顺序

固定适配器准备一个不可变请求：登记的 endpoint 别名、方法、固定路径及一次序列化的 body bytes。物理参数指纹绑定 Run profile hash、snapshot 内容 hash、subcall、方法、路径和 body 摘要；业务索引的 profile hash 单独校验。秘密、调用 ID、TraceContext 和 timeout 不进入业务参数 hash。

派发层向执行器协调者申请许可，使用 v2 Conversation 验证绑定、顺序和原期限，在实际发送前消费一次许可；同一 body bytes 只交 HTTP transport 一次。客户端关闭自动重试、重定向和代理环境继承，拒绝并发排队。在线期限统一为 Linux CLOCK_BOOTTIME；Windows 真实运行使用 Linux 容器，测试可以显式注入时钟。

`DispatchHooks` 是可信协调者的入口：authorize 返回对应许可，settle 返回对应计量 ACK；observe 等待持久确认并返回普通 `call_observation_ack` Frame，由原dispatcher Conversation检查身份/hash/顺序/截止后才返回业务结果。此合同依照[ADR-0019](../adr/0019-executor-confirmation-and-exit-contract.md)，不能把写入管道当成持久确认。模块测试使用替身协调者，正式 Go IPC/PG/gRPC 见[运行时](runtime.md)；HTTP模块本身不连接数据库或替代持久授权。

完整响应和业务有效响应分开。先有界读取并捕获完整可信计量，再验证业务结果，最后报告 observation。版本、模型 digest、向量、snapshot/index/policy 和来源绑定均须在 accepted 之前通过。search_policy 的 version、tags、embedding、search 四次请求分别授权；坏前置响应不能触发后继 HTTP。

计量通过独立窄通道确认，超界原值仍保留并立即停止后续派发；业务输出非法不能丢弃已取得的合法 usage。取消后不继续等待供应商输出，不发新请求。已捕获报告仅留在当前步骤的有界内存中供协调者收尾，不是持久 journal；Go/主机在数据库提交前死亡仍可能留下 unknown 全额 hold。网络层不能授予 Run lease 或决定恢复。

已确认 rejected observation 后，协议状态机阻止新调用、允许合法 error step_result；超限、身份错误、取消、超时或未确认保持停止。模型内容非法与内部 `size_limit` 区分，后者不得进入有界纠正路径。W3C traceparent 经验证后随请求传递，不进入参数 hash；HTTP 层验证透传；Trace 后端的存储与运维属于独立部署边界。公开错误为固定码，删除底层异常链中可能保留的 Authorization 或模型正文。

## DeepSeek 约束

当前版本在有界读取后、业务校验前冻结 typed provider audit，通过独立计量 FD 交 Go 持久上报；不再把内存审计当作可查询证据。五类 report ACK、模型身份/模式停发、observed 与 settled 的区别见[审计指南](provider-audit.md)。

固定官方 Chat Completions 路径、`deepseek-flash`、非思考、非流式、JSON object、temperature=0、最多1024输出 tokens。模型可见业务内容16KiB、请求及完整响应各64KiB、模型内容16KiB；超限明确失败。适配器不装载业务 gold，不接收动态工具、模型 URL 或命令。方案/决定校验由登记的 support 策略提供；S2 的消息/请求上界见[动态 Agent](support-agent.md)。HTTP 层不决定政策结论或安排纠正。

usage 的原始整数必须精确且内部一致；缺失、矛盾、断连或截断保持 unknown，不填零。完整 usage 与模型内容 JSON/方案是否合法分别处理。模型响应身份用于审计和漂移检测，不能证明服务端版本被不可变锁定。费用计算、持久预留和本批冻结上限由控制账本负责。HTTP 模块不自行注册 profile 或授予预算。

协议依据：[Chat Completions](https://api-docs.deepseek.com/api/create-chat-completion/)、[JSON 输出](https://api-docs.deepseek.com/guides/json_mode/)；2026-09-16复核。供应商资料可能变化，真实批次前仍须重新核验身份、价格和账户可用性。

## 验证

纯校验和真实 loopback TCP 检查覆盖序列化/hash、严格响应、发送次数、取消/截断及许可顺序；模型、向量、协调者是测试替身。当前固定 Linux 检查使用 `tools/agentruntimecheck/Dockerfile` 的 `python-check`，命令与 lint 见[测试指南](../tests.md)。历史模块证据见[HTTP 记录](../evidence/agent-v3-s1c2-http-2026-09-16.md)，持久 PG/进程与真实模型结果从[证据索引](../evidence/README.md)分层读取。
