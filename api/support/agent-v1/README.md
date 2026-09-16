# support-agent-decision-v1

本目录实现 [ADR-0024](../../../docs/adr/0024-bounded-support-agent.md) 的闭合模型决定。`schema.json` 仅校验形状；Go/Python同时验证字节数、快照绑定、来源和重复JSON键。共同工具向量在两种语言测试中执行。

模型返回单个 `tool` 或 `final`；工具只允许 get_order/get_delivery（捕获的order_id，含null）和search_policy（1～512 UTF-8字节query）。首尾ASCII空格/tab/CR/LF规范化；同名同参数决定可持久查询，但Go在BeginTool扣额之前拒绝重复派发。最终方案使用既有[六字段合同](../v1/README.md)，不是新的业务评分标准。

`model_decision` 是追加的步骤枚举；内容存入现有StepResult.content，最终方案存入proposal。Go计算唯一后继，Python仅执行一个被授权步骤。最多一次全Run纠错，多次检索只累积实际返回且身份/正文一致的政策段落。
