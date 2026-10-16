# 贡献指南

开始前阅读[文档导航](docs/README.md)、[当前状态](docs/status.md)、相关[产品契约](docs/product/README.md)与[ADR](docs/adr/README.md)。可靠性不变量以契约为准。

## 工作流与提交

外部贡献者先fork仓库，从最新 `main` 创建 `<type>/<short-description>` 短分支，例如 `docs/explain-receipts`。维护者可以保留个人前缀，它不是公共贡献要求。保持提交聚焦，不混入无关格式化/重构；发起 PR，说明问题、结果、相关影响和实际验证。评审与必需检查通过后 squash merge，日常不直接推送 `main`。

## 第一次文档贡献

准备Git和Python后，可用约15分钟完成一次小修改；安装、下载和等待CI的时间另计，不需要Docker、数据库或模型环境。

1. 从[英文部署摘要](https://github.com/XJfyrh/JobForge/issues/65)、[取消/retry示例](https://github.com/XJfyrh/JobForge/issues/66)或[迟到确认例子](https://github.com/XJfyrh/JobForge/issues/67)选一个 `good first issue`，按其范围与验收条件修改当前指南。
2. 从fork创建上述短分支，修改后在仓库根目录执行 `python -m tools.ci_docs`，检查引用、锚点和Linux大小写。
3. 执行 `git diff --check`，提交并向 `main` 发PR，写明改动与实际检查结果。

术语见[术语表](docs/glossary.md)，后续方向见[路线图](ROADMAP.md)。修改政策、PRD、ADR、gold或机器证据需要按其契约维护，不作为普通文案重排。代码改动按下文运行相应局部检查。

提交使用 Conventional Commits：`type(scope): 简短描述`，scope 按需。type 为 `feat/fix/docs/refactor/test/perf/build/ci/chore/style/revert`，小写英文；描述默认简体中文，PR 内语言一致，尽量不超过 72 字符且末尾无句号。不兼容变更用 `!` 和 `BREAKING CHANGE:` 说明影响。

## 审查与复核

一次主审聚焦本次行为、相关失败路径和契约。发现问题时给出位置、触发条件与影响；修复后的复核只覆盖原问题、修复影响和新增差异。格式、类型和机械一致性由自动检查负责，风格偏好、无关重构与猜测性扩展不阻塞。新增实质问题仍需处理，不以固定复核轮数强行通过。

PR 按可独立验证的能力拆分，避免把页面、观测、恢复和评测工具合成阶段大包。API、迁移及行为说明随代码提交；最终执行日志与审查记录可放 Actions artifact / PR 说明，必须入库的后续验收报告可用独立小 PR。PR 说明引用实际检查链接，不逐次复制全套检查清单。

可靠性与公开契约决策继续使用 ADR。收费 manifest、正式集冻结和阶段放行按对应合同执行，不作为普通改动的多轮审批模板。历史失败不改写，工程验证不替代业务验收。

## 代码与契约

遵守[编码规范](docs/code-standards.md)与 [AGENTS 不变量](AGENTS.md#事实来源与不变量)。状态转换属于 domain/service，goroutine 有明确所有者与取消/退出路径。数据库新增 migration，不改已应用历史；生成代码通过源生成；日志不记录秘密、Authorization 或完整敏感 payload。

可靠性语义、公开契约、事实源、调度、安全边界和关键依赖的决策须由 ADR 记录。新 ADR 可取代旧决策，不能静默改历史结论。新增状态/错误码/指标/API/Proto 时同 PR 同步契约、指南与相关正常/失败路径测试。

## 验证要求

统一入口和 CI 路由见[测试指南](docs/tests.md)。本地先执行 `python -m tools.check quick`，需要完整环境时用 `affected` 或 `full`；并发修改必须全仓 race，可靠性使用真实 PG，API 变更验证契约/兼容性。选择器列出的检查必须在最新 head 成功，由 `CI required` 汇总后才能合并；未选择与意外 skip 分开处理。

安装 SDK 并设置跨语言解释器；Windows 先启测试 PG/DSN，Linux 进程套件用固定镜像、`--init` 和专用环境开关。同 DSN 清理测试串行；skip、合成响应、未启动依赖不能冒充验收通过。说明已运行/失败/未运行及原因，不为文档重复云端收费。

`.sqlfluffignore` 只保存冻结历史格式基线，不把新 migration 加入 ignore。运行时/审计/故障、真实模型与观测配置各按测试指南路由，不用较低层结果替代更高层证据。

## 文档维护

简洁、清晰且必要，服务当前主线。先写结论/最短操作，再给按需细节；一个主题维护一个主要来源，入口用链接。当前状态只更新[状态页](docs/status.md)，指南讲概念和操作，过程记录放归档。报告展开影响本轮结论的范围、失败、费用和证据，避免在每页重复无关历史限制。

版本化 PRD/ADR、数据语料/fixture/gold 和机器证据保持合同/原始事实；改路径同步引用并检查锚点/Linux 大小写。不要为每个移动文件制造空跳转页，不复制两份同等权威正文。

## 安全问题

未公开漏洞或凭据按 [SECURITY.md](SECURITY.md) 私密报告，不提交到公开 Issue/PR。
