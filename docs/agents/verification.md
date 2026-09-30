# Agent 验证专题

仅在修改对应领域时读取。人类验证入口见[验证指南](../verification.md)，实际 CI 以[工作流](../../.github/workflows/ci.yml)为准。


- 运行与修改范围相称的格式、lint、单元、集成、故障和 race 检查。
- Windows 上运行集成测试（`go test ./tests/integration/...`）前，必须先执行 `docker compose -f deploy/compose.yaml up -d postgres` 并设置 `JOBFORGE_TEST_DSN=postgres://jobforge:jobforge@localhost:5433/jobforge?sslmode=disable`（testcontainers 不支持 Windows 上的 Docker Desktop rootless/WSL2 后端；Linux CI 无需该变量，testcontainers 会自动启动临时 PostgreSQL）。
- 其中机械检查子集（golangci-lint、`go test -race ./...`、ruff、mypy、SQLFluff 历史基线校验、`sqlfluff lint migrations`、buf lint）由 [.github/workflows/ci.yml](../../.github/workflows/ci.yml) 在每个 Pull Request 上强制执行，检查项清单与 [CONTRIBUTING.md](../../CONTRIBUTING.md) 验证要求保持一致，详见 [docs/development.md](../development.md) 的 “CI 质量门禁” 一节。
- Python SDK 单测与真实 HTTP 跨语言契约也属于 CI 门禁；安装 SDK 后设置 `JOBFORGE_TEST_PYTHON`，否则 Go 契约测试的 skip 不计通过。
- 可观测配置变更还需通过 promtool 配置/告警规则测试，以及 Grafana 仪表盘生成一致性检查；这些检查在 PR CI 中执行。依赖真实模型的验收在独立工作流与本地真实模型层运行，不用替身替代。
- Agent v3 S0 探针的确定性 Python guardrails、Linux 类型检查和独立容器内 Go race/真实进程故障均由 CI 执行；常规 Go 测试因未启用专用进程环境产生的 skip 不计验收通过。模型协议探针使用的工具 fixture 不代表真实业务工具已验收。
- S1-C1 v2共同fixture随Go/Python测试执行；Go/Python共享CLOCK_BOOTTIME由同一固定Linux镜像内的`clock.test`实际验证。Windows非Linux分支或协议fixture不能替代正式Worker进程/真实云端验收。
- S1-C2授权HTTP适配随`python/tests`进入Linux CI；Windows本地使用`tools/agenthttpcheck/Dockerfile`验证实际BOOTTIME和回环TCP。协调者、向量和供应商响应是测试替身，不能据此宣称持久IPC或真实云端通过。
- S1-C3b运行时使用`tools/agentruntimecheck/Dockerfile`的`process-check`与`integration-check`，由`agent-runtime-contract` CI job运行真实Linux进程/race及真实PG/gRPC联合检查；Python全套仍由`python-lint`执行。Windows必须运行相同Linux镜像并带`--init`；缺少`JOBFORGE_RUNEXECUTOR_PROCESS_TESTS`或`JOBFORGE_RUNEXECUTOR_INTEGRATION_TESTS`导致的skip不计通过。联合测试先启动本地postgres并设置DSN，同一DSN只允许一个可能清理数据库的测试进程。
- 生产registry仅登记support-fixed-v1与support-agent-v1；合成adapter与固定回环供应商origin仅在专用测试target构建时安装，不得进入生产Dockerfile或通过运行时开关启用。正式输入/manifest/全部profile必须匹配固定executor_version；Go保留唯一执行权，ACK/Wait/EOF/Join/组消失屏障不可用替身成功信号代替。support共同schema/fixture与离线开发数据锚校验由CI执行；注册能力不等于收费profile启用或真实云端验收。复现与分层边界见[运行时指南](../agent-v3-runtime.md)。
- 不得把缺少代码、服务或依赖误报为检查通过；明确区分已运行、未适用和无法运行。
- ADR-0020的审计版本还需通过共同audit/report/observation向量、真实PG首报告/冲突/冻结/晚到/批次屏障、安装SDK的Calls真实HTTP契约，以及固定Linux的`TestRunProviderAuditExecutor`确认丢失/停发检查。observed usage不等于已结算费用，unknown/full hold不能记成零费；生产长期留存与真实云端验收仍须单列。当前合同见[审计指南](../agent-v3-provider-audit.md)。
- 提交前检查 staged diff、敏感信息、迁移安全和文档链接。
