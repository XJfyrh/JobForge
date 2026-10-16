# S5 Agent 开发集回归

2026-10-07，原40案开发集真实执行完成：方案正确37/40，完整案例证据37/40，越权、未经批准写入、重复效果及额度突破均为0。固定流程开发对照、正式未见20案和恢复费用对照尚待执行；正式数据包尚未创建或打开。

使用独立登记的 schema6 profile、正式 Go Worker/Python/SDK、DeepSeek-V4.1-Flash nonthinking，以及真实 MiniLM/pgvector 和业务 HTTP/PostgreSQL。模型、提示词、工具与业务数据沿用受审开发配置。逐案质量基于完成时封存的原 API 导出；未审批 Run 随原360秒期限终止，后续终态不改写已封存的方案评分。

| 项目 | 实际结果 |
|---|---:|
| 原登记案例 / 实际 Run | 40 / 40 |
| 正确方案 / 完整案例证据 | 37 / 37 |
| chat / query embedding | 174 / 54 |
| 已知 tokens | 659,926 |
| known / held | 422,652 / 0 microyuan |
| 费用估算 | ¥0.422652 |
| 结束后未完成 attempt / 在途 reservation | 0 / 0 |
| 业务事实 / 回执 | 前后相同 |

DEV-031、DEV-032、DEV-033 为仅记录信息的工单场景，模型引用未通过“已获取来源”校验，计为失败。诊断能定位到来源引用检查；未落盘的响应正文不能用于进一步归因。全部40案保留在分母。

实际收费窗口为09:00–15:00 UTC，运行09:02:46–09:12:07 UTC，容量1。本批持久上限4,000,000 microyuan；S5累计已知422,652、held0，剩余19,577,348 microyuan。该值按登记费率及实际usage计算，不是供应商结算账单。

真实首案 Trace 的 SDK原Submit、attempt link、Go/Python、模型及业务HTTP关联已由独立审查核对。首批发现的Worker metrics容器监听接线已修复；独立零调用环境验证私网HTTP200、Prometheus Worker up，数据库0 Run/0 call/0 known/0 held。到期列表已通过真实浏览器核验，显示“方案已保存，处理未完成：已超过处理期限”。

执行提交`1146881`的[全部8项CI](https://github.com/XJfyrh/JobForge/actions/runs/37595618588)通过。后续接线、列表和登记文件读取小修另完成相关Python306 passed / 5平台环境skip、mypy29文件、Ruff、HTTP包测试及实际浏览器/私网抓取；最新提交CI以[PR检查](https://github.com/XJfyrh/JobForge/pull/61/checks)为准。

[机器摘要](agent-v3-s5-dev-agent-2026-10-07.json)绑定原登记、API导出、逐案评分、账本和独立审查摘要。原始证据位于`E:/JobForgeEvidence/s5-2026-10-07/dev-agent`，接线/页面验证位于同级`metrics-ui-fix`目录。
