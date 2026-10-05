# S1 固定流程：最终真实验收

2026-09-17，冻结版本完整执行 **40/40 开发案例，业务正确 11/40（27.5%），安全证据 40/40 完整、硬失败 0**。S1 是固定流程基线，没有额外业务正确率门槛；结果不表示全部业务正确。[PR #51](https://github.com/XJfyrh/JobForge/pull/51)交付，后续实现统一见[当前状态](../status.md)。

## 范围与结果

正式 SDK→控制 PG→Go Worker/固定 Python→真实业务 HTTP/pgvector/MiniLM→DeepSeek→持久方案/账本。一个冻结版本独立执行全部 40 案，不跨批拼接、不改 gold/scorer、不读取保留集。

| 项目 | 实测 |
|---|---|
| 状态 | 39 个 `awaiting_approval`，1 个 `no_action` succeeded；未尝试/中断 0 |
| 结构与来源 | 各 40/40 通过 |
| 业务 | **11/40**；失败全部留在分母 |
| 安全 | 七张业务事实表前后摘要一致，reader 写权限全部 false |
| 调用 | 40 chat、118 业务工具、40 embedding、80 metadata，共 278 physical |
| 计量 | 87,109 known tokens；unknown chat/held/anomaly 0 |
| 延迟 | p50 3.734005s、p95 4.190391s、max 6.029242s；创建到完成导出状态更新时间，非生产 SLO |

业务错误可重叠：缺必要主张 19、无依据主张 25、动作错误 11、结论错误 5、决定错误 4、请求字段错误 4、目标状态错误 7。逐案原因见[机器评分](agent-v3-s1-delivery-2026-09-17.json)。`awaiting_approval` 仅表示持久方案，未批准、未写入、未解决工单；后续 deadline 不改写完成时评分。

## 费用

| 范围 | known（CNY） | 未释放 monetary hold（CNY） |
|---|---:|---:|
| 原首批，旧授权单列 | 0.097526 | 0 |
| 上一收尾批 | 0.048653 | 2.105344 |
| 本次完整 40 案 | 0.137418 | 0 |
| 新增授权累计 | 0.186071 | 2.105344 |

known 是按冻结费率逐调用向上取整的保守账本值，**不是供应商结算账单**。hold 是未知上界，不记成零或实际消费；原首批另有 512 embedding-token hold。按共享 batch 聚合一次，不叠加 tenant/family 镜像。保留旧 hold 后本批持久 cap 为 2,846,003 microyuan，未超原新增累计 5 CNY。

## 证据与历史失败

[部署/逐案 receipt](agent-v3-s1-delivery-receipt-2026-09-17.json)关联版本、价格、Run/steps/calls、只读安全与耗时；原始 SDK、模型正文和秘密在仓库外。收费容器已停止，旧库/卷和失败保留。[干净环境复现](agent-v3-s1-reproduction-2026-09-17.md)验证部署/安装 SDK/停止，没有再次收费。

本次修复了[短 Run 心跳饥饿](agent-v3-s1-heartbeat-2026-09-17.md)，此前[首批](agent-v3-s1-first-cloud-2026-09-16.md)和[收尾批](agent-v3-s1-closeout-2026-09-17.md)的中断/unknown 仍是原失败。运行版本 `353ded3` 的[8 项 CI](https://github.com/XJfyrh/JobForge/actions/runs/35129644678)通过；最终交付检查以 PR 记录为准。

完整验收映射与原过程见[原提交报告](https://github.com/XJfyrh/JobForge/blob/ef0cab8523470f3c9ced77f683a8b14ac3778ab3/docs/evidence/agent-v3-s1-delivery-2026-09-17.md)，操作见[批次指南](../agent-v3/cloud-batch.md)。开发集结果不证明保留集泛化，跨阶段限制见[状态页](../status.md#限制与未结事项)。
