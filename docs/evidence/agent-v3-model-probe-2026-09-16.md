# Agent v3 S0 本地模型协议试验

- 日期：2026-09-16（Asia/Shanghai）；对应[路线 v3](../plans/agent-execution-roadmap-v3.md)。
- 状态：进行中；此记录不代表 S1/S2 业务验收通过。
- 实际运行：Ollama 0.32.5、Python 3.12.10、已有 httpx 0.28.1，无新增 Python 依赖、无付费调用。
- 范围：真实模型原生工具调用与结构化输出；工具返回均为[明确标注的合成 fixture](../../tools/agent_probe_data/README.md)，尚无真实业务服务或检索。

## 资源与候选

[环境快照](agent-v3-model-probe-2026-09-16-environment.json)记录物理 CPU、内存、Docker 资源及磁盘余量。主机为 Ryzen 7 7840HS（8 核/16 线程）、31.28 GiB 可见内存。Docker Desktop Linux VM 限额约 15.26 GiB；下载前可用内存采样约 8 GiB，C/E 两盘空闲均超过 50 GiB。其他项目容器继续运行，未停止或调整它们。

原有模型仅 `qwen2.5:0.5b`、`all-minilm:22m`，本机原生 Ollama 另有 `bge-m3`。旧抽取模型的成功不作为 Agent 能力证据。按照最多两个本地 chat 候选，先试 qwen3:4b，其时间门槛失败后再试 qwen3:1.7b。

| 候选 | 权重大小/许可 | 固定身份 | 状态 |
|---|---|---|---|
| qwen3:4b | 实际 2,497,293,931 bytes；Q4_K_M；Apache 2.0 | `359d7dd4bcdab3d86b87d73ac27966f4dbb9f5efdfcc75d34a8764a09474fae7` | 冷、暖两次均未完成首条调用，60s 超时 |
| qwen3:1.7b | 实际 1,359,293,444 bytes；Q4_K_M；Apache 2.0 | `8f68893c685c3ddff2aa3fffce2aa60a30bb2da65ca488b61fff134a4d1730e7` | 下载成功，试验中 |

模型目录与协议依据：[qwen3:4b](https://ollama.com/library/qwen3:4b)、[qwen3:1.7b](https://ollama.com/library/qwen3:1.7b)、[Ollama 原生工具调用](https://docs.ollama.com/capabilities/tool-calling)。目录能力标签只作为候选筛选，不能替代本机实测。

## 固定探针边界

[探针实现](../../tools/agent_model_probe.py)仅接受带显式端口的 loopback HTTP origin，拒绝凭据/路径/远程端点，关闭重定向、环境代理和隐式重试。模型名称和 manifest digest 显式传入并校验。

- 并发 1；`think=false`、`temperature=0`、`seed=42`、`num_ctx=4096`、`num_predict=1024`。
- 每条调用墙钟最多 60s，单案例最多 180s，最多 8 次 chat 和 6 次工具，最多一次协议纠正。
- 输出消息最多 16KiB、HTTP 响应最多 64KiB，模型输入消息最多 24KiB；本探针的小型上下文还在报告中记录实际 prompt token 数。
- 整条调用先校验，未知/多工具/错参数/跨对象不得局部执行。没有任意 shell、代码、URL、数据库或工具名的动态执行。
- 发请求前先计数。超时保留一次请求且 usage 为 unknown，不当成 0 token；不因失败自动重试。
- 超时/断流使后端完成状态不确定时停止后续探针；确认后端已取消且空闲后，才开始新的独立试验。
- 报告只保存结构检查、错误码、工具名称、耗时、usage 及响应 hash，不保存完整输入、输出或模型推理。

正常试验包括三个真实模型工具循环，一个单独 JSON Schema `format` 输出检查，以及两个“人工注入非法 assistant 调用后”的真实纠正观察。后两者明确不同于模型自然产生未知/畸形调用。注入工具内容攻击是 fixture 中的恶意文本，也不冒充真实攻击覆盖率。

## qwen3:4b 实际失败

| 试验 | 实际请求 | 结果 | 证据 |
|---|---:|---|---|
| 冷启动 | 1 | 首条 delayed 案例在约 60.016s 超时；未派发工具 | [冷启动 JSON](agent-v3-model-probe-2026-09-16-qwen3-4b.json) |
| 已加载复测 | 1 | 相同 profile 首条调用仍在 60s 超时；未派发工具 | [预热 JSON](agent-v3-model-probe-2026-09-16-qwen3-4b-warm.json) |

后端日志给出的模型加载耗时约 23.13s；预热复测缓存命中 402/403 输入 token，采样生成速率约 2.14～2.35 token/s，超时附近仍在生成。没有足够完整响应判断工具协议或结论质量。因此不能把失败简单归因于冷加载，也不能声称模型不支持工具调用；结论限于当前机器、共享容器环境与固定调用时间预算下未通过。

Ollama `/api/ps` 记录模型分配约 3.184GB、`size_vram=0`、实际 context=4096；容器点采样约 4.4～4.5GiB、CPU 约 784%。这是点采样，不是峰值内存、吞吐或稳定容量验收；未观察到 OOM。

两次断开后分别观察后端 `cancel task`、`release`、`all slots are idle`，没有立即用新的调用覆盖未知状态。第二次结束后只执行 `ollama stop qwen3:4b` 卸载本次模型内存，`/api/ps` 回到空；保留权重缓存用于复现。

## 远程备选与授权前缺口

如果两个本地候选都无法满足时间与协议边界，优先保留一个远程候选：阿里云百炼北京区 `qwen3.7-flash-2026-07-15`，只用纯文本、非思考、自定义只读工具，不开启内置联网或付费工具。[官方快照能力与价格](https://help.aliyun.com/zh/model-studio/qwen3-7-flash)显示其支持 Function Calling、结构化输出，输入不超过 32k 时输入/输出分别为 0.2/0.8 元每百万 token；本次没有实际调用验证。

开始远程探针之前仍缺：已开通且允许该模型的账号/地域、可信 endpoint、由本地环境注入的 Key、维护者明确授权的金额上限 B，以及跨重试/进程重启持久预留调用额度的实现。不要在聊天或报告中提供 Key。

费用算例仅供审阅：若保守限制每个请求总输入 8192 token（含工具 Schema/历史）、输出 1024 token，同一轮最多 27 次物理请求，则上述文本价格计算约为 `27 × (8192 × 0.2 + 1024 × 0.8) / 1,000,000 = 0.06636 元`。这不是授权额度、实际账单或完整 S2/S5 预算；额外试验、未知响应和后续业务评测另占预留。可先提议 S0 单批 B=1 元、27 请求硬限，但必须明确获准并落地额度后才能调用。无法对计费 token 作保守界定时，不能宣称这个公式保证硬预算。

当前探针故意不接受远程 origin，也没有凭据参数。远程适配需要单独实现和审查，不能把本工具临时改 URL 当作已具备持久费用治理。

## 验证与环境副作用

探针守卫测试 12 项通过；Ruff check/format、单文件 mypy 和 py_compile 通过。它们只是确定性代码检查；没有运行或声称 Go/PG/业务/RAG/恢复/费用/Trace 验收。

初始调查执行原生 `ollama list` 时，Ollama Windows 客户端自动启动了本机 app/server。根据创建时间（01:34:24/25）及 11434 无已建立连接，确认两个进程属于本次调查后已停止。后续所有下载/推理均使用既有 `jobforge-agent-rag-ollama-1` 容器的 11435 测试端口，没有修改原生 Ollama 配置或更新软件。

本次只新增模型缓存与本目录/工具中的探针证据；未修改服务配置、核心代码、数据库或历史模型。没有读取或输出 API Key，未调用远程模型。
