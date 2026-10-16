# S5 v6 开发验证：补证有效，行动政策仍有遗漏

2026-10-13，按 [ADR-0036](../adr/0036-s5-controlled-policy-supplement.md)执行新的开发 40 案清单。DEV-020、DEV-031 实际触发补证并通过；DEV-033 未申请所需状态政策，最终输入双闭合失败，逐案守卫停止剩余提交。**原 40 分母为 32 通过、1 失败、7 未执行；不冻结、不启动后继批次、不补跑。**

## 实际结果

| 项目 | 结果 |
|---|---|
| 已执行 | 33 Runs、177 chat、429 次物理调用 |
| 正确 / 完整案例证据 | 各 32/40；已执行案为 32/33 |
| 调查 marker 双闭合缺口 | DEV-020、DEV-031、DEV-033，3/33 |
| 最终输入双闭合缺口 | DEV-033，1/33；其余 32 案完整 |
| 补证 | DEV-020、DEV-031 各一次，均成功；无第二次补证或格式重试 |
| 四类硬失败 | 越权、未经批准写入、重复效果、费用突破均为 0 |
| 推理截断风险标记 | 0；未执行的 7 案不计通过 |
| 停止 | 第 33 案导出后 `FINAL_DEFINITION_CLOSURE_GAP`、`FINAL_RETRIEVAL_CLOSURE_GAP`；launcher exit 1，PID 0 |

前 10 案在原进程暂停，独立核对 52 个实际 chat 参数哈希、事实、边界条件、来源和费用后继续。结果为 10/10，未提前提交第 11 案。批次结束后收费、控制与业务配套容器均已退出。全表 `proposal_audit_complete=false` 保留，未执行案没有方案可供审计。

## 两次补证

DEV-020 调查只取得 P02.1/P07.1/P07.2，初稿遗漏含糊问题描述。首次 thinking 返回严格 `missing_facts` 意图，请求 P03.1/P03.2；Go 检索实际取得 P03.1/P06.2/P03.2，之后再次独立终裁，输出 `request_information / insufficient`，要求补充 `ticket.problem_description`，引用实际描述及 P03 政策。两次 completion 为 7,278/870，耗时 43.114/5.165 秒，裁决阶段估算 ¥0.071849，整案 chat ¥0.080850。独立审查重建该案全部 6 个实际请求哈希，确认第二次输入不含初稿、意图或前次推理。

DEV-031 的调查缺少工单状态政策。补证后取得 P08.1/P08.2，最终 `no_action` 与时效结论均正确，状态主张和时效主张完整。两次 completion 为 4,292/1,820，耗时 25.655/11.226 秒。两案 marker 原缺口保持可见，没有用补证后的输入覆盖原记录。

## DEV-033 为什么失败

捕获工单为 `informational_only`，描述明确要求只回答送达时效、不更新工单。调查仍只取得 P02.1/P07.1/P07.2。最终方案正确判断 `delivered_late / delayed`，也保留了 `target_ticket_status=informational_only`，却选择 `decision=proposal / action=record_conclusion`，没有必需的 `ticket_status:informational_no_action` 主张或 P08.1。

这不是截断：终裁 3,385 reasoning / 3,480 completion tokens，21.477 秒，120 秒期限；HTTP 与计量完整。已确认的是**状态事实存在、时效判断正确、行动及其必要政策缺失、未触发补证**。受控补证可以修复模型已识别的缺口，不能保证模型发现全部行动规则依赖。

独立核验进一步确认：DEV-031/033 的实际输入都含 `allowed_ticket_status_modes=["informational_no_action"]`，且补证目录允许申请 P08.1；DEV-033 仅用 3 个逻辑工具，申请该段落可通过意图校验。不是状态投影丢失、补证入口不可用或额度耗尽；模型内部为何未选择补证没有可验证结论。

下一方向是将行动规则的取证要求独立于模型拟写的主张：先核验实际工单状态所需的政策，再允许最终行动裁决。应离线比较 DEV-031/033 及全部已见特殊状态样本，确认适用边界后形成独立新决策；本轮不据失败题直接修改提示词，也不增加调用。

## 费用与验证

本批 observed-usage 估算 **¥1.053030**。S5 26 库累计 **502 Runs、2,106 chat、¥11.644792**，held/unknown/inflight/unfinished attempt 均 0；¥28 上限剩余 **¥16.355208**。金额按冻结峰值费率计算，不是供应商结算账单。

免费机制沿用[已通过记录](agent-v3-s5-v6-mechanism-2026-10-13.md)，本次新增宿主 `guard.py`、最终闭合停发测试与 CI 镜像排除检查。S5 和外部 driver/host gate 共 **188 项通过**；Ruff、格式、mypy 19 文件及 `git diff --check` 通过。三层实际镜像的安装源码、继承层、registry 与 gold/host-only 文件隔离核验通过。远程 8 项 CI 未运行，已见 20、Fixed、新未见集、恢复和演示未启动。

独立代码及精确 manifest 审查由 `/root/s5_design_review` 完成。manifest SHA-256 `07175003f8a20a4d9af8989c1115eeca9e9f092b2e8b8cbd91bea9107f0f7644`；profile `3facf0b5fdca5905484427c33a7b08a7bbaae82f10ea3704d107a11049d0dfd9`。本次为未提交开发源码快照，不是正式冻结。

批后独立复算原导出和评分一致，177 个实际 chat 参数哈希全部一致；429 个物理调用无重复、各 Run 无 attempt 重试，业务事实和回执未变。结论是“证据核验通过，质量未通过，保持停止”，转录记录为私有 `result-review.json`。

[机器摘要](agent-v3-s5-v6-development-2026-10-13.json)保留逐案结果、阶段计量、最终账本、停止事实及工件哈希。私有原始证据位于 `E:/JobForgeEvidence/s5-2026-10-07/v6-20261013/`，历史 v5 证据未改。
