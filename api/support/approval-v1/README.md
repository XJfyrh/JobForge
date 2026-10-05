# 单次人工审批 profile

[profile-schema.json](profile-schema.json) 仅新增 schema 4，要求 `linux-v2-approval-runtime-1`、原 `confirmed_uncommitted_v1` 与 `ticket_resolution_v1`。`action` 固定唯一操作、接收 origin、受信 key_id 和公钥 SHA-256；这些身份进入完整 profile hash。原 schema 1–3 的 hash 和执行合同不变。

模型与 Python 仍只产生原八字段 support-v1 方案及执行登记的读取步骤。独立 approver 只能批准/拒绝原方案 hash，Go 持有写入、租约和恢复权。历史 pending 方案不因状态名称获得写权限。[fixtures.json](fixtures.json) 为合成合同向量，未声称当前源码、部署或真实模型验收；正式准备必须重新绑定实际源码及审查凭证。
