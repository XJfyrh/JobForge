# ADR-0025：显式限定供应商代理与临时凭据输入

状态：本分支实现提案；实际安全配置与真实调用须取得本次单独确认

## 问题与边界

正式 Worker 清空子进程环境，Python transport 不信任环境代理。该默认值必须保留；云环境已有的安全鉴权代理不能通过继承全部环境接通，也不能将真实 key 写进 Docker 配置或凭据文件。

## 决定

- 可信部署配置按租户可选提供 `deepseek_proxy_origin` 与 `deepseek_ca_file`，两者必须同时设置。默认仍直接连接，不读取通用代理/CA环境。
- 代理只支持无用户信息、路径、query、fragment 的 HTTP(S) origin；CA 仅接受绝对规范路径。由部署者只读挂载公共 CA 文件，Worker 只传递这两个明确命名的字段。
- Python 仅为固定 `https://api.deepseek.com` endpoint 创建显式代理 transport；业务和 embedding endpoint 拒绝这些字段。保留 hostname/证书验证、禁止重定向、零 HTTP 重试，不放宽模型 URL 或调用授权。
- 不增加通用环境凭据查找接口。现有凭据读取器可从匿名管道路径（如 `/dev/stdin`）读取一次 JSON。实际验收 launcher 仅在获准后，用已有环境凭据在内存中构造输入并交给 Worker；不输出、不写文件、不把秘密放进 Docker 创建配置。
- 本轮确认只覆盖短期隔离验收，不授予持久秘密访问或生产出站配置权。任何后续生产接入仍需独立决定。

## 验证

验证默认环境隔离、代理字段配对与 URL/路径拒绝、非 DeepSeek endpoint 拒绝、CA 加载失败闭锁、显式 transport 配置与业务直连。实际端到端另要求真实 PG/gRPC、正式 Worker/生产 adapter、真实 DeepSeek、逐物理调用审计与有限预算；离线检查不冒充收费验收。
