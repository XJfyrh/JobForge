# JobForge

**把工单、订单、物流与政策证据整理成可审阅的售后方案，经人工批准后记录到工单。**

[![CI](https://github.com/XJfyrh/JobForge/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/XJfyrh/JobForge/actions/workflows/ci.yml)
[![License](https://img.shields.io/badge/license-Apache--2.0-blue)](LICENSE)
[![Release](https://img.shields.io/github/v/release/XJfyrh/JobForge?include_prereleases)](https://github.com/XJfyrh/JobForge/releases)

[观看三分钟演示](https://github.com/XJfyrh/JobForge/releases/download/v0.1.0-preview.1/jobforge-demo.mp4) · [开始免费 Run](docs/quickstart.md) · [业务边界](docs/support-overview.md) · [工程亮点](docs/engineering.md) · [English](README.en.md)

[![任务工作台：方案、依据与实际更新结果（隔离测试数据）](docs/images/workbench.jpg)](https://github.com/XJfyrh/JobForge/releases/tag/v0.1.0-preview.1)

客服在同一工作台查看方案与依据，独立审批者确认后保存结论或工单标记，最后核对实际更新结果。当前动作范围见[业务说明](docs/support-overview.md)。

| 已核验结果 | 证据与范围 |
|---|---|
| Agent正确 **18/20**，完整证据 **18/20** | [V9未见正式集](docs/evidence/agent-v3-s5-v9-formal-v4-2026-10-16.md)，2026-10-16，含逐案结果与两案错误 |
| Fixed各 **11/20**；配对胜8 / 平11 / 负1 | 同一20案；报告列明两流程及提示的对照条件 |
| 业务提交后丢失响应，恢复确认唯一回执 | [真实故障演示](docs/evidence/agent-v3-s5-closeout-2026-10-16.md)，接管后复用原模型步骤 |

Go/PostgreSQL管理执行权、持久步骤、恢复、审批与费用，Python执行登记的Agent单步。交付为at-least-once，外部副作用通过业务幂等和回执核对；设计与验证见[工程亮点](docs/engineering.md)。

## 按任务进入

- 体验与接入：[Quickstart](docs/quickstart.md)、[SDK/HTTP示例](examples/run-api/README.md)、[Run API](docs/agent-v3/runs.md)、[OpenAPI](api/run/v2/openapi.yaml)。
- 评估部署：[适用性与配置](docs/deployment.md)、[架构](docs/architecture.md)、[术语表](docs/glossary.md)。
- 参与贡献：[第一次贡献](CONTRIBUTING.md)、[近期路线图](ROADMAP.md)。
- 深入核验：[当前状态](docs/status.md)、[文档导航](docs/README.md)、[验收证据](docs/evidence/README.md)。

免费体验使用模拟模型响应、向量和费用，Run、审批与回执运行于隔离的真实服务，启动不需要云端密钥。

已有 Job API 及本地 Ollama Agent/RAG 示例继续可用，使用[旧 Job 路径](docs/legacy/README.md)；它们与 v3 的 Run 服务分开启动。

代码采用 [Apache-2.0](LICENSE)，界面第三方资源保留[各自许可](internal/run/httpapi/ui/assets/README.md)。安全问题按 [SECURITY.md](SECURITY.md) 私密报告。
