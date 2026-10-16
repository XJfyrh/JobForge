# 公开预览交付与验证

2026-10-16，按[批准方案](../plans/public-readiness-2026-10-16.md)补齐项目展示、免费v3接入、部署判断和贡献入口。文档仍通过仓库Markdown维护，没有建设文档站或搜索。

## 交付与证据

| 范围 | 结果与入口 |
|---|---|
| 许可 | [PR #64](https://github.com/XJfyrh/JobForge/pull/64)已合并；两个Python包的wheel/sdist及从独立sdist重建的wheel均包含Apache-2.0表达式和与根LICENSE一致的正文；UI第三方资源保留原许可 |
| 对外说明 | [首页](../../README.md)、[英文摘要](../../README.en.md)、[业务边界](../support-overview.md)、[工程亮点](../engineering.md)，配套可编辑组件/恢复图 |
| 首次接入 | [Quickstart](../quickstart.md)、SDK独立脚本、[HTTP合集](../../examples/run-api/README.md)、OpenAPI连接与fixture来源示例 |
| 部署判断 | [适用性矩阵、配置/端口/窗口及SLO声明](../deployment.md)、[术语表](../glossary.md) |
| 外部贡献 | [15分钟路径](../../CONTRIBUTING.md)、[路线图及3个真实入门Issue](../../ROADMAP.md) |
| 公开演示 | [180秒新版录像](../demo.md)、双语字幕、封面、分段来源和SHA-256；原真实素材完整保留 |

免费HTTP检查创建真实Run `801e6011-461a-4cde-bef4-4c2880c91ff3`，提交到方案5.656秒、独立审批及回执核对共6.875秒，回执hash `6d4d35f3db6d97b247091a835a1339659ec66f326764c783bc6a159f2c2e0b4d`。覆盖权限、原方案绑定、重放、唯一版本更新及同一回执核对。SDK提交脚本与新版UI另共用Run `a87ede28-0103-47cb-b625-e7eb59584bde`，UI批准后SDK确认applied、工单版本4→5。

这些是Windows Docker Desktop上的真实Go/PG/Python机制路径；模型、向量及账本金额模拟。Linux CI在runtime core分片运行相同Compose和HTTP检查，保持自然30秒lease、180秒attempt合同；远程结果以对应PR最新head的[CI required](https://github.com/XJfyrh/JobForge/actions/workflows/ci.yml)及各分片记录为准。

本地quick通过202项Python检查及5个subtest、Ruff和Go单元检查；新增OpenAPI示例契约16项通过，SDK mypy、Go vet/golangci及文档链接检查通过。完整真实PG/race、固定Linux进程、镜像边界与观测配置由本PR选择的全套CI执行，不把本地平台skip记为通过。

## 发布范围

GitHub Preview `v0.1.0-preview.1`固定最终合并提交；SDK/Agent包本身沿用0.1.0版本，本轮不上传PyPI。公开附件仅包含视频、字幕、封面、来源清单、两包分发产物、许可和SHA-256，没有本机配置、DSN、秘密、原账本导出或完整证据目录。

本轮新增收费模型调用为0。真实V9结果以无Worker、无scanner、无可执行profile的GET-only服务及PG强制只读回看；历史Run、冻结profile/schema、gold、manifest及原始报告没有重写。质量数字仍属于原V9冻结候选，不因新UI或包元数据变更形成新质量验收。
