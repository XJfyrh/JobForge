# 安全策略

## 支持状态

JobForge 尚未发布可运行版本，因此当前没有受支持的发布分支或安全补丁承诺。首个公开版本发布时，本文件必须同步更新支持范围。

## 私密报告漏洞

建立 GitHub 远程仓库后，请通过仓库 **Security** 页面中的 Private Vulnerability Reporting 私密提交漏洞，不要创建公开 Issue、Discussion 或 Pull Request。

报告应尽量包含：

- 受影响的版本、commit 或组件；
- 可复现步骤和最小验证材料；
- 可能影响、攻击前提与已知缓解方式；
- 已去除凭据、个人信息和敏感业务数据的日志。

维护者会先确认是否收到报告，再评估影响、修复方式和披露时间。项目当前不承诺固定响应 SLA。

## 安全边界

- 不接受真实 API key、Authorization header、私钥或生产 payload 作为复现材料。
- 不运行用户上传的任意 shell 或代码，只执行预注册 Handler。
- pprof、metrics 和管理端点不得默认暴露到公网。
- 在修复公开前，请避免披露可直接利用的细节。
