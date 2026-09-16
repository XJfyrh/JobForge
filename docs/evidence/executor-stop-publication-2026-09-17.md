# 执行器停止通知竞态修复

这是对[ADR-0019](../adr/0019-executor-confirmation-and-exit-contract.md)既有非阻塞Stop和有界清理合同的修复，不改变公开接口、任务状态、期限、调度或数据库语义。

## 缺陷与最小改动

`Process.Stop()`先CAS设置`stopping=true`，再关闭`p.stop`唤醒生命周期协程。两个调用并发时，第一个可能停在CAS与close之间；生命周期协程从context取消路径调用第二次Stop，CAS失败立即返回。旧代码只检查`p.stop`是否关闭，因此可能跳过TERM及两个清理计时器。后续循环不再读取这个通知，metering writer又在等`p.closed`，最终无法Join。

修复只将这个判断改为读取已经发布的原子`stopping`状态。TERM宽限100ms、KILL与2秒清理期限保持原值，不增加重试、锁等待或后台调度。

## 实际验证

同包回归`TestContextCancellationDuringStopPublicationCleansProcess`启动真实Linux子进程，以ready帧确认启动，固定“CAS已完成、通知未发布”的合法中间态后取消context。测试检查原期限内真实KILL、Wait、EOF、Join及进程组消失。失败清理只在断言之后强杀并唤醒writer，不能用来通过正常断言；不依靠概率sleep。

- **旧代码红灯**：同一固定race镜像、相同测试，3.21秒以`context cancellation waited for an unpublished Stop notification`失败；没有卡住测试清理。
- **修复后绿灯**：新增回归0.26秒通过；受影响的固定Linux进程套件19个顶层测试全部通过，0失败、0跳过、0race warning。实际使用`--init --network none --cpus 1 --memory 384m --pids-limit 64`。
- 独立上下文Agent只读审查该条件、真实时序及回归测试，无P1/P2；完整仓库强制检查由修复PR的CI执行。

复现：按[运行时指南](../agent-v3-runtime.md)构建`process-check`镜像，执行`/app/process.test -test.run=^TestContextCancellationDuringStopPublicationCleansProcess$ -test.v -test.timeout=15s`；去掉`-test.run`运行该包全部进程检查。生产镜像不包含test peer。

## 证据边界

调查线索是[CI 35117180332](https://github.com/XJfyrh/JobForge/actions/runs/35117180332)的`before-commit-lock`收尾超时。原失败没有此处栈，不能直接认定它或此前另一CI超时就是本竞态。定向原用例在宿主PG网络及相同容器网络分别10次通过；后续[带栈CI 35118249801](https://github.com/XJfyrh/JobForge/actions/runs/35118249801)八项通过。这些不覆盖原失败记录；本修复的直接证据是上述确定时序红绿结果。

首批真实云端中断原因仍缺直接证据，业务提示质量仍待真实复验；本修复不表示S1或整个路线完成。历史W4性能门禁失败、AT-25跳过和生产长期留存未验收继续保留。此改动只影响停止清理分支，没有修改Claim或数据库热路径，未追加性能基准。
