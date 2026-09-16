# 内部执行器 v2 源合同

[schema.json](schema.json)与[共同 fixture](fixtures/frames.json)定义严格普通通道和独立计量通道。依据为[ADR-0019](../../../docs/adr/0019-executor-confirmation-and-exit-contract.md)及[PRD v0.11](../../../docs/product/JobForge_PRD_v0.11.md)；本次是尚未正式部署的内部 v2 协调修订，旧无观察 ACK 序列明确不兼容。部署必须使用新的不可变 `executor_version` / profile hash 和匹配的固定 Go/Python 镜像，不能混用旧实现或启用兼容模式。v1 及公开 HTTP/SDK/gRPC 合同不变。

普通 `call_observation_ack` 恰含 `version`、`kind`、`request_id`、完整 `binding`、`emitted_mono_ms`、`call_sequence`、`physical_call_id`、`observation_hash`。它只确认原 observation，不含结果、许可、输入、错误或期限增量。Go 只能在原 `ObserveCall` 明确成功、返回调用/参数/工具身份一致且执行权有效后发送一次 ACK。免费、unknown、rejected 和最后一次调用都需要 ACK。

reported observation 还必须匹配标准 metering report 和首次 settled ACK。Go 按 SettleUsage→ObserveCall 顺序确认；两个 FD 的接收顺序可相反。普通 ACK 提前到达时，只保存当前调用的一份有界、不可变 pending ACK，等待相同调用/hash 的 settled ACK；普通 ACK 的 emitted 时间不得早于 observation 或首次 settled ACK。两种确认汇合前不能发新 intent/result。unknown 不伪造或等待计量结算，完整 hold 保留。

ACK 接收及 pending 汇合均检查原 call/step 截止。汇合后下一 intent/result 的实际接收也必须早于上一 call 截止，且 emitted 时间不早于两种确认；下一 intent 已及时接受后，其新 permit 使用自己的 call 截止和原 step 截止。任何 ACK 均不续期。错身份/hash、重复普通 ACK、坏时钟、逆因果、停止或过期关闭普通执行；之后的计量确认不能复活。计量帧的原调用幂等确认不等于普通 ACK 可重复使用。

`observation_hash` 与控制账本 `ObserveCallRequest.Hash()` 相同。对以下每个 UTF-8 字符串分别写入其 **uint64 大端字节长度**和原字节，再求 SHA256 小写十六进制：

1. `jobforge.run.observation.v1`
2. `transport_outcome`
3. 十进制 `http_status`
4. 映射后的领域错误码
5. `business_outcome`
6. `usage_hash`，unknown 使用空串

生成 RPC 和 hash 前统一映射 `OUTPUT_INVALID→MODEL_PROTOCOL_ERROR`、`INPUT_INVALID→INVALID_ARGUMENT`、`PROTOCOL_ERROR→EXECUTOR_PROTOCOL_ERROR`。空串、`TIMEOUT`、`DEPENDENCY_UNAVAILABLE`、`PROFILE_UNAVAILABLE`、`BUDGET_EXHAUSTED` 保持原值。`STOP_REQUESTED`、`STALE_LEASE`、`CALL_CONFLICT` 是控制拒绝，禁止转成业务 observation。纠正资格仍检查原 wire `OUTPUT_INVALID` 和严格 marker。

共同 fixture 的 `observation_hash_cases` 每项包含 `name`、完整原 wire `frame`、`domain_error`、`hash`、`accept`。`accept=false` 时后两项为空串，表示原帧必须在严格解码或 hash 验证时拒绝；不是空错误/空 hash 的合法观察。合法 reported 向量引用共同 metering report 的完整 usage hash，供 Go 同时核对实际领域请求 hash，Python 核对同一字节合同。

fixture 保留原 11 个 `valid_frames` 索引和原 49 个会话名称。原成功序列加入 ACK；原失败序列在可接受 observation 后补上 ACK/pending ACK，以便继续命中原计量、拒绝或时钟失败。`legacy_without_observation_ack_*` 保留旧顺序并明确拒绝。样例矩阵包括：

| 范围 | 场景 |
|---|---|
| ACK 严格形状 | 必填字段、null、safe integer、hash/UUID、未知字段、重复 key、禁止放入计量通道 |
| 成功与兼容拒绝 | reported 的两种 ACK 接收顺序、ACK 早于 report、免费/unknown 最后调用、旧无 ACK 结果与下一 intent |
| 身份与内容 | request/call/sequence/hash，以及完整 binding 的每个字段分别错配；同一 hash 不代替身份绑定 |
| 因果与期限 | 普通 ACK 早于 observation/settled、未来/倒退时钟、pending 与已汇合阶段的原截止、后续 emitted 下界 |
| 停止与计量 | pending/已结算后 STOP、重复 ACK、anomaly/unconfirmed、迟到 settled、停止后原计量可核算但不能复活 |
| 错误和 hash | 三个固定映射、其余允许码、reported/unknown、三个控制码和非法 wire shape 拒绝 |

Go 与 Python 测试实际消费相同 JSON。通过这些确定性检查只说明 codec/会话/hash 合同一致，不能证明 PostgreSQL 持久确认、真实双向 FD/进程清理或真实云端验收。后续正式 supervisor/Worker 仍须按 PRD 单独提供相应证据。
