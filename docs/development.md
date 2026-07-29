# 开发环境与本地检查

仓库当前只有文档和检查配置，没有产品代码、`go.mod` 或 Python package metadata。本页记录首次初始化实际验证过的工具；Go/Python 产品最低版本将在代码骨架确定时另行固定。

## 已验证环境

| 工具 | 版本 | 安装位置/来源 |
|---|---:|---|
| Go | 1.26.5 | 本机环境，仅作为首次验证记录 |
| Python | 3.12.10 | 本机环境，仅作为首次验证记录 |
| golangci-lint | 2.12.2 | `.tools/bin`，官方 Windows amd64 发布包 |
| Buf | 1.72.0 | `.tools/bin`，官方 Windows amd64 发布包 |
| Ruff | 0.16.0 | `.venv`，由 `tools/requirements-lint.txt` 锁定 |
| mypy | 2.3.0 | `.venv`，由 `tools/requirements-lint.txt` 锁定 |
| SQLFluff | 4.2.2 | `.venv`，由 `tools/requirements-lint.txt` 锁定 |

`.venv` 和 `.tools` 都被 Git 忽略。本地安装不会写入系统 PATH，也不应被提交。

## Python 检查工具

Windows PowerShell：

```powershell
python -m venv .venv
.\.venv\Scripts\python.exe -m pip install --requirement .\tools\requirements-lint.txt
```

macOS/Linux：

```sh
python3 -m venv .venv
.venv/bin/python -m pip install --requirement tools/requirements-lint.txt
```

升级工具时先确认新版本支持项目 Python 基线，再更新 requirements、本文版本表和配置，且在同一 Pull Request 中运行完整检查。

## Go 与 Proto 检查工具

golangci-lint 和 Buf 使用官方 release 二进制。下载与当前平台匹配的稳定版本，核对发布页 checksum 后放入 `.tools/bin`。不要把该目录加入全局 PATH；从仓库根目录显式调用。

Windows：

```powershell
.\.tools\bin\golangci-lint.exe config verify
.\.tools\bin\buf.exe lint
```

macOS/Linux：

```sh
.tools/bin/golangci-lint config verify
.tools/bin/buf lint
```

## 常用检查

产品目录存在后，提交前按改动范围运行：

```text
go test -race ./...
go vet ./...
.tools/bin/golangci-lint run
.venv/bin/ruff check .
.venv/bin/ruff format --check .
.venv/bin/mypy sdk/python
.venv/bin/sqlfluff lint migrations
.tools/bin/buf lint
.tools/bin/buf breaking --against '.git#branch=main'
```

Windows 将 `.venv/bin` 替换为 `.venv\Scripts`，将无扩展名工具替换为 `.exe`。目录尚未创建时应标记检查“未适用”，不能把“没有输入文件”报告成产品代码通过。

## 测试分层

- 单元测试：状态转换、错误分类、退避、配额和 Handler 生命周期。
- 数据库集成测试：使用真实 PostgreSQL 验证 claim、事务、租约、幂等与 outbox。
- 契约测试：验证 HTTP/gRPC 错误映射、deadline、重复提交和 Proto 兼容性。
- 故障测试：kill Worker/Scheduler、阻断 heartbeat、ACK 前崩溃和陈旧写入。
- 性能测试：固定环境后报告吞吐、延迟、CPU、heap 与 goroutine 稳态。

## 配置来源

- golangci-lint：<https://golangci-lint.run/docs/configuration/file/>
- Ruff：<https://docs.astral.sh/ruff/configuration/>
- mypy：<https://mypy.readthedocs.io/en/stable/config_file.html>
- SQLFluff：<https://docs.sqlfluff.com/en/latest/configuration/setting_configuration.html>
- Buf：<https://buf.build/docs/configuration/v2/buf-yaml/>
