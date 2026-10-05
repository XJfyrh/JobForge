# 批准的工单记录合同

[schema.json](schema.json) 固定 ADR0026 的唯一 `apply_ticket_resolution`，请求包含不可变授权、原方案重建的参数和 Ed25519 签名。仅受信控制端持有私钥；Go Worker 的物理许可另由控制库核验。接收端使用固定 tenant/key_id 公钥配置。

对象严格闭集、UTF-8、无重复键/尾随值，不可空字段禁止 null；version_vector 中声明为 nullable 的字段允许 null。数值为 JSON 安全整数。时间为 UTC Unix 微秒。请求最多 32KiB，参数最多 16KiB，回执最多 4KiB。源 schema 复用原 support-v1 声明，身份、hash、期限、依赖版本和权限由 Go/PG 验证。只有首次记录工单结论/待补充/升级人工，不自动关闭工单。

`POST /business/v1/actions/apply_ticket_resolution` 原子提交 ticket revision/body、resolution 和首次回执。同内容重放优先返回首次回执，改内容或依赖冲突为 `ACTION_CONFLICT`。首次过期为 `ACTION_AUTHORIZATION_EXPIRED`。独立 reader 的 `GET /business/v1/actions/{operation_id}/receipt` 按 tenant 查询，无写入或 snapshot/向量/模型调用；不存在为 404，至少 30 日保留不构成 GET 截止。

指纹把 domain 与各 UTF-8 字段逐个编码为 `uint64` 大端字节长度加原字节，再 SHA-256。参数/vector/receipt 对象按 Go `encoding/json` 的 map 键顺序及默认 HTML/U+2028/U+2029 转义规范化，数组保持顺序；Ed25519 签 raw 32 字节授权摘要，签名为无 padding 的 URL base64。完整字段顺序见 [ADR0026](../../../docs/adr/0026-approval-actions-and-receipt-recovery.md)。

[fixtures.json](fixtures.json) 仅为标明的合成跨语言向量，测试公钥来自全零 seed，不能部署。Go 和 Python 独立核验编码/hash；这些向量不代表真实业务验收。
