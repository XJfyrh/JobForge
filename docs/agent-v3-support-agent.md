# S2 有界客服 Agent

契约为[PRD v0.16](product/JobForge_PRD_v0.16.md)与[ADR-0024](adr/0024-bounded-support-agent.md)。`support_agent_v1`/`support-agent-v1`使用独立`linux-v2-agent-runtime-1`，S1历史profile与结果保留。模型可在读取订单、物流、不同政策检索之间选择下一步，最后提交既有结构化方案；Go保有调度、预算、来源和状态转换的决定权。

## 部署与预算

沿用[云端批次运行指南](agent-v3-cloud-batch.md)的现有服务、只读业务角色、独立控制库、prepare/bootstrap/inspect/launcher与SDK导出流程。新source从[动态策略样例](../deploy/support-agent.source.example.json)复制，definition.schema_version=2；模型/费率核对日期、prompt、decision schema、完整Python包摘要和实际构建receipt必须对应本次部署。prompt摘要为`python/jobforge_agent/support_agent.py`整文件；其依赖的固定方案说明也被完整包摘要绑定。审查、gold与评分器不进入Worker镜像。

S2最初新增累计操作预算20 CNY，已获合理调整授权；启动记录按每个已启动S2 batch的known+held扣减一次。`--batch-cost-microyuan`对S2接受正安全整数；每批仍冻结具体有限cap，不按新批自动重置累计授权。S1准备器保留原5 CNY边界。相同manifest内不混合两种执行器版本。模型身份与原费率兼容性仍在每次真实调用中审计。

模型最多12 chat、8逻辑工具、8 query embedding和1次全Run纠错。S2消息64 KiB、请求128 KiB，其余输出/响应/checkpoint/时间上限不变。相同参数重复调用保留决定并以MODEL_PROTOCOL_ERROR结束，在安全日志写固定reason=repeated_tool_call，不再扣工具额度。错误的结构或来源最多纠错一次；完整审计的终态错误计入40案分母并允许继续下一案。unknown/full hold保留且停止当前批次；不把缺失usage当零费。

## 验证与验收

`python -m pytest python/tests tools/support_evaluation`覆盖共同决定、来源合并、拒绝路径和动态证据导出，业务gold与谓词不变。Windows平台skip不计Linux验收。固定镜像integration-check加入`TestRunSupportAgentExecutor`，真实Go Worker、Python进程、gRPC和PostgreSQL验证动态路径、跨检索、全局纠错和重复工具无扣额；供应商响应仅为机制fixture。

正式质量验收是冻结版本的完整40个开发案例，至少32个业务正确、安全硬失败0；不读取保留集、不拼接不同版本结果。另以隔离批次对真实云端响应注入一次截断，验证未知费用查询与停止；它不是供应商原生故障。源码能力、机制通过、真实验收分别记录，不互相代替。

可选 outbound 挂载还保存 `.model-rejection.json`：只有 physical call ID、固定校验模块及源码行号，按本批冻结源码摘要定位；不保存异常文字、模型内容或引用值。该诊断不参与成功判定、授权或费用结算。

## 使用结果

准备时仍按云端指南登记新 profile、有限 batch 与专用 Worker；S2 的 source 选择 `deploy/support-agent.source.example.json`，镜像可标记为 `jobforge-support-cloud:s2`，Compose 覆盖该 image。`prepare-support --batch-cost-microyuan <本次剩余上限>` 冻结费用上限；启动后不改 profile，也不重新启动 attempted 批次。准备不发模型请求，`support-cloud` 启动才执行已登记任务。

SDK 接口不变。下面在部署已启用、凭据和实际身份通过环境提供后提交一案，再查询持久结果；查询本身不触发推理：

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

`steps` 按分页读取完整历史；模型决定在 `model_decision` 的已提交结果中，随后才有对应真实工具步骤。`awaiting_approval` 表示可审阅的方案，不表示工单解决或业务写入。Run 达到原 deadline 后可能转成失败；原完成时的 SDK 导出是本次评分依据，后续查询只追加状态，不覆盖历史。

提示中的 `policy_retrieval` 只列出公开业务政策主题、建议搜索词和实际已取得的段落别名。某主题非空不代表每个主张已有充分依据；模型仍须选择工具、解释政策、提出主张，Go/Python 仍拒绝未取得来源。时间辅助只计算已接受时间戳之间的秒差，不给出业务答案。
