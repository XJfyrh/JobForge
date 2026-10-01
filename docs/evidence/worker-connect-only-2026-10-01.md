# 原实例恢复后的无凭据连接核查

2026-10-01 00:16～00:21 UTC，在恢复的原 JobForge 实例中核查。原分支 HEAD
`fc27fc6ec48741d7bbe8209e3b42e561fc845063` 存在且工作区干净。
本轮没有启动收费 Worker、没有读取供应商密钥、没有模型 API 或鉴权 HTTP 请求。

## 已确认结果

复用正式验收的 provider-diagnostics 镜像、UID 65532、host 网络、2 CPU/512 MiB/96 PID
限制、core=0 和只读 `/opt/jobforge-provider-ca.pem`。该公开 CA 的 SHA256 与当前既有
公共 CA 完全一致；非 root 可读，证书及主机名验证均开启。进程环境通过 env -i 明确重建，
既无通用代理变量，也无 DEEPSEEK_API_KEY。

既有代理 URL 的 scheme 为 http、port 为 8080，无 userinfo/path；没有把 HTTP 代理误作
HTTPS 代理。Python 正式 `AuthorizedDispatcher._client` 使用相同的显式 endpoint、
proxy、CA、trust_env=False、connect=5 和其他 timeout=60，但 bearer_key=None。
调用的只是安装包中原客户端构造方法，不运行 Agent 许可/计费或模型步骤。

| 检查 | 本次结果 |
|---|---|
| 标准库 socket CONNECT + SSLContext | CONNECT 200，目标 TLS 验证成功，0.239 秒 |
| 正式 HTTPX transport | TCP 0.041 秒，CONNECT 响应完成 0.169 秒，目标 TLS 完成 0.171 秒 |
| 目标 HTTP 请求 | trace 在 proxy.start_tls.complete 主动终止，未发送 |
| 额外防线 | 第二次 send_request_headers 开始前即终止，禁止隧道内 HTTP |

HTTPX trace 中首次 http11.send_request_headers/body 是代理 CONNECT 及空 body，
不是发向 DeepSeek 的模型请求。只输出 event 名称和耗时，不输出 trace info、headers、
代理地址或响应内容。容器自动删除，公开 CA/诊断脚本只读挂载；不建立持久权限。
最初辅助脚本因宿主默认权限不可读，在 Python 启动前失败；只调整这个无秘密脚本的读权限后完成检查，
没有调整 CA 权限或安全策略。

[脱敏事件记录](worker-connect-only-2026-10-01.json)随 Git 保存；工作区脚本和原日志位于
`.cache/verification/connect-only-diagnostic.py`、`connect-only-diagnostic.log`。

## 判断与限制

纠正一个比较前提：早期成功烟测实际使用 urllib.request，不是 HTTPX，原脚本证明这一点。
本次标准库和正式 HTTPX 在相同容器网络/CA 下均完成握手，未复现协议解释、CA 可读性、
TLS 校验、非 root 或 5 秒 connect timeout 的配置故障。Go 入口将两个显式配置字段原样
传给 executor，后者用白名单生成子进程环境；本轮另跑相关配置/匿名管道回归。

这证明当前连接前置条件可用，不能证明昨日失败请求发出或到达供应商，不能排除历史瞬时网络故障，
也不能验证发送业务 body 之后的真实响应/usage。历史失败的阶段 trace 没有保存，无法追回；
本轮没有发现应修改的运行配置，因此不调大 timeout、不关闭 TLS、不放开环境继承。

## 费用与再次验收条件

用户提供的控制台小时记录确认实际费用仅为成功烟测 **0.000076 元**（48 输入、7 输出）；
失败尝试目前没有计费记录。软件未知 hold **2.105344 元不是实际花费**，不阻碍本轮零收费诊断。
历史 JSON 账本保留原始事实，不为了报告好看改写；其中 0.000152 元是此前保守计价而非账户扣款。

再次真实整链路验收的连接前置检查已通过，但实模执行尚未获新一次批准。
需要用户明确批准新的隔离单任务，保留旧未知 hold，并在执行前重新核实当时价格和剩余额度；
选择带诊断的新镜像，在模型发出前预留上限。不能复用或重开旧冻结批次，不能把本次握手成功
当作已经通过 Worker 实模任务。若之后再失败，新的阶段/退出码诊断应保留供定位。

本轮配置回归：6 项 Go 顶层测试（含 race）通过，覆盖环境白名单、注入拒绝、代理/CA 显式范围、
凭据格式与匿名管道；Python provider network 19 passed。日志为
`.cache/verification/connect-only-config-{go,python}.log`。本轮仅提交证据文档，不改生产代码。

## 后续发现：分层检查遗漏中间筛选

随后获准的正式 Worker 重试揭示 coordinator 的 stepEnvironment 漏传 proxy/CA，
此前直接构造客户端的检查未经过该层。因此“客户端连接前置条件通过”不能替代完整传参验收。
具体根因、修复与新预留见[单任务重试记录](worker-retry-2026-10-01.md)。
