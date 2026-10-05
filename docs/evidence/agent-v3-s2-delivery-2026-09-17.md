# S2 有界 Agent：最终真实验收

2026-09-17，冻结候选 `a77064e` 完整执行 **40 案，业务正确 37/40（92.5%），安全证据完整、硬失败 0**，达到 [PRD v0.16](../product/JobForge_PRD_v0.16.md) 的至少 32/40 门槛。另有隔离批次对真实云端响应注入截断，unknown/full hold、冻结与停止符合合同。[PR #56](https://github.com/XJfyrh/JobForge/pull/56)交付。

## 范围、结果与失败

Go 提交模型决定后才授权下一步，Python 单步执行；模型选择订单/物流/政策等只读工具，累计已取得来源并提交方案。固定开发数据/gold 不变，没有拼接候选结果或打开保留集。

| 项目 | 最终候选实测 |
|---|---|
| 状态/业务 | 37 个 `awaiting_approval`；**DEV-031/032/033 为 MODEL_PROTOCOL_ERROR**，保留在 40 案分母 |
| 安全 | 40 案完整、硬失败 0；事实表摘要一致，reader 无写权限 |
| 动态路径 | 5 种已提交路径；13 案多次政策检索，检索共 54 次 |
| 调用 | 174 chat（3 次纠错）、131 业务读、54 embedding、108 metadata，共 467 physical |
| 计量 | 660,855 known tokens；unknown chat/held/anomaly 0 |
| 延迟 | p50 7.096449s、p95 8.795201s、max 11.251763s；创建到完成时更新时间，非生产 SLO |

37 个业务通过结果同时满足主张、覆盖和来源谓词。方案未批准/写入，后续 Run deadline 到期不覆盖原执行时的 SDK/评分结果。

## 候选与费用

| 冻结候选 | 业务正确 | known（CNY） | 结果 |
|---|---:|---:|---|
| `746cb35` | 7/40 | 0.293113 | 主张/政策覆盖不足 |
| `e669895` | 1/40 | 0.426545 | 39 案协议拒绝 |
| `377be60` | 27/40 | 0.301352 | 仍缺补充政策取证 |
| `a77064e` | **37/40** | **0.449934** | 达到门槛 |

S2 累计 known **1.470944 CNY**；两个隔离故障各 held 2.105344 CNY，共 **4.210688 CNY**，保守累计占用 **5.681632 CNY**，在新增 20 CNY 授权内。known 按冻结费率/usage 计算，**不是供应商结算账单**；hold 不记成零或已消费。S1 旧 known/hold 未释放、未转用。

## 截断故障

首个故障实验因代理无权读 0600 元数据，在 send 阶段断连，`injected=false`，**不计响应截断通过**；一次 unknown/full hold 与冻结保留。

修正实验使用相同 UID 65532，代理仅透传 TLS 密文。实际客户端收到 HTTP 200 后，代理将下一 565 字节 TLS 记录只转发 16 字节记录体再断开，`injected=true`；同 call 缺完整响应/usage，held 为 1,049,600 tokens / 2,105,344 microyuan。batch 以 CHAT_USAGE_UNKNOWN 冻结，Worker/driver 退出 1 并实际 Wait/清理，其后 HTTP 0、39 案未尝试。该注入故障单独验收，不计入最终 40 案质量分母，也不能称供应商原生故障或已知最终计费。

## 证据

[机器评分](agent-v3-s2-delivery-2026-09-17.json)与[receipt](agent-v3-s2-delivery-receipt-2026-09-17.json)包含原候选、调用路径、故障、费用和原始材料摘要。固定 Linux 进程/PG/gRPC/race验证机制；最终候选的[8 项 CI](https://github.com/XJfyrh/JobForge/actions/runs/35141834063)通过，交付以 PR 记录为准。

收费 Worker/故障代理已停止，旧账户/库/失败材料保留。完整故障时点、审查和初次联合检查 DNS 失败见[原提交报告](https://github.com/XJfyrh/JobForge/blob/ef0cab8523470f3c9ced77f683a8b14ac3778ab3/docs/evidence/agent-v3-s2-delivery-2026-09-17.md)。使用见[动态 Agent](../agent-v3/support-agent.md)，后续交付与限制见[状态页](../status.md)。
