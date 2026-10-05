# Protobuf 源契约

| 服务 | 源文件 | 用途 |
|---|---|---|
| `jobforge.agent.v1.AgentService` | [agent.proto](jobforge/agent/v1/agent.proto) | 当前 Run Worker 的执行身份、步骤、工具/物理调用、审计确认 |
| `jobforge.worker.v1.WorkerService` | [worker.proto](jobforge/worker/v1/worker.proto) | 既有 Job Worker Register/Poll/Heartbeat/Complete/Fail |

只做兼容新增；删除字段保留 `reserved` 编号/名称，不复用。service/RPC/message/field/enum 注释使用英文。生成代码输出到源文件同目录，禁止手改。

在仓库根目录执行（Windows 使用对应 `.exe`）：

```sh
.tools/bin/buf format
.tools/bin/buf lint
.tools/bin/buf breaking --against '.git#branch=main'
.tools/bin/buf generate
```

配置见 [buf.gen.yaml](../buf.gen.yaml)，运行与验证见[测试指南](../docs/tests.md)。
