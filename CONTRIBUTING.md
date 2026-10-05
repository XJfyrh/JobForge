# 贡献指南

开始前阅读[文档导航](docs/README.md)、[当前状态](docs/status.md)、相关[产品契约](docs/product/README.md)与[ADR](docs/adr/README.md)。可靠性不变量以契约为准。

## 工作流与提交

从最新 `main` 创建短分支，名称用 `XJfyrh/<type>/<short-description>`。保持提交聚焦，不混入无关格式化/重构；发起 PR，说明问题、结果、相关影响和实际验证。评审与必需检查通过后 squash merge，日常不直接推送 `main`。

提交使用 Conventional Commits：`type(scope): 简短描述`，scope 按需。type 为 `feat/fix/docs/refactor/test/perf/build/ci/chore/style/revert`，小写英文；描述默认简体中文，PR 内语言一致，尽量不超过 72 字符且末尾无句号。不兼容变更用 `!` 和 `BREAKING CHANGE:` 说明影响。

## 代码与契约

遵守[编码规范](docs/code-standards.md)与 [AGENTS 不变量](AGENTS.md#事实来源与不变量)。状态转换属于 domain/service，goroutine 有明确所有者与取消/退出路径。数据库新增 migration，不改已应用历史；生成代码通过源生成；日志不记录秘密、Authorization 或完整敏感 payload。

可靠性语义、公开契约、事实源、调度、安全边界和关键依赖的决策须由 ADR 记录。新 ADR 可取代旧决策，不能静默改历史结论。新增状态/错误码/指标/API/Proto 时同 PR 同步契约、指南与相关正常/失败路径测试。

## 验证要求

权威命令和 8 项 PR CI 清单见[测试指南](docs/tests.md)。本地执行与本次改动相关的检查；并发修改必须全仓 race，可靠性使用真实 PG，API 变更验证契约/兼容性。所有必需 CI 在最新 head 通过后才能合并。

安装 SDK 并设置跨语言解释器；Windows 先启测试 PG/DSN，Linux 进程套件用固定镜像、`--init` 和专用环境开关。同 DSN 清理测试串行；skip、合成响应、未启动依赖不能冒充验收通过。说明已运行/失败/未运行及原因，不为文档重复云端收费。

`.sqlfluffignore` 只保存冻结历史格式基线，不把新 migration 加入 ignore。运行时/审计/故障、真实模型与观测配置各按测试指南路由，不用较低层结果替代更高层证据。

## 文档维护

简洁、清晰且必要，服务当前主线。先写结论/最短操作，再给按需细节；一个主题维护一个主要来源，入口用链接。当前状态只更新[状态页](docs/status.md)，指南讲概念和操作，过程记录放归档。报告展开影响本轮结论的范围、失败、费用和证据，避免在每页重复无关历史限制。

版本化 PRD/ADR、数据语料/fixture/gold 和机器证据保持合同/原始事实；改路径同步引用并检查锚点/Linux 大小写。不要为每个移动文件制造空跳转页，不复制两份同等权威正文。

## 安全问题

未公开漏洞或凭据按 [SECURITY.md](SECURITY.md) 私密报告，不提交到公开 Issue/PR。
