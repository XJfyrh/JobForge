# Protobuf schema root

JobForge Protobuf 契约位于此目录。当前已实现 Worker 协议：

- 路径：`jobforge/worker/v1/worker.proto`
- Package：`jobforge.worker.v1`
- 服务：`WorkerService`（Register、Poll、Heartbeat、Complete、Fail）

## 要求

- 使用 `buf format`、`buf lint` 和 `buf breaking --against '.git#branch=main'`。
- service、RPC、message、field、enum 与 enum value 均使用英文契约注释。
- 只做向后兼容新增；删除字段时 `reserved` 原编号和名称，禁止复用。

## 生成代码

生成代码输出到 Proto 源文件同目录，禁止手工编辑：

```sh
.tools/bin/buf generate
```

生成配置见仓库根目录 `buf.gen.yaml`。
