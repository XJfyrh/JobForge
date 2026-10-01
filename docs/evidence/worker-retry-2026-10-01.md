# 单任务重试：定位并修复模型步骤丢失代理配置

基线 `085d08fa75ba51b584f48e0362ec9b7cd159d679`。用户明确批准新的单合成任务、并发 1、
本批上限 3 元计入原 50 元；保留旧未知 hold，不落盘凭据，不改变固定 endpoint、TLS 或环境隔离。
重新核实官方价格页 HTTP 200，SHA256 仍为
`5a7b1832592387340f2fc456399b34b89b05f3fa167c2e35909e2fa4afe021e3`；
Flash 高峰每百万输入未命中 2 元、缓存命中 0.04 元、输出 8 元。
正式 profile 输出上限 1024，输入合同上界 1,048,576，单 chat 最坏预留 2.105344 元。
本轮没有降低合同上界绕过预算，也没有更换供应商。

## 实际结果

真实任务未生成方案，退出后没有自动再试。模型配置为 deepseek-flash，未得到响应模型或 usage。

- Run：`3699c1ae-977d-4fa7-a3a6-efe8aed344f7`。
- 唯一 chat physical call：`f3eef6b7-d7e3-4e8f-a1b6-48dccbba32ae`，attempt 1、ordinal 1。
- 2026-10-01 00:38:19.998941 UTC 预留；00:38:20.108117 开始 origin TCP；
  00:38:25.109654 TCP 失败；00:38:25.121488 未知报告落 PG。
- trace 只有 `connection.connect_tcp.started/failed`，hop=origin；没有 CONNECT、TLS、
  HTTP headers/body 发送或响应事件。错误为 `connect_timeout`，已缓冲字节 0。
- 本次已定位在 TCP 建连阶段，不能说模型 HTTP 请求已发出或供应商接受。
- Worker exit code 1，两个 guardian 进程组消失；批次 `CHAT_USAGE_UNKNOWN` 冻结。
  harness 未启动 scanner，导出时 Run 为 running，不称业务终态成功。

完整[脱敏账本与阶段证据](worker-retry-2026-10-01.json)已入 Git。
运行镜像：`sha256:9040e74dcf5cc24d69dd4cdb2139373a46c23b97c68b898afefbc4fd90ba5064`。
该镜像包括本次新增的闭集 HTTPcore 阶段诊断，不含后续代理传递修复。
原始目录：`.cache/verification/approved-worker-retry-20261001/`。

## 根因与修复

**确定缺陷：`internal/runworker/coordinator.go` 的 stepEnvironment 为模型步骤筛选环境时，
只复制 DeepSeekKey，遗漏 DeepSeekProxyOrigin 和 DeepSeekCAFile。**
入口已读取这两个字段，底层 childEnvironment 也支持它们，但中间筛选将其清空。
Python 因而构造直连 transport，这与本次 origin TCP 超时 trace 一致。

这是先前代理支持的实现遗漏。之前的入口、executor、Python 分层测试以及手工客户端握手
绕过了该筛选环节，未覆盖完整传递链。昨日原请求缺少阶段 trace，不能用本次记录补造昨日发送事实。

修复为只向三个模型步骤传递既有明确配置的 key/proxy/CA；业务、embedding 和其他步骤继续
不接收供应商配置。不启用 ambient proxy，不扩大目标 allowlist，不关闭 TLS，不调大 timeout。
新增回归在修复前对三个模型步骤全部失败，修复后通过；同时检查非模型步骤的负面隔离。

诊断新增 `.transport.jsonl`，只记录固定 event、hop、时间和 physical_call_id。
不序列化 HTTPcore info、异常文本、URL、headers 或正文；区分代理 CONNECT 和目标请求。
文件仅在已启用的短期审计目录写入，沿用清理前导出流程；不影响计费、授权或重试语义。

## 费用与边界

| 项目 | 人民币 | 含义 |
|---|---:|---|
| 用户控制台已确认实际费用 | 0.000076 | 早期成功烟测 |
| 旧请求软件预留 | 2.105344 | 非实际费用，保留 |
| 本次软件预留 | 2.105344 | 未知报告导致保留，非实际费用 |
| 已确认费用加两笔预留 | 4.210764 | 保守占用，不是账单 |
| 50 元扣除上述占用 | 45.789236 | 预算余量，不是自动重试授权 |

本次没有 usage 或供应商扣款记录，不把软件 known_cost=0 当成账户账单结论。
阶段证据表明本次停在模型 HTTP 发送之前；没有因此擅自改写软件冻结或退还 hold。
旧账本原封保留。临时 PG、Worker、诊断容器和匿名卷全部清理，镜像和脱敏证据保留。
构建一度磁盘满，只清理未使用的可再生构建缓存，仓库与账本未删。

修复后没有再次执行模型请求。再次实模前必须使用重新构建的修复镜像并取得新的明确批准，
保留累计预算边界、重查价格；不能把本次离线修复称为完整 Worker 实模验收通过。

验证：新增筛选回归先红后绿；`go test -race -count=1 ./cmd/agent-worker ./internal/runworker ./internal/runexecutor`
通过，普通层专用进程 skip 不充作本次独立完整进程验收。Python 全套 1814 passed；
39 项网络/出站诊断专项通过，Go lint 0 issues，Ruff check/format、Agent/启动器 mypy 通过。
本轮未在修复后重建/运行真实模型镜像，旧 `provider-retry` 标签仍是失败时版本。
日志：`.cache/verification/retry-env-regression-{before,after}.log`、`retry-python-full.log`、
`retry-preflight-tests.log`、`retry-go-lint.log`。没有推送、PR 或部署。
