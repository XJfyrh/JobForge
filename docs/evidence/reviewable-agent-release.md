# 可审阅 Agent 阶段版本

基线：`e2b9bad01d2d355d0b1116161c9a6c897a9cddc7`（PR #56），分支 `feat/reviewable-agent-release`。本记录只陈述本次工作，不覆盖历史冻结报告。

## 路线判断

最小定位：可恢复、受预算约束、可审计的售后 Agent，当前交付到有来源的待审方案。证据：`cmd/agent-control`、`cmd/agent-worker`、`internal/run`、`internal/runworker`、`python/jobforge_agent/support_agent.py` 及 ADR-0024；基线历史包含 S1 与 S2 实现，不能按旧 v0.6 入口判断仍是初始队列项目。

Go / PostgreSQL / gRPC 保留执行与事务责任；Python / RAG 保留业务与模型接缝；多租户、恢复、预算和审计属于安全正确性。旧 Jobs / Redis / 本地模型产物有现存入口和集成测试，作为兼容能力保留，不成为本轮扩展目标。审批写入、恢复成本量化及保留集仍未实现，本轮不引入新框架。

## 清理依据

| 类别 | 证据 | 处理 |
|---|---|---|
| 默认导航漂移 | README 演示 v0.6，产品索引缺 v0.16，v3 规划仍称 S2 未开始 | 当前范围集中在产品索引；README 聚焦 S2；详细 Jobs 内容移至独立指南 |
| Agent 上下文膨胀 | 根 AGENTS 默认要求读最早 PRD，叠加所有历史验证段落 | 短索引按任务披露；验证专题保留安全与分层边界 |
| 机械检查重复 | CONTRIBUTING、development 与 AGENTS 多份相似清单 | 人类命令集中到 verification；其余链接指向权威入口 |
| 原有 Jobs 测试 | cmd/jobforge、SDK Client、Compose 和 CI 仍消费对应实现 | 不删除；保留恢复、隔离、幂等、取消、事件保障 |
| S0 探针测试 | CI executor-process-probe 与 Python guardrails 仍显式调用 | 不是死测试；保留独立协议/生命周期边界，不冒充正式运行时验收 |
| 固定 S1 测试 | 生产 registry 仍保留 support-fixed-v1，S2 复用其方案与审计契约 | 保留；动态测试不能替代固定 profile 回归 |
| 旧规划和 PRD | 含已接受边界、取代关系和历史验收依据 | 保留 Git 与文档溯源，但移出默认阅读路径 |

唯一删除的是 `TestCancelAT25ControlStreamDegradation`：函数只有无条件 `t.Skip`，无生产调用、无断言，也不存在已实现 ControlStream；AT-25 继续明确未实现。它没有需要替换的行为覆盖，已实现 heartbeat 取消/配额回归保留。

没有证据支持批量删除测试。本轮先消除文档重复并修复实际缺口；不通过删测试获得通过。

## 本次验证

环境：Linux 6.18.44、Go 1.26.5、Python 3.12.14、Docker 28.4.0（vfs）。历史 S2 37/40 不算本次重跑。

| 检查 | 本次结果 |
|---|---|
| 完整离线 Python（SDK、业务、探针、评测） | 1774 passed；基线 1767，新增 7 项有界读取/关闭/压缩回归 |
| Go build / vet / golangci-lint | 通过；lint 0 issues |
| Ruff check / format | 通过 |
| mypy：SDK、Agent、S0、评测工具 | 通过 |
| SQLFluff 历史基线 / migrations；Buf lint | 通过 |
| `bash tools/test-linux.sh` | 新建 PostgreSQL、pgvector、AOF Redis；全量 Go race 通过，1646 pass / 77 skip 测试事件（含子测试） |
| 正式执行器进程层 | 非 root、真实进程、race、`--init --network none`：44 pass、0 fail；包含 guardian/父进程死亡、FD 与组清理 |
| Worker 协调/时钟 | 固定 Linux 容器下 Worker race 125 pass、S0 进程探针 14 pass、BOOTTIME 15 pass |
| 正式联合机制 | 真实 PG/TCP gRPC/正式 Worker/Python/合成业务 HTTP：41 pass、0 fail；动态取证、纠错、报告停发、确认丢失、launcher 停止全覆盖 |
| 文档本地链接 / diff 空白；仪表盘生成一致性 | 通过 |
| 收费模型、保留集、scale、Prometheus 镜像规则测试 | 本次未运行；模型、scale、观测配置未修改 |

服务层的 skip 保留在 JSON 日志中，不能合算为通过。专用进程层另跑；真实模型层未跑。证据日志保存在本地忽略目录 `.cache/verification/`：`python-tests.log`、`service-acceptance.log`、`linux-aCf6FKNd/go-test.jsonl`、`runtime-process-final.log`、`runtime-worker.log`、`probe.log`、`clock.log`、`runtime-integration.log`。

## 实际修复与验收中发现的问题

- Calls SDK 原先下载整个响应后才检查 256 KiB，导出也是读完后才检查 2 MiB。现在在消费解码后的流时检查，超限停止，失败路径关闭连接，不隐式重试；导出保留精确数值并避免压缩响应双重解码。分配边界不涵盖自定义 transport 自行预缓存的数据。
- Linux 服务验收不再手工指向现有 DSN：新建独立资源，重新安装当前 SDK，覆盖真实 HTTP 跨语言契约，按创建的容器 ID 清理。Redis 发布随机端口在 stop/start 后会改变，改为启动时选择空闲端口并固定绑定；竞争占用时失败，不连接现有 broker。
- 保存工作区的文件模式为 0600/0700，COPY 后非 root 无法读取 fixture；测试 Dockerfile 显式只读开放非秘密 fixture，不改变生产权限或 registry。
- 本环境没有 `/proc/<pid>/task/<tid>/children`（[Linux 6.18 的可选 PROC_CHILDREN 配置](https://github.com/torvalds/linux/blob/v6.18/fs/proc/Kconfig)），导致测试不能发现真实 guardian。两个测试辅助函数改用标准 `/proc/*/stat` 的 PPID，保留 Kill/Wait/组消失断言，不使用 skip 绕过。
- 最初 Go 缓存指向只读 home，现默认仓库缓存；测试镜像 Docker Hub 拉取遇到 429，改从公开镜像源拉取相同锁定 digest，并通过已有公共代理 CA 验证 TLS。
- Docker vfs 多阶段缓存耗尽 32GB 磁盘，影响一次服务验收。已清除本轮构建缓存并重新验收，不删除源文件、Git 或既有服务数据。受磁盘限制，最终进程容器使用相同 Python 基础 digest/安装包和宿主 Go 1.26.5 编译的 race 二进制；源路径映射到 `/src`，以非 root 运行。此结果是实际机制验收，**不是未改动 Dockerfile 在原生 CI 环境成功构建的证明**。

未扩大 API、未迁移数据库、未变更模型策略或评测标签、未引入框架。生产镜像的完整原生重建留给 CI；本轮没有远端 CI 运行或发布。

## API 预算

本次授权最多 50 CNY。未发起付费调用：实际本次花费 **0 CNY**，预留 **0**，未知费用 **0**，剩余授权 **50 CNY**。历史账本不作本次消费，也不作为可退款余额。

仅确认环境存在 `DEEPSEEK_API_KEY` 名称，未读取、打印、复制密钥。生产代码 origin 固定 `https://api.deepseek.com`，S2 样例为 `deepseek-flash`。2026-09-30 核对[官方价格页](https://api-docs.deepseek.com/quick_start/pricing/)与[token usage 说明](https://api-docs.deepseek.com/quick_start/token_usage/)：模型身份仍为 DeepSeek-V4.1-Flash，官方英文页以 USD/百万 tokens 区分峰谷及 cache hit/miss，实际 tokens 以响应 usage 为准。中文 CNY 价格页未能获取，因此没有把旧 CNY 快照或猜测汇率用于新收费调用。

本次修改不涉及模型策略/提示/检索，不为消耗预算重跑 40 案。未来付费验收须重新核实 CNY 计费与冻结 profile，并以每次调用保守上界先预留，计量不明全额 hold，累计不得超过本次授权。

## 交付边界

仅云工作区分支提交，无远端推送、PR、合并或部署。本次创建的隔离服务容器已全部清理。验收中间镜像、可再生缓存和详细日志留在本工作区供复查，不进入 Git。当前环境的低磁盘复现材料为 `.cache/verification/runtime-context/Dockerfile`、`run-runtime.sh` 及已编译 race 二进制；标准环境使用验证指南中的仓库 Dockerfile。
