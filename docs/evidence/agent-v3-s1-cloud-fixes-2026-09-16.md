# 首批失败后的最小修复

> 阶段历史记录。2026-09-17 的修复合并、累计新授权及实际重验见[最新收尾证据](agent-v3-s1-closeout-2026-09-17.md)；本页原批事实不改写。

基线为 `2991138`，原始[真实验收报告](agent-v3-s1-first-cloud-2026-09-16.md)及全部40案评分保留。此次只处理实际发现的诊断缺口和业务提示歧义；没有新模型请求、Submit、重启原批次或放宽评分。

## 改动与依据

- launcher 保存停止触发来源及两个子进程实际退出码，保留 Worker stderr。Worker 只按本地错误身份输出固定类别，清理失败额外输出已有 Wait/Join/进程组事实，不输出异常原文、模型正文或秘密。没有增加日志服务、状态、重试或调度语义。
- 只读检查全部10个持久方案，均有把 open 工单误断言为既有特殊状态的额外 `ticket_status`；其中9个方案唯一业务失败就是这条主张。DEV-003另把未来承诺误判为逾期不足48小时。提示明确“已捕获状态”和“建议目标状态”的区别、最少必要主张、相关政策引用及先比较时间先后再计算阈值。仍只有一次纠错；生产校验仍按结构与来源，不把评分器或 gold 放入 Worker。
- 示例 source 的 prompt/adapter 内容摘要随源码同步；原批次受保护 profile、登记、镜像和证据保持原值。`support-fixed-prompt-v1` 仍标识同一六字段提示合同，具体实现由内容摘要区分。新源码不能冒充首批已冻结部署。

## 定向验证

| 检查 | 实际结果与范围 |
|---|---|
| Go Worker入口/协调器 | `go test ./cmd/agent-worker ./internal/runworker`通过；含原清理失败分支及新增错误文本不泄露检查 |
| Linux launcher真实子进程 | 固定已有测试镜像、只读挂载新源码、`--init --network none`；4项通过，4.28秒；包含Worker退出诊断、driver退出/SIGKILL、强杀和Wait。子进程是测试helper，不是收费模型 |
| 业务适配与输出合同 | `test_support_adapter.py`和`test_support_contract.py`共158项通过；不证明新提示的真实模型质量 |
| 离线准备/源码摘要 | `TestPrepareSupport*`与`TestSupportSourceExampleMatchesReviewedFilesButRequiresReceipts`通过；未创建收费profile |
| Ruff/类型 | 改动Python文件ruff通过；launcher的Linux mypy通过。首次mypy因缺`--explicit-package-bases`产生模块重复错误，修正调用后通过，未改代码绕过 |

完整强制门禁由同一PR CI执行，不在本地重复无关套件。基线 `2991138` 的[八项CI](https://github.com/XJfyrh/JobForge/actions/runs/35115836473)已全部通过；不以基线结果代替本补丁或业务验收。

实现提交 `2b5b5b7` 已经独立上下文审查，无P1/P2；[CI 35117180332](https://github.com/XJfyrh/JobForge/actions/runs/35117180332)实际结果为**7项通过、1项失败**。失败是既有 `TestRunExecutorObserveUnconfirmedHasNoFollowingHTTP/before-commit-lock`：真实PG阻塞窗口已建立，但第二次Claim进入信号20秒未到，取消Worker后5秒仍未Join。该次launcher正式联合故障测试通过。不能将此失败归为已证明的环境波动，也不能与首批实际中断直接视为同一根因。

为这个实际失败在原20秒超时点补充最多128KiB goroutine栈，不改变超时、故障锁或执行语义。用当前固定Linux/race镜像、真实测试PG、与CI相同2CPU/512MiB/96PID限制定向执行该失败子例10次，全部通过；Windows Docker到宿主PG网络与CI的host网络不同。**未复现不等于已修复**；没有因10次通过删除CI失败记录，也没有追加无关全仓重跑。

## 仍未解决

DEV-016原退出触发来源及根因仍缺直接证据。最后embedding Reserve后约1.12秒launcher开始停止，早于该调用10秒截止；没有chat预留或FailAttempt。driver记录操作停止只能证明收到信号，不能证明谁最先退出。未据猜测修改超时、租约或清理逻辑。

提示修复尚未经过新的真实模型运行。PR #51保持Draft、不合并，S1及S2～S5未完成。后续真实批次受ADR-0021的停止后运行安排约束，不得改原标记/身份续跑。历史W4性能门禁失败、AT-25跳过、生产长期留存未验收及此前未定位runtime CI超时继续保留。
