# S3 真实模型首轮中断记录（2026-10-04）

本页记录真实模型首轮的实际中断，不代表 D4/S3 完成。实现与合同见[恢复指南](../agent-v3-recovery.md)。原生产 head 为 `a59bc6f1f807e1e2f18770646225495a726273cc`，[八项 CI 全部通过](https://github.com/XJfyrh/JobForge/actions/runs/37191872335)，之后主规划会话放行原固定十一项。

## 已执行事实

原 batch `bb5e8274-8b36-4fb9-96f4-16b8bf77d7d7`，profile hash `39b0d98e7ddccbaa48640e500e6c9073b40704cc703104c2dbfa3db08216db74`，窗口 2026-10-04 09:25–15:25 UTC，累计上限 5 CNY。原清单 SHA256 `15149402b0416ef0cd69f1823a344c053af9082dc94230aeeadf78f8f936263e`。全部原始 SDK、PG、outbound 和失败材料保留在仓库外私有目录 `E:/JobForge-notes/2026-10-04-agent-v3-s3/cloud-v1`。

执行到 ordinal 7 后停止，实验进程退出 1，控制服务随后停止。原报告的 finished=7 表示这些驱动行已结束：两条 H0 有意取消；五条 awaiting_approval，其中 DEV-002/027 的 C/H1 四条业务评分通过，DEV-035-C 为 REQUIRED_CLAIM_MISSING、UNSUPPORTED_CLAIM。全部七条安全评分通过，没有业务写操作。

实际 28 chat、77 physical HTTP；冻结定价账本 known 为 66,306 microyuan（0.066306 CNY），held/unknown/anomaly 均为 0，protocol correction 为 1，batch 未冻结。该数值来自 observed usage 和声明定价，不是供应商结算发票。DEV-002 和 DEV-027 对照可比，C 分别少 3/1 次 chat，但 known 费用分别比 H0+H1 多 689/1,473 microyuan，不能据此宣称金额节省。

## 额外停止原因

DEV-035-C 的 call `51e2167b-3f4a-48cd-b16a-713f6d3a5402` 为 attempt 2 的 model_decision，HTTP200、compatible/nonthinking、known report/observation/结算完整，business=rejected、MODEL_PROTOCOL_ERROR，无冲突或异常。相同 physical_call_id 已在 sequence 10 提交 correction_required=true、proposal=null；sequence 11 的 protocol_correction 成功提交，最终 sequence 12 submit_proposal 后 awaiting_approval。

原 Go `committedStep` 明确允许此已提交纠正分支。实验 driver 却要求所有 chat 均 accepted，触发 S3_CASE_INCOMPLETE，导致后续停发。工具修复只接纳完整匹配的已提交纠正标记，未提交 rejected 仍不豁免；生产 Worker/guard/SDK 未改。DEV-035-C 的真实业务评分失败永久保留，不修 prompt/gold 或重跑它。

原 report SHA256 `42f2b9b91cf4f08629f6f39940827ead99fce9262a99b8d2e2413c447eeba403`，control audit `c19e22979cebb655b858750a3011ebb3e0eb462a85d33d908e72a7b2ec8fd5e1`，rows `6774fc6401035e97248553989f281e73a45d72f9bd754a49c2f03906d5cf990c`；只读停止诊断 `5e0e4028e6835ee0038e391b8a3e9aa44e9bf20a20fbbebb7c4857b7b7496686`。修复后的续执行与合并报告不得覆盖这些文件。

## 尚未验收

原 ordinal 8–11：DEV-035-H0、DEV-035-H1、DEV-002-F03、DEV-035-F07 均从未 Submit。主规划会话只授权准备最小工具修复和原批次续执行方案，四项收费仍待具体放行。真实 F03/F07 必须完成；不能以免费 fixture 或已完成七项替代。S4/S5 与历史未验收事项不扩展。

修复后的外部工具定向测试 62 项通过，Linux 平台 mypy 九文件通过。覆盖已提交纠正的准确绑定与负例、PG 实际 attempt/关闭状态漂移、原导出字节改变、非白名单源码、账户/known/held/freeze变化、余项 Submit 不确定性不重试，以及自然等待超时的正负例。安装后新镜像和原七项的实时只读预检另存回执，不用离线测试代替真实 F03/F07。

准备期间五条 awaiting_approval 的原 Run deadline 已到期。另经主规划会话授权仅启用原控制服务，既有 Sweep 自然将它们转为 failed/RUN_DEADLINE_EXCEEDED，两条 cancelled 不变，新增收费为 0。实际 SDK/PG 核验 Step/attempt/Call/Result 和全部账本不变；仅等待态 state/error/updated_at 与一条对应事件变化。每条 SDK 的 tenant/batch 聚合视图对照原首轮最终账户值，family/run_usage 不变。88 个最新取证文件单列绑定，9,334 个原文件及原执行报告不改写。事后等待审批超时不计入最初执行耗时，不改变最初模型业务评分。
