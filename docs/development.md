# 本地开发

从仓库根目录执行以下命令。当前主线是 Agent v3；环境启动与检查分开，完整验证命令见[测试指南](tests.md)，阶段结果见[当前状态](status.md)。

## 工具与安装

需要 Go 1.26（以 [go.mod](../go.mod) 为准）、Python 3.12、Docker Desktop Linux 容器或 Linux Docker、PostgreSQL 16。Python 工具版本由 [requirements-lint.txt](../tools/requirements-lint.txt) 固定；golangci-lint 2.12.2、Buf 1.72.0 与 [CI](../.github/workflows/ci.yml) 一致。

Windows PowerShell：

```powershell
python -m venv .venv
.venv/Scripts/python.exe -m pip install -r tools/requirements-lint.txt
.venv/Scripts/python.exe -m pip install --no-deps ./sdk/python
```

Linux/macOS：

```sh
python3 -m venv .venv
.venv/bin/python -m pip install -r tools/requirements-lint.txt
.venv/bin/python -m pip install --no-deps ./sdk/python
```

Go/Proto 工具使用官方 release，核验 checksum 后放入 `.tools/bin`；使用显式路径，不修改系统 PATH。`.tools`、`.venv` 和本地秘密均不提交。

## 构建与最短启动

```powershell
go build ./...
docker compose -f deploy/compose.agent.yaml --profile control build control
docker compose -f deploy/compose.agent.yaml --profile control up -d --wait control-postgres
docker compose -f deploy/compose.agent.yaml --profile control run --rm control bootstrap
docker compose -f deploy/compose.agent.yaml --profile control up -d control
Invoke-RestMethod http://127.0.0.1:8093/health/ready
```

默认控制配置有租户和 Worker 身份，零模型/预算；它用于查询和开发接缝，提交未登记 profile 会失败。`bootstrap` 需要迁移权限，重复执行不清零预算。原生命令和鉴权变量见[Run 指南](agent-v3/runs.md)。

业务快照/真实 embedding 使用[业务准备](agent-v3/business.md)，正式 Python Worker 使用[固定运行时](agent-v3/runtime.md)，有界收费部署使用[批次指南](agent-v3/cloud-batch.md)。收费执行需独立登记与明确预算；复制示例不构成运行授权。

本地端口彼此独立：旧 Job/测试库 5433，业务库/HTTP/Ollama 5434/8092/11436，v3 控制库/HTTP/RPC 5435/8093/9093。停止控制组件用 `docker compose -f deploy/compose.agent.yaml --profile control stop control control-postgres`，保留数据时不删 volumes。

## 源码入口

| 路径 | 用途 |
|---|---|
| `cmd/agent-control`、`internal/run` | Run 接入、领域/存储、扫描和 RPC |
| `cmd/agent-worker`、`internal/runworker`、`internal/runexecutor` | 执行权、协议协调和实际进程 |
| `python/jobforge_agent` | 固定 guardian/step、HTTP adapter 与 Agent 策略 |
| `cmd/support-business`、`internal/business` | 独立业务事实、快照和检索 |
| `api`、`proto`、`sdk/python` | 源契约、生成代码与 Python 客户端 |
| `tools/support_evaluation`、`tools/support_recovery`、`tools/support_approval` | 离线评分、有界实验与导出 |
| `cmd/jobforge`、`internal/worker` | 既有 Job API/Handler 路径 |

SDK 开发保持在仓库根目录，编辑后重新安装 `./sdk/python`；运行测试/lint 无需 `cd sdk/python`。Proto 修改后使用 `.tools/bin/buf.exe generate`（Linux/macOS 为 `.tools/bin/buf generate`），不手改生成代码。规范见[编码要求](code-standards.md)。

## 检查与旧路径

按[测试指南](tests.md)选择定向检查和全部 8 项 CI 门禁。Windows 集成测试必须先启动测试 PostgreSQL 并设置 DSN，安装 SDK 后设置跨语言解释器；专用 Linux 进程检查、同 DSN 串行和 skip 规则也在那里维护。

为保留现有引用，本页的“CI 质量门禁”入口如下；权威清单在测试指南。

### CI 质量门禁

[8 项 PR CI 与本地命令](tests.md#ci-质量门禁)。

既有 jobs 的任务目录、Redis、运维 CLI 与观测 profile 见[旧运行配置](legacy/operations.md)；本地 Ollama 示例见[真实任务](legacy/real-tasks.md)。Windows 一键检查与时间异常排查见[运行手册](runbooks/windows-acceptance.md)。
