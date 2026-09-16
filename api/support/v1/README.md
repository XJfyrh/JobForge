# support-proposal-v1

本目录实现已接受 ADR-0018 §4 的闭集方案合同，对应 `support_fixed_v1`。
生产 registry 登记 Python support adapter；收费 profile 尚未启用，真实模型和40例验收另行执行。

`schema.json` 是手工维护的源 JSON Schema。顶层验证模型的恰六字段；
`$defs.persistedProposal` 验证可信代码展开后的恰八字段。旧
`bounded_readonly_v1` 与 `run.CanonicalStepResult` 仍严格接受原四字段方案。
`run.CanonicalStepResultForStrategy` 显式按策略验证；已受权 checkpoint
投影和重复提交比对使用闭集 `CanonicalRegisteredStepResult`，新提交仍由
`DecideCommit` 核对注册 profile，格式识别不授予策略或执行权限。

模型只填写 decision、action、conclusion、requested_fields、target_ticket_status、claims。
summary/evidence_refs 由固定代码生成。缺字段、未知/重复键、null、尾随JSON、
非法 UTF-8 或 surrogate、超过16KiB均拒绝。schema 的长度是字符数，解析器另外执行
UTF-8字节上限；不能把单独的 JSON Schema 验证当成完整运行时验收。

短引用 `T#/description` 等只取源schema白名单；T对应捕获的ticket对象本身，
E1/E2对应本Run返回的order/delivery evidence envelope。指针必须在实际对象中存在，
显式null可以作为缺失事实被引用。P01.1～P10.2必须确实出现在本Run检索结果中，
其`/text`指针相对于对应的policy hit对象。每个claim至少引用一个已返回政策段落。

持久claim.refs保存`{evidence_ref, source_pointer}`。事件ID字段只能引用已绑定E2中
唯一存在的事件；可信代码将对应`/delivery/events/N`追加到该claim.refs。
模型不能填写数组下标。先保留输入refs顺序，再按event ID字段顺序追加并去重；
持久refs上限为10（模型8个+最多2个事件），完整evidence_refs按首次出现顺序去重、上限32。
claim重复判断忽略refs顺序。claim的字段和排列顺序保持不变。

来源验证核对snapshot、tenant、实体、版本向量和索引，且不进行业务政策判断。
同一授权订单与物流的状态冲突会保留供`order_delivery`声明及评分使用，物流所属订单ID不匹配仍拒绝；
不能将来源检查误报为政策支持度检查。模板逐条写明`Claim asserted`，忠实保留模型判断，
不补充完成、退款、关闭或获批承诺。动作目标规则仅固定为request_information→awaiting_information、
escalate→escalated、record_conclusion/no_action→捕获时状态；它不会实际修改工单。

新图只增加一个条件：成功验证get_order明确missing时，下一步为search_policy；
存在订单时仍为get_delivery。其余步骤、一次protocol_correction、审批等待和no_action完成
沿用既有事务与观察绑定。不会加载gold、预测答案、按case ID分支或引入通用DAG。

`fixtures.json` 是 Go/Python 共同的合成来源与协议向量，包含10个有效和43个无效输入。
每项提供runtime同形snapshot、`steps[{kind,result_json}]`、原始`model_json`文本，
有效项另提供精确持久`expected`（包括summary与refs）。其中存在故意不成立的业务主张，
用于证明生产代码不会代替评分器修正结论；不能把这些向量当业务gold或质量证据。

fixture源在`internal/run/support_fixtures_test.go`及同包`support_test.go`，不要手工修改生成JSON。
更新源后执行：

```powershell
$env:JOBFORGE_UPDATE_SUPPORT_FIXTURES = '1'
go test ./internal/run -run TestSupportSharedFixtures -count=1
Remove-Item Env:JOBFORGE_UPDATE_SUPPORT_FIXTURES
go test ./internal/run ./internal/runinput ./internal/runworker
```

普通Go测试会重新计算fixture并检查文件一致性；下游Python适配必须实际消费同一文件，
校验六字段解析、来源展开及精确模板，而不是另抄一组预期结果。本切片同时交付
Go领域、Python adapter、真实PG事务及Linux进程检查，分层结果见
[验证记录](../../../docs/evidence/agent-v3-s1-support-2026-09-16.md)；合成供应商不能替代真实云端验收。
