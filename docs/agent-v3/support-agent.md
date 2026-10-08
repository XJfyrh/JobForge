# 有界客服 Agent：接入与方案

Agent读取工单快照，由模型选择订单、物流和政策检索工具，累计真实来源并提交结构化处理方案。Go负责Run、租约、预算、来源核验与状态转换；Python只执行登记单步。带审批能力的profile完成方案后进入`awaiting_approval`，经独立审批身份批准后，Go记录结论并更新工单标记。任务页面入口为控制服务`/ui/`，操作见[页面与运维](operations.md)。

默认Compose没有收费profile。当前能力及真实质量结果见[状态页](../status.md)，部署流程见[云端批次指南](cloud-batch.md)，审批和回执见[审批指南](approval.md)。

S5已恢复诊断与修复，实际结果和待验收项见[当前状态](../status.md)。

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

| 受审版本 | definition / 部署manifest | 用途 |
|---|---|---|
| 原有界Agent | schema 2 / manifest 1 | 只读工具选择；[v0.16](../product/JobForge_PRD_v0.16.md)与[ADR-0024](../adr/0024-bounded-support-agent.md) |
| 恢复 / 审批 | schema 3 / 4，manifest 1 | 已提交步骤复用、自然接管、独立动作许可 |
| S5初版公平对照 | Agent schema 6 / Fixed schema 5 | 相同模型、工具、数据与family上限 |
| S5历史候选v2 | Agent schema 7 / Fixed schema 8，均使用manifest 2 | `support-agent-prompt-v2`；版本选择见[ADR-0028](../adr/0028-versioned-support-evidence-navigation.md) |
| S5候选v3 | Agent schema 9 / Fixed schema 10，均使用manifest 2 | `support-agent-prompt-v3`；必要条件核对见[ADR-0030](../adr/0030-s5-policy-condition-candidate.md) |

schema 7保留原Agent完整指令、事实字段、`policy_retrieval`与`previous_tools`，补充已提交请求去重、细分政策导航、缺失段落并集、correction来源和活跃事件说明。导航只展示实际取回及尚缺段落，不提供业务答案或自动取工具。模型仍选择全部工具、主张和结论。最终响应为`{"type":"final","proposal":{...}}`，未知字段和其他外层类型拒绝。

schema 9在原输入上追加冲突、严格较后纠正、时间差与继续取证的核对提示，不计算政策真值或改变输出合同。Fixed schema 10仅增加新数据身份，原Fixed行为保留。开发通过并冻结后，正式v3须对开发及正式v1/v2均完成独立场景/模板新颖性审查。

S5两策略每个family最多12 chat、8逻辑工具、8 query embedding、44物理HTTP和1次纠错；消息64KiB、请求128KiB，另受本批持久费用/chat上限和期限约束。相同参数重复调用以`MODEL_PROTOCOL_ERROR`结束，不再次扣工具额度。unknown/full hold保留并停批，缺失usage不按零费处理。不同executor版本不能混入同一部署manifest，映射见[运行时指南](runtime.md)。

## 验证与结果

适用Python、真实PG、固定Linux进程及安装SDK检查见[测试指南](../tests.md)。源码机制fixture与真实模型质量分别报告，gold、评分器及故障工具不进入生产Worker镜像。可选outbound诊断只保存调用ID、固定校验模块和源码行号，不改变许可、计量或评分。

S5正式合同要求完整20案、方案正确至少16案、完整案例证据至少18案、四类硬失败0。完整案例要求主张成立、实际来源覆盖及必要主张齐全；合法来源ID本身不满足门槛。当前冻结候选的实际开发和正式配对结果统一在[S5本轮报告](../evidence/agent-v3-s5-real-2026-10-08.md)。历史[S2验收](../evidence/agent-v3-s2-delivery-2026-09-17.md)保留原40案合同；不拼接不同版本结果。
