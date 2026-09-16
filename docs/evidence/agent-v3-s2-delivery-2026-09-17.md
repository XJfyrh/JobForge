# S2 有界 Agent 真实验收与交付

2026-09-17（UTC+8），冻结候选 `a77064ede306ea596fb25c9f879b69cfaa40a7c8` 完整执行原40个开发案例，**业务正确37/40（92.5%），安全证据40/40完整、硬失败0**，超过[PRD v0.16](../product/JobForge_PRD_v0.16.md)要求的32/40。另一个隔离批次在真实云端HTTP响应开始后注入传输截断，验证了unknown/full hold、批次冻结与停止。交付PR为[PR #56](https://github.com/XJfyrh/JobForge/pull/56)。

[机器评分](agent-v3-s2-delivery-2026-09-17.json)、[冻结部署/历史批次/调用路径/费用收据](agent-v3-s2-delivery-receipt-2026-09-17.json)、[使用指南](../agent-v3-support-agent.md)。原始SDK导出、启动与停止记录、构建receipt及审查保存在仓库外 `E:/JobForge-notes/2026-09-17-agent-v3-s2`；公开文件不包含凭据或完整业务正文。

## 可用结果

S2使用`support_agent_v1` / `support-agent-v1` / `linux-v2-agent-runtime-1`。Go提交模型决定后才授权下一步，Python执行单步。模型可以查询订单、物流，按已取得证据补充政策检索，最后生成既有结构化方案。重复参数在第二次工具扣额前拒绝；8次工具、12次chat、全Run一次纠错及原lease/fencing/审计/进程清理边界保留。

| 项目 | 最终候选实测 |
|---|---|
| 正式主后端 | DeepSeek `deepseek-flash`，非思考JSON；本地MiniLM只做embedding |
| 40案状态 | 37个awaiting_approval方案，3个MODEL_PROTOCOL_ERROR；未尝试0 |
| 业务正确 | **37/40（92.5%）**；DEV-031、032、033协议失败均留在分母 |
| 安全 | 40案证据完整、硬失败0；业务事实表前后摘要一致，reader无写权限 |
| 动态路径 | 5种已提交步骤路径；13案多次政策检索，总检索54次 |
| 实际调用 | 174 chat，含3次协议纠错；131业务读工具、54 query embedding、108 metadata，共467物理调用 |
| 计量 | 660,855 known tokens，unknown chat 0、held 0、measurement anomaly 0 |
| 延迟 | Run创建至完成时更新时间，nearest rank：p50 7.096449s，p95 8.795201s，max 11.251763s |

37个成功业务结果都通过原主张真实性、必要主张覆盖和来源评分，没有用结构正确代替业务正确。开发数据、gold和业务谓词未修改，未打开保留集，不拼接候选结果。评分器的增量只支持实际动态步骤、累计检索来源和S2固定profile。

方案进入awaiting_approval只表示可供审批，未批准、未写入、未解决工单。原Run的360秒deadline过后可转成failed；历史评分使用完成时的原始SDK导出，不覆盖为晚到状态。验收后只读SDK再次查询确认：即使该Run已因deadline变为failed，持久proposal、Steps与Calls仍可读取。审批写入属于S4。

## 候选历史与费用

| 候选 | 冻结代码 | 业务正确 | known费用估算（CNY） | 结论 |
|---|---|---:|---:|---|
| v1 | 746cb35 | 7/40 | 0.293113 | 主张与政策覆盖不足，失败保留 |
| v2 | e669895 | 1/40 | 0.426545 | 压缩提示后39案协议拒绝，失败保留 |
| v3 | 377be60 | 27/40 | 0.301352 | 恢复完整结构合同；仍缺补充政策取证 |
| v4 | a77064e | **37/40** | **0.449934** | 同一版本完整40案达到门槛 |

v3新增可选诊断只保存固定校验模块与行号，不保存被拒模型正文。v4的静态政策导航只把主题与实际已取得别名相交，不加入新来源、不读取gold、不替模型选工具。四个候选均安全硬失败0、各自账本完整，不把前次失败改成成功。

S2累计known为 **1.470944 CNY**；两次隔离故障各保留 **2.105344 CNY** 全额上界，共held **4.210688 CNY**，保守累计占用 **5.681632 CNY**，未超过初始新增20 CNY。每次新batch cap都扣除全部已启动S2 batch的known+held，按batch计一次，不叠加tenant/family镜像。无需因费用再请求确认或调高预算。

known采用登记峰时费率逐调用整数向上取整，**不是供应商结算账单**；hold是尚未知的保守上界，不是已花费金额，也不记成零费。S1历史known 0.283597 CNY、monetary hold 2.105344 CNY和另512 embedding-token hold保持原样，未释放或转作S2授权。

## 隔离故障证据

第一次`cloud-response-cut`中，代理以root且移除全部capabilities运行，无法读取Worker拥有的0600元数据文件；该权限失败已离线复现。连接在send阶段断开，`injected=false`，**不计响应截断验收通过**。该批只有1次unknown chat，全额hold保留，账本冻结并停止；下一实验使用新身份与剩余额度，没有重启或解冻旧批。

修正实验`cloud-response-cut-v2`让代理与Worker使用同一UID65532，并独占全新空outbound目录。代理只透传TLS密文，不终止TLS、不读取凭据/明文/会话密钥，正式Worker仍访问`https://api.deepseek.com`并验证官方证书。DNS、连接、总过程分别有10/90/600秒上限，容器另有660秒OS硬截止。

同一physical call `69302fb5-8c3e-4468-8dde-ec9199474aad` 的链路为：

1. 真实客户端在 `19:49:25.225583Z` 记录HTTP 200响应头。
2. 外部代理在 `19:49:25.990279Z` 将下一565字节TLS记录只转发16字节记录体后关闭；`injected=true`。
3. 客户端记录`stage=body / reason=http_error / response_complete=false`，未取得完整响应与usage；同call在Calls保持unknown，held为1,049,600 tokens与2,105,344 microyuan。
4. batch以`CHAT_USAGE_UNKNOWN`冻结；Worker和driver均退出1，`19:49:26.345111Z`完成Wait，`children_reaped=true`。只有1次实际chat，其后没有任何HTTP派发，39案未尝试。

故障批Run在恢复扫描后可回到ready，但冻结批次仍不可派发；不能把Run状态当作停止屏障。该实验是注入的响应传输故障，不能声称供应商原生故障或知道其最终计费。故障导出由于刻意缺完整响应，常规40案评分的安全完整性条件不成立；它只按上述故障专用证据验收，不纳入成功候选的质量分母。

## 机制、审查与复现

- 共同Go/Python决定fixture、累计来源、对象绑定、全局纠错、重复工具和上限检查通过。新增0025只扩展physical call step_kind约束，没有改写历史migration。
- 固定Linux真实Go Worker/Python进程/gRPC/PostgreSQL通过动态、纠错后动态、重复工具不再扣额、纠错后再次非法、纠错仍非法五类联合检查；此前有效审计确认/停止故障证据继续复用。测试供应商fixture只证明机制，真实业务质量来自本次云端40案。
- 本地完整联合检查最初漏传`--add-host control:127.0.0.1`导致launcher DNS失败，补齐运行参数后只重跑该失败项通过；原日志和SHA256均保留，没有伪称首次全通过。
- 代码提交a77064e的[CI 35141834063](https://github.com/XJfyrh/JobForge/actions/runs/35141834063)八项全通过，包含全仓race/真实PG、固定Linux进程、Python/SDK、SQL/Buf及可观测配置。最终文档head的必需检查单独绑定在PR，不以旧代码CI冒充。
- Go/迁移/SDK和Python/提示分别经独立审查，无未解决P1/P2。真实证据另作独立核对，记录在外部`final-evidence-review.md`；合并须等待该审查与最终CI通过。

所有收费Worker与两个故障代理均已停止；控制服务切回最终成功候选的独立控制库，并挂载disabled配置供只读查询。所有旧库、冻结profile、attempted、hold与原始导出保留。复现沿用[运行指南](../agent-v3-support-agent.md)及S1已完成的干净环境证据，不为文档修改再收费跑40案。

S3恢复成本实验、S4审批写入、S5页面/保留集与生产长期运维不在本次验收。历史W4性能失败、AT-25跳过和RQ-06检索未命中继续保留。本报告是固定40案开发验收，不声称保留集泛化或生产SLO。
