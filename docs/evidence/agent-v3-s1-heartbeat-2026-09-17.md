# S1 连续短任务心跳饥饿修复

## 已证实的问题

正式 Worker 在每个 Run 返回后，把下一次 idle heartbeat 推迟到当前时刻后5秒；该 Run 内的 lease heartbeat 同样从新的5秒 ticker开始。连续短于5秒的 Run会先结束并取消该ticker，又不断推迟idle heartbeat。Claim只验证会话，不续约；执行成功不能替代心跳。因此Worker仍工作时，原60秒会话会到期并撤销执行权。

2026-09-17收尾批的真实控制库只读取样：session `f1b08159-8d1b-41a0-874d-b8a6108c3a1e` 的 created_at = seen_at = `2026-09-16T16:31:12.972071Z`，expires_at = `16:32:12.972071Z`。会话从未收到续约。DEV-015 chat在 `16:32:12.912308Z` 结束且响应不完整，早于它的独立call deadline `16:33:11.861103Z`，随后批冻结。Go将服务端60秒权限定界到本地保守单调时间，时间与本地会话到期取消一致。此前没有读取阶段诊断，不能从缺失正文单独排除同时发生的网络故障；但心跳缺陷本身有代码、持久会话和确定性红绿证据，不再仅把问题归为供应商响应未知。

原首批session最后seen_at为 `2026-09-16T15:20:48.671627Z`，expires_at为 `15:21:48.671627Z`；其部分心跳与缺失原退出日志保留，不能仅凭新批事实断言原DEV-016唯一根因。既有Stop发布竞态是另一个已修复缺陷。

## 最小修复与验证

只删除每次Claim完成后重置idle heartbeat计划的代码，保持注册后的原计划以及真正idle heartbeat后的调度。长Run继续使用原lease heartbeat；所有会话/租约期限、迟到拒绝、fencing、Stop、Wait/Join和账本合同不变。此修复实现现有续约约定，不变更可靠性契约。

新增回归用例由真实runLoop/runClaim运行20次短任务，注入的单调时间每次前进4秒、累计超过原60秒会话；RPC和步骤结果为测试替身。修复前在第15次后执行权过期，idle heartbeat为0（15.01秒失败）；修复后要求完成20次且至少9次session-only heartbeat。修复后 `go test -race ./internal/runworker -count=1` 全套通过（26.378秒）。该单元用例不冒充真实PG/进程或云端验收。

原未知chat report、full hold、旧批冻结及退出记录全部保留。下一完整40例使用新身份、包含此修复的新镜像、既有真实业务/index和冻结评分器；不重启旧批。固定Linux/真实PG/进程门禁由最终PR CI执行，实际云端与各层证据分别记录。
