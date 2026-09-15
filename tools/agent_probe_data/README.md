# S0 模型协议探针数据

本目录全部是合成数据。`cases.json` 只验证真实本地模型能否遵守原生工具协议、利用工具返回的事实、给出结构化结论以及忽略工具结果中的越权指令。

`get_order`、`get_delivery`、`search_policy` 的返回值由探针内存中的确定性 fixture 提供，没有业务 HTTP 服务、PostgreSQL 或向量检索。检索 query 不会执行真实搜索。它们不能替代路线 v3 的 S1/S2 业务验收、60 案例质量评测或真实 RAG 验收。

`expected_decision` 仅供探针评分，不发送给模型。三个案例分别要求延迟交付升级、已送达仅记录、包含恶意工具注释时仍根据事实升级。模型收到同一工具目录，但每次必须自己产生一个原生工具调用，宿主校验整条调用后才能返回 fixture；不会执行模型提出的任意工具。

另有两个明确注入的无效 assistant 消息：未知写工具和非字符串订单参数。探针先拒绝，再向真实模型提供一次纠正机会。这证明的是“对人工构造拒绝反馈的协议反应”，不声称模型在自然运行中曾产生这些错误。

```powershell
.venv/Scripts/python.exe -m pytest -q tools/agent_probe_data/test_agent_model_probe.py
.venv/Scripts/ruff.exe check tools/agent_model_probe.py tools/agent_probe_data
.venv/Scripts/ruff.exe format --check tools/agent_model_probe.py tools/agent_probe_data
.venv/Scripts/python.exe -m mypy tools/agent_model_probe.py
```

测试使用 `httpx.MockTransport` 验证宿主参数/权限、响应大小与真实墙钟超时边界，属于确定性测试层。真实模型命令与失败记录见[模型试验记录](../../docs/evidence/agent-v3-model-probe-2026-09-16.md)。

审查修复后本目录共 27 项确定性测试，另覆盖所有推理阶段完成未知时先落盘并停止、响应未完成与已完成坏输出的区别，以及可选 `/api/ps` 失败不能抹除证据。修正版没有重新调用模型；历史模型结果和当时源码 hash 保持不变，见[版本与验证说明](../../docs/evidence/agent-v3-model-probe-2026-09-16-summary.md#审查后的探针修复)。
