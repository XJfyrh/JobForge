# S1 历史未知费用准入与诊断增量

维护者要求重新评估过严的计量确认决策；[ADR-0023](../adr/0023-held-unknown-cross-batch-admission.md) / PRD v0.15 将符合条件的历史传输失败与独立新批预算准入分开。旧 unknown 全额 hold、旧批冻结、批内 guard 和成功 Commit/退出要求不变。此前[14 案方案/DEV-015 中断记录](agent-v3-s1-closeout-2026-09-17.md)保留，底层中断原因尚未定位。

## 最小实现

- `prepare-support --batch-cost-microyuan` 只降低共享 batch 的持久费用上限，接受 1～5,000,000，省略保持原默认；读取 support 部署时再次拒绝零、负数和超过 5 CNY 的 batch cap。配置摘要、inspect 和注册评分使用同一值，未增加重复协议字段。
- profile/family/tenant、次数/token、40 案、6h/单 Worker 不变。PG 原三账户事务预留负责实际上限，不引入全局预算平台、退款或自动续跑。
- 响应未完整读取时追加 `<call>.failure.json`，只记录固定阶段/原因及已缓冲字节数。原异常原样继续抛出，没有新重试或延长 deadline；usage/report/停止逻辑不变。既有 `.jsonl` 出站证据形状和评分器不改；旁路诊断不构成完整响应或成功证据。
- source 样例的 Python adapter 源集合摘要同步；旧已冻结 source/profile/产物不覆盖。

## 已运行的定向验证

| 检查 | 实际结果与边界 |
|---|---|
| Go prepare/部署/source | `go test ./cmd/agent-control -run 'TestPrepareSupport\|TestDeployment\|TestSupportSource' -count=1` 通过；覆盖默认/较小cap、非法cap、两配置及hash、历史输出不覆盖 |
| 真实 PG/race | Windows 按 AGENTS 启动 5433 测试 PG，`TestRunSupportReducedBatchCostCapAndImmutableHold` 三子例通过；cap 2,105,343 拒绝首 chat，无调用行/部分扣账/Run更新；2,105,344 与 2,846,003 接受原保守预留 |
| 跨租户共享额度 | 真实 PG/race 既有竞争测试扩展 chat/cost 两子例，均通过；两个租户争最后一份许可只允许一方成功，失败方无 family/tenant 部分扣账，共享实际金额恰为一份保守预留 |
| unknown 与重放 | 上述 PG 用真实 support profile、Reserve、首 report、Inspect、CreateBudget 验证 full hold 2,105,344 保留且仍冻结；相同 setup 不变，扩大已存在 cap 返回冲突；租户仍为原5 CNY |
| Windows 出站诊断 | 18 项通过，实际 loopback TCP 断流、header/大小拒绝、读体途中取消；确认无自动重发、unknown仍保留、诊断无正文/凭据且保留首事实 |
| 固定 Linux HTTP/派发 | 既有固定 `jobforge-agent-runtime:audit-python` 镜像、`--init --network none`、只读挂载实际源码，dispatch+outbound **87 项通过，无 skip**；不调用模型 |
| 格式/类型 | 变更 Python Ruff format/check通过；Linux mypy 对两变更模块通过。首轮定向 mypy 未配置源码查找路径，误读无py.typed的已安装包而报9个import-untyped；显式MYPYPATH后通过，未改规则抑制错误 |

原三账户并发、批内审计和正式进程故障证据继续有效；本次不重复无关全量本地测试，最终提交仍由必需 CI 执行全仓 race/真实 PG/Python/SQL/Buf/实际 Linux 进程门禁。独立审查及最终 CI 与部署绑定分别记录在 PR 中。

## 收费前置与范围

后续真实运行必须在新决策已接受、实现独立审查和必需 CI 完成后，重新核对所有旧进程退出/窗口结束、原未知报告和 full hold，以及官方模型/定价和账户可用性。本次上一取样的剩余额度为 2,846,003 microyuan；实际准备器须显式设置最新核对的 cap，不把本页金额当自动许可。

本文件只证明实现与确定性检查，不代表新40案成功。本次准备暂沿用累计5 CNY核算。维护者随后明确授权随任务合理调整预算；如需扩额，记录新的累计上限与部署依据即可，不再把原5 CNY视为外部阻塞。旧known/hold不释放，gold/scorer不改变；S1验收标准不降低，S2～S5不启动。历史W4、AT-25、RQ-06及生产长期留存限制保留。
