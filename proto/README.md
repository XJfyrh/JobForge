# Protobuf schema root

未来的 JobForge Protobuf 契约放在此目录，首个 Worker 协议路径计划为 `jobforge/worker/v1`，package 为 `jobforge.worker.v1`。

要求：

- 使用 `buf format`、`buf lint` 和 `buf breaking --against '.git#branch=main'`。
- service、RPC、message、field、enum 与 enum value 均使用英文契约注释。
- 只做向后兼容新增；删除字段时 `reserved` 原编号和名称，禁止复用。
- 生成代码输出到独立目录，禁止手工编辑。

当前目录没有 `.proto` 文件；本文件仅建立模块根目录并记录治理要求。
