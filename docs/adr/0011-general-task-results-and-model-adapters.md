# ADR-0011：通用结果引用与预注册模型业务适配器

- 状态：Proposed（当前分支为维护者已授权范围内的实现候选，PR 评审后转 Accepted）
- 日期：2026-09-14
- 关联：PRD v0.6；补充 ADR-0002/0009/0010，不改写历史结论

## 上下文与决策依据

现有 Complete 参数已声明 result_ref 但存储忽略；Python 错误 envelope 与 ADR-0002 不一致；PageWise 示例仅模拟 sleep。维护者本轮明确要求两个真实通用任务并移除 PageWise。本候选先补契约，保留 PostgreSQL 队列和现有可靠性语义。

## 决策

1. 新增 0020 `jobs.result_ref text null`，约束空值、UTF-8、2048 字节上限和无控制字符。领域校验与数据库约束一致；历史 NULL 无需回填。HTTP get/list 增加 nullable 字段，SDK 对旧响应缺失字段兼容。绝不自动解引用、暴露业务凭据或用引用替代存储鉴权。
2. Complete/Fail 事务返回内部 `AttemptResult`（queue/type/state/outcome/duration/run_at/changed），供 Gateway 响应及观测使用，避免提交后另查 state 的竞争。保留原存储方法包装器；公开 Proto 方法与既有字段保持。重复 ACK 必须匹配当前 fencing token 和 attempt 的 owner、outcome，不能仅按终态吞错；已恢复/重新领取后旧 token 始终 STALE_LEASE。Complete 首次结果不覆盖。
3. SDK 映射完整 ADR-0002 错误，补 `CANCEL_REQUESTED` / 已有 `INVALID_TRANSITION` 类型；后者 HTTP 409。未知 code、非 JSON、畸形 envelope 和无效成功响应归 `InternalError`，不把原始异常响应内容带入异常。网络异常/超时独立稳定类型；不隐式重试。可选 W3C traceparent/当前 OTel context 传播。
4. Go Runtime 是唯一租约持有者。两个预注册 Go Handler 调用固定配置的 Ollama HTTP API；模型后端不领取任务、不持有 lease、不写 jobs。无 Python subprocess 或第二套调度。Worker 退出断开 HTTP，已开始的后端计算可能继续，恢复可重复计算。
5. 业务包独立于 domain/store；业务产物存储按 tenant + type + business key 唯一，内容/版本指纹判冲突。采用独立业务 schema/table 的 PostgreSQL 原子发布 JSON 产物（≤2 MiB），不在 jobs 中放向量和抽取内容；无 jobs 外键。仅为可本地复现的业务存储示例，可替换为业务自身支持幂等的存储。独立业务 HTTP 查询服务按同一 API key→tenant 映射鉴权；核心 API 只返回引用。
6. `ClaimedJob` Proto 向后兼容新增 tenant_id，使可信 Worker 适配器能够按实际 tenant 发布产物。不得从 payload 接受 tenant。Gateway 依旧只部署可信网络；本轮不新增身份系统。
7. 固定语料、Schema、算法与模型 manifest digest；配置可指定可信远程 Ollama 兼容 endpoint，模型请求无任意 tools。HTTP 输出有界、重定向关闭、context/超时传播；持久化前再次检查取消。业务发布与 Complete 不跨系统原子，因此以持久业务键覆盖 ACK 前崩溃窗口。人工 retry 保留业务键，共享首次产物；更换业务键/版本才是新效果。

## 替代方案与取舍

- Python Worker / subprocess：当前两能力通过 HTTP 足够，新增生命周期和取消语义无收益，不采用。
- 在 jobs 写完整产物：扩大队列热行、安全和容量边界，不采用。
- 文件系统去重：多进程/跨平台原子发布、访问控制和回滚更难复现；小型验收选择独立业务表。业务表不成为任务事实源。
- 外部向量库/Agent 框架：当前固定小语料不需要，模型调用及小矩阵检索即可验证真实能力。
- 只查当前 state 吸收重复 RPC：会把旧 Worker 伪装成成功，拒绝。

## 兼容性与验证

实施复核：0020 使用的 POSIX 控制字符类随数据库 locale 变化，会额外拒绝 C1。新增 0022 改为显式 U+0001～001F / U+007F 范围，与既有领域 C0/DEL 契约一致（NUL 由领域和 PostgreSQL text 自身拒绝）；不改写已应用的 0020。0022 down 恢复旧约束，有新 C1 引用时可能拒绝回退，必须审查引用后再决定，不自动改写业务元数据。

公开接口只增加 result_ref 和 tenant_id；旧 Worker 不传结果仍成功，旧 SDK 忽略新增字段。PageWise 删除由本轮明确要求覆盖，不设兼容期。新 migration 只追加、不改历史。AT-32～39 覆盖回滚、同租户查询、旧 token/取消/重复 ACK、真实 HTTP SDK、真实模型、进程 kill、业务唯一发布；Buf breaking、race 与定向性能比较必跑。PR 评审状态与实际验收状态分别记录，不能以本 ADR 的草案状态声称验收已完成。
