# 有界客服 Agent：接入与方案

Agent读取工单快照，由模型选择订单、物流和政策检索工具，累计真实来源并提交结构化处理方案。Go负责Run、租约、预算、来源核验与状态转换；Python只执行登记单步。带审批能力的profile完成方案后进入`awaiting_approval`，经独立审批身份批准后，Go记录结论并更新工单标记。任务页面入口为控制服务`/ui/`，操作见[页面与运维](operations.md)。

默认Compose没有收费profile。当前能力及真实质量结果见[状态页](../status.md)，部署流程见[云端批次指南](cloud-batch.md)，审批和回执见[审批指南](approval.md)。

## 接入顺序

1. 核对业务HTTP、只读角色、政策索引及readiness；新环境记录实际索引UUID和摘要，保留环境不重复seed/index。
2. 选择受审策略版本，以`prepare-support`生成不可变profile、有限batch、专用Worker身份和部署manifest。source、模型/费率、完整Python包摘要与实际构建receipt必须一致。
3. disabled bootstrap后做实际inspect，登记完整案例名单并审查源码、预算、期限和凭据。准备和只读查询不触发推理。
4. 启用匹配profile的控制服务，只启动一次有限launcher。每批结束核对known+held、在途许可和实际进程回收；新库、身份或batch不重置累计授权。
5. 查询方案、步骤、来源与费用。需要动作的方案由独立approver审阅批准；实际工单结果通过独立effect和业务回执核对。

SDK每个方法只发一次HTTP，没有自动重试或后台工作。凭据和实际身份从部署环境提供：

```python
import os
from jobforge import RunClient

with RunClient(os.environ["JOBFORGE_RUN_URL"], os.environ["JOBFORGE_RUN_TOKEN"]) as client:
    submitted = client.submit(
        ticket_id=os.environ["JOBFORGE_TICKET_ID"],
        business_request_key=os.environ["JOBFORGE_BUSINESS_REQUEST_KEY"],
        profile_id=os.environ["JOBFORGE_PROFILE_ID"],
        budget_batch_id=os.environ["JOBFORGE_BUDGET_BATCH_ID"],
        idempotency_key=os.environ["JOBFORGE_SUBMIT_KEY"],
    )
    run = client.get(submitted.run.run_id)
    result = client.result(run.run_id)
    steps = client.steps(run.run_id)
    calls = client.calls(run.run_id)
    print(run.run_id, run.state, result.available, len(steps.items), len(calls.items))
```

`steps`须分页读取完整历史；模型决定提交后才执行对应真实工具。`awaiting_approval`表示方案可审阅，批准后的`applied`表示结论/标记已提交。无需动作可直接成功。Run达到原deadline后可能失败；原完成时SDK导出保留，后续状态只追加，不覆盖历史评分。

## 版本与执行边界

当前正式验收使用以下两种 profile。完整历史版本与 executor/manifest 映射在[运行时版本表](runtime.md)维护。

| 流程 | definition / 部署 manifest | 提示与用途 |
|---|---|---|
| Agent V9 | schema 18 / manifest 2 | `support-agent-prompt-v9`；动态调查、独立终裁、恢复与人工审批 |
| Fixed 对照 | schema 17 / manifest 2 | `support-fixed-prompt-v2`；固定取证图与相同参数终裁 |

两者在 `read_ticket` 后先读取快照绑定的全部20段政策，再开始模型调用；政策包计一次逻辑工具与一次业务HTTP。Agent在调查中选择只读工具、累计来源，并提交 `investigation_complete` 标记；Go随后安排一次final-only终裁。最终响应为 `{"type":"final","proposal":{...}}`，通过校验后才形成可审阅方案。V9明确要求主张完整，并为时间类主张提供自身证据。

完整政策前置读取见[ADR-0038](../adr/0038-policy-bundle-before-adjudication.md)，V9输出约束的形成与验证见[开发报告](../evidence/agent-v3-s5-v9-development-2026-10-15.md)。Fixed与Agent的工具调度和提示不同，配对结果按两种流程报告。

S5两策略每个family最多12 chat、8逻辑工具、8 query embedding、44物理HTTP和1次纠错；消息64KiB、请求128KiB，另受本批持久费用/chat上限和期限约束。相同参数重复调用以`MODEL_PROTOCOL_ERROR`结束，不再次扣工具额度。unknown/full hold保留并停批，缺失usage不按零费处理。不同executor版本不能混入同一部署manifest，映射见[运行时指南](runtime.md)。

## 验证与结果

适用Python、真实PG、固定Linux进程及安装SDK检查见[测试指南](../tests.md)。源码机制fixture与真实模型质量分别报告，gold、评分器及故障工具不进入生产Worker镜像。可选outbound诊断只保存调用ID、固定校验模块和源码行号，不改变许可、计量或评分。

S5正式合同要求完整20案、方案正确至少16案、完整案例证据至少18案、四类硬失败0。完整案例要求主张成立、实际来源覆盖及必要主张齐全；合法来源ID本身不满足门槛。V9正式结果为正确18/20、完整证据18/20，Fixed各11/20；逐案结果见[未见正式配对](../evidence/agent-v3-s5-v9-formal-v4-2026-10-16.md)，恢复与审批演示见[S5最终报告](../evidence/agent-v3-s5-closeout-2026-10-16.md)。历史[S2验收](../evidence/agent-v3-s2-delivery-2026-09-17.md)保留原40案合同；不拼接不同版本结果。
