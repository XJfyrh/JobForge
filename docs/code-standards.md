# 代码与注释规范

本文适用于 JobForge 的 Go 服务、Python SDK、PostgreSQL SQL/migration 和 Protobuf 契约。规则以正确性、可维护性和可靠性证据为优先，不设置武断的全局覆盖率、复杂度或函数长度阈值。

## 1. 通用原则

- 先保证语义正确，再优化局部性能；性能变更必须附相同环境下的测量证据。
- 状态转换集中在 domain/service 层。HTTP、gRPC、CLI 等 transport 只处理鉴权、输入校验、调用编排和错误映射。
- 依赖方向从 transport/infrastructure 指向 domain contract，不让 domain 依赖具体数据库、网络框架或可观测性实现。
- 时间、随机数、重试策略、ID 生成和外部副作用必须可注入，以便确定性测试。
- 默认显式处理错误、超时、取消和资源释放；不得依赖进程退出清理正常路径资源。
- 日志使用结构化字段，不拼接敏感 payload。`job_id`、`tenant_id`、`attempt`、`worker_id`、`trace_id` 进入日志或 trace，但 `job_id`、`trace_id` 不作为 metrics label。
- 新依赖必须解决明确问题，说明维护、安全和可替代性成本。不得为少量简单逻辑引入大型框架。
- 生成代码不得手工修改；应修改 Proto、schema 或生成配置后重新生成。

## 2. 命名与文件组织

- 名称应表达领域含义，避免 `data`、`info`、`manager`、`util` 等缺少边界的泛化命名。
- 状态、错误码、RPC 和指标名称必须与 PRD/ADR 使用同一词汇。
- 布尔值使用可直接判断真假的名称，例如 `retryable`、`cancelRequested`，避免否定套否定。
- 单个文件聚焦一个职责；跨层共享只通过明确 contract，不建立通用“杂物包”。
- 测试文件靠近被测 Go/Python 代码；需要真实服务编排的测试放入计划中的 `tests/integration` 或 `tests/fault`。

## 3. Go

### 3.1 格式与命名

- 所有 Go 文件必须通过 `gofmt` 和 `goimports`；不手工对齐格式化器会处理的空白。
- package 名使用短小、全小写单词，不使用下划线、复数堆叠或重复上级目录含义。
- 遵循 Go initialism 惯例，例如 `ID`、`HTTP`、`RPC`、`URL`、`TTL`。
- 避免 package stutter，例如在 `worker` 包中优先 `Pool` 而不是 `WorkerPool`。
- interface 放在消费方附近，并保持最小；不要为了 mock 提前为每个 concrete type 创建 interface。

### 3.2 错误与 context

- 正常业务错误不得 panic；panic 仅用于真正不可恢复的编程不变量，并应由进程边界记录后终止或隔离。
- 错误文本以小写开头且不加句号，便于上层包装。
- 使用 `%w` 保留错误链，并通过 `errors.Is`/`errors.As` 判断；不要依赖错误字符串匹配。
- 领域错误必须稳定映射到 HTTP/gRPC 错误码，不泄露 SQL、凭据或内部堆栈。
- `context.Context` 通常作为函数第一个参数，不存入长期存活 struct，不传 `nil`。
- 调用外部服务、数据库和 RPC 必须尊重 deadline/cancel；不得用 `context.Background()` 绕过请求取消。

### 3.3 并发与资源生命周期

- 每个 goroutine 必须有明确创建者、退出条件、取消来源和等待/回收方式。
- channel 由发送方或拥有发送生命周期的一方关闭；接收方不得猜测关闭时机。
- 不使用无界 goroutine、channel 或内存队列；容量、背压和拒绝行为必须显式。
- 对 lease、fencing、Complete/Cancel 竞争等正确性关键区段，注释事务和并发不变量，并提供竞态/故障测试。
- 锁的持有范围尽量小；存在多个锁时记录并保持固定获取顺序。
- 打开的 rows、response body、timer、ticker 和 span 必须在所有路径关闭或停止。

### 3.4 测试

- 纯逻辑优先表驱动测试；测试名称描述场景和预期，而不是实现函数名的机械重复。
- 并发测试避免任意 sleep；使用 channel、barrier、可注入时钟或最终一致断言建立确定同步。
- 涉及数据库事务、锁或 migration 的测试使用真实 PostgreSQL。
- 并发相关修改必须通过 `go test -race ./...`，并覆盖至少一个取消、超时或失败路径。

## 4. Python SDK

- 使用 Ruff formatter：4 空格缩进、双引号、88 字符软限制和 LF 换行。
- import 由 Ruff 排序；禁止通配符 import，包内优先清晰的绝对 import。
- 公共模块、类、函数、方法和返回值必须提供类型标注；mypy 采用基础检查而非全局 strict。
- 不使用可变默认参数；用 `None` 后在函数体中创建新容器。
- 禁止裸 `except`；捕获最具体的异常，保留异常链，并将服务端错误映射为稳定 SDK 异常类型。
- 同步/异步边界必须明确，不在库代码中隐式启动事件循环或吞掉取消。
- SDK 不复制服务端调度、重试或租户判定逻辑；它只负责契约、超时、认证、错误映射和 trace 透传。

## 5. PostgreSQL SQL 与 migration

- SQL 使用 PostgreSQL 方言；关键字、函数和未加引号标识符使用小写，标识符使用 `snake_case`。
- 查询显式列出字段，禁止生产查询使用 `select *`。
- 所有动态值使用驱动参数绑定；禁止把用户输入或 payload 拼接进 SQL。
- 事务保持短小，外部网络调用不得位于数据库事务内。
- 锁顺序、`skip locked`、隔离级别和重试条件必须在查询附近说明原因与不变量。
- 索引必须对应已说明的查询模式，并用真实数据形态的 `explain (analyze, buffers)` 验证。
- migration 只新增、不改写已应用版本；文件头说明目的、预计锁行为、数据风险、回滚/前滚策略和验证方式。
- 高风险 DDL 或数据回填必须分阶段设计，避免在单个长事务中阻塞核心队列。

## 6. Protobuf 与 API

- Proto package 带显式版本，例如 `jobforge.worker.v1`；文件和字段用 `lower_snake_case`，message/service/RPC 用 `PascalCase`。
- 只做向后兼容新增。删除字段或 enum value 时同时 `reserved` 原编号和名称，绝不复用。
- RPC 必须传递 deadline；重复 Complete/Fail 必须保持幂等，陈旧 fencing token 返回稳定错误。
- 不随意改变字段语义、默认行为或 JSON 映射；无法兼容的变更通过新版本 package 发布。
- HTTP 与 gRPC 的领域错误保持一致映射，并用契约测试锁定。
- 生成代码不进入人工风格评审，但其源 Proto 和生成配置必须接受评审和兼容性检查。

## 7. 注释与 docstring

源码注释统一使用英文；中文主要用于 README、PRD、ADR 和面向人的工程文档。

- 注释解释 **why、invariant、trade-off 和 failure mode**，不逐行复述代码。
- Go 导出标识符的 doc comment 以标识符名称开头；复杂 internal package 也应有 package comment。
- Python 公共 API 使用 Google 风格 docstring，按需包含 `Args`、`Returns`、`Raises` 和语义/线程安全说明。
- Proto 的 service、RPC、message、field、enum 和 enum value 都写契约注释，说明单位、边界、可选性和安全含义。
- migration 注释必须包含目的、锁/数据风险、回滚或前滚策略和验证步骤。
- 并发所有权、事务边界、状态竞争、幂等假设和故障恢复条件必须记录在最接近实现的位置。
- TODO 使用 `TODO(#123): explain the required follow-up.`；尚无远程 Issue 时允许 `TODO(XJfyrh): ...`。
- 禁止无负责人/Issue 的裸 `TODO`，禁止把长期设计讨论隐藏在 `FIXME` 中。
- lint/type 抑制必须指定规则并附英文理由，例如 `//nolint:rule // reason` 或 `# noqa: CODE -- reason`，范围限制到单行或最小块。
- 注释、示例和测试数据不得包含 API key、Authorization header、私钥、个人信息或完整敏感 payload。
- 生成文件只保留生成器写入的注释，不手工补充。

## 8. 评审清单

- 命名是否与 PRD/ADR 的领域词汇一致？
- 是否明确处理错误、deadline、取消和资源释放？
- 是否维护 at-least-once、幂等、lease 与 fencing 不变量？
- 是否引入无界并发、长事务、高基数指标或敏感日志？
- 关键注释是否说明原因和失败语义，而非复述代码？
- 正常路径和至少一个相关失败路径是否有自动化测试？
- 公开契约、migration、指标、错误码和文档是否同步？
